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

	"github.com/creever/crawler-api/models"
	"github.com/creever/crawler-api/worker"
)

// QueueHandler holds dependencies for crawl queue endpoints.
type QueueHandler struct {
	db          *mongo.Database
	asynqClient *asynq.Client
}

// NewQueueHandler creates a new QueueHandler.
func NewQueueHandler(db *mongo.Database, asynqClient *asynq.Client) *QueueHandler {
	return &QueueHandler{db: db, asynqClient: asynqClient}
}

func (h *QueueHandler) col() *mongo.Collection {
	return h.db.Collection("crawl_queue")
}

// List godoc
// @Summary      List crawl queue entries (optionally filtered by project or status)
// @Tags         queue
// @Produce      json
// @Param        project_id  query  string  false  "Project ID filter"
// @Param        status      query  string  false  "Status filter (pending|processing|done|failed)"
// @Success      200  {array}   models.QueueEntry
// @Router       /api/v1/queue [get]
func (h *QueueHandler) List(c *gin.Context) {
	filter := bson.M{}
	if pid := c.Query("project_id"); pid != "" {
		oid, err := bson.ObjectIDFromHex(pid)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project_id"})
			return
		}
		filter["project_id"] = oid
	}
	if status := c.Query("status"); status != "" {
		filter["status"] = status
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Higher priority first; within same priority, oldest enqueued first.
	opts := options.Find().SetSort(bson.D{
		{Key: "priority", Value: -1},
		{Key: "enqueued_at", Value: 1},
	})
	cursor, err := h.col().Find(ctx, filter, opts)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch queue entries"})
		return
	}
	defer cursor.Close(ctx)

	var results []models.QueueEntry
	if err = cursor.All(ctx, &results); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to decode queue entries"})
		return
	}
	if results == nil {
		results = []models.QueueEntry{}
	}
	c.JSON(http.StatusOK, results)
}

// Get godoc
// @Summary      Get a single crawl queue entry by ID
// @Tags         queue
// @Produce      json
// @Param        id  path  string  true  "Queue entry ID"
// @Success      200  {object}  models.QueueEntry
// @Failure      404  {object}  map[string]string
// @Router       /api/v1/queue/{id} [get]
func (h *QueueHandler) Get(c *gin.Context) {
	id, err := bson.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var entry models.QueueEntry
	if err = h.col().FindOne(ctx, bson.M{"_id": id}).Decode(&entry); err != nil {
		if err == mongo.ErrNoDocuments {
			c.JSON(http.StatusNotFound, gin.H{"error": "queue entry not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch queue entry"})
		return
	}
	c.JSON(http.StatusOK, entry)
}

// Enqueue godoc
// @Summary      Add a URL to the crawl queue — creates an asynq background job
// @Tags         queue
// @Accept       json
// @Produce      json
// @Param        data  body      models.QueueEntry  true  "Queue entry (task_type: seo|render)"
// @Success      201   {object}  models.QueueEntry
// @Router       /api/v1/queue [post]
func (h *QueueHandler) Enqueue(c *gin.Context) {
	var entry models.QueueEntry
	if err := c.ShouldBindJSON(&entry); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Default task type to SEO when not specified.
	if entry.TaskType == "" {
		entry.TaskType = models.QueueTaskTypeSEO
	}

	// Look up the project to build the crawler config.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var project models.Project
	if err := h.db.Collection("projects").FindOne(ctx, bson.M{"_id": entry.ProjectID}).Decode(&project); err != nil {
		if err == mongo.ErrNoDocuments {
			c.JSON(http.StatusNotFound, gin.H{"error": "project not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch project"})
		return
	}

	config := models.ProjectConfig{
		ProjectID: project.ID.Hex(),
		SeedURLs:  []string{entry.URL},
		UseJS:     project.UseJS,
		MaxPages:  project.MaxPages,
	}

	// Assign a stable ID so the task payload can reference it.
	entry.ID = bson.NewObjectID()
	entry.Status = models.QueueStatusPending
	entry.EnqueuedAt = time.Now().UTC()

	// Build and dispatch the asynq task.
	var (
		task *asynq.Task
		err  error
	)
	switch entry.TaskType {
	case models.QueueTaskTypeRender:
		task, err = worker.NewCrawlRenderTask(entry.ID.Hex(), config)
	default:
		task, err = worker.NewCrawlSEOTask(entry.ID.Hex(), config)
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to build crawl task"})
		return
	}

	info, err := h.asynqClient.EnqueueContext(ctx, task)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to dispatch crawl task: " + err.Error()})
		return
	}
	entry.AsynqTaskID = info.ID

	// Persist the queue entry after successful dispatch.
	if _, err = h.col().InsertOne(ctx, entry); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to store queue entry"})
		return
	}
	c.JSON(http.StatusCreated, entry)
}

// UpdateStatus godoc
// @Summary      Update the processing status of a queue entry
// @Tags         queue
// @Accept       json
// @Produce      json
// @Param        id    path      string                           true  "Queue entry ID"
// @Param        data  body      models.QueueEntryStatusUpdate    true  "Status update"
// @Success      200   {object}  models.QueueEntry
// @Failure      404   {object}  map[string]string
// @Router       /api/v1/queue/{id} [patch]
func (h *QueueHandler) UpdateStatus(c *gin.Context) {
	id, err := bson.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}

	var input models.QueueEntryStatusUpdate
	if err = c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Record the completion time when the entry reaches a terminal state.
	if input.Status == models.QueueStatusDone || input.Status == models.QueueStatusFailed {
		now := time.Now().UTC()
		input.ProcessedAt = &now
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var updated models.QueueEntry
	err = h.col().FindOneAndUpdate(
		ctx,
		bson.M{"_id": id},
		bson.M{"$set": input},
		options.FindOneAndUpdate().SetReturnDocument(options.After),
	).Decode(&updated)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			c.JSON(http.StatusNotFound, gin.H{"error": "queue entry not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update queue entry"})
		return
	}
	c.JSON(http.StatusOK, updated)
}

// Delete godoc
// @Summary      Remove a crawl queue entry
// @Tags         queue
// @Param        id  path  string  true  "Queue entry ID"
// @Success      204
// @Failure      404  {object}  map[string]string
// @Router       /api/v1/queue/{id} [delete]
func (h *QueueHandler) Delete(c *gin.Context) {
	id, err := bson.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, err := h.col().DeleteOne(ctx, bson.M{"_id": id})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete queue entry"})
		return
	}
	if result.DeletedCount == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "queue entry not found"})
		return
	}
	c.Status(http.StatusNoContent)
}
