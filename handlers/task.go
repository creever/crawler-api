package handlers

import (
	"context"
	"errors"
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

// taskSuggestion is a suggestion as listed on its task.
type taskSuggestion struct {
	ID     bson.ObjectID           `json:"id"`
	Page   string                  `json:"page"`
	Kind   string                  `json:"kind"`
	Status models.SuggestionStatus `json:"status"`
}

// taskView is a task with the brief a harness works from and its suggestions.
type taskView struct {
	models.SEOTask
	Brief       string           `json:"brief"`
	Suggestions []taskSuggestion `json:"suggestions"`
}

// taskRef is what a suggestion shows of its task.
type taskRef struct {
	ID     bson.ObjectID     `json:"id"`
	Title  string            `json:"title"`
	Status models.TaskStatus `json:"status"`
	Scope  string            `json:"scope"`
	Pages  int               `json:"pages"`
}

// suggestionView is a suggestion with the current state of its task.
type suggestionView struct {
	models.Suggestion
	Task *taskRef `json:"task,omitempty"`
}

// withTaskRefs adds to each suggestion the live state of its task.
func withTaskRefs(ctx context.Context, db *mongo.Database, suggestions []models.Suggestion) ([]suggestionView, error) {
	views := make([]suggestionView, 0, len(suggestions))
	var ids []bson.ObjectID
	for _, s := range suggestions {
		views = append(views, suggestionView{Suggestion: s})
		if s.TaskID != nil {
			ids = append(ids, *s.TaskID)
		}
	}
	if len(ids) == 0 {
		return views, nil
	}
	cursor, err := db.Collection("seo_tasks").Find(ctx, bson.M{"_id": bson.M{"$in": ids}},
		options.Find().SetProjection(bson.M{"title": 1, "status": 1, "scope": 1, "metrics": 1}))
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx) //nolint:errcheck
	var tasks []models.SEOTask
	if err = cursor.All(ctx, &tasks); err != nil {
		return nil, err
	}
	refs := make(map[bson.ObjectID]*taskRef, len(tasks))
	for _, t := range tasks {
		refs[t.ID] = &taskRef{ID: t.ID, Title: t.Title, Status: t.Status, Scope: t.Scope, Pages: t.Metrics.Pages}
	}
	for i := range views {
		if id := views[i].TaskID; id != nil {
			views[i].Task = refs[*id]
		}
	}
	return views, nil
}

// TaskHandler holds dependencies for the task endpoints.
type TaskHandler struct {
	db          *mongo.Database
	asynqClient *asynq.Client
}

// NewTaskHandler creates a new TaskHandler.
func NewTaskHandler(db *mongo.Database, asynqClient *asynq.Client) *TaskHandler {
	return &TaskHandler{db: db, asynqClient: asynqClient}
}

// projectFromRequest resolves the :id project the caller may access. It writes
// the error response itself.
func projectFromRequest(ctx context.Context, c *gin.Context, db *mongo.Database) (bson.ObjectID, bool) {
	id, err := bson.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project id"})
		return id, false
	}
	if err = db.Collection("projects").FindOne(ctx, gscProjectFilter(c, id)).Decode(&struct{}{}); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			c.JSON(http.StatusNotFound, gin.H{"error": "project not found"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch project"})
		}
		return id, false
	}
	return id, true
}

func listTasks(ctx context.Context, db *mongo.Database, query bson.M, limit int64) ([]models.SEOTask, error) {
	cursor, err := db.Collection("seo_tasks").Find(ctx, query, options.Find().
		SetSort(bson.D{{Key: "metrics.impressions", Value: -1}, {Key: "metrics.pages", Value: -1}, {Key: "_id", Value: 1}}).
		SetLimit(limit))
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx) //nolint:errcheck
	tasks := []models.SEOTask{}
	if err = cursor.All(ctx, &tasks); err != nil {
		return nil, err
	}
	return tasks, nil
}

