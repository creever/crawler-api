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

// PrerenderHandler holds dependencies for prerender analytics endpoints
type PrerenderHandler struct {
	db *mongo.Database
}

// NewPrerenderHandler creates a new PrerenderHandler
func NewPrerenderHandler(db *mongo.Database) *PrerenderHandler {
	return &PrerenderHandler{db: db}
}

func (h *PrerenderHandler) col() *mongo.Collection {
	return h.db.Collection("prerender_data")
}

// List godoc
// @Summary      List prerender records (optionally filtered by project)
// @Tags         prerender
// @Produce      json
// @Param        project_id  query  string  false  "Project ID filter"
// @Success      200  {array}   models.PrerenderData
// @Router       /api/v1/prerender [get]
func (h *PrerenderHandler) List(c *gin.Context) {
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

	opts := options.Find().SetSort(bson.D{{Key: "cached_at", Value: -1}})
	cursor, err := h.col().Find(ctx, filter, opts)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch prerender data"})
		return
	}
	defer cursor.Close(ctx)

	var results []models.PrerenderData
	if err = cursor.All(ctx, &results); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to decode prerender data"})
		return
	}
	if results == nil {
		results = []models.PrerenderData{}
	}
	c.JSON(http.StatusOK, results)
}

// Get godoc
// @Summary      Get a single prerender record by ID
// @Tags         prerender
// @Produce      json
// @Param        id  path  string  true  "Prerender record ID"
// @Success      200  {object}  models.PrerenderData
// @Failure      404  {object}  map[string]string
// @Router       /api/v1/prerender/{id} [get]
func (h *PrerenderHandler) Get(c *gin.Context) {
	id, err := bson.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var record models.PrerenderData
	if err = h.col().FindOne(ctx, bson.M{"_id": id}).Decode(&record); err != nil {
		if err == mongo.ErrNoDocuments {
			c.JSON(http.StatusNotFound, gin.H{"error": "prerender record not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch prerender record"})
		return
	}
	c.JSON(http.StatusOK, record)
}

// Create godoc
// @Summary      Ingest prerender data submitted by the bot
// @Tags         prerender
// @Accept       json
// @Produce      json
// @Param        data  body      models.PrerenderData  true  "Prerender data"
// @Success      201   {object}  models.PrerenderData
// @Router       /api/v1/prerender [post]
func (h *PrerenderHandler) Create(c *gin.Context) {
	var data models.PrerenderData
	if err := c.ShouldBindJSON(&data); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	now := time.Now().UTC()
	data.ID = bson.NewObjectID()
	if data.CachedAt.IsZero() {
		data.CachedAt = now
	}
	if data.ExpiresAt.IsZero() {
		data.ExpiresAt = now.Add(24 * time.Hour)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := h.col().InsertOne(ctx, data); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to store prerender data"})
		return
	}
	c.JSON(http.StatusCreated, data)
}

// Delete godoc
// @Summary      Delete a prerender record
// @Tags         prerender
// @Param        id  path  string  true  "Prerender record ID"
// @Success      204
// @Failure      404  {object}  map[string]string
// @Router       /api/v1/prerender/{id} [delete]
func (h *PrerenderHandler) Delete(c *gin.Context) {
	id, err := bson.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, err := h.col().DeleteOne(ctx, bson.M{"_id": id})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete prerender record"})
		return
	}
	if result.DeletedCount == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "prerender record not found"})
		return
	}
	c.Status(http.StatusNoContent)
}
