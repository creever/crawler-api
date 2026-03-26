package routes

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/hibiken/asynq"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.uber.org/zap"

	"github.com/creever/crawler-api/handlers"
	"github.com/creever/crawler-api/middleware"
)

// Setup registers all routes on the given router
func Setup(router *gin.Engine, db *mongo.Database, logger *zap.Logger, corsOrigins string, asynqClient *asynq.Client) {
	// Global middleware
	router.Use(middleware.CORS(corsOrigins))
	router.Use(middleware.Logger(logger))

	// Health check
	router.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	// Instantiate handlers
	dashboardH := handlers.NewDashboardHandler(db)
	projectH := handlers.NewProjectHandler(db)
	seoH := handlers.NewSEOHandler(db)
	prerenderH := handlers.NewPrerenderHandler(db)
	settingsH := handlers.NewSettingsHandler(db)
	cacheH := handlers.NewCacheHandler(db)
	queueH := handlers.NewQueueHandler(db, asynqClient)
	serveH := handlers.NewServeHandler(db, asynqClient)
	discoveryH := handlers.NewDiscoveryHandler(db, asynqClient, logger)

	// Prerender server — synchronous endpoint for nginx proxy.
	// Usage: GET /prerender?url=https://example.com/page
	router.GET("/prerender", serveH.Serve)

	api := router.Group("/api/v1")
	{
		// Dashboard
		api.GET("/dashboard", dashboardH.Get)

		// Projects
		projects := api.Group("/projects")
		{
			projects.GET("", projectH.List)
			projects.POST("", projectH.Create)
			projects.GET("/:id", projectH.Get)
			projects.PUT("/:id", projectH.Update)
			projects.DELETE("/:id", projectH.Delete)
			projects.GET("/:id/config", projectH.GetConfig)
			projects.GET("/:id/seo-summary", seoH.GetProjectSummary)
		}

		// SEO Analytics
		seo := api.Group("/seo")
		{
			seo.GET("", seoH.List)
			seo.POST("", seoH.Create)
			seo.GET("/:id", seoH.Get)
			seo.DELETE("/:id", seoH.Delete)
		}

		// Prerender Analytics
		prerender := api.Group("/prerender")
		{
			prerender.GET("", prerenderH.List)
			prerender.POST("", prerenderH.Create)
			prerender.GET("/stats", prerenderH.Stats)
			prerender.GET("/:id", prerenderH.Get)
			prerender.DELETE("/:id", prerenderH.Delete)
		}

		// Settings
		settings := api.Group("/settings")
		{
			settings.GET("", settingsH.List)
			settings.PUT("", settingsH.Upsert)
			settings.DELETE("/:key", settingsH.Delete)
		}

		// Cache Manager
		cache := api.Group("/cache")
		{
			cache.GET("", cacheH.List)
			cache.POST("", cacheH.Create)
			cache.GET("/:id", cacheH.Get)
			cache.PUT("/:id", cacheH.Update)
			cache.DELETE("/:id", cacheH.Delete)
		}

		// Crawl Queue
		queue := api.Group("/queue")
		{
			queue.GET("", queueH.List)
			queue.POST("", queueH.Enqueue)
			queue.GET("/:id", queueH.Get)
			queue.PATCH("/:id", queueH.UpdateStatus)
			queue.DELETE("/:id", queueH.Delete)
		}

		// Discovery
		discover := api.Group("/discover")
		{
			discover.GET("", discoveryH.List)
			discover.POST("", discoveryH.Start)
			discover.GET("/:id", discoveryH.Get)
			discover.GET("/:id/seo-summary", seoH.GetDiscoverySummary)
			discover.DELETE("/:id", discoveryH.Delete)
		}
	}
}
