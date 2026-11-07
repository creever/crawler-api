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

// maxImpactPages caps the before-snapshots a task records. Each one is an
// aggregate over the page's search rows.
const maxImpactPages = 20

// taskView is a task with the brief a harness works from.
type taskView struct {
	models.SEOTask
	Brief string `json:"brief"`
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

func listTasks(ctx context.Context, db *mongo.Database, projectID bson.ObjectID, status string, limit int64) ([]models.SEOTask, error) {
	query := bson.M{"project_id": projectID}
	if status != "" {
		query["status"] = models.TaskStatus(status)
	}
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
	tasks, err := listTasks(ctx, h.db, id, c.Query("status"), 200)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch tasks"})
		return
	}
	views := make([]taskView, 0, len(tasks))
	for i := range tasks {
		views = append(views, taskView{SEOTask: tasks[i], Brief: tasks[i].Brief()})
	}
	c.JSON(http.StatusOK, views)
}

// Update godoc
// @Summary      Change a task's status
// @Description  action is claim, done, dismiss or reopen. done starts the recheck.
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

	var task *models.SEOTask
	switch input.Action {
	case "claim":
		task, err = claimTask(ctx, h.db, bson.M{"_id": tid, "project_id": id}, "user")
	case "done":
		task, err = completeTask(ctx, h.db, h.asynqClient, bson.M{"_id": tid, "project_id": id}, input.Summary)
	case "dismiss":
		task, err = setTaskStatus(ctx, h.db, bson.M{"_id": tid, "project_id": id}, models.TaskStatusDismissed)
	case "reopen":
		task, err = setTaskStatus(ctx, h.db, bson.M{"_id": tid, "project_id": id}, models.TaskStatusOpen)
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "action must be claim, done, dismiss or reopen"})
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
	c.JSON(http.StatusOK, taskView{SEOTask: *task, Brief: task.Brief()})
}

func updateTask(ctx context.Context, db *mongo.Database, filter, set bson.M) (*models.SEOTask, error) {
	var task models.SEOTask
	err := db.Collection("seo_tasks").FindOneAndUpdate(ctx, filter, bson.M{"$set": set},
		options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&task)
	if err != nil {
		return nil, err
	}
	return &task, nil
}

func setTaskStatus(ctx context.Context, db *mongo.Database, filter bson.M, status models.TaskStatus) (*models.SEOTask, error) {
	return updateTask(ctx, db, filter, bson.M{"status": status, "updated_at": time.Now().UTC()})
}

// claimTask gives a task to one worker: of two claims only the first succeeds.
func claimTask(ctx context.Context, db *mongo.Database, filter bson.M, agent string) (*models.SEOTask, error) {
	now := time.Now().UTC()
	filter["status"] = bson.M{"$in": []models.TaskStatus{models.TaskStatusOpen, models.TaskStatusReopened}}
	return updateTask(ctx, db, filter, bson.M{
		"status": models.TaskStatusClaimed, "claimed_by": agent, "claimed_at": now, "updated_at": now,
	})
}

// completeTask records that the change is live, keeps the before-numbers for the
// Impact page and starts the recheck.
func completeTask(ctx context.Context, db *mongo.Database, client *asynq.Client, filter bson.M, summary string) (*models.SEOTask, error) {
	now := time.Now().UTC()
	filter["status"] = bson.M{"$in": []models.TaskStatus{models.TaskStatusOpen, models.TaskStatusClaimed, models.TaskStatusReopened}}
	task, err := updateTask(ctx, db, filter, bson.M{
		"status": models.TaskStatusDone, "done_at": now, "done_summary": summary, "updated_at": now,
	})
	if err != nil {
		return nil, err
	}
	recordTaskImpact(ctx, db, task, now)
	verify, err := worker.NewVerifyTask(task.ID.Hex())
	if err != nil {
		return task, err
	}
	if _, err = client.EnqueueContext(ctx, verify); err != nil {
		return task, err
	}
	return task, nil
}

