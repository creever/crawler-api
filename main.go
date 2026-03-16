package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/creever/crawler-api/config"
	"github.com/creever/crawler-api/db"
	"github.com/creever/crawler-api/routes"
)

func main() {
	cfg := config.Load()

	// Logger
	var logger *zap.Logger
	var err error
	if cfg.GinMode == "release" {
		logger, err = zap.NewProduction()
	} else {
		logger, err = zap.NewDevelopment()
	}
	if err != nil {
		log.Fatalf("Failed to initialize logger: %v", err)
	}
	defer logger.Sync() //nolint:errcheck

	// MongoDB
	mongoClient := db.Connect(cfg.MongoURI)
	defer db.Disconnect(mongoClient)
	database := mongoClient.Database(cfg.MongoDB)

	// GIN
	gin.SetMode(cfg.GinMode)
	router := gin.New()
	router.Use(gin.Recovery())

	routes.Setup(router, database, logger, cfg.CORSOrigins)

	srv := &http.Server{
		Addr:         ":" + cfg.ServerPort,
		Handler:      router,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Start server in a goroutine so we can listen for shutdown signals
	go func() {
		logger.Info("Starting server", zap.String("port", cfg.ServerPort))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatal("Server error", zap.Error(err))
		}
	}()

	// Graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	logger.Info("Shutting down server...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		logger.Fatal("Server forced to shutdown", zap.Error(err))
	}
	logger.Info("Server exited")
}
