package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/creever/crawler-api/models"
)

// RequireAdmin rejects requests from non-admin users with HTTP 403.
// Must be placed after RequireAuth in the middleware chain.
func RequireAdmin() gin.HandlerFunc {
	return func(c *gin.Context) {
		if GetUserRole(c) != models.UserRoleAdmin {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "admin access required"})
			return
		}
		c.Next()
	}
}
