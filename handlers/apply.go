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

// ApplyHandler holds dependencies for applying suggestions and impact reads.
type ApplyHandler struct {
	db *mongo.Database
}

// NewApplyHandler creates a new ApplyHandler.
func NewApplyHandler(db *mongo.Database) *ApplyHandler {
	return &ApplyHandler{db: db}
}

// Apply godoc
// @Summary      Mark a decided suggestion as Apply on site
// @Description  Only sets the status: nothing reaches the site from here. A task can be created from the suggestion later.
// @Tags         projects
// @Produce      json
// @Param        id   path  string  true  "Project ID"
// @Param        sid  path  string  true  "Suggestion ID"
// @Success      200  {object}  models.Suggestion
// @Router       /api/v1/projects/{id}/suggestions/{sid}/apply [post]
func (h *ApplyHandler) Apply(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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

	var sug models.Suggestion
	err = h.db.Collection("suggestions").FindOneAndUpdate(ctx,
		bson.M{"_id": sid, "project_id": id, "status": bson.M{"$in": []models.SuggestionStatus{models.SuggestionStatusApproved, models.SuggestionStatusEdited}}},
		bson.M{"$set": bson.M{"status": models.SuggestionStatusApplyOnSite, "updated_at": time.Now().UTC()}},
		options.FindOneAndUpdate().SetReturnDocument(options.After),
	).Decode(&sug)
	if err == mongo.ErrNoDocuments {
		c.JSON(http.StatusBadRequest, gin.H{"error": "only approved or edited suggestions can be marked Apply on site"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update suggestion"})
		return
	}
	c.JSON(http.StatusOK, sug)
}

// Impact godoc
// @Summary      List applied changes with 28-day before/after GSC metrics
// @Tags         projects
// @Produce      json
// @Param        id  path  string  true  "Project ID"
// @Success      200  {array}  map[string]interface{}
// @Router       /api/v1/projects/{id}/impact [get]
func (h *ApplyHandler) Impact(c *gin.Context) {
	id, err := bson.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project id"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err = h.db.Collection("projects").FindOne(ctx, gscProjectFilter(c, id)).Decode(&struct{}{}); err != nil {
		if err == mongo.ErrNoDocuments {
			c.JSON(http.StatusNotFound, gin.H{"error": "project not found"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch project"})
		}
		return
	}

	cursor, err := h.db.Collection("applied_changes").Find(
		ctx,
		bson.M{"project_id": id, "status": "applied"},
		options.Find().SetSort(bson.D{{Key: "applied_at", Value: -1}}).SetLimit(200),
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch applied changes"})
		return
	}
	defer cursor.Close(ctx) //nolint:errcheck

	var changes []models.AppliedChange
	if err = cursor.All(ctx, &changes); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to decode changes"})
		return
	}

	// GSC data lags ~3 days.
	dataEnd := time.Now().UTC().AddDate(0, 0, -3).Truncate(24 * time.Hour)

	result := make([]gin.H, 0, len(changes))
	for _, ch := range changes {
		afterStart := ch.AppliedAt.Truncate(24 * time.Hour)
		afterEnd := afterStart.AddDate(0, 0, 28)
		if afterEnd.After(dataEnd) {
			afterEnd = dataEnd
		}
		var after models.PageMetrics
		if afterEnd.After(afterStart) || afterEnd.Equal(afterStart) {
			after = pageMetrics(ctx, h.db, id, ch.Page, afterStart, afterEnd)
		}
		result = append(result, gin.H{
			"change":     ch,
			"before":     ch.BeforeSnapshot,
			"after":      after,
			"after_days": int(afterEnd.Sub(afterStart).Hours()/24) + 1,
		})
	}
	c.JSON(http.StatusOK, result)
}

// pageMetrics aggregates gsc_rows for one page over [start, end] (inclusive,
// YYYY-MM-DD strings compared lexicographically).
func pageMetrics(ctx context.Context, db *mongo.Database, projectID bson.ObjectID, page string, start, end time.Time) models.PageMetrics {
	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: bson.D{
			{Key: "project_id", Value: projectID},
			{Key: "page", Value: page},
			{Key: "date", Value: bson.D{
				{Key: "$gte", Value: start.Format("2006-01-02")},
				{Key: "$lte", Value: end.Format("2006-01-02")},
			}},
		}}},
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: nil},
			{Key: "clicks", Value: bson.D{{Key: "$sum", Value: "$clicks"}}},
			{Key: "impressions", Value: bson.D{{Key: "$sum", Value: "$impressions"}}},
			{Key: "pos_weighted", Value: bson.D{{Key: "$sum", Value: bson.D{{Key: "$multiply", Value: bson.A{"$position", "$impressions"}}}}}},
		}}},
	}
	cursor, err := db.Collection("gsc_rows").Aggregate(ctx, pipeline)
	if err != nil {
		return models.PageMetrics{}
	}
	defer cursor.Close(ctx) //nolint:errcheck

	var rows []struct {
		Clicks      float64 `bson:"clicks"`
		Impressions float64 `bson:"impressions"`
		PosWeighted float64 `bson:"pos_weighted"`
	}
	if err = cursor.All(ctx, &rows); err != nil || len(rows) == 0 || rows[0].Impressions == 0 {
		return models.PageMetrics{}
	}
	r := rows[0]
	return models.PageMetrics{
		Clicks:      r.Clicks,
		Impressions: r.Impressions,
		CTR:         r.Clicks / r.Impressions,
		Position:    r.PosWeighted / r.Impressions,
	}
}
