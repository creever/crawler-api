package middleware

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// RequireMCPToken authenticates MCP requests with a static bearer token.
// The token itself is never stored — only its SHA-256 hash, in the settings
// collection under the key "mcp_token_hash" (set by an admin).
func RequireMCPToken(db *mongo.Database) gin.HandlerFunc {
	return func(c *gin.Context) {
		auth := c.GetHeader("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(auth, prefix) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing bearer token"})
			return
		}
		token := strings.TrimPrefix(auth, prefix)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		var s struct {
			Value string `bson:"value"`
		}
		err := db.Collection("settings").FindOne(ctx, bson.M{"key": "mcp_token_hash"}).Decode(&s)
		if err != nil || s.Value == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "MCP token not configured"})
			return
		}

		sum := sha256.Sum256([]byte(token))
		if hex.EncodeToString(sum[:]) != s.Value {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid MCP token"})
			return
		}
		c.Next()
	}
}

var _ = mongo.ErrNoDocuments // keep mongo import if unused in future edits
