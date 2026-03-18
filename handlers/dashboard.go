package handlers

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/creever/crawler-api/models"
)

// DashboardHandler holds dependencies for the dashboard endpoints
type DashboardHandler struct {
	db     *mongo.Database
}

// NewDashboardHandler creates a new DashboardHandler
func NewDashboardHandler(db *mongo.Database) *DashboardHandler {
	return &DashboardHandler{db: db}
}

// dashboardResponse is the combined response for the dashboard
type dashboardResponse struct {
	ProjectCount int64                   `json:"project_count"`
	SEO          models.SEOSummary       `json:"seo"`
	Prerender    models.PrerenderSummary `json:"prerender"`
	GeneratedAt  time.Time               `json:"generated_at"`
}

// Get godoc
// @Summary      Get dashboard overview
// @Description  Returns aggregate statistics across all projects
// @Tags         dashboard
// @Produce      json
// @Success      200  {object}  dashboardResponse
// @Router       /api/v1/dashboard [get]
func (h *DashboardHandler) Get(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	projectCount, err := h.db.Collection("projects").CountDocuments(ctx, bson.M{})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to count projects"})
		return
	}

	seoSummary := h.buildSEOSummary(ctx)
	prerenderSummary := h.buildPrerenderSummary(ctx)

	c.JSON(http.StatusOK, dashboardResponse{
		ProjectCount: projectCount,
		SEO:          seoSummary,
		Prerender:    prerenderSummary,
		GeneratedAt:  time.Now().UTC(),
	})
}

func (h *DashboardHandler) buildSEOSummary(ctx context.Context) models.SEOSummary {
	col := h.db.Collection("seo_data")

	total, _ := col.CountDocuments(ctx, bson.M{})
	withH1, _ := col.CountDocuments(ctx, bson.M{"h1_tags": bson.M{"$exists": true, "$ne": bson.A{}}})
	pages4xx, _ := col.CountDocuments(ctx, bson.M{"status_code": bson.M{"$gte": 400, "$lt": 500}})
	pages5xx, _ := col.CountDocuments(ctx, bson.M{"status_code": bson.M{"$gte": 500, "$lt": 600}})

	var avgLoadTime float64
	pipeline := mongo.Pipeline{
		{{Key: "$group", Value: bson.D{{Key: "_id", Value: nil}, {Key: "avg", Value: bson.D{{Key: "$avg", Value: "$load_time_ms"}}}}}},
	}
	cursor, err := col.Aggregate(ctx, pipeline)
	if err == nil {
		defer cursor.Close(ctx)
		var result []struct {
			Avg float64 `bson:"avg"`
		}
		if cursor.All(ctx, &result) == nil && len(result) > 0 {
			avgLoadTime = result[0].Avg
		}
	}

	return models.SEOSummary{
		TotalPages:  total,
		AvgLoadTime: avgLoadTime,
		PagesWithH1: withH1,
		Pages4xx:    pages4xx,
		Pages5xx:    pages5xx,
	}
}

func (h *DashboardHandler) buildPrerenderSummary(ctx context.Context) models.PrerenderSummary {
	col := h.db.Collection("prerender_data")

	total, _ := col.CountDocuments(ctx, bson.M{})
	cacheHits, _ := col.CountDocuments(ctx, bson.M{"from_cache": true})
	cacheMisses := total - cacheHits

	var hitRate float64
	if total > 0 {
		hitRate = float64(cacheHits) / float64(total) * 100
	}

	var avgRender float64
	pipeline := mongo.Pipeline{
		{{Key: "$group", Value: bson.D{{Key: "_id", Value: nil}, {Key: "avg", Value: bson.D{{Key: "$avg", Value: "$render_time_ms"}}}}}},
	}
	cursor, err := col.Aggregate(ctx, pipeline)
	if err == nil {
		defer cursor.Close(ctx)
		var result []struct {
			Avg float64 `bson:"avg"`
		}
		if cursor.All(ctx, &result) == nil && len(result) > 0 {
			avgRender = result[0].Avg
		}
	}

	return models.PrerenderSummary{
		TotalRequests: total,
		CacheHits:     cacheHits,
		CacheMisses:   cacheMisses,
		HitRate:       hitRate,
		AvgRenderMs:   avgRender,
	}
}
