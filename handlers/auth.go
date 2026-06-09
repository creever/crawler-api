package handlers

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"golang.org/x/crypto/bcrypt"

	"github.com/creever/crawler-api/middleware"
	"github.com/creever/crawler-api/models"
)

// AuthHandler handles authentication endpoints.
type AuthHandler struct {
	db               *mongo.Database
	jwtSecret        string
	jwtExpirySec     int
	jwtRefreshExpSec int
}

// NewAuthHandler creates an AuthHandler.
func NewAuthHandler(db *mongo.Database, jwtSecret string, jwtExpirySec, jwtRefreshExpSec int) *AuthHandler {
	return &AuthHandler{
		db:               db,
		jwtSecret:        jwtSecret,
		jwtExpirySec:     jwtExpirySec,
		jwtRefreshExpSec: jwtRefreshExpSec,
	}
}

// ---------------------------------------------------------------------------
// Request / response shapes
// ---------------------------------------------------------------------------

type registerInput struct {
	Email    string `json:"email"    binding:"required"`
	Password string `json:"password" binding:"required,min=8"`
	Name     string `json:"name"     binding:"required"`
}

type loginInput struct {
	Email    string `json:"email"    binding:"required"`
	Password string `json:"password" binding:"required"`
}

type refreshInput struct {
	RefreshToken string `json:"refresh_token" binding:"required"`
}

type logoutInput struct {
	RefreshToken string `json:"refresh_token" binding:"required"`
}

type tokenResponse struct {
	AccessToken  string      `json:"access_token"`
	RefreshToken string      `json:"refresh_token"`
	ExpiresIn    int         `json:"expires_in"`
	User         models.User `json:"user"`
}

// ---------------------------------------------------------------------------
// Register
// ---------------------------------------------------------------------------

// Register godoc
// @Summary Register a new user account
// @Tags auth
// @Accept json
// @Produce json
// @Router /api/v1/auth/register [post]
func (h *AuthHandler) Register(c *gin.Context) {
	var input registerInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	input.Email = strings.ToLower(strings.TrimSpace(input.Email))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Check for duplicate email.
	count, err := h.db.Collection("users").CountDocuments(ctx, bson.M{"email": input.Email})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "registration failed"})
		return
	}
	if count > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "email already registered"})
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(input.Password), 12)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "registration failed"})
		return
	}

	// First registered user becomes admin automatically.
	total, _ := h.db.Collection("users").CountDocuments(ctx, bson.M{})
	role := models.UserRoleUser
	if total == 0 {
		role = models.UserRoleAdmin
	}

	now := time.Now().UTC()
	user := models.User{
		ID:           bson.NewObjectID(),
		Email:        input.Email,
		PasswordHash: string(hash),
		Name:         input.Name,
		Role:         role,
		Active:       true,
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	if _, err = h.db.Collection("users").InsertOne(ctx, user); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "registration failed"})
		return
	}

	resp, err := h.buildTokenResponse(ctx, user)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "token generation failed"})
		return
	}
	c.JSON(http.StatusCreated, resp)
}

// ---------------------------------------------------------------------------
// Login
// ---------------------------------------------------------------------------

