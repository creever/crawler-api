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

// CacheHandler holds dependencies for cache manager endpoints.
type CacheHandler struct {
	db *mongo.Database
}

// NewCacheHandler creates a new CacheHandler.
func NewCacheHandler(db *mongo.Database) *CacheHandler {
	return &CacheHandler{db: db}
}

func (h *CacheHandler) col() *mongo.Collection {
	return h.db.Collection("cache_entries")
}

// List godoc
// @Summary      List cached page entries (optionally filtered by project)
// @Tags         cache
// @Produce      json
// @Param        project_id  query  string  false  "Project ID filter"
// @Param        url         query  string  false  "Exact URL filter"
// @Success      200  {array}   models.CacheEntry
// @Router       /api/v1/cache [get]
func (h *CacheHandler) List(c *gin.Context) {
	filter := bson.M{}
	if pid := c.Query("project_id"); pid != "" {
		oid, err := bson.ObjectIDFromHex(pid)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project_id"})
			return
		}
		filter["project_id"] = oid
	}
	if url := c.Query("url"); url != "" {
		filter["url"] = url
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	opts := options.Find().SetSort(bson.D{{Key: "cached_at", Value: -1}})
	cursor, err := h.col().Find(ctx, filter, opts)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch cache entries"})
		return
	}
	defer cursor.Close(ctx)

	var results []models.CacheEntry
	if err = cursor.All(ctx, &results); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to decode cache entries"})
		return
	}
	if results == nil {
		results = []models.CacheEntry{}
	}
	c.JSON(http.StatusOK, results)
}

// Get godoc
// @Summary      Get a single cached page entry by ID
// @Tags         cache
// @Produce      json
// @Param        id  path  string  true  "Cache entry ID"
// @Success      200  {object}  models.CacheEntry
// @Failure      404  {object}  map[string]string
// @Router       /api/v1/cache/{id} [get]
func (h *CacheHandler) Get(c *gin.Context) {
	id, err := bson.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var entry models.CacheEntry
	if err = h.col().FindOne(ctx, bson.M{"_id": id}).Decode(&entry); err != nil {
		if err == mongo.ErrNoDocuments {
			c.JSON(http.StatusNotFound, gin.H{"error": "cache entry not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch cache entry"})
		return
	}
	c.JSON(http.StatusOK, entry)
}

// Create godoc
// @Summary      Store a prerendered page with its full HTML and extracted SEO data
// @Tags         cache
// @Accept       json
// @Produce      json
// @Param        data  body      models.CacheEntry  true  "Cache entry"
// @Success      201   {object}  models.CacheEntry
// @Router       /api/v1/cache [post]
func (h *CacheHandler) Create(c *gin.Context) {
	var entry models.CacheEntry
	if err := c.ShouldBindJSON(&entry); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	now := time.Now().UTC()
	entry.ID = bson.NewObjectID()
	if entry.CachedAt.IsZero() {
		entry.CachedAt = now
	}
	if entry.ExpiresAt.IsZero() {
		entry.ExpiresAt = now.Add(24 * time.Hour)
	}
	entry.UpdatedAt = now

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := h.col().InsertOne(ctx, entry); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to store cache entry"})
		return
	}
	c.JSON(http.StatusCreated, entry)
}

// Update godoc
// @Summary      Refresh the HTML and/or SEO data of an existing cache entry
// @Tags         cache
// @Accept       json
// @Produce      json
// @Param        id    path      string                        true  "Cache entry ID"
// @Param        data  body      models.CacheEntryUpdateInput  true  "Fields to update"
// @Success      200   {object}  models.CacheEntry
// @Failure      404   {object}  map[string]string
// @Router       /api/v1/cache/{id} [put]
func (h *CacheHandler) Update(c *gin.Context) {
	id, err := bson.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}

	var input models.CacheEntryUpdateInput
	if err = c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	input.UpdatedAt = time.Now().UTC()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var updated models.CacheEntry
	err = h.col().FindOneAndUpdate(
		ctx,
		bson.M{"_id": id},
		bson.M{"$set": input},
		options.FindOneAndUpdate().SetReturnDocument(options.After),
	).Decode(&updated)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			c.JSON(http.StatusNotFound, gin.H{"error": "cache entry not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update cache entry"})
		return
	}
	c.JSON(http.StatusOK, updated)
}

// Delete godoc
// @Summary      Delete a cached page entry
// @Tags         cache
// @Param        id  path  string  true  "Cache entry ID"
// @Success      204
// @Failure      404  {object}  map[string]string
// @Router       /api/v1/cache/{id} [delete]
func (h *CacheHandler) Delete(c *gin.Context) {
	id, err := bson.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, err := h.col().DeleteOne(ctx, bson.M{"_id": id})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete cache entry"})
		return
	}
	if result.DeletedCount == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "cache entry not found"})
		return
	}
	c.Status(http.StatusNoContent)
}
