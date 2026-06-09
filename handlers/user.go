package handlers

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"golang.org/x/crypto/bcrypt"

	"github.com/creever/crawler-api/middleware"
	"github.com/creever/crawler-api/models"
)

// UserHandler provides admin-only user management endpoints.
type UserHandler struct {
	db *mongo.Database
}

// NewUserHandler creates a UserHandler.
func NewUserHandler(db *mongo.Database) *UserHandler {
	return &UserHandler{db: db}
}

func (h *UserHandler) col() *mongo.Collection {
	return h.db.Collection("users")
}

// List godoc
// @Summary List all users (admin only)
// @Tags users
// @Produce json
// @Router /api/v1/users [get]
func (h *UserHandler) List(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	opts := options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}})
	cursor, err := h.col().Find(ctx, bson.M{}, opts)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch users"})
		return
	}
	defer cursor.Close(ctx) //nolint:errcheck

	var users []models.User
	if err = cursor.All(ctx, &users); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to decode users"})
		return
	}
	if users == nil {
		users = []models.User{}
	}
	c.JSON(http.StatusOK, users)
}

// Get godoc
// @Summary Get a user by ID (admin only)
// @Tags users
// @Produce json
// @Param id path string true "User ID"
// @Router /api/v1/users/{id} [get]
func (h *UserHandler) Get(c *gin.Context) {
	oid, err := bson.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid user id"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var user models.User
	if err = h.col().FindOne(ctx, bson.M{"_id": oid}).Decode(&user); err != nil {
		if err == mongo.ErrNoDocuments {
			c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch user"})
		return
	}
	c.JSON(http.StatusOK, user)
}

// Update godoc
// @Summary Update a user's name or active status (admin only)
// @Tags users
// @Accept json
// @Produce json
// @Param id path string true "User ID"
// @Router /api/v1/users/{id} [put]
func (h *UserHandler) Update(c *gin.Context) {
	oid, err := bson.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid user id"})
		return
	}

	var input struct {
		Name   string `json:"name"`
		Active *bool  `json:"active"`
	}
	if err = c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	update := bson.M{"updated_at": time.Now().UTC()}
	if input.Name != "" {
		update["name"] = input.Name
	}
	if input.Active != nil {
		update["active"] = *input.Active
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res := h.col().FindOneAndUpdate(
		ctx,
		bson.M{"_id": oid},
		bson.M{"$set": update},
		options.FindOneAndUpdate().SetReturnDocument(options.After),
	)
	if res.Err() != nil {
		if res.Err() == mongo.ErrNoDocuments {
			c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update user"})
		return
	}

	var user models.User
	if err = res.Decode(&user); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to decode user"})
		return
	}
	c.JSON(http.StatusOK, user)
}

// Delete godoc
// @Summary Delete a user (admin only, cannot delete self)
// @Tags users
// @Param id path string true "User ID"
// @Router /api/v1/users/{id} [delete]
func (h *UserHandler) Delete(c *gin.Context) {
	oid, err := bson.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid user id"})
		return
	}
	if oid == middleware.GetUserID(c) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cannot delete your own account"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := h.col().DeleteOne(ctx, bson.M{"_id": oid})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete user"})
		return
	}
	if res.DeletedCount == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}

	// Clean up refresh tokens for the deleted user.
	_, _ = h.db.Collection("refresh_tokens").DeleteMany(ctx, bson.M{"user_id": oid})

	c.Status(http.StatusNoContent)
}

// UpdateRole godoc
// @Summary Change a user's role (admin only, cannot demote self)
// @Tags users
// @Accept json
// @Produce json
// @Param id path string true "User ID"
// @Router /api/v1/users/{id}/role [patch]
func (h *UserHandler) UpdateRole(c *gin.Context) {
	oid, err := bson.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid user id"})
		return
	}
	if oid == middleware.GetUserID(c) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cannot change your own role"})
		return
	}

	var input struct {
		Role models.UserRole `json:"role" binding:"required"`
	}
	if err = c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if input.Role != models.UserRoleAdmin && input.Role != models.UserRoleUser {
		c.JSON(http.StatusBadRequest, gin.H{"error": "role must be 'admin' or 'user'"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res := h.col().FindOneAndUpdate(
		ctx,
		bson.M{"_id": oid},
		bson.M{"$set": bson.M{"role": input.Role, "updated_at": time.Now().UTC()}},
		options.FindOneAndUpdate().SetReturnDocument(options.After),
	)
	if res.Err() != nil {
		if res.Err() == mongo.ErrNoDocuments {
			c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update role"})
		return
	}

	var user models.User
	if err = res.Decode(&user); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to decode user"})
		return
	}
	c.JSON(http.StatusOK, user)
}

// ResetPassword godoc
// @Summary Reset a user's password (admin resets any user; user resets own)
// @Tags users
// @Accept json
// @Produce json
// @Param id path string true "User ID"
// @Router /api/v1/users/{id}/password [patch]
func (h *UserHandler) ResetPassword(c *gin.Context) {
	oid, err := bson.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid user id"})
		return
	}

	// Non-admin users can only change their own password.
	callerID := middleware.GetUserID(c)
	if !middleware.IsAdmin(c) && oid != callerID {
		c.JSON(http.StatusForbidden, gin.H{"error": "cannot reset another user's password"})
		return
	}

	var input struct {
		NewPassword string `json:"new_password" binding:"required,min=8"`
	}
	if err = c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(input.NewPassword), 12)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to hash password"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := h.col().UpdateOne(ctx, bson.M{"_id": oid}, bson.M{
		"$set": bson.M{
			"password_hash": string(hash),
			"updated_at":    time.Now().UTC(),
		},
	})
	if err != nil || res.MatchedCount == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}

	// Invalidate all existing refresh tokens for security.
	_, _ = h.db.Collection("refresh_tokens").DeleteMany(ctx, bson.M{"user_id": oid})

	c.JSON(http.StatusOK, gin.H{"message": "password updated"})
}
