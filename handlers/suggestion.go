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
}

// NewSuggestionHandler creates a new SuggestionHandler.
func NewSuggestionHandler(db *mongo.Database, asynqClient *asynq.Client) *SuggestionHandler {
	return &SuggestionHandler{db: db, asynqClient: asynqClient}
}

// List godoc
// @Summary      List AI suggestions for a project
// @Tags         projects
// @Produce      json
// @Param        id      path  string  true  "Project ID"
// @Param        status  query string  false "Filter by status (pending, approved, rejected, edited)"
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
	if suggestions == nil {
		suggestions = []models.Suggestion{}
	}
	c.JSON(http.StatusOK, suggestions)
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
// @Summary      Approve, reject or edit a suggestion
// @Tags         projects
// @Accept       json
// @Produce      json
// @Param        id     path  string  true  "Project ID"
// @Param        sid    path  string  true  "Suggestion ID"
// @Param        review body  object  true  "Review action: {action: approve|reject|edit, value?: string}"
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
		Action string `json:"action" binding:"required"` // approve | reject | edit
		Value  string `json:"value"`                     // new text when action=edit
	}
	if err = c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
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

	update := bson.M{
		"updated_at":  time.Now().UTC(),
		"reviewed_by": middleware.GetUserID(c),
	}

	switch input.Action {
	case "approve":
		update["status"] = models.SuggestionStatusApproved
		update["final_value"] = existing.ProposedValue
	case "reject":
		update["status"] = models.SuggestionStatusRejected
	case "edit":
		if input.Value == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "value is required for edit"})
			return
		}
		update["status"] = models.SuggestionStatusEdited
		update["final_value"] = input.Value
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "action must be approve, reject or edit"})
		return
	}

	result := h.db.Collection("suggestions").FindOneAndUpdate(
		ctx,
		bson.M{"_id": sid, "project_id": id},
		bson.M{"$set": update},
		options.FindOneAndUpdate().SetReturnDocument(options.After),
	)
	if result.Err() != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update suggestion"})
		return
	}

	var suggestion models.Suggestion
	if err = result.Decode(&suggestion); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to decode suggestion"})
		return
	}
	c.JSON(http.StatusOK, suggestion)
}

func parsePositiveInt(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid positive int")
	}
	return n, nil
}
