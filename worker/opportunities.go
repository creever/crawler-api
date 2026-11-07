package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hibiken/asynq"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.uber.org/zap"

	"github.com/creever/crawler-api/models"
)

const (
	// oppWindowDays is the comparison window: current 28d vs previous 28d.
	oppWindowDays = 28
	// oppMinImpressions filters noise: query×page pairs below this over 28d are ignored (low for pilot scale).
	oppMinImpressions = 25
	// oppCrawlIssueTopPages caps how many traffic pages are checked for crawl issues.
	oppCrawlIssueTopPages = 50
)

// oppQueryPage aggregates one query × page pair over a date window.
type oppQueryPage struct {
	ID struct {
		Query string `bson:"query"`
		Page  string `bson:"page"`
	} `bson:"_id"`
	Clicks      float64 `bson:"clicks"`
	Impressions float64 `bson:"impressions"`
	PosWeighted float64 `bson:"pos_weighted"`
}

// oppPage aggregates one page over a date window.
type oppPage struct {
	ID          string  `bson:"_id"`
	Clicks      float64 `bson:"clicks"`
	Impressions float64 `bson:"impressions"`
}

// oppQuery aggregates one query over a date window.
type oppQuery struct {
	ID          string   `bson:"_id"`
	Impressions float64  `bson:"impressions"`
	Pages       []string `bson:"pages"`
}

// expectedCTR is a rough average CTR curve by average position, used to flag
// queries whose actual CTR is far below what their position should earn.
func expectedCTR(position float64) float64 {
	switch {
	case position <= 3:
		return 0.08
	case position <= 5:
		return 0.05
	case position <= 10:
		return 0.02
	default:
		return 0.01
	}
}

