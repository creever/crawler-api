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
	"github.com/creever/crawler-api/worker"
)

const (
	dashboardSearchDays   = 28
	dashboardPrerenderDay = 14
	dashboardTopOpps      = 5
	dashboardHistory      = 12
)

// DashboardHandler serves the project dashboard: one call that brings the
// site crawl, search, the work list and prerender together.
type DashboardHandler struct {
	db *mongo.Database
}

// NewDashboardHandler creates a DashboardHandler.
func NewDashboardHandler(db *mongo.Database) *DashboardHandler {
	return &DashboardHandler{db: db}
}

// searchDay is one day of Search Console totals.
type searchDay struct {
	Date        string  `bson:"_id"         json:"date"`
	Clicks      float64 `bson:"clicks"      json:"clicks"`
	Impressions float64 `bson:"impressions" json:"impressions"`
}

// searchSummary compares the last 28 days of search with the 28 before.
type searchSummary struct {
	Clicks          float64     `json:"clicks"`
	Impressions     float64     `json:"impressions"`
	PrevClicks      float64     `json:"prev_clicks"`
	PrevImpressions float64     `json:"prev_impressions"`
	LastDate        string      `json:"last_date"`
	Daily           []searchDay `json:"daily"`
}

// auditPoint is one site crawl in the trend.
type auditPoint struct {
	Date     time.Time `json:"date"`
	Score    float64   `json:"score"`
	Critical int       `json:"critical"`
	Warning  int       `json:"warning"`
	Pages    int       `json:"pages"`
}

// workSummary is the state of the work list.
type workSummary struct {
	Tasks              map[string]int64     `json:"tasks"`
	Verdicts           map[string]int64     `json:"verdicts"`
	PendingSuggestions int64                `json:"pending_suggestions"`
	Opportunities      int64                `json:"opportunities"`
	TopOpportunities   []models.Opportunity `json:"top_opportunities"`
}

type dashboardResponse struct {
	Site        *models.SiteAudit `json:"site"`
	SiteHistory []auditPoint      `json:"site_history"`
	Search      searchSummary     `json:"search"`
	Work        workSummary       `json:"work"`
	Prerender   *prerenderStats   `json:"prerender"`
	GeneratedAt time.Time         `json:"generated_at"`
}

// Get godoc
// @Summary      Project dashboard
// @Description  Site health and its trend, search clicks against the previous 28 days, the work list and the prerender picture in one call.
// @Tags         projects
// @Produce      json
// @Param        id  path  string  true  "Project ID"
// @Router       /api/v1/projects/{id}/dashboard [get]
func (h *DashboardHandler) Get(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	id, ok := projectFromRequest(ctx, c, h.db)
	if !ok {
		return
	}
	now := time.Now().UTC()
	resp := dashboardResponse{GeneratedAt: now, SiteHistory: []auditPoint{}}
	var err error
	if resp.Site, err = worker.LatestAudit(ctx, h.db, id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch the site crawl"})
		return
	}
	if resp.Site != nil {
		resp.Site.Groups = nil // the dashboard shows totals; Site Health has the groups
	}
	if resp.SiteHistory, err = h.siteHistory(ctx, id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch the crawl history"})
		return
	}
	if resp.Search, err = h.search(ctx, id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to sum search data"})
		return
	}
	if resp.Work, err = h.work(ctx, id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to sum the work list"})
		return
	}
	if resp.Prerender, err = buildPrerenderStats(ctx, h.db, id, dashboardPrerenderDay, now); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to build prerender stats"})
		return
	}
	c.JSON(http.StatusOK, resp)
}

func (h *DashboardHandler) siteHistory(ctx context.Context, id bson.ObjectID) ([]auditPoint, error) {
	cursor, err := h.db.Collection("site_audits").Find(ctx, bson.M{"project_id": id, "status": "done", "pages_fetched": bson.M{"$gt": 0}},
		options.Find().SetSort(bson.D{{Key: "started_at", Value: -1}}).SetLimit(dashboardHistory).
			SetProjection(bson.M{"started_at": 1, "score": 1, "issues_by_severity": 1, "pages_fetched": 1}))
	if err != nil {
		return nil, err
	}
	var audits []models.SiteAudit
	if err = cursor.All(ctx, &audits); err != nil {
		return nil, err
	}
	points := make([]auditPoint, len(audits))
	for i, a := range audits {
		points[len(audits)-1-i] = auditPoint{
			Date: a.StartedAt, Score: a.Score, Pages: a.PagesFetched,
			Critical: a.IssuesBySeverity[models.SeverityCritical], Warning: a.IssuesBySeverity[models.SeverityWarning],
		}
	}
	return points, nil
}

