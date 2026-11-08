package handlers

import (
	"context"
	"encoding/json"
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
	inspector   *asynq.Inspector
}

// NewOpportunityHandler creates a new OpportunityHandler.
func NewOpportunityHandler(db *mongo.Database, asynqClient *asynq.Client, inspector *asynq.Inspector) *OpportunityHandler {
	return &OpportunityHandler{db: db, asynqClient: asynqClient, inspector: inspector}
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
	views, err := withPageStates(ctx, h.db, id, opps)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch page states"})
		return
	}
	c.JSON(http.StatusOK, views)
}

// oppPageState is what the rest of the system knows about an opportunity's
// page: whether the site crawl read it, its audit issues, and the work on it.
type oppPageState struct {
	Read               bool               `json:"read"`
	Issues             []models.PageIssue `json:"issues"`
	PendingSuggestions int                `json:"pending_suggestions"`
	Tasks              []taskRef          `json:"tasks"`
}

// opportunityView is an opportunity with its page state.
type opportunityView struct {
	models.Opportunity
	PageState *oppPageState `json:"page_state,omitempty"`
}

// finishedTaskStatuses are tasks no longer worked on.
var finishedTaskStatuses = []models.TaskStatus{models.TaskStatusClosed, models.TaskStatusDismissed, models.TaskStatusResolved}

// withPageStates adds the page state to each opportunity with a page, in a
// fixed number of queries for the whole list.
func withPageStates(ctx context.Context, db *mongo.Database, projectID bson.ObjectID, opps []models.Opportunity) ([]opportunityView, error) {
	views := make([]opportunityView, 0, len(opps))
	states := make(map[string]*oppPageState)
	var spellings, normalized bson.A
	for _, o := range opps {
		views = append(views, opportunityView{Opportunity: o})
		if o.Page == "" {
			continue
		}
		key := worker.NormalizePageURL(o.Page)
		if _, ok := states[key]; ok {
			continue
		}
		states[key] = &oppPageState{Issues: []models.PageIssue{}, Tasks: []taskRef{}}
		spellings = append(spellings, o.Page, key, key+"/")
		normalized = append(normalized, key)
	}
	if len(states) == 0 {
		return views, nil
	}

	var profiles []models.PageProfile
	pc, err := db.Collection("page_profiles").Find(ctx, bson.M{"project_id": projectID, "url": bson.M{"$in": normalized}, "version": bson.M{"$gte": 1}},
		options.Find().SetProjection(bson.M{"url": 1, "issues": 1}))
	if err != nil {
		return nil, err
	}
	if err = pc.All(ctx, &profiles); err != nil {
		return nil, err
	}
	for _, pr := range profiles {
		if st := states[pr.URL]; st != nil {
			st.Read = true
			if pr.Issues != nil {
				st.Issues = pr.Issues
			}
		}
	}

	var pending []models.Suggestion
	sc, err := db.Collection("suggestions").Find(ctx, bson.M{"project_id": projectID, "page": bson.M{"$in": spellings}, "status": models.SuggestionStatusPending},
		options.Find().SetProjection(bson.M{"page": 1}))
	if err != nil {
		return nil, err
	}
	if err = sc.All(ctx, &pending); err != nil {
		return nil, err
	}
	for _, s := range pending {
		if st := states[worker.NormalizePageURL(s.Page)]; st != nil {
			st.PendingSuggestions++
		}
	}

	var tasks []models.SEOTask
	tc, err := db.Collection("seo_tasks").Find(ctx, bson.M{"project_id": projectID, "pages": bson.M{"$in": spellings}, "status": bson.M{"$nin": finishedTaskStatuses}},
		options.Find().SetProjection(bson.M{"title": 1, "status": 1, "scope": 1, "metrics": 1, "pages": 1}))
	if err != nil {
		return nil, err
	}
	if err = tc.All(ctx, &tasks); err != nil {
		return nil, err
	}
	for _, t := range tasks {
		ref := taskRef{ID: t.ID, Title: t.Title, Status: t.Status, Scope: t.Scope, Pages: t.Metrics.Pages}
		added := make(map[string]bool)
		for _, pg := range t.Pages {
			key := worker.NormalizePageURL(pg)
			if st := states[key]; st != nil && !added[key] {
				added[key] = true
				st.Tasks = append(st.Tasks, ref)
			}
		}
	}

	for i := range views {
		if views[i].Page != "" {
			views[i].PageState = states[worker.NormalizePageURL(views[i].Page)]
		}
	}
	return views, nil
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

// SuggestForOpportunity godoc
// @Summary      Enqueue AI suggestion generation for a single opportunity
// @Tags         projects
// @Produce      json
// @Param        id   path  string  true  "Project ID"
// @Param        oid  path  string  true  "Opportunity ID"
// @Success      202  {object}  map[string]string
// @Router       /api/v1/projects/{id}/opportunities/{oid}/suggest [post]
func (h *OpportunityHandler) SuggestForOpportunity(c *gin.Context) {
	id, err := bson.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project id"})
		return
	}
	oid, err := bson.ObjectIDFromHex(c.Param("oid"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid opportunity id"})
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

	task, err := worker.NewGenSuggestTask(worker.GenSuggestPayload{
		ProjectID:     id.Hex(),
		OpportunityID: oid.Hex(),
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to build task"})
		return
	}
	info, err := h.asynqClient.EnqueueContext(ctx, task)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to enqueue task"})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"status": "enqueued", "task_id": info.ID, "opportunity_id": oid.Hex()})
}

// Status godoc
// @Summary      Opportunity compute job status for a project
// @Description  Reports whether an opportunities:compute task is pending or active in the asynq queue, plus when the stored list was last computed and how many rows it has.
// @Tags         projects
// @Produce      json
// @Param        id  path  string  true  "Project ID"
// @Success      200  {object}  map[string]interface{}
// @Router       /api/v1/projects/{id}/opportunities/status [get]
func (h *OpportunityHandler) Status(c *gin.Context) {
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

	resp := gin.H{"project_id": id.Hex(), "pending": false, "active": false, "count": 0, "last_computed_at": nil}

	if h.inspector != nil {
		pending, err := h.inspector.ListPendingTasks("default")
		if err == nil {
			for _, t := range pending {
				if t.Type == "opportunities:compute" && payloadHasProject(t.Payload, id.Hex()) {
					resp["pending"] = true
				}
			}
		}
		active, err := h.inspector.ListActiveTasks("default")
		if err == nil {
			for _, t := range active {
				if t.Type == "opportunities:compute" && payloadHasProject(t.Payload, id.Hex()) {
					resp["active"] = true
				}
			}
		}
	}

	count, _ := h.db.Collection("opportunities").CountDocuments(ctx, bson.M{"project_id": id})
	resp["count"] = count

	var last struct {
		ComputedAt time.Time `bson:"computed_at"`
	}
	opts := options.FindOne().SetSort(bson.D{{Key: "computed_at", Value: -1}}).SetProjection(bson.M{"computed_at": 1})
	if err := h.db.Collection("opportunities").FindOne(ctx, bson.M{"project_id": id}, opts).Decode(&last); err == nil {
		resp["last_computed_at"] = last.ComputedAt
	}

	c.JSON(http.StatusOK, resp)
}

// payloadHasProject checks whether a JSON task payload references the project.
func payloadHasProject(payload []byte, projectID string) bool {
	var p struct {
		ProjectID string `json:"project_id"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return false
	}
	return p.ProjectID == projectID
}