// HandleComputeOpportunities processes an opportunities:compute task. It
// derives the five opportunity signals from gsc_rows (current 28d vs the
// previous 28d) plus the latest crawl data, ranks them per project, and
// replaces the project's stored opportunities.
func (p *Processor) HandleComputeOpportunities(ctx context.Context, t *asynq.Task) error {
	var payload ComputeOppsPayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		return fmt.Errorf("%w: unmarshal opportunities:compute payload: %v", asynq.SkipRetry, err)
	}
	projectOID, err := bson.ObjectIDFromHex(payload.ProjectID)
	if err != nil {
		return fmt.Errorf("%w: invalid project_id: %v", asynq.SkipRetry, err)
	}

	var project models.Project
	if err = p.db.Collection("projects").FindOne(ctx, bson.M{"_id": projectOID}).Decode(&project); err != nil {
		return fmt.Errorf("%w: project not found: %v", asynq.SkipRetry, err)
	}

	// GSC data lags ~3 days; compare the last two complete 28-day windows.
	end := time.Now().UTC().AddDate(0, 0, -gscDataLagDays).Truncate(24 * time.Hour)
	curStart := end.AddDate(0, 0, -(oppWindowDays - 1))
	prevEnd := curStart.AddDate(0, 0, -1)
	prevStart := prevEnd.AddDate(0, 0, -(oppWindowDays - 1))

	p.logger.Info("computing opportunities",
		zap.String("project_id", payload.ProjectID),
		zap.String("current", curStart.Format("2006-01-02")+".."+end.Format("2006-01-02")),
		zap.String("previous", prevStart.Format("2006-01-02")+".."+prevEnd.Format("2006-01-02")),
	)

	opps := make([]models.Opportunity, 0)
	now := time.Now().UTC()

	curQP, err := p.aggQueryPage(ctx, projectOID, curStart, end)
	if err != nil {
		return fmt.Errorf("aggregate current window: %w", err)
	}
	prevPages, err := p.aggPage(ctx, projectOID, prevStart, prevEnd)
	if err != nil {
		return fmt.Errorf("aggregate previous window: %w", err)
	}
	curPages, err := p.aggPage(ctx, projectOID, curStart, end)
	if err != nil {
		return fmt.Errorf("aggregate current pages: %w", err)
	}
	cannib, err := p.aggCannibalization(ctx, projectOID, curStart, end)
	if err != nil {
		return fmt.Errorf("aggregate cannibalization: %w", err)
	}

	for _, r := range curQP {
		if r.Impressions < oppMinImpressions || r.Impressions <= 0 {
			continue
		}
		ctr := r.Clicks / r.Impressions
		pos := r.PosWeighted / r.Impressions
		metrics := map[string]float64{
			"impressions": r.Impressions,
			"clicks":      r.Clicks,
			"ctr":         ctr,
			"position":    pos,
		}
		// Striking distance: position 5–20 with high impressions.
		if pos >= 5 && pos <= 20 {
			score := r.Impressions * (21 - pos)
			opps = append(opps, models.Opportunity{
				ID: bson.NewObjectID(), ProjectID: projectOID,
				Type: models.OppStrikingDistance, Query: r.ID.Query, Page: r.ID.Page,
				Score: score, Metrics: metrics, ComputedAt: now,
			})
		}
		// Low CTR for position: actual CTR below half the expected curve.
		exp := expectedCTR(pos)
		if pos <= 20 && ctr < exp*0.5 {
			score := r.Impressions * (exp - ctr)
			opps = append(opps, models.Opportunity{
				ID: bson.NewObjectID(), ProjectID: projectOID,
				Type: models.OppLowCTR, Query: r.ID.Query, Page: r.ID.Page,
				Score: score, Metrics: metrics, ComputedAt: now,
				Details: fmt.Sprintf("CTR %.2f%% vs expected ~%.1f%% at position %.1f", ctr*100, exp*100, pos),
			})
		}
	}

	// Losing clicks: pages whose current-28d clicks dropped ≥20% vs previous.
	prevByPage := make(map[string]float64, len(prevPages))
	for _, r := range prevPages {
		prevByPage[r.ID] = r.Clicks
	}
	for _, r := range curPages {
		prev := prevByPage[r.ID]
		if prev < 3 || r.Clicks >= prev*0.8 {
			continue
		}
		opps = append(opps, models.Opportunity{
			ID: bson.NewObjectID(), ProjectID: projectOID,
			Type: models.OppLosingClicks, Page: r.ID,
			Score: prev - r.Clicks,
			Metrics: map[string]float64{
				"clicks":      r.Clicks,
				"prev_clicks": prev,
				"impressions": r.Impressions,
			},
			ComputedAt: now,
			Details:    fmt.Sprintf("clicks %.0f → %.0f (-%.0f%%) 28d vs previous 28d", prev, r.Clicks, (1-r.Clicks/prev)*100),
		})
	}

	// Cannibalization: 2+ distinct pages with impressions for the same query.
	for _, r := range cannib {
		if len(r.Pages) < 2 {
			continue
		}
		opps = append(opps, models.Opportunity{
			ID: bson.NewObjectID(), ProjectID: projectOID,
			Type: models.OppCannibalization, Query: r.ID,
			Page: r.Pages[0], Pages: r.Pages,
			Score: r.Impressions,
			Metrics: map[string]float64{
				"impressions": r.Impressions,
				"pages":       float64(len(r.Pages)),
			},
			ComputedAt: now,
			Details:    "pages: " + strings.Join(r.Pages, ", "),
		})
	}

	// Crawl issues on pages that actually get traffic.
	crawlOpps, err := p.crawlIssueOpportunities(ctx, projectOID, curPages, now)
	if err != nil {
		p.logger.Warn("crawl-issue scan failed, continuing without it",
			zap.String("project_id", payload.ProjectID), zap.Error(err))
	} else {
		opps = append(opps, crawlOpps...)
	}

	// Rank by score descending.
	sort.Slice(opps, func(i, j int) bool { return opps[i].Score > opps[j].Score })
	for i := range opps {
		opps[i].Rank = i + 1
	}

	col := p.db.Collection("opportunities")
	if _, err = col.DeleteMany(ctx, bson.M{"project_id": projectOID}); err != nil {
		return fmt.Errorf("clear old opportunities: %w", err)
	}
	if len(opps) > 0 {
		docs := make([]interface{}, 0, len(opps))
		for _, o := range opps {
			docs = append(docs, o)
		}
		if _, err = col.InsertMany(ctx, docs); err != nil {
			return fmt.Errorf("insert opportunities: %w", err)
		}
	}

	p.logger.Info("opportunities computed",
		zap.String("project_id", payload.ProjectID),
		zap.Int("count", len(opps)),
	)
	return nil
}