// recordTaskImpact logs the change per page for impact tracking. A failure here
// loses a measurement, not the task.
func recordTaskImpact(ctx context.Context, db *mongo.Database, task *models.SEOTask, now time.Time) {
	for i, page := range task.Pages {
		if i == maxImpactPages {
			break
		}
		before := pageMetrics(ctx, db, task.ProjectID, page, now.AddDate(0, 0, -28), now.AddDate(0, 0, -1))
		taskID := task.ID
		_, _ = db.Collection("applied_changes").UpdateOne(ctx,
			bson.M{"task_id": taskID, "page": page},
			bson.M{"$setOnInsert": models.AppliedChange{
				ID: bson.NewObjectID(), ProjectID: task.ProjectID, TaskID: &taskID, Kind: task.Issue, Page: page,
				Value: task.Title, Adapter: "task", Status: "applied", AppliedAt: now, BeforeSnapshot: before,
			}},
			options.UpdateOne().SetUpsert(true))
	}
}

// createTaskFrom stores the task of a suggestion and links it. A suggestion
// already covered by a task keeps that task.
func createTaskFrom(ctx context.Context, db *mongo.Database, s models.Suggestion) (*models.SEOTask, error) {
	var analysis *models.PageAnalysis
	if s.AnalysisID != nil {
		found, err := worker.LoadAnalyses(ctx, db, []bson.ObjectID{*s.AnalysisID})
		if err != nil {
			return nil, err
		}
		if a, ok := found[*s.AnalysisID]; ok {
			analysis = &a
		}
	}
	now := time.Now().UTC()
	draft := worker.TaskFromSuggestion(s, analysis, worker.PageStat(ctx, db, s.ProjectID, s.Page))
	task, err := worker.UpsertTask(ctx, db, draft, time.Time{}, now)
	if err != nil {
		return nil, err
	}
	_, err = db.Collection("suggestions").UpdateOne(ctx, bson.M{"_id": s.ID},
		bson.M{"$set": bson.M{"task_id": task.ID, "updated_at": now}})
	return task, err
}

// CreateFromSuggestion godoc
// @Summary      Create a task from a suggestion marked Apply on site
// @Tags         projects
// @Produce      json
// @Param        id   path  string  true  "Project ID"
// @Param        sid  path  string  true  "Suggestion ID"
// @Success      201  {object}  models.SEOTask
// @Router       /api/v1/projects/{id}/suggestions/{sid}/task [post]
func (h *TaskHandler) CreateFromSuggestion(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	id, ok := projectFromRequest(ctx, c, h.db)
	if !ok {
		return
	}
	sid, err := bson.ObjectIDFromHex(c.Param("sid"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid suggestion id"})
		return
	}
	var s models.Suggestion
	if err = h.db.Collection("suggestions").FindOne(ctx, bson.M{"_id": sid, "project_id": id}).Decode(&s); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "suggestion not found"})
		return
	}
	if s.Status != models.SuggestionStatusApplyOnSite {
		c.JSON(http.StatusBadRequest, gin.H{"error": "only suggestions marked Apply on site can become a task (current status: " + string(s.Status) + ")"})
		return
	}
	task, err := createTaskFrom(ctx, h.db, s)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, taskView{SEOTask: *task, Brief: task.Brief()})
}

// CreateFromMarked godoc
// @Summary      Create tasks from every suggestion marked Apply on site
// @Tags         projects
// @Produce      json
// @Param        id  path  string  true  "Project ID"
// @Success      200  {object}  map[string]int
// @Router       /api/v1/projects/{id}/suggestions/tasks [post]
func (h *TaskHandler) CreateFromMarked(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	id, ok := projectFromRequest(ctx, c, h.db)
	if !ok {
		return
	}
	cursor, err := h.db.Collection("suggestions").Find(ctx, bson.M{
		"project_id": id, "status": models.SuggestionStatusApplyOnSite, "task_id": bson.M{"$exists": false},
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch suggestions"})
		return
	}
	var list []models.Suggestion
	if err = cursor.All(ctx, &list); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to decode suggestions"})
		return
	}
	created := 0
	for _, s := range list {
		if _, err = createTaskFrom(ctx, h.db, s); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error(), "created": created})
			return
		}
		created++
	}
	c.JSON(http.StatusOK, gin.H{"created": created})
}
