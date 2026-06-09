package routes

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/hibiken/asynq"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.uber.org/zap"

	"github.com/creever/crawler-api/config"
	"github.com/creever/crawler-api/handlers"
	"github.com/creever/crawler-api/middleware"
)

// Setup registers all routes on the given router
func Setup(router *gin.Engine, db *mongo.Database, logger *zap.Logger, corsOrigins string, asynqClient *asynq.Client, cfg *config.Config) {
	jwtSecret := cfg.JWTSecret
	// Global middleware
	router.Use(middleware.CORS(corsOrigins))
	router.Use(middleware.Logger(logger))

	// Public: health check
	router.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	// Public: nginx prerender proxy (must stay unauthenticated)
	serveH := handlers.NewServeHandler(db, asynqClient)
	router.GET("/prerender", serveH.Serve)

	// Instantiate handlers
	dashboardH := handlers.NewDashboardHandler(db)
	projectH := handlers.NewProjectHandler(db)
	seoH := handlers.NewSEOHandler(db)
	prerenderH := handlers.NewPrerenderHandler(db)
	settingsH := handlers.NewSettingsHandler(db)
	cacheH := handlers.NewCacheHandler(db)
	queueH := handlers.NewQueueHandler(db, asynqClient)
	discoveryH := handlers.NewDiscoveryHandler(db, asynqClient)
	blogConfigH := handlers.NewBlogConfigHandler(db)
	blogPostH := handlers.NewBlogPostHandler(db, asynqClient)
	authH := handlers.NewAuthHandler(db, jwtSecret, cfg.JWTExpirySec, cfg.JWTRefreshExpSec)
	userH := handlers.NewUserHandler(db)

	api := router.Group("/api/v1")
	{
		// ── Public auth endpoints ────────────────────────────────────────────
		auth := api.Group("/auth")
		{
			auth.POST("/register", authH.Register)
			auth.POST("/login", authH.Login)
			auth.POST("/refresh", authH.Refresh)
			// logout and me validate the token internally but live in the public group
			// so the /auth group path is clean and consistent.
			auth.POST("/logout", middleware.RequireAuth(jwtSecret), authH.Logout)
			auth.GET("/me", middleware.RequireAuth(jwtSecret), authH.Me)
		}

		// ── Protected endpoints (all require valid JWT) ──────────────────────
		protected := api.Group("", middleware.RequireAuth(jwtSecret))
		{
			// Dashboard
			protected.GET("/dashboard", dashboardH.Get)

			// Projects
			projects := protected.Group("/projects")
			{
				projects.GET("", projectH.List)
				projects.POST("", projectH.Create)
				projects.GET("/:id", projectH.Get)
				projects.PUT("/:id", projectH.Update)
				projects.DELETE("/:id", projectH.Delete)
				projects.GET("/:id/config", projectH.GetConfig)
			}

			// SEO Analytics
			seo := protected.Group("/seo")
			{
				seo.GET("", seoH.List)
				seo.POST("", seoH.Create)
				seo.GET("/:id", seoH.Get)
				seo.DELETE("/:id", seoH.Delete)
			}

			// Prerender Analytics
			prerender := protected.Group("/prerender")
			{
				prerender.GET("", prerenderH.List)
				prerender.POST("", prerenderH.Create)
				prerender.GET("/stats", prerenderH.Stats)
				prerender.GET("/:id", prerenderH.Get)
				prerender.DELETE("/:id", prerenderH.Delete)
			}

			// Settings (write operations admin-only)
			settings := protected.Group("/settings")
			{
				settings.GET("", settingsH.List)
				settings.PUT("", middleware.RequireAdmin(), settingsH.Upsert)
				settings.DELETE("/:key", middleware.RequireAdmin(), settingsH.Delete)
			}

			// Cache Manager
			cache := protected.Group("/cache")
			{
				cache.GET("", cacheH.List)
				cache.POST("", cacheH.Create)
				cache.GET("/:id", cacheH.Get)
				cache.PUT("/:id", cacheH.Update)
				cache.DELETE("/:id", cacheH.Delete)
			}

			// Crawl Queue
			queue := protected.Group("/queue")
			{
				queue.GET("", queueH.List)
				queue.POST("", queueH.Enqueue)
				queue.GET("/:id", queueH.Get)
				queue.PATCH("/:id", queueH.UpdateStatus)
				queue.DELETE("/:id", queueH.Delete)
			}

			// Discovery
			discover := protected.Group("/discover")
			{
				discover.GET("", discoveryH.List)
				discover.POST("", discoveryH.Start)
				discover.GET("/:id", discoveryH.Get)
				discover.DELETE("/:id", discoveryH.Delete)
			}

			// Blog generation
			blog := protected.Group("/blog")
			{
				configs := blog.Group("/configs")
				{
					configs.POST("", blogConfigH.Create)
					configs.GET("", blogConfigH.List)
					configs.GET("/:id", blogConfigH.Get)
					configs.PUT("/:id", blogConfigH.Update)
					configs.DELETE("/:id", blogConfigH.Delete)
				}
				blog.POST("/generate", blogPostH.Trigger)
				posts := blog.Group("/posts")
				{
					posts.GET("", blogPostH.List)
					posts.GET("/:id", blogPostH.Get)
					posts.DELETE("/:id", blogPostH.Delete)
				}
			}

			// User management (admin only)
			users := protected.Group("/users", middleware.RequireAdmin())
			{
				users.GET("", userH.List)
				users.GET("/:id", userH.Get)
				users.PUT("/:id", userH.Update)
				users.DELETE("/:id", userH.Delete)
				users.PATCH("/:id/role", userH.UpdateRole)
				users.PATCH("/:id/password", userH.ResetPassword)
			}

			// Own password change — available to any authenticated user
			protected.PATCH("/users/:id/password", userH.ResetPassword)
		}
	}
}
