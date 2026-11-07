package handlers

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/creever/crawler-api/adapters"
	"github.com/creever/crawler-api/middleware"
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
// @Summary      Push an approved suggestion to the site via its adapter
// @Description  Only approved/edited suggestions can be applied. Captures a 28-day "before" GSC snapshot and logs the change for impact tracking.
// @Tags         projects
// @Produce      json
// @Param        id   path  string  true  "Project ID"
// @Param        sid  path  string  true  "Suggestion ID"
// @Success      200  {object}  models.AppliedChange
// @Router       /api/v1/projects/{id}/suggestions/{sid}/apply [post]
func (h *ApplyHandler) Apply(c *gin.Context) {
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

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
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

	var sug models.Suggestion
	if err = h.db.Collection("suggestions").FindOne(ctx, bson.M{"_id": sid, "project_id": id}).Decode(&sug); err != nil {
		if err == mongo.ErrNoDocuments {
			c.JSON(http.StatusNotFound, gin.H{"error": "suggestion not found"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch suggestion"})
		}
		return
	}
	if sug.Status != models.SuggestionStatusApproved && sug.Status != models.SuggestionStatusEdited {
		c.JSON(http.StatusBadRequest, gin.H{"error": "only approved or edited suggestions can be applied (current status: " + string(sug.Status) + ")"})
		return
	}
	if !models.IsApplyableKind(sug.Kind) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "suggestion kind " + sug.Kind + " is a manual task and cannot be applied through an adapter"})
		return
	}
	if project.AdapterType == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "project has no adapter_type configured"})
		return
	}

	adapter, err := adapters.Get(project.AdapterType)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	now := time.Now().UTC()
	before := h.pageMetrics(ctx, id, sug.Page, now.AddDate(0, 0, -28), now.AddDate(0, 0, -1))

	change := models.AppliedChange{
		ID:             bson.NewObjectID(),
		ProjectID:      id,
		SuggestionID:   &sid,
		Kind:           sug.Kind,
		Page:           sug.Page,
		Value:          sug.FinalValue,
		Adapter:        adapter.Name(),
		AppliedBy:      ptrOID(middleware.GetUserID(c)),
		AppliedAt:      now,
		BeforeSnapshot: before,
	}

	applyErr := adapter.Apply(ctx, project.AdapterEndpoint, project.AdapterAPIKey, adapters.Change{
		Kind:  sug.Kind,
		Page:  sug.Page,
		Value: sug.FinalValue,
	})
	if applyErr != nil {
		change.Status = "failed"
		change.Error = applyErr.Error()
		c.JSON(http.StatusBadGateway, gin.H{"error": applyErr.Error(), "change": change})
		_, _ = h.db.Collection("applied_changes").InsertOne(ctx, change)
		return
	}

	change.Status = "applied"
	if _, err = h.db.Collection("applied_changes").InsertOne(ctx, change); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to log applied change"})
		return
	}
	_, _ = h.db.Collection("suggestions").UpdateOne(ctx, bson.M{"_id": sid}, bson.M{
		"$set": bson.M{"applied_at": now, "updated_at": now},
	})

	c.JSON(http.StatusOK, change)
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
			after = h.pageMetrics(ctx, id, ch.Page, afterStart, afterEnd)
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
func (h *ApplyHandler) pageMetrics(ctx context.Context, projectID bson.ObjectID, page string, start, end time.Time) models.PageMetrics {
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
	cursor, err := h.db.Collection("gsc_rows").Aggregate(ctx, pipeline)
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

func ptrOID(id bson.ObjectID) *bson.ObjectID {
	return &id
}
