package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/creever/crawler-api/models"
)

const (
	ctxKeyUserID   = "user_id"
	ctxKeyUserRole = "user_role"
)

// RequireAuth validates the Bearer JWT from the Authorization header.
// On success it injects the user ID (bson.ObjectID) and role (models.UserRole)
// into the gin context under the keys "user_id" and "user_role".
func RequireAuth(jwtSecret string) gin.HandlerFunc {
	return func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		if header == "" || !strings.HasPrefix(header, "Bearer ") {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authorization header required"})
			return
		}

		rawToken := strings.TrimPrefix(header, "Bearer ")
		token, err := jwt.Parse(rawToken, func(t *jwt.Token) (interface{}, error) {
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, jwt.ErrSignatureInvalid
			}
			return []byte(jwtSecret), nil
		}, jwt.WithValidMethods([]string{"HS256"}))

		if err != nil || !token.Valid {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired token"})
			return
		}

		claims, ok := token.Claims.(jwt.MapClaims)
		if !ok {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid token claims"})
			return
		}

		sub, _ := claims["sub"].(string)
		userOID, err := bson.ObjectIDFromHex(sub)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid token subject"})
			return
		}

		role, _ := claims["role"].(string)

		c.Set(ctxKeyUserID, userOID)
		c.Set(ctxKeyUserRole, models.UserRole(role))
		c.Next()
	}
}

// GetUserID extracts the authenticated user's ObjectID from the gin context.
// Panics if called outside a RequireAuth-protected handler.
func GetUserID(c *gin.Context) bson.ObjectID {
	v, _ := c.Get(ctxKeyUserID)
	id, _ := v.(bson.ObjectID)
	return id
}

// GetUserRole extracts the authenticated user's role from the gin context.
func GetUserRole(c *gin.Context) models.UserRole {
	v, _ := c.Get(ctxKeyUserRole)
	role, _ := v.(models.UserRole)
	return role
}

// IsAdmin returns true when the authenticated user has the admin role.
func IsAdmin(c *gin.Context) bool {
	return GetUserRole(c) == models.UserRoleAdmin
}