func (h *TaskHandler) views(ctx context.Context, tasks []models.SEOTask) ([]taskView, error) {
	views := make([]taskView, 0, len(tasks))
	ids := make([]bson.ObjectID, 0, len(tasks))
	index := make(map[bson.ObjectID]int, len(tasks))
	for i := range tasks {
		views = append(views, taskView{SEOTask: tasks[i], Brief: tasks[i].Brief(), Suggestions: []taskSuggestion{}})
		ids = append(ids, tasks[i].ID)
		index[tasks[i].ID] = i
	}
	if len(ids) == 0 {
		return views, nil
	}
	cursor, err := h.db.Collection("suggestions").Find(ctx, bson.M{"task_id": bson.M{"$in": ids}},
		options.Find().SetProjection(bson.M{"page": 1, "kind": 1, "status": 1, "task_id": 1}))
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx) //nolint:errcheck
	var rows []models.Suggestion
	if err = cursor.All(ctx, &rows); err != nil {
		return nil, err
	}
	for _, s := range rows {
		i := index[*s.TaskID]
		views[i].Suggestions = append(views[i].Suggestions, taskSuggestion{ID: s.ID, Page: s.Page, Kind: s.Kind, Status: s.Status})
	}
	return views, nil
}

// List godoc
// @Summary      List a project's tasks, biggest reach first
// @Tags         projects
// @Produce      json
// @Param        id      path  string  true  "Project ID"
// @Param        status  query string  false "Filter by status"
// @Success      200  {array}  models.SEOTask
// @Router       /api/v1/projects/{id}/tasks [get]
func (h *TaskHandler) List(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id, ok := projectFromRequest(ctx, c, h.db)
	if !ok {
		return
	}
	query := bson.M{"project_id": id}
	if status := c.Query("status"); status != "" {
		query["status"] = models.TaskStatus(status)
	}
	tasks, err := listTasks(ctx, h.db, query, 200)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch tasks"})
		return
	}
	views, err := h.views(ctx, tasks)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch task suggestions"})
		return
	}
	c.JSON(http.StatusOK, views)
}

// Impact godoc
// @Summary      List finished tasks with their measured effect, newest first
// @Tags         projects
// @Produce      json
// @Param        id  path  string  true  "Project ID"
// @Success      200  {array}  models.SEOTask
// @Router       /api/v1/projects/{id}/impact [get]
func (h *TaskHandler) Impact(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id, ok := projectFromRequest(ctx, c, h.db)
	if !ok {
		return
	}
	cursor, err := h.db.Collection("seo_tasks").Find(ctx, bson.M{"project_id": id, "done_at": bson.M{"$exists": true}},
		options.Find().SetSort(bson.D{{Key: "done_at", Value: -1}}).SetLimit(200))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch tasks"})
		return
	}
	defer cursor.Close(ctx) //nolint:errcheck
	tasks := []models.SEOTask{}
	if err = cursor.All(ctx, &tasks); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to decode tasks"})
		return
	}
	c.JSON(http.StatusOK, tasks)
}