// search sums Search Console by day, ending on the last day with data.
func (h *DashboardHandler) search(ctx context.Context, id bson.ObjectID) (searchSummary, error) {
	out := searchSummary{Daily: []searchDay{}}
	var last struct {
		Date string `bson:"date"`
	}
	err := h.db.Collection("gsc_rows").FindOne(ctx, bson.M{"project_id": id},
		options.FindOne().SetSort(bson.D{{Key: "date", Value: -1}}).SetProjection(bson.M{"date": 1})).Decode(&last)
	if err == mongo.ErrNoDocuments {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	end, err := time.Parse("2006-01-02", last.Date)
	if err != nil {
		return out, nil
	}
	out.LastDate = last.Date
	curStart := end.AddDate(0, 0, -(dashboardSearchDays - 1))
	prevStart := curStart.AddDate(0, 0, -dashboardSearchDays)
	cursor, err := h.db.Collection("gsc_rows").Aggregate(ctx, mongo.Pipeline{
		{{Key: "$match", Value: bson.M{"project_id": id, "date": bson.M{"$gte": prevStart.Format("2006-01-02"), "$lte": last.Date}}}},
		{{Key: "$group", Value: bson.M{"_id": "$date", "clicks": bson.M{"$sum": "$clicks"}, "impressions": bson.M{"$sum": "$impressions"}}}},
		{{Key: "$sort", Value: bson.M{"_id": 1}}},
	})
	if err != nil {
		return out, err
	}
	var days []searchDay
	if err = cursor.All(ctx, &days); err != nil {
		return out, err
	}
	cur := curStart.Format("2006-01-02")
	for _, d := range days {
		if d.Date >= cur {
			out.Clicks += d.Clicks
			out.Impressions += d.Impressions
			out.Daily = append(out.Daily, d)
		} else {
			out.PrevClicks += d.Clicks
			out.PrevImpressions += d.Impressions
		}
	}
	return out, nil
}

func (h *DashboardHandler) work(ctx context.Context, id bson.ObjectID) (workSummary, error) {
	out := workSummary{Tasks: map[string]int64{}, Verdicts: map[string]int64{}, TopOpportunities: []models.Opportunity{}}
	var groups []struct {
		ID struct {
			Status  string `bson:"status"`
			Verdict string `bson:"verdict"`
		} `bson:"_id"`
		N int64 `bson:"n"`
	}
	cursor, err := h.db.Collection("seo_tasks").Aggregate(ctx, mongo.Pipeline{
		{{Key: "$match", Value: bson.M{"project_id": id}}},
		{{Key: "$group", Value: bson.M{"_id": bson.M{"status": "$status", "verdict": "$measurement.verdict"}, "n": bson.M{"$sum": 1}}}},
	})
	if err != nil {
		return out, err
	}
	if err = cursor.All(ctx, &groups); err != nil {
		return out, err
	}
	for _, g := range groups {
		out.Tasks[g.ID.Status] += g.N
		if g.ID.Verdict != "" {
			out.Verdicts[g.ID.Verdict] += g.N
		}
	}
	if out.PendingSuggestions, err = h.db.Collection("suggestions").CountDocuments(ctx,
		bson.M{"project_id": id, "status": models.SuggestionStatusPending}); err != nil {
		return out, err
	}
	if out.Opportunities, err = h.db.Collection("opportunities").CountDocuments(ctx, bson.M{"project_id": id}); err != nil {
		return out, err
	}
	oc, err := h.db.Collection("opportunities").Find(ctx, bson.M{"project_id": id},
		options.Find().SetSort(bson.D{{Key: "rank", Value: 1}}).SetLimit(dashboardTopOpps))
	if err != nil {
		return out, err
	}
	if err = oc.All(ctx, &out.TopOpportunities); err != nil {
		return out, err
	}
	return out, nil
}
