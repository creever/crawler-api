package middleware

import (
	"strings"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
)

// CORS returns a middleware that allows cross-origin requests.
// Pass a comma-separated list of allowed origins via the origins parameter.
// Use "*" to allow all origins (only suitable for development).
// For production, restrict to specific trusted domains, e.g. "https://app.example.com".
func CORS(origins string) gin.HandlerFunc {
	cfg := cors.DefaultConfig()
	if origins == "*" {
		cfg.AllowAllOrigins = true
	} else {
		cfg.AllowOrigins = strings.Split(origins, ",")
	}
	cfg.AllowHeaders = []string{
		"Origin",
		"Content-Type",
		"Accept",
		"Authorization",
		"X-Requested-With",
	}
	cfg.AllowMethods = []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"}
	return cors.New(cfg)
}
