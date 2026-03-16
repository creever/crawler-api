package routes

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.uber.org/zap"

	"github.com/creever/crawler-api/handlers"
	"github.com/creever/crawler-api/middleware"
)

// Setup registers all routes on the given router
func Setup(router *gin.Engine, db *mongo.Database, logger *zap.Logger, corsOrigins string) {
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
	}
}
