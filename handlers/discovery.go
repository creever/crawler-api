package handlers

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/hibiken/asynq"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.uber.org/zap"

	"github.com/creever/crawler-api/models"
	"github.com/creever/crawler-api/worker"
)

// DiscoveryHandler holds dependencies for discovery endpoints.
type DiscoveryHandler struct {
	db          *mongo.Database
	asynqClient *asynq.Client
	logger      *zap.Logger
}

// NewDiscoveryHandler creates a new DiscoveryHandler.
func NewDiscoveryHandler(db *mongo.Database, asynqClient *asynq.Client, logger *zap.Logger) *DiscoveryHandler {
	return &DiscoveryHandler{db: db, asynqClient: asynqClient, logger: logger}
}

func (h *DiscoveryHandler) col() *mongo.Collection {
	return h.db.Collection("discoveries")
}

// List godoc
// @Summary      List discovery runs (optionally filtered by project), including real-time crawl progress
// @Tags         discovery
// @Produce      json
// @Param        project_id  query  string  false  "Project ID filter"
// @Success      200  {array}   models.DiscoverySummary
// @Router       /api/v1/discover [get]
func (h *DiscoveryHandler) List(c *gin.Context) {
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

	opts := options.Find().SetSort(bson.D{{Key: "started_at", Value: -1}})
	cursor, err := h.col().Find(ctx, filter, opts)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch discoveries"})
		return
	}
	defer cursor.Close(ctx)

	var discoveries []models.Discovery
	if err = cursor.All(ctx, &discoveries); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to decode discoveries"})
		return
	}

	// Collect all discovery ObjectIDs for the queue-count aggregation.
	discoveryOIDs := make([]bson.ObjectID, 0, len(discoveries))
	for _, d := range discoveries {
		discoveryOIDs = append(discoveryOIDs, d.ID)
	}

	// pageCounts maps discoveryID -> per-status counts.
	type statusCounts struct{ done, failed, pending int }
	pageCounts := make(map[bson.ObjectID]*statusCounts, len(discoveries))
	for _, oid := range discoveryOIDs {
		pageCounts[oid] = &statusCounts{}
	}

	if len(discoveryOIDs) > 0 {
		pipeline := mongo.Pipeline{
			{{Key: "$match", Value: bson.M{
				"discovery_id": bson.M{"$in": discoveryOIDs},
			}}},
			{{Key: "$group", Value: bson.M{
				"_id": bson.M{
					"discovery_id": "$discovery_id",
					"status":       "$status",
				},
				"count": bson.M{"$sum": 1},
			}}},
		}

		type aggEntry struct {
			ID struct {
				DiscoveryID bson.ObjectID     `bson:"discovery_id"`
				Status      models.QueueStatus `bson:"status"`
			} `bson:"_id"`
			Count int `bson:"count"`
		}

		aggCursor, aggErr := h.db.Collection("crawl_queue").Aggregate(ctx, pipeline)
		if aggErr != nil {
			h.logger.Warn("discovery list: failed to aggregate queue counts", zap.Error(aggErr))
		} else {
			defer aggCursor.Close(ctx)
			var aggResults []aggEntry
			if aggErr = aggCursor.All(ctx, &aggResults); aggErr != nil {
				h.logger.Warn("discovery list: failed to decode queue counts", zap.Error(aggErr))
			}
			for _, ar := range aggResults {
				if sc, ok := pageCounts[ar.ID.DiscoveryID]; ok {
					switch ar.ID.Status {
					case models.QueueStatusDone:
						sc.done += ar.Count
					case models.QueueStatusFailed:
						sc.failed += ar.Count
					case models.QueueStatusPending, models.QueueStatusProcessing:
						sc.pending += ar.Count
					}
				}
			}
		}
	}

	results := make([]models.DiscoverySummary, 0, len(discoveries))
	for _, d := range discoveries {
		s := models.DiscoverySummary{Discovery: d}
		if sc, ok := pageCounts[d.ID]; ok {
			s.PagesDone = sc.done
			s.PagesFailed = sc.failed
			s.PagesPending = sc.pending
		}
		results = append(results, s)
	}

	c.JSON(http.StatusOK, results)
}

