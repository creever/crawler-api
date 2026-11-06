package handlers

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/hibiken/asynq"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/creever/crawler-api/middleware"
	"github.com/creever/crawler-api/models"
	"github.com/creever/crawler-api/worker"
)

// GSCHandler holds dependencies for the GSC sync endpoints.
type GSCHandler struct {
	db          *mongo.Database
	asynqClient *asynq.Client
}

// NewGSCHandler creates a new GSCHandler.
func NewGSCHandler(db *mongo.Database, asynqClient *asynq.Client) *GSCHandler {
	return &GSCHandler{db: db, asynqClient: asynqClient}
}

// projectFilter returns a filter restricted to the caller's project (admins
// see any project).
func gscProjectFilter(c *gin.Context, id bson.ObjectID) bson.M {
	filter := bson.M{"_id": id}
	if !middleware.IsAdmin(c) {
		filter["owner_id"] = middleware.GetUserID(c)
	}
	return filter
}

// TriggerSync godoc
// @Summary      Enqueue a GSC sync for a project
// @Description  Enqueues a gsc:sync asynq task. Pass ?backfill=true to force a full 16-month history fetch, or ?start=YYYY-MM-DD&end=YYYY-MM-DD for an explicit range.
// @Tags         projects
// @Produce      json
// @Param        id       path  string  true  "Project ID"
// @Param        backfill query bool    false "Force full history backfill"
// @Param        start    query string  false "Start date YYYY-MM-DD"
// @Param        end      query string  false "End date YYYY-MM-DD"
// @Success      202  {object}  map[string]string
// @Router       /api/v1/projects/{id}/gsc/sync [post]
func (h *GSCHandler) TriggerSync(c *gin.Context) {
	id, err := bson.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project id"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var project models.Project
	if err = h.db.Collection("projects").FindOne(ctx, gscProjectFilter(c, id)).Decode(&project); err != nil {
		if err == mongo.ErrNoDocuments {
			c.JSON(http.StatusNotFound, gin.H{"error": "project not found"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch project"})
		}
		return
	}
	if _, _, cfgErr := worker.ResolveGSCConfig(ctx, h.db, project); cfgErr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": cfgErr.Error()})
		return
	}

	payload := worker.GSCSyncPayload{
		ProjectID: id.Hex(),
		StartDate: c.Query("start"),
		EndDate:   c.Query("end"),
		Backfill:  c.Query("backfill") == "true",
	}
	task, err := worker.NewGSCSyncTask(payload)
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
// @Summary      Get GSC sync status and row count for a project
// @Tags         projects
// @Produce      json
// @Param        id  path  string  true  "Project ID"
// @Success      200  {object}  map[string]interface{}
// @Router       /api/v1/projects/{id}/gsc/status [get]
func (h *GSCHandler) Status(c *gin.Context) {
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

	var state models.GSCSyncState
	stateErr := h.db.Collection("gsc_sync_state").FindOne(ctx, bson.M{"project_id": id}).Decode(&state)

	count, countErr := h.db.Collection("gsc_rows").CountDocuments(ctx, bson.M{"project_id": id})
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