// aggQueryPage groups gsc_rows by query × page over [start, end].
func (p *Processor) aggQueryPage(ctx context.Context, projectOID bson.ObjectID, start, end time.Time) ([]oppQueryPage, error) {
	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: bson.D{
			{Key: "project_id", Value: projectOID},
			{Key: "date", Value: bson.D{
				{Key: "$gte", Value: start.Format("2006-01-02")},
				{Key: "$lte", Value: end.Format("2006-01-02")},
			}},
		}}},
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: bson.D{{Key: "query", Value: "$query"}, {Key: "page", Value: "$page"}}},
			{Key: "clicks", Value: bson.D{{Key: "$sum", Value: "$clicks"}}},
			{Key: "impressions", Value: bson.D{{Key: "$sum", Value: "$impressions"}}},
			{Key: "pos_weighted", Value: bson.D{{Key: "$sum", Value: bson.D{{Key: "$multiply", Value: bson.A{"$position", "$impressions"}}}}}},
		}}},
	}
	cursor, err := p.db.Collection("gsc_rows").Aggregate(ctx, pipeline)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx) //nolint:errcheck
	var rows []oppQueryPage
	if err = cursor.All(ctx, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// aggPage groups gsc_rows by page over [start, end].
func (p *Processor) aggPage(ctx context.Context, projectOID bson.ObjectID, start, end time.Time) ([]oppPage, error) {
	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: bson.D{
			{Key: "project_id", Value: projectOID},
			{Key: "date", Value: bson.D{
				{Key: "$gte", Value: start.Format("2006-01-02")},
				{Key: "$lte", Value: end.Format("2006-01-02")},
			}},
		}}},
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$page"},
			{Key: "clicks", Value: bson.D{{Key: "$sum", Value: "$clicks"}}},
			{Key: "impressions", Value: bson.D{{Key: "$sum", Value: "$impressions"}}},
		}}},
	}
	cursor, err := p.db.Collection("gsc_rows").Aggregate(ctx, pipeline)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx) //nolint:errcheck
	var rows []oppPage
	if err = cursor.All(ctx, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// aggCannibalization groups gsc_rows by query over [start, end] and collects
// the distinct pages that received impressions, strongest page first.
func (p *Processor) aggCannibalization(ctx context.Context, projectOID bson.ObjectID, start, end time.Time) ([]oppQuery, error) {
	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: bson.D{
			{Key: "project_id", Value: projectOID},
			{Key: "date", Value: bson.D{
				{Key: "$gte", Value: start.Format("2006-01-02")},
				{Key: "$lte", Value: end.Format("2006-01-02")},
			}},
			{Key: "impressions", Value: bson.D{{Key: "$gt", Value: 0}}},
		}}},
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: bson.D{{Key: "query", Value: "$query"}, {Key: "page", Value: "$page"}}},
			{Key: "clicks", Value: bson.D{{Key: "$sum", Value: "$clicks"}}},
			{Key: "impressions", Value: bson.D{{Key: "$sum", Value: "$impressions"}}},
		}}},
		{{Key: "$sort", Value: bson.D{{Key: "clicks", Value: -1}, {Key: "impressions", Value: -1}}}},
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$_id.query"},
			{Key: "impressions", Value: bson.D{{Key: "$sum", Value: "$impressions"}}},
			{Key: "pages", Value: bson.D{{Key: "$push", Value: "$_id.page"}}},
		}}},
		{{Key: "$match", Value: bson.D{
			{Key: "impressions", Value: bson.D{{Key: "$gte", Value: oppMinImpressions * 2}}},
		}}},
	}
	cursor, err := p.db.Collection("gsc_rows").Aggregate(ctx, pipeline)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx) //nolint:errcheck
	var rows []oppQuery
	if err = cursor.All(ctx, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// crawlIssueOpportunities checks the top-traffic pages' latest crawl data for
// common SEO issues (missing title/meta, noindex, non-200 status).
func (p *Processor) crawlIssueOpportunities(ctx context.Context, projectOID bson.ObjectID, curPages []oppPage, now time.Time) ([]models.Opportunity, error) {
	sorted := make([]oppPage, len(curPages))
	copy(sorted, curPages)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Clicks > sorted[j].Clicks })
	if len(sorted) > oppCrawlIssueTopPages {
		sorted = sorted[:oppCrawlIssueTopPages]
	}

	var opps []models.Opportunity
	for _, pg := range sorted {
		if pg.Clicks < 3 {
			continue
		}
		var seo models.SEOData
		seoErr := p.db.Collection("seo_data").FindOne(ctx, bson.M{
			"project_id": projectOID, "url": pg.ID,
		}).Decode(&seo)

		var issues []string
		if seoErr == nil {
			if strings.TrimSpace(seo.Title) == "" {
				issues = append(issues, "missing title")
			}
			if strings.TrimSpace(seo.MetaDescription) == "" {
				issues = append(issues, "missing meta description")
			}
			if strings.Contains(strings.ToLower(seo.MetaRobots), "noindex") {
				issues = append(issues, "noindex")
			}
			if seo.StatusCode >= 400 {
				issues = append(issues, fmt.Sprintf("HTTP %d", seo.StatusCode))
			}
		}

		if len(issues) == 0 {
			continue
		}
		opps = append(opps, models.Opportunity{
			ID: bson.NewObjectID(), ProjectID: projectOID,
			Type: models.OppCrawlIssue, Page: pg.ID,
			Score: pg.Clicks, // weight by current traffic
			Metrics: map[string]float64{
				"clicks":      pg.Clicks,
				"impressions": pg.Impressions,
			},
			Details:    strings.Join(issues, ", "),
			ComputedAt: now,
		})
	}
	return opps, nil
}
