package handlers

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/creever/crawler-api/models"
)

const analysisListLimit = 50

// AnalysisHandler serves page analyses.
type AnalysisHandler struct {
	db *mongo.Database
}

// NewAnalysisHandler creates a new AnalysisHandler.
func NewAnalysisHandler(db *mongo.Database) *AnalysisHandler {
	return &AnalysisHandler{db: db}
}

// analysisView is an analysis with the suggestions it produced.
type analysisView struct {
	models.PageAnalysis `bson:",inline"`
	Suggestions         []models.Suggestion `json:"suggestions"`
}

// projectExists writes the error response and returns false when the caller
// cannot access the project.
func (h *AnalysisHandler) projectExists(ctx context.Context, c *gin.Context, id bson.ObjectID) bool {
	err := h.db.Collection("projects").FindOne(ctx, gscProjectFilter(c, id)).Decode(&struct{}{})
	if err == nil {
		return true
	}
	if err == mongo.ErrNoDocuments {
		c.JSON(http.StatusNotFound, gin.H{"error": "project not found"})
	} else {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch project"})
	}
	return false
}

func (h *AnalysisHandler) withSuggestions(ctx context.Context, projectID bson.ObjectID, analyses []models.PageAnalysis) ([]analysisView, error) {
	ids := make([]bson.ObjectID, 0, len(analyses))
	views := make([]analysisView, 0, len(analyses))
	index := make(map[bson.ObjectID]int, len(analyses))
	for i, a := range analyses {
		ids = append(ids, a.ID)
		index[a.ID] = i
		views = append(views, analysisView{PageAnalysis: a, Suggestions: []models.Suggestion{}})
	}
	if len(ids) == 0 {
		return views, nil
	}
	cursor, err := h.db.Collection("suggestions").Find(ctx,
		bson.M{"project_id": projectID, "analysis_id": bson.M{"$in": ids}},
		options.Find().SetSort(bson.D{{Key: "priority", Value: 1}}),
	)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx) //nolint:errcheck
	var suggestions []models.Suggestion
	if err = cursor.All(ctx, &suggestions); err != nil {
		return nil, err
	}
	for _, s := range suggestions {
		if s.AnalysisID == nil {
			continue
		}
		if i, ok := index[*s.AnalysisID]; ok {
			views[i].Suggestions = append(views[i].Suggestions, s)
		}
	}
	return views, nil
}

// List godoc
// @Summary      List recent page analyses with their suggestions
// @Tags         projects
// @Produce      json
// @Param        id  path  string  true  "Project ID"
// @Success      200  {array}  analysisView
// @Router       /api/v1/projects/{id}/analyses [get]
func (h *AnalysisHandler) List(c *gin.Context) {
	id, err := bson.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project id"})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if !h.projectExists(ctx, c, id) {
		return
	}

	// Prompts are large; the detail endpoint returns them.
	cursor, err := h.db.Collection("page_analyses").Find(ctx, bson.M{"project_id": id},
		options.Find().
			SetSort(bson.D{{Key: "created_at", Value: -1}}).
			SetLimit(analysisListLimit).
			SetProjection(bson.M{"prompt": 0, "raw_response": 0}),
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch analyses"})
		return
	}
	defer cursor.Close(ctx) //nolint:errcheck
	var analyses []models.PageAnalysis
	if err = cursor.All(ctx, &analyses); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to decode analyses"})
		return
	}
	views, err := h.withSuggestions(ctx, id, analyses)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch suggestions"})
		return
	}
	c.JSON(http.StatusOK, views)
}

// Get godoc
// @Summary      Get one page analysis with its prompt and suggestions
// @Tags         projects
// @Produce      json
// @Param        id   path  string  true  "Project ID"
// @Param        aid  path  string  true  "Analysis ID"
// @Success      200  {object}  analysisView
// @Router       /api/v1/projects/{id}/analyses/{aid} [get]
func (h *AnalysisHandler) Get(c *gin.Context) {
	id, err := bson.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project id"})
		return
	}
	aid, err := bson.ObjectIDFromHex(c.Param("aid"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid analysis id"})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if !h.projectExists(ctx, c, id) {
		return
	}

	var a models.PageAnalysis
	if err = h.db.Collection("page_analyses").FindOne(ctx, bson.M{"_id": aid, "project_id": id}).Decode(&a); err != nil {
		if err == mongo.ErrNoDocuments {
			c.JSON(http.StatusNotFound, gin.H{"error": "analysis not found"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch analysis"})
		}
		return
	}
	views, err := h.withSuggestions(ctx, id, []models.PageAnalysis{a})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch suggestions"})
		return
	}
	c.JSON(http.StatusOK, views[0])
}
