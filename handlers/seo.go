package handlers

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/creever/crawler-api/middleware"
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Only the caller's projects: an admin sees all, others their own.
	owned, err := accessibleProjectIDs(ctx, c, h.db)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch projects"})
		return
	}
	filter := bson.M{"project_id": bson.M{"$in": owned}}
	if pid := c.Query("project_id"); pid != "" {
		oid, err := bson.ObjectIDFromHex(pid)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project_id"})
			return
		}
		filter = bson.M{"project_id": oid, "$and": bson.A{bson.M{"project_id": bson.M{"$in": owned}}}}
	}

	opts := options.Find().SetSort(bson.D{{Key: "crawled_at", Value: -1}}).SetLimit(1000)
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
	owned, err := accessibleProjectIDs(ctx, c, h.db)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch projects"})
		return
	}
	if err = h.col().FindOne(ctx, bson.M{"_id": id, "project_id": bson.M{"$in": owned}}).Decode(&record); err != nil {
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

	if err := h.db.Collection("projects").FindOne(ctx, gscProjectFilter(c, data.ProjectID)).Err(); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "project not found"})
		return
	}
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

	owned, err := accessibleProjectIDs(ctx, c, h.db)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch projects"})
		return
	}
	result, err := h.col().DeleteOne(ctx, bson.M{"_id": id, "project_id": bson.M{"$in": owned}})
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

// accessibleProjectIDs lists the projects the caller may read.
func accessibleProjectIDs(ctx context.Context, c *gin.Context, db *mongo.Database) ([]bson.ObjectID, error) {
	filter := bson.M{}
	if !middleware.IsAdmin(c) {
		filter["owner_id"] = middleware.GetUserID(c)
	}
	cursor, err := db.Collection("projects").Find(ctx, filter, options.Find().SetProjection(bson.M{"_id": 1}))
	if err != nil {
		return nil, err
	}
	var rows []struct {
		ID bson.ObjectID `bson:"_id"`
	}
	if err = cursor.All(ctx, &rows); err != nil {
		return nil, err
	}
	ids := make([]bson.ObjectID, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	return ids, nil
}
