package handlers

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/creever/crawler-api/models"
)

// SEOHandler holds dependencies for SEO analytics endpoints
type SEOHandler struct {
	db *mongo.Database
}

// NewSEOHandler creates a new SEOHandler
func NewSEOHandler(db *mongo.Database) *SEOHandler {
	return &SEOHandler{db: db}
}

func (h *SEOHandler) col() *mongo.Collection {
	return h.db.Collection("seo_data")
}

// List godoc
// @Summary      List SEO data (optionally filtered by project)
// @Tags         seo
// @Produce      json
// @Param        project_id  query  string  false  "Project ID filter"
// @Success      200  {array}   models.SEOData
// @Router       /api/v1/seo [get]
func (h *SEOHandler) List(c *gin.Context) {
	filter := bson.M{}
	if pid := c.Query("project_id"); pid != "" {
		oid, err := bson.ObjectIDFromHex(pid)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project_id"})
			return
		}
		filter["project_id"] = oid
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	opts := options.Find().SetSort(bson.D{{Key: "crawled_at", Value: -1}})
	cursor, err := h.col().Find(ctx, filter, opts)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch seo data"})
		return
	}
	defer cursor.Close(ctx)

	var results []models.SEOData
	if err = cursor.All(ctx, &results); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to decode seo data"})
		return
	}
	if results == nil {
		results = []models.SEOData{}
	}
	c.JSON(http.StatusOK, results)
}

// Get godoc
// @Summary      Get a single SEO record by ID
// @Tags         seo
// @Produce      json
// @Param        id  path  string  true  "SEO record ID"
// @Success      200  {object}  models.SEOData
// @Failure      404  {object}  map[string]string
// @Router       /api/v1/seo/{id} [get]
func (h *SEOHandler) Get(c *gin.Context) {
	id, err := bson.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var record models.SEOData
	if err = h.col().FindOne(ctx, bson.M{"_id": id}).Decode(&record); err != nil {
		if err == mongo.ErrNoDocuments {
			c.JSON(http.StatusNotFound, gin.H{"error": "seo record not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch seo record"})
		return
	}
	c.JSON(http.StatusOK, record)
}

// Create godoc
// @Summary      Ingest SEO data collected by a bot
// @Tags         seo
// @Accept       json
// @Produce      json
// @Param        data  body      models.SEOData  true  "SEO data"
// @Success      201   {object}  models.SEOData
// @Router       /api/v1/seo [post]
func (h *SEOHandler) Create(c *gin.Context) {
	var data models.SEOData
	if err := c.ShouldBindJSON(&data); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	data.ID = bson.NewObjectID()
	if data.CrawledAt.IsZero() {
		data.CrawledAt = time.Now().UTC()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := h.col().InsertOne(ctx, data); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to store seo data"})
		return
	}
	c.JSON(http.StatusCreated, data)
}

// Delete godoc
// @Summary      Delete a SEO record
// @Tags         seo
// @Param        id  path  string  true  "SEO record ID"
// @Success      204
// @Failure      404  {object}  map[string]string
// @Router       /api/v1/seo/{id} [delete]
func (h *SEOHandler) Delete(c *gin.Context) {
	id, err := bson.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, err := h.col().DeleteOne(ctx, bson.M{"_id": id})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete seo record"})
		return
	}
	if result.DeletedCount == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "seo record not found"})
		return
	}
	c.Status(http.StatusNoContent)
}

// GetProjectSummary godoc
// @Summary      Get aggregated SEO summary for a project
// @Tags         seo
// @Produce      json
// @Param        id  path  string  true  "Project ID"
// @Success      200  {object}  models.ProjectSeoSummary
// @Failure      400  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Router       /api/v1/projects/{id}/seo-summary [get]
func (h *SEOHandler) GetProjectSummary(c *gin.Context) {
	projectOID, err := bson.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project id"})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	// Fetch all SEO records for this project, sorted by word_count desc so the
	// first N records are already the top pages by content volume.
	cursor, err := h.col().Find(
		ctx,
		bson.M{"project_id": projectOID},
		options.Find().SetSort(bson.D{{Key: "word_count", Value: -1}}),
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to query seo data"})
		return
	}
	defer cursor.Close(ctx)

	var records []models.SEOData
	if err = cursor.All(ctx, &records); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to decode seo data"})
		return
	}

	summary := models.ProjectSeoSummary{
		ProjectID: c.Param("id"),
		TopPages:  []models.ProjectSeoTopPage{},
	}

	if len(records) == 0 {
		c.JSON(http.StatusOK, summary)
		return
	}

	summary.TotalPagesAnalyzed = len(records)

	var totalWordCount, totalInternalLinks, totalExternalLinks int
	var totalLoadTimeMs int64

	for _, r := range records {
		totalWordCount += r.WordCount
		totalInternalLinks += r.InternalLinks
		totalExternalLinks += r.ExternalLinks
		totalLoadTimeMs += r.LoadTimeMs

		if r.Title == "" {
			summary.Issues.MissingTitle++
		}
		if r.MetaDescription == "" {
			summary.Issues.MissingDescription++
		}
		if len(r.H1Tags) == 0 {
			summary.Issues.MissingH1++
		}
		if r.CanonicalURL == "" {
			summary.Issues.MissingCanonical++
		}
		summary.Issues.ImagesWithoutAlt += r.ImagesWithoutAlt
		if r.StatusCode >= 400 {
			summary.Issues.PagesWithErrors++
		}
	}

	n := len(records)
	summary.AvgWordCount = float64(totalWordCount) / float64(n)
	summary.AvgInternalLinks = float64(totalInternalLinks) / float64(n)
	summary.AvgExternalLinks = float64(totalExternalLinks) / float64(n)
	summary.AvgLoadTimeMs = float64(totalLoadTimeMs) / float64(n)

	// Top 10 pages by word count (records are already sorted desc).
	topN := 10
	if n < topN {
		topN = n
	}
	for i := 0; i < topN; i++ {
		r := records[i]
		page := models.ProjectSeoTopPage{
			URL:           r.URL,
			WordCount:     r.WordCount,
			InternalLinks: r.InternalLinks,
			LoadTimeMs:    r.LoadTimeMs,
		}
		if r.Title != "" {
			title := r.Title
			page.Title = &title
		}
		summary.TopPages = append(summary.TopPages, page)
	}

	c.JSON(http.StatusOK, summary)
}
