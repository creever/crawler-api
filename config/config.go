package config

import (
	"os"
)

// Config holds all application configuration
type Config struct {
	MongoURI    string
	MongoDB     string
	ServerPort  string
	GinMode     string
	CORSOrigins string
}

// Load returns a Config populated from environment variables with sensible defaults
func Load() *Config {
	return &Config{
		MongoURI:    getEnv("MONGO_URI", "mongodb://localhost:9090"),
		MongoDB:     getEnv("MONGO_DB", "crawler"),
		ServerPort:  getEnv("SERVER_PORT", "8080"),
		GinMode:     getEnv("GIN_MODE", "debug"),
		CORSOrigins: getEnv("CORS_ORIGINS", "*"),
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
