package handlers

import (
	"context"
	"net/http"
	"strconv"
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
// @Summary      List prerender log entries with optional filters and pagination
// @Tags         prerender
// @Produce      json
// @Param        project_id     query  string  false  "Project ID filter"
// @Param        requester_type query  string  false  "Requester type filter: human | bot | ai"
// @Param        from_cache     query  bool    false  "Cache hit filter: true | false"
// @Param        since          query  string  false  "Start of date range (RFC3339, e.g. 2026-01-01T00:00:00Z)"
// @Param        until          query  string  false  "End of date range (RFC3339, e.g. 2026-12-31T23:59:59Z)"
// @Param        limit          query  int     false  "Max records to return (default 50, max 200)"
// @Param        offset         query  int     false  "Number of records to skip (default 0)"
// @Success      200  {object}  models.PrerenderListResponse
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

	if rt := c.Query("requester_type"); rt != "" {
		switch models.RequesterType(rt) {
		case models.RequesterTypeHuman, models.RequesterTypeBot, models.RequesterTypeAI:
			filter["requester_type"] = models.RequesterType(rt)
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid requester_type: must be one of human, bot, ai"})
			return
		}
	}

	if fc := c.Query("from_cache"); fc != "" {
		val, err := strconv.ParseBool(fc)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid from_cache: must be true or false"})
			return
		}
		filter["from_cache"] = val
	}

	dateFilter := bson.M{}
	if since := c.Query("since"); since != "" {
		t, err := time.Parse(time.RFC3339, since)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid since: must be RFC3339 format"})
			return
		}
		dateFilter["$gte"] = t.UTC()
	}
	if until := c.Query("until"); until != "" {
		t, err := time.Parse(time.RFC3339, until)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid until: must be RFC3339 format"})
			return
		}
		dateFilter["$lte"] = t.UTC()
	}
	if len(dateFilter) > 0 {
		filter["cached_at"] = dateFilter
	}

	const defaultLimit int64 = 50
	const maxLimit int64 = 200

	limit := defaultLimit
	if l := c.Query("limit"); l != "" {
		parsed, err := strconv.ParseInt(l, 10, 64)
		if err != nil || parsed < 1 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid limit: must be a positive integer"})
			return
		}
		if parsed > maxLimit {
			parsed = maxLimit
		}
		limit = parsed
	}

	var offset int64
	if o := c.Query("offset"); o != "" {
		parsed, err := strconv.ParseInt(o, 10, 64)
		if err != nil || parsed < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid offset: must be a non-negative integer"})
			return
		}
		offset = parsed
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	total, err := h.col().CountDocuments(ctx, filter)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to count prerender data"})
		return
	}

	opts := options.Find().
		SetSort(bson.D{{Key: "cached_at", Value: -1}}).
		SetLimit(limit).
		SetSkip(offset)

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

	c.JSON(http.StatusOK, models.PrerenderListResponse{
		Total:  total,
		Limit:  limit,
		Offset: offset,
		Data:   results,
	})
}

// Stats godoc
// @Summary      Aggregate statistics for prerender log entries
// @Description  Returns totals, cache hit rate, average render time and a
//
//	per-requester-type breakdown.  All filters are optional.
//
// @Tags         prerender
// @Produce      json
// @Param        project_id  query  string  false  "Project ID filter"
// @Param        since       query  string  false  "Start of date range (RFC3339)"
// @Param        until       query  string  false  "End of date range (RFC3339)"
// @Success      200  {object}  models.PrerenderStats
// @Router       /api/v1/prerender/stats [get]
func (h *PrerenderHandler) Stats(c *gin.Context) {
	filter := bson.M{}

	if pid := c.Query("project_id"); pid != "" {
		oid, err := bson.ObjectIDFromHex(pid)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project_id"})
			return
		}
		filter["project_id"] = oid
	}

	dateFilter := bson.M{}
	if since := c.Query("since"); since != "" {
		t, err := time.Parse(time.RFC3339, since)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid since: must be RFC3339 format"})
			return
		}
		dateFilter["$gte"] = t.UTC()
	}
	if until := c.Query("until"); until != "" {
		t, err := time.Parse(time.RFC3339, until)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid until: must be RFC3339 format"})
			return
		}
		dateFilter["$lte"] = t.UTC()
	}
	if len(dateFilter) > 0 {
		filter["cached_at"] = dateFilter
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	col := h.col()

	total, err := col.CountDocuments(ctx, filter)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to count prerender records"})
		return
	}
	cacheHits, _ := col.CountDocuments(ctx, mergeBSON(filter, bson.M{"from_cache": true}))
	cacheMisses := total - cacheHits

	var hitRate float64
	if total > 0 {
		hitRate = float64(cacheHits) / float64(total) * 100
	}

	// Average render time via aggregation pipeline.
	var avgRenderMs float64
	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: filter}},
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: nil},
			{Key: "avg", Value: bson.D{{Key: "$avg", Value: "$render_time_ms"}}},
		}}},
	}
	cur, err := col.Aggregate(ctx, pipeline)
	if err == nil {
		var res []struct {
			Avg float64 `bson:"avg"`
		}
		if cur.All(ctx, &res) == nil && len(res) > 0 {
			avgRenderMs = res[0].Avg
		}
	}

	// Per-requester-type counts.
	humanCount, _ := col.CountDocuments(ctx, mergeBSON(filter, bson.M{"requester_type": models.RequesterTypeHuman}))
	botCount, _ := col.CountDocuments(ctx, mergeBSON(filter, bson.M{"requester_type": models.RequesterTypeBot}))
	aiCount, _ := col.CountDocuments(ctx, mergeBSON(filter, bson.M{"requester_type": models.RequesterTypeAI}))

	c.JSON(http.StatusOK, models.PrerenderStats{
		TotalRequests: total,
		CacheHits:     cacheHits,
		CacheMisses:   cacheMisses,
		HitRate:       hitRate,
		AvgRenderMs:   avgRenderMs,
		ByRequester: models.PrerenderRequesterBreakdown{
			Human: humanCount,
			Bot:   botCount,
			AI:    aiCount,
		},
	})
}

// mergeBSON returns a new bson.M that is the union of base and extra.
// Keys in extra overwrite keys in base.
func mergeBSON(base, extra bson.M) bson.M {
	merged := make(bson.M, len(base)+len(extra))
	for k, v := range base {
		merged[k] = v
	}
	for k, v := range extra {
		merged[k] = v
	}
	return merged
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
