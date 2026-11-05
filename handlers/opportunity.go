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
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/creever/crawler-api/models"
	"github.com/creever/crawler-api/worker"
)

// OpportunityHandler holds dependencies for the opportunity endpoints.
type OpportunityHandler struct {
	db          *mongo.Database
	asynqClient *asynq.Client
}

// NewOpportunityHandler creates a new OpportunityHandler.
func NewOpportunityHandler(db *mongo.Database, asynqClient *asynq.Client) *OpportunityHandler {
	return &OpportunityHandler{db: db, asynqClient: asynqClient}
}

// List godoc
// @Summary      List ranked opportunities for a project
// @Tags         projects
// @Produce      json
// @Param        id    path  string  true  "Project ID"
// @Param        type  query string  false "Filter by type (striking_distance, low_ctr, losing_clicks, cannibalization, crawl_issue)"
// @Param        limit query int     false "Max results (default 100)"
// @Success      200  {array}  models.Opportunity
// @Router       /api/v1/projects/{id}/opportunities [get]
func (h *OpportunityHandler) List(c *gin.Context) {
	id, err := bson.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project id"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	filter := gscProjectFilter(c, id)
	if err = h.db.Collection("projects").FindOne(ctx, filter).Decode(&struct{}{}); err != nil {
		if err == mongo.ErrNoDocuments {
			c.JSON(http.StatusNotFound, gin.H{"error": "project not found"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch project"})
		}
		return
	}

	query := bson.M{"project_id": id}
	if t := c.Query("type"); t != "" {
		query["type"] = models.OpportunityType(t)
	}
	limit := 100
	if l, convErr := strconv.Atoi(c.DefaultQuery("limit", "100")); convErr == nil && l > 0 && l <= 500 {
		limit = l
	}

	opts := options.Find().SetSort(bson.D{{Key: "rank", Value: 1}}).SetLimit(int64(limit))
	cursor, err := h.db.Collection("opportunities").Find(ctx, query, opts)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch opportunities"})
		return
	}
	defer cursor.Close(ctx) //nolint:errcheck

	var opps []models.Opportunity
	if err = cursor.All(ctx, &opps); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to decode opportunities"})
		return
	}
	if opps == nil {
		opps = []models.Opportunity{}
	}
	c.JSON(http.StatusOK, opps)
}

// Trigger godoc
// @Summary      Enqueue an opportunity recomputation for a project
// @Tags         projects
// @Produce      json
// @Param        id  path  string  true  "Project ID"
// @Success      202  {object}  map[string]string
// @Router       /api/v1/projects/{id}/opportunities/compute [post]
func (h *OpportunityHandler) Trigger(c *gin.Context) {
	id, err := bson.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project id"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	filter := gscProjectFilter(c, id)
	if err = h.db.Collection("projects").FindOne(ctx, filter).Decode(&struct{}{}); err != nil {
		if err == mongo.ErrNoDocuments {
			c.JSON(http.StatusNotFound, gin.H{"error": "project not found"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch project"})
		}
		return
	}

	task, err := worker.NewComputeOppsTask(id.Hex())
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
