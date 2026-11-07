package handlers

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/hibiken/asynq"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.uber.org/zap"

	"github.com/creever/crawler-api/worker"
)

// GA4Handler holds dependencies for the GA4 endpoints.
type GA4Handler struct {
	db          *mongo.Database
	asynqClient *asynq.Client
	logger      *zap.Logger
}

// NewGA4Handler creates a new GA4Handler.
func NewGA4Handler(db *mongo.Database, asynqClient *asynq.Client, logger *zap.Logger) *GA4Handler {
	return &GA4Handler{db: db, asynqClient: asynqClient, logger: logger}
}

// TriggerSync godoc
// @Summary      Enqueue a GA4 sync for a project
// @Tags         projects
// @Produce      json
// @Param        id  path  string  true  "Project ID"
// @Success      202  {object}  map[string]string
// @Router       /api/v1/projects/{id}/ga4/sync [post]
func (h *GA4Handler) TriggerSync(c *gin.Context) {
	id, err := bson.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project id"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err = h.db.Collection("projects").FindOne(ctx, gscProjectFilter(c, id)).Decode(&struct{}{}); err != nil {
		if err == mongo.ErrNoDocuments {
			c.JSON(http.StatusNotFound, gin.H{"error": "project not found"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch project"})
		}
		return
	}

	task, err := worker.NewGA4SyncTask(id.Hex(), c.Query("start"), c.Query("end"))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to build task"})
		return
	}
	info, err := h.asynqClient.EnqueueContext(ctx, task)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to enqueue task"})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"status": "enqueued", "task_id": info.ID, "project_id": id.Hex()})
}

// Status godoc
// @Summary      Get GA4 sync status and row count for a project
// @Tags         projects
// @Produce      json
// @Param        id  path  string  true  "Project ID"
// @Success      200  {object}  map[string]interface{}
// @Router       /api/v1/projects/{id}/ga4/status [get]
func (h *GA4Handler) Status(c *gin.Context) {
	id, err := bson.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project id"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err = h.db.Collection("projects").FindOne(ctx, gscProjectFilter(c, id)).Decode(&struct{}{}); err != nil {
		if err == mongo.ErrNoDocuments {
			c.JSON(http.StatusNotFound, gin.H{"error": "project not found"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch project"})
		}
		return
	}

	var state struct {
		LastDate  string    `bson:"last_date" json:"last_date"`
		LastRunAt time.Time `bson:"last_run_at" json:"last_run_at"`
		LastError string    `bson:"last_error" json:"last_error"`
		TotalRows int64     `bson:"total_rows" json:"total_rows"`
	}
	stateErr := h.db.Collection("ga4_sync_state").FindOne(ctx, bson.M{"project_id": id}).Decode(&state)

	count, countErr := h.db.Collection("ga4_rows").CountDocuments(ctx, bson.M{"project_id": id})
	if countErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to count rows"})
		return
	}

	resp := gin.H{"project_id": id.Hex(), "row_count": count, "synced": stateErr == nil}
	if stateErr == nil {
		resp["state"] = state
	}
	c.JSON(http.StatusOK, resp)
}

// Report godoc
// @Summary      GA4 landing-page report (sessions, conversions, revenue)
// @Tags         projects
// @Produce      json
// @Param        id    path  string  true  "Project ID"
// @Param        days  query int     false "Lookback days (default 28)"
// @Success      200  {array}  map[string]interface{}
// @Router       /api/v1/projects/{id}/ga4/report [get]
func (h *GA4Handler) Report(c *gin.Context) {
	id, err := bson.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project id"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err = h.db.Collection("projects").FindOne(ctx, gscProjectFilter(c, id)).Decode(&struct{}{}); err != nil {
		if err == mongo.ErrNoDocuments {
			c.JSON(http.StatusNotFound, gin.H{"error": "project not found"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch project"})
		}
		return
	}

	days := 28
	if d, convErr := strconv.Atoi(c.DefaultQuery("days", "28")); convErr == nil && d > 0 && d <= 400 {
		days = d
	}
	since := time.Now().UTC().AddDate(0, 0, -days).Format(worker.GA4DateLayout)

	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: bson.D{
			{Key: "project_id", Value: id},
			{Key: "date", Value: bson.D{{Key: "$gte", Value: since}}},
		}}},
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$page"},
			{Key: "sessions", Value: bson.D{{Key: "$sum", Value: "$sessions"}}},
			{Key: "conversions", Value: bson.D{{Key: "$sum", Value: "$conversions"}}},
			{Key: "revenue", Value: bson.D{{Key: "$sum", Value: "$revenue"}}},
		}}},
		{{Key: "$sort", Value: bson.D{{Key: "revenue", Value: -1}}}},
		{{Key: "$limit", Value: 100}},
	}

	cursor, err := h.db.Collection("ga4_rows").Aggregate(ctx, pipeline)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to aggregate ga4 rows"})
		return
	}
	defer cursor.Close(ctx) //nolint:errcheck

	var rows []bson.M
	if err = cursor.All(ctx, &rows); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to decode report"})
		return
	}
	if rows == nil {
		rows = []bson.M{}
	}
	c.JSON(http.StatusOK, gin.H{"project_id": id.Hex(), "days": days, "rows": rows})
}
