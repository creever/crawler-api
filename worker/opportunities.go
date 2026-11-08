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
	"go.mongodb.org/mongo-driver/v2/mongo/options"
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

// Every opportunity is scored in the same unit, the estimated extra clicks
// per 28 days, so that one ranked list can mix the signal types.
const (
	// strikingTargetPosition is where a striking-distance page could get to.
	strikingTargetPosition = 3
	// cannibalizationCTRGain is the CTR a query is assumed to gain once one
	// page answers it instead of several.
	cannibalizationCTRGain = 0.01
	// warningIssueShare is how much of a page's clicks a warning-level audit
	// issue is assumed to put at risk; a critical one puts all of them at risk.
	warningIssueShare = 0.25
)

// strikingGain estimates the clicks a page at position pos would gain at
// strikingTargetPosition. The further away it ranks, the less likely it gets
// there, so the gain shrinks linearly from position 5 to 20.
func strikingGain(impressions, pos float64) float64 {
	gain := expectedCTR(strikingTargetPosition) - expectedCTR(pos)
	if gain <= 0 {
		return 0
	}
	reach := (21 - pos) / 16
	if reach > 1 {
		reach = 1
	}
	return impressions * gain * reach
}

// lowCTRGain is the clicks a query would gain at the CTR its position earns.
func lowCTRGain(impressions, ctr, pos float64) float64 {
	return impressions * (expectedCTR(pos) - ctr)
}

// cannibalizationGain estimates what a query gains when one page answers it.
func cannibalizationGain(impressions float64) float64 {
	return impressions * cannibalizationCTRGain
}

// crawlIssueGain is the clicks at risk from the worst audit severity.
func crawlIssueGain(clicks float64, severity string) float64 {
	if severity == models.SeverityCritical {
		return clicks
	}
	return clicks * warningIssueShare
}

// opportunityKey identifies an opportunity across daily recomputations, so
// that suggestions and the page keep pointing at the same row.
func opportunityKey(o models.Opportunity) string {
	return string(o.Type) + "|" + o.Query + "|" + o.Page
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
			score := strikingGain(r.Impressions, pos)
			opps = append(opps, models.Opportunity{
				ID: bson.NewObjectID(), ProjectID: projectOID,
				Type: models.OppStrikingDistance, Query: r.ID.Query, Page: r.ID.Page,
				Score: score, Metrics: metrics, ComputedAt: now,
			})
		}
		// Low CTR for position: actual CTR below half the expected curve.
		exp := expectedCTR(pos)
		if pos <= 20 && ctr < exp*0.5 {
			score := lowCTRGain(r.Impressions, ctr, pos)
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
			Score: cannibalizationGain(r.Impressions),
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

	if err = p.storeOpportunities(ctx, projectOID, opps, now); err != nil {
		return err
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

// storeOpportunities writes this run's opportunities by key: a row that
// returns keeps its id, and rows no longer found are removed.
func (p *Processor) storeOpportunities(ctx context.Context, projectOID bson.ObjectID, opps []models.Opportunity, now time.Time) error {
	col := p.db.Collection("opportunities")
	if len(opps) > 0 {
		writes := make([]mongo.WriteModel, 0, len(opps))
		seen := make(map[string]bool, len(opps))
		for _, o := range opps {
			o.Key = opportunityKey(o)
			if seen[o.Key] {
				continue
			}
			seen[o.Key] = true
			set, err := bson.Marshal(o)
			if err != nil {
				return fmt.Errorf("encode opportunity: %w", err)
			}
			var doc bson.M
			if err = bson.Unmarshal(set, &doc); err != nil {
				return fmt.Errorf("encode opportunity: %w", err)
			}
			delete(doc, "_id")
			writes = append(writes, mongo.NewUpdateOneModel().
				SetFilter(bson.M{"project_id": projectOID, "key": o.Key}).
				SetUpdate(bson.M{"$set": doc, "$setOnInsert": bson.M{"_id": o.ID}}).
				SetUpsert(true))
		}
		for i := 0; i < len(writes); i += 1000 {
			j := i + 1000
			if j > len(writes) {
				j = len(writes)
			}
			if _, err := col.BulkWrite(ctx, writes[i:j], options.BulkWrite().SetOrdered(false)); err != nil {
				return fmt.Errorf("store opportunities: %w", err)
			}
		}
	}
	if _, err := col.DeleteMany(ctx, bson.M{"project_id": projectOID, "computed_at": bson.M{"$lt": now}}); err != nil {
		return fmt.Errorf("clear old opportunities: %w", err)
	}
	return nil
}

// crawlIssueOpportunities turns the site audit's critical and warning issues
// on pages with search traffic into opportunities, weighted by the clicks at
// risk. The page keeps its Search Console URL, so it groups with the page's
// other signals.
func (p *Processor) crawlIssueOpportunities(ctx context.Context, projectOID bson.ObjectID, curPages []oppPage, now time.Time) ([]models.Opportunity, error) {
	gscURL := make(map[string]oppPage, len(curPages))
	for _, pg := range curPages {
		key := normalizePageURL(pg.ID)
		if prev, ok := gscURL[key]; !ok || pg.Clicks > prev.Clicks {
			gscURL[key] = pg
		}
	}
	cursor, err := p.db.Collection("page_profiles").Find(ctx, bson.M{
		"project_id":      projectOID,
		"version":         bson.M{"$gte": profileVersion},
		"issues.severity": bson.M{"$in": []string{models.SeverityCritical, models.SeverityWarning}},
	}, options.Find().SetProjection(bson.M{"url": 1, "source_url": 1, "issues": 1}))
	if err != nil {
		return nil, fmt.Errorf("query page profiles: %w", err)
	}
	var profiles []models.PageProfile
	if err = cursor.All(ctx, &profiles); err != nil {
		return nil, fmt.Errorf("decode page profiles: %w", err)
	}
	var opps []models.Opportunity
	for _, prof := range profiles {
		pg, ok := gscURL[prof.URL]
		if !ok || pg.Clicks < 3 {
			continue
		}
		if o, found := crawlIssueOpportunity(projectOID, pg, prof.Issues, now); found {
			opps = append(opps, o)
		}
	}
	sort.Slice(opps, func(i, j int) bool { return opps[i].Score > opps[j].Score })
	if len(opps) > oppCrawlIssueTopPages {
		opps = opps[:oppCrawlIssueTopPages]
	}
	return opps, nil
}

// crawlIssueOpportunity builds the opportunity for one page's audit issues.
// Notices are left out: they are not worth a page analysis on their own.
func crawlIssueOpportunity(projectOID bson.ObjectID, pg oppPage, issues []models.PageIssue, now time.Time) (models.Opportunity, bool) {
	var codes []string
	worst := ""
	for _, is := range issues {
		if is.Severity != models.SeverityCritical && is.Severity != models.SeverityWarning {
			continue
		}
		codes = append(codes, is.Code)
		if worst != models.SeverityCritical {
			worst = is.Severity
		}
	}
	if len(codes) == 0 {
		return models.Opportunity{}, false
	}
	return models.Opportunity{
		ID: bson.NewObjectID(), ProjectID: projectOID,
		Type: models.OppCrawlIssue, Page: pg.ID,
		Score: crawlIssueGain(pg.Clicks, worst),
		Metrics: map[string]float64{
			"clicks":      pg.Clicks,
			"impressions": pg.Impressions,
		},
		Details:    worst + ": " + strings.Join(codes, ", "),
		ComputedAt: now,
	}, true
}
