package config

import (
	"os"
	"strconv"
)

// Config holds all application configuration
type Config struct {
	MongoURI    string
	MongoDB     string
	ServerPort  string
	GinMode     string
	CORSOrigins string
	RedisAddr   string
	CrawlerAddr string
	// Auth
	JWTSecret        string
	JWTExpirySec     int // access token lifetime (default 15 min)
	JWTRefreshExpSec int // refresh token lifetime (default 7 days)
}

// Load returns a Config populated from environment variables with sensible defaults
func Load() *Config {
	return &Config{
		MongoURI:         getEnv("MONGO_URI", "mongodb://localhost:27017"),
		MongoDB:          getEnv("MONGO_DB", "crawler"),
		ServerPort:       getEnv("SERVER_PORT", "9090"),
		GinMode:          getEnv("GIN_MODE", "debug"),
		CORSOrigins:      getEnv("CORS_ORIGINS", "*"),
		RedisAddr:        getEnv("REDIS_ADDR", "localhost:6379"),
		CrawlerAddr:      getEnv("CRAWLER_ADDR", "http://localhost:8080"),
		JWTSecret:        getEnv("JWT_SECRET", "change-me-in-production"),
		JWTExpirySec:     getEnvInt("JWT_EXPIRY_SEC", 900),      // 15 min
		JWTRefreshExpSec: getEnvInt("JWT_REFRESH_EXPIRY_SEC", 604800), // 7 days
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