// Get godoc
// @Summary      Get a discovery run by ID, including real-time crawl progress
// @Tags         discovery
// @Produce      json
// @Param        id  path  string  true  "Discovery ID"
// @Success      200  {object}  models.DiscoverySummary
// @Failure      404  {object}  map[string]string
// @Router       /api/v1/discover/{id} [get]
func (h *DiscoveryHandler) Get(c *gin.Context) {
	id, err := bson.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var discovery models.Discovery
	if err = h.col().FindOne(ctx, bson.M{"_id": id}).Decode(&discovery); err != nil {
		if err == mongo.ErrNoDocuments {
			c.JSON(http.StatusNotFound, gin.H{"error": "discovery not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch discovery"})
		return
	}

	summary := models.DiscoverySummary{Discovery: discovery}

	// Count the crawl-queue entries that belong to this discovery.
	if len(discovery.QueueEntryIDs) > 0 {
		queueCol := h.db.Collection("crawl_queue")
		for _, status := range []models.QueueStatus{
			models.QueueStatusDone,
			models.QueueStatusFailed,
			models.QueueStatusPending,
			models.QueueStatusProcessing,
		} {
			n, countErr := queueCol.CountDocuments(ctx, bson.M{
				"discovery_id": id,
				"status":       status,
			})
			if countErr != nil {
				continue
			}
			switch status {
			case models.QueueStatusDone:
				summary.PagesDone = int(n)
			case models.QueueStatusFailed:
				summary.PagesFailed = int(n)
			case models.QueueStatusPending, models.QueueStatusProcessing:
				summary.PagesPending += int(n)
			}
		}
	}

	c.JSON(http.StatusOK, summary)
}

// startDiscoveryRequest is the request body for POST /api/v1/discover.
type startDiscoveryRequest struct {
	ProjectID string `json:"project_id" binding:"required"`
}

// Start godoc
// @Summary      Start a whole-website discovery run for a project
// @Tags         discovery
// @Accept       json
// @Produce      json
// @Param        data  body      startDiscoveryRequest  true  "Project to discover"
// @Success      201   {object}  models.Discovery
// @Failure      404   {object}  map[string]string
// @Router       /api/v1/discover [post]
func (h *DiscoveryHandler) Start(c *gin.Context) {
	var req startDiscoveryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	projectOID, err := bson.ObjectIDFromHex(req.ProjectID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project_id"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Fetch project to build crawler config.
	var project models.Project
	if err = h.db.Collection("projects").FindOne(ctx, bson.M{"_id": projectOID}).Decode(&project); err != nil {
		if err == mongo.ErrNoDocuments {
			c.JSON(http.StatusNotFound, gin.H{"error": "project not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch project"})
		return
	}

	// Cap at 10 pages when the project has no explicit limit.
	maxPages := project.MaxPages
	if maxPages <= 0 {
		maxPages = 10
	}

	config := models.ProjectConfig{
		ProjectID: project.ID.Hex(),
		SeedURLs:  []string{project.URL},
		UseJS:     project.UseJS,
		MaxPages:  maxPages,
	}

	// Persist the discovery document first so the worker can update it.
	discovery := models.Discovery{
		ID:            bson.NewObjectID(),
		ProjectID:     projectOID,
		Status:        models.DiscoveryStatusPending,
		QueueEntryIDs: []string{},
		StartedAt:     time.Now().UTC(),
	}
	if _, err = h.col().InsertOne(ctx, discovery); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create discovery"})
		return
	}

	// Dispatch the discover task.
	task, err := worker.NewCrawlDiscoverTask(discovery.ID.Hex(), config)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to build discover task"})
		return
	}
	if _, err = h.asynqClient.EnqueueContext(ctx, task); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to dispatch discover task: " + err.Error()})
		return
	}

	c.JSON(http.StatusCreated, discovery)
}

// Delete godoc
// @Summary      Delete a discovery run record
// @Tags         discovery
// @Param        id  path  string  true  "Discovery ID"
// @Success      204
// @Failure      404  {object}  map[string]string
// @Router       /api/v1/discover/{id} [delete]
func (h *DiscoveryHandler) Delete(c *gin.Context) {
	id, err := bson.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, err := h.col().DeleteOne(ctx, bson.M{"_id": id})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete discovery"})
		return
	}
	if result.DeletedCount == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "discovery not found"})
		return
	}
	c.Status(http.StatusNoContent)
}