// Login godoc
// @Summary Authenticate and receive tokens
// @Tags auth
// @Accept json
// @Produce json
// @Router /api/v1/auth/login [post]
func (h *AuthHandler) Login(c *gin.Context) {
	var input loginInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	input.Email = strings.ToLower(strings.TrimSpace(input.Email))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var user models.User
	if err := h.db.Collection("users").FindOne(ctx, bson.M{"email": input.Email}).Decode(&user); err != nil {
		// Use the same error message to prevent user enumeration.
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid email or password"})
		return
	}

	if !user.Active {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "account is disabled"})
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(input.Password)); err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid email or password"})
		return
	}

	resp, err := h.buildTokenResponse(ctx, user)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "token generation failed"})
		return
	}
	c.JSON(http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// Refresh
// ---------------------------------------------------------------------------

// Refresh godoc
// @Summary Exchange a refresh token for a new access + refresh token pair
// @Tags auth
// @Accept json
// @Produce json
// @Router /api/v1/auth/refresh [post]
func (h *AuthHandler) Refresh(c *gin.Context) {
	var input refreshInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tokenHash := hashToken(input.RefreshToken)
	var stored models.RefreshToken
	err := h.db.Collection("refresh_tokens").FindOne(ctx, bson.M{
		"token_hash": tokenHash,
		"expires_at": bson.M{"$gt": time.Now().UTC()},
	}).Decode(&stored)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired refresh token"})
		return
	}

	// Rotate: delete old token immediately.
	_, _ = h.db.Collection("refresh_tokens").DeleteOne(ctx, bson.M{"_id": stored.ID})

	var user models.User
	if err = h.db.Collection("users").FindOne(ctx, bson.M{"_id": stored.UserID}).Decode(&user); err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "user not found"})
		return
	}
	if !user.Active {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "account is disabled"})
		return
	}

	resp, err := h.buildTokenResponse(ctx, user)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "token generation failed"})
		return
	}
	c.JSON(http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// Logout
// ---------------------------------------------------------------------------

// Logout godoc
// @Summary Revoke the current refresh token
// @Tags auth
// @Accept json
// @Produce json
// @Router /api/v1/auth/logout [post]
func (h *AuthHandler) Logout(c *gin.Context) {
	// Auth is validated inline here so the /auth group stays fully public.
	authHeader := c.GetHeader("Authorization")
	if authHeader == "" || !strings.HasPrefix(authHeader, "Bearer ") {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authorization header required"})
		return
	}

	var input logoutInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tokenHash := hashToken(input.RefreshToken)
	_, _ = h.db.Collection("refresh_tokens").DeleteOne(ctx, bson.M{"token_hash": tokenHash})

	c.JSON(http.StatusOK, gin.H{"message": "logged out"})
}

// ---------------------------------------------------------------------------
// Me
// ---------------------------------------------------------------------------

// Me godoc
// @Summary Get the currently authenticated user
// @Tags auth
// @Produce json
// @Router /api/v1/auth/me [get]
func (h *AuthHandler) Me(c *gin.Context) {
	userID := middleware.GetUserID(c)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var user models.User
	if err := h.db.Collection("users").FindOne(ctx, bson.M{"_id": userID}).Decode(&user); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}
	c.JSON(http.StatusOK, user)
}

// ---------------------------------------------------------------------------
// Token helpers
// ---------------------------------------------------------------------------

func (h *AuthHandler) buildTokenResponse(ctx context.Context, user models.User) (*tokenResponse, error) {
	accessToken, err := h.signAccessToken(user)
	if err != nil {
		return nil, fmt.Errorf("sign access token: %w", err)
	}

	rawRefresh, err := generateRawToken()
	if err != nil {
		return nil, fmt.Errorf("generate refresh token: %w", err)
	}

	expiry := time.Now().UTC().Add(time.Duration(h.jwtRefreshExpSec) * time.Second)
	rt := models.RefreshToken{
		ID:        bson.NewObjectID(),
		UserID:    user.ID,
		TokenHash: hashToken(rawRefresh),
		ExpiresAt: expiry,
		CreatedAt: time.Now().UTC(),
	}
	if _, err = h.db.Collection("refresh_tokens").InsertOne(ctx, rt); err != nil {
		return nil, fmt.Errorf("store refresh token: %w", err)
	}

	return &tokenResponse{
		AccessToken:  accessToken,
		RefreshToken: rawRefresh,
		ExpiresIn:    h.jwtExpirySec,
		User:         user,
	}, nil
}

func (h *AuthHandler) signAccessToken(user models.User) (string, error) {
	now := time.Now().UTC()
	claims := jwt.MapClaims{
		"sub":  user.ID.Hex(),
		"role": string(user.Role),
		"iat":  now.Unix(),
		"exp":  now.Add(time.Duration(h.jwtExpirySec) * time.Second).Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(h.jwtSecret))
}

// generateRawToken creates a 32-byte cryptographically random hex string.
func generateRawToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// hashToken returns the SHA-256 hex digest of a raw token string.
func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