// Update godoc
// @Summary      Change a task's status
// @Description  action is approve, claim, done, confirm, dismiss or reopen. done starts the recheck.
// @Tags         projects
// @Accept       json
// @Produce      json
// @Param        id   path  string  true  "Project ID"
// @Param        tid  path  string  true  "Task ID"
// @Success      200  {object}  models.SEOTask
// @Router       /api/v1/projects/{id}/tasks/{tid} [patch]
func (h *TaskHandler) Update(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	id, ok := projectFromRequest(ctx, c, h.db)
	if !ok {
		return
	}
	tid, err := bson.ObjectIDFromHex(c.Param("tid"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid task id"})
		return
	}
	var input struct {
		Action  string `json:"action" binding:"required"`
		Summary string `json:"summary"`
	}
	if err = c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	filter := bson.M{"_id": tid, "project_id": id}
	var task *models.SEOTask
	switch input.Action {
	case "approve":
		task, err = moveTask(ctx, h.db, filter, []models.TaskStatus{models.TaskStatusProposed}, bson.M{"status": models.TaskStatusOpen})
	case "claim":
		task, err = claimTask(ctx, h.db, filter, "user")
	case "done":
		task, err = completeTask(ctx, h.db, h.asynqClient, filter, input.Summary)
	case "confirm":
		filter["verification.checked"] = 0
		task, err = moveTask(ctx, h.db, filter, []models.TaskStatus{models.TaskStatusDone}, bson.M{"status": models.TaskStatusVerified})
	case "dismiss":
		task, err = dismissTask(ctx, h.db, filter)
	case "reopen":
		task, err = moveTask(ctx, h.db, filter, []models.TaskStatus{models.TaskStatusDismissed, models.TaskStatusResolved, models.TaskStatusClosed},
			bson.M{"status": models.TaskStatusOpen, "reopen_reason": "A person reopened this task."})
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "action must be approve, claim, done, confirm, dismiss or reopen"})
		return
	}
	if errors.Is(err, mongo.ErrNoDocuments) {
		c.JSON(http.StatusConflict, gin.H{"error": "task not found, or its status does not allow this"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if err = worker.RebalanceTasks(ctx, h.db, id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	views, err := h.views(ctx, []models.SEOTask{*task})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, views[0])
}

// moveTask changes a task's state if it is in one of the given statuses.
func moveTask(ctx context.Context, db *mongo.Database, filter bson.M, from []models.TaskStatus, set bson.M) (*models.SEOTask, error) {
	filter["status"] = bson.M{"$in": from}
	set["updated_at"] = time.Now().UTC()
	var task models.SEOTask
	err := db.Collection("seo_tasks").FindOneAndUpdate(ctx, filter, bson.M{"$set": set},
		options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&task)
	if err != nil {
		return nil, err
	}
	return &task, nil
}

// dismissTask keeps the task's reach, which it has to double to come back.
func dismissTask(ctx context.Context, db *mongo.Database, filter bson.M) (*models.SEOTask, error) {
	var current models.SEOTask
	if err := db.Collection("seo_tasks").FindOne(ctx, filter).Decode(&current); err != nil {
		return nil, err
	}
	return moveTask(ctx, db, filter, []models.TaskStatus{
		models.TaskStatusProposed, models.TaskStatusQueued, models.TaskStatusOpen, models.TaskStatusClaimed, models.TaskStatusReopened,
	}, bson.M{"status": models.TaskStatusDismissed, "dismissed_impressions": current.Metrics.Impressions})
}

// claimTask gives a task to one worker: of two claims only the first succeeds.
func claimTask(ctx context.Context, db *mongo.Database, filter bson.M, agent string) (*models.SEOTask, error) {
	return moveTask(ctx, db, filter, []models.TaskStatus{models.TaskStatusOpen, models.TaskStatusReopened},
		bson.M{"status": models.TaskStatusClaimed, "claimed_by": agent, "claimed_at": time.Now().UTC()})
}

// completeTask records that the change is live and starts the recheck. The
// clock of the measurement starts here.
func completeTask(ctx context.Context, db *mongo.Database, client *asynq.Client, filter bson.M, summary string) (*models.SEOTask, error) {
	task, err := moveTask(ctx, db, filter, []models.TaskStatus{models.TaskStatusOpen, models.TaskStatusClaimed, models.TaskStatusReopened},
		bson.M{"status": models.TaskStatusDone, "done_at": time.Now().UTC(), "done_summary": summary, "reopen_reason": ""})
	if err != nil {
		return nil, err
	}
	verify, err := worker.NewVerifyTask(task.ID.Hex())
	if err != nil {
		return task, err
	}
	if _, err = client.EnqueueContext(ctx, verify); err != nil {
		return task, err
	}
	return task, nil
}
