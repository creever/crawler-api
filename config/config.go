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
	RedisAddr   string
	CrawlerAddr string
}

// Load returns a Config populated from environment variables with sensible defaults
func Load() *Config {
	return &Config{
		MongoURI:    getEnv("MONGO_URI", "mongodb://localhost:27017"),
		MongoDB:     getEnv("MONGO_DB", "crawler"),
		ServerPort:  getEnv("SERVER_PORT", "9090"),
		GinMode:     getEnv("GIN_MODE", "debug"),
		CORSOrigins: getEnv("CORS_ORIGINS", "*"),
		RedisAddr:   getEnv("REDIS_ADDR", "localhost:6379"),
		CrawlerAddr: getEnv("CRAWLER_ADDR", "http://localhost:8080"),
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
