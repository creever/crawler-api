package handlers

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/hibiken/asynq"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/creever/crawler-api/middleware"
	"github.com/creever/crawler-api/models"
	"github.com/creever/crawler-api/worker"
)

// SuggestionHandler holds dependencies for the suggestion endpoints.
type SuggestionHandler struct {
	db          *mongo.Database
	asynqClient *asynq.Client
	inspector   *asynq.Inspector
}

// NewSuggestionHandler creates a new SuggestionHandler.
func NewSuggestionHandler(db *mongo.Database, asynqClient *asynq.Client, inspector *asynq.Inspector) *SuggestionHandler {
	return &SuggestionHandler{db: db, asynqClient: asynqClient, inspector: inspector}
}

// List godoc
// @Summary      List AI suggestions for a project
// @Tags         projects
// @Produce      json
// @Param        id      path  string  true  "Project ID"
// @Param        status  query string  false "Filter by status (pending, accepted, rejected, covered)"
// @Success      200  {array}  models.Suggestion
// @Router       /api/v1/projects/{id}/suggestions [get]
func (h *SuggestionHandler) List(c *gin.Context) {
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

	query := bson.M{"project_id": id}
	if s := c.Query("status"); s != "" {
		query["status"] = models.SuggestionStatus(s)
	}

	opts := options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}).SetLimit(500)
	cursor, err := h.db.Collection("suggestions").Find(ctx, query, opts)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch suggestions"})
		return
	}
	defer cursor.Close(ctx) //nolint:errcheck

	var suggestions []models.Suggestion
	if err = cursor.All(ctx, &suggestions); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to decode suggestions"})
		return
	}
	views, err := withTaskRefs(ctx, h.db, suggestions)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch tasks"})
		return
	}
	c.JSON(http.StatusOK, views)
}

// Generate godoc
// @Summary      Enqueue AI suggestion generation for a project
// @Tags         projects
// @Produce      json
// @Param        id    path  string  true  "Project ID"
// @Param        limit query int     false "Max opportunities to process (default 10)"
// @Success      202  {object}  map[string]string
// @Router       /api/v1/projects/{id}/suggestions/generate [post]
func (h *SuggestionHandler) Generate(c *gin.Context) {
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

	limit := 10
	if l := c.Query("limit"); l != "" {
		if parsed, convErr := parsePositiveInt(l); convErr == nil {
			limit = parsed
		}
	}

	task, err := worker.NewGenSuggestTask(worker.GenSuggestPayload{ProjectID: id.Hex(), Limit: limit})
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

// Review godoc
// @Summary      Accept or reject a suggestion
// @Description  Accepting creates a task, or joins one of the same problem on that page.
// @Tags         projects
// @Accept       json
// @Produce      json
// @Param        id     path  string  true  "Project ID"
// @Param        sid    path  string  true  "Suggestion ID"
// @Param        review body  object  true  "Review: {action: accept|reject, value?: string, reason?: string}"
// @Success      200  {object}  models.Suggestion
// @Router       /api/v1/projects/{id}/suggestions/{sid}/review [patch]
func (h *SuggestionHandler) Review(c *gin.Context) {
	id, err := bson.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project id"})
		return
	}
	sid, err := bson.ObjectIDFromHex(c.Param("sid"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid suggestion id"})
		return
	}

	var input struct {
		Action string `json:"action" binding:"required"` // accept | reject
		Value  string `json:"value"`                     // the reviewer's text, if edited
		Reason string `json:"reason"`                    // why it was rejected, optional
	}
	if err = c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if !models.IsRejectReason(input.Reason) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unknown rejection reason"})
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

	var existing models.Suggestion
	if err = h.db.Collection("suggestions").FindOne(ctx, bson.M{"_id": sid, "project_id": id}).Decode(&existing); err != nil {
		if err == mongo.ErrNoDocuments {
			c.JSON(http.StatusNotFound, gin.H{"error": "suggestion not found"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch suggestion"})
		}
		return
	}

	if existing.Status != models.SuggestionStatusPending {
		c.JSON(http.StatusConflict, gin.H{"error": "this suggestion was already decided (status: " + string(existing.Status) + ")"})
		return
	}

	switch input.Action {
	case "accept":
		value := input.Value
		if value == "" {
			value = existing.ProposedValue
		}
		if _, err = worker.AcceptSuggestion(ctx, h.db, existing, value, middleware.GetUserID(c)); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to accept suggestion"})
			return
		}
	case "reject":
		_, err = h.db.Collection("suggestions").UpdateOne(ctx, bson.M{"_id": sid, "project_id": id}, bson.M{"$set": bson.M{
			"status": models.SuggestionStatusRejected, "reject_reason": input.Reason, "reviewed_by": middleware.GetUserID(c),
			"rejected_impressions": worker.PageStat(ctx, h.db, id, existing.Page).Impressions, "updated_at": time.Now().UTC(),
		}})
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update suggestion"})
			return
		}
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "action must be accept or reject"})
		return
	}

	var suggestion models.Suggestion
	if err = h.db.Collection("suggestions").FindOne(ctx, bson.M{"_id": sid}).Decode(&suggestion); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch suggestion"})
		return
	}
	views, err := withTaskRefs(ctx, h.db, []models.Suggestion{suggestion})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch its task"})
		return
	}
	c.JSON(http.StatusOK, views[0])
}

func parsePositiveInt(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid positive int")
	}
	return n, nil
}

// Status godoc
// @Summary      Suggestion generation job status for a project
// @Description  Reports whether a suggestions:generate task is pending/active in the asynq queue, plus counts by status.
// @Tags         projects
// @Produce      json
// @Param        id  path  string  true  "Project ID"
// @Success      200  {object}  map[string]interface{}
// @Router       /api/v1/projects/{id}/suggestions/status [get]
func (h *SuggestionHandler) Status(c *gin.Context) {
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

	resp := gin.H{"project_id": id.Hex(), "pending": false, "active": false, "pending_count": 0, "last_task_error": nil}

	// Surface the last failure of a suggestions:generate task for this project
	// from archived (failed) asynq tasks, so a silently empty run is visible.
	if h.inspector != nil {
		archived, archErr := h.inspector.ListArchivedTasks("default", asynq.PageSize(20))
		if archErr == nil {
			for _, t := range archived {
				if t.Type == "suggestions:generate" && payloadHasProject(t.Payload, id.Hex()) {
					resp["last_task_error"] = t.LastErr
					break
				}
			}
		}
	}

	if h.inspector != nil {
		pending, err := h.inspector.ListPendingTasks("default")
		if err == nil {
			for _, t := range pending {
				if t.Type == "suggestions:generate" && payloadHasProject(t.Payload, id.Hex()) {
					resp["pending"] = true
				}
			}
		}
		active, err := h.inspector.ListActiveTasks("default")
		if err == nil {
			for _, t := range active {
				if t.Type == "suggestions:generate" && payloadHasProject(t.Payload, id.Hex()) {
					resp["active"] = true
				}
			}
		}
	}

	count, _ := h.db.Collection("suggestions").CountDocuments(ctx, bson.M{"project_id": id, "status": models.SuggestionStatusPending})
	resp["pending_count"] = count

	c.JSON(http.StatusOK, resp)
}
