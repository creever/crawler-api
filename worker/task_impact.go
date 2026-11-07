package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/hibiken/asynq"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.uber.org/zap"

	"github.com/creever/crawler-api/models"
)

const (
	impactWindowDays = 28
	// impactGraceDays skips the days while search engines pick up the change.
	impactGraceDays = 5
	// minBaselineClicks is the least traffic a window needs for its percentages
	// to mean anything.
	minBaselineClicks = 30
	// minControlPages keeps a few odd pages from serving as a comparison.
	minControlPages = 5
	// impactBand is the difference from the comparison group that counts as a
	// change. It is a starting value, not measured on real tasks yet.
	impactBand = 0.10
	// regressionDrop is the fall in clicks that brings a closed task back.
	regressionDrop = 0.30
	// watchDays is how long a closed task is watched for a regression.
	watchDays    = 180
	yearDays     = 364 // whole weeks, so weekdays line up
	maxTaskMoves = 3
	measureEvery = 24 * time.Hour
)

const dayLayout = "2006-01-02"

// timeWindow is an inclusive range of days.
type timeWindow struct{ Start, End time.Time }

func (w timeWindow) days() int {
	if w.End.Before(w.Start) {
		return 0
	}
	return int(w.End.Sub(w.Start).Hours()/24) + 1
}

func (w timeWindow) shifted(days int) timeWindow {
	return timeWindow{w.Start.AddDate(0, 0, days), w.End.AddDate(0, 0, days)}
}

// impactWindows returns the two windows of a task. Before ends the day before
// the change, within the days search data exists for, and after starts when
// the grace days are over. Comparing daily averages keeps a window still
// filling up comparable.
func impactWindows(doneAt, lastData time.Time) (before, after timeWindow) {
	done := doneAt.UTC().Truncate(24 * time.Hour)
	end := done.AddDate(0, 0, -1)
	if lastData.Before(end) {
		end = lastData
	}
	before = timeWindow{end.AddDate(0, 0, -(impactWindowDays - 1)), end}
	start := done.AddDate(0, 0, impactGraceDays)
	last := start.AddDate(0, 0, impactWindowDays-1)
	if lastData.Before(last) {
		last = lastData
	}
	return before, timeWindow{start, last}
}

// windowTotals is what a set of pages did in a window.
type windowTotals struct {
	Clicks, Impressions, PosWeighted float64
	PerPage                          map[string]float64
}

func (w windowTotals) daily(days int) models.DailyMetrics {
	if days <= 0 {
		return models.DailyMetrics{}
	}
	m := models.DailyMetrics{Clicks: w.Clicks / float64(days), Impressions: w.Impressions / float64(days)}
	if w.Impressions > 0 {
		m.CTR = w.Clicks / w.Impressions
		m.Position = w.PosWeighted / w.Impressions
	}
	return m
}

// relativeChange returns the change in daily clicks as a fraction.
func relativeChange(before windowTotals, beforeDays int, after windowTotals, afterDays int) float64 {
	b, a := before.daily(beforeDays).Clicks, after.daily(afterDays).Clicks
	if b <= 0 {
		return 0
	}
	return (a - b) / b
}

func (p *Processor) pagesInWindow(ctx context.Context, projectOID bson.ObjectID, pages []string, w timeWindow) (windowTotals, error) {
	totals := windowTotals{PerPage: make(map[string]float64)}
	if len(pages) == 0 || w.days() == 0 {
		return totals, nil
	}
	cursor, err := p.db.Collection("gsc_rows").Aggregate(ctx, bson.A{
		bson.D{{Key: "$match", Value: bson.D{
			{Key: "project_id", Value: projectOID}, {Key: "page", Value: bson.M{"$in": pages}}, {Key: "date", Value: dateRange(w.Start, w.End)},
		}}},
		bson.D{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$page"},
			{Key: "clicks", Value: bson.D{{Key: "$sum", Value: "$clicks"}}},
			{Key: "impressions", Value: bson.D{{Key: "$sum", Value: "$impressions"}}},
			{Key: "pos_weighted", Value: bson.D{{Key: "$sum", Value: bson.D{{Key: "$multiply", Value: bson.A{"$position", "$impressions"}}}}}},
		}}},
	})
	if err != nil {
		return totals, err
	}
	defer cursor.Close(ctx) //nolint:errcheck
	var rows []gscGroup
	if err = cursor.All(ctx, &rows); err != nil {
		return totals, err
	}
	for _, r := range rows {
		totals.Clicks += r.Clicks
		totals.Impressions += r.Impressions
		totals.PosWeighted += r.PosWeighted
		totals.PerPage[r.ID] = r.Clicks
	}
	return totals, nil
}

// impactVerdict judges a finished window against its comparison group.
func impactVerdict(afterDays int, beforeClicks, change float64, controlKind string, controlChange float64) string {
	switch {
	case afterDays < impactWindowDays:
		return models.VerdictMeasuring
	case beforeClicks < minBaselineClicks || controlKind == models.ControlNone:
		return models.VerdictInconclusive
	case change-controlChange >= impactBand:
		return models.VerdictImproved
	case change-controlChange <= -impactBand:
		return models.VerdictWorse
	}
	return models.VerdictNoChange
}

// topMoves returns the pages that gained and lost the most clicks per day.
func topMoves(before map[string]float64, beforeDays int, after map[string]float64, afterDays int) []models.PageMove {
	if beforeDays <= 0 || afterDays <= 0 {
		return nil
	}
	var moves []models.PageMove
	seen := make(map[string]bool)
	add := func(page string) {
		if !seen[page] {
			seen[page] = true
			moves = append(moves, models.PageMove{Page: page, Before: before[page] / float64(beforeDays), After: after[page] / float64(afterDays)})
		}
	}
	for page := range before {
		add(page)
	}
	for page := range after {
		add(page)
	}
	delta := func(m models.PageMove) float64 { return m.After - m.Before }
	sort.Slice(moves, func(i, j int) bool {
		if delta(moves[i]) != delta(moves[j]) {
			return delta(moves[i]) > delta(moves[j])
		}
		return moves[i].Page < moves[j].Page
	})
	if len(moves) <= 2*maxTaskMoves {
		return moves
	}
	return append(moves[:maxTaskMoves:maxTaskMoves], moves[len(moves)-maxTaskMoves:]...)
}

// comparison finds how similar pages moved between two windows: unaffected
// pages of the same type, or else the same pages a year earlier.
func (p *Processor) comparison(ctx context.Context, t *models.SEOTask, profiles []models.PageProfile, before, after timeWindow) (string, float64, error) {
	if t.PageGroup != "" {
		own := make(map[string]bool, len(t.Pages))
		for _, page := range t.Pages {
			own[normalizePageURL(page)] = true
		}
		var control []string
		for _, pr := range profiles {
			if pr.Group == t.PageGroup && !own[pr.URL] {
				control = append(control, pr.SourceURL)
			}
		}
		if len(control) >= minControlPages {
			b, err := p.pagesInWindow(ctx, t.ProjectID, control, before)
			if err != nil {
				return "", 0, err
			}
			a, err := p.pagesInWindow(ctx, t.ProjectID, control, after)
			if err != nil {
				return "", 0, err
			}
			if b.Clicks >= minBaselineClicks {
				return models.ControlPageType, relativeChange(b, before.days(), a, after.days()), nil
			}
		}
	}
	b, err := p.pagesInWindow(ctx, t.ProjectID, t.Pages, before.shifted(-yearDays))
	if err != nil {
		return "", 0, err
	}
	a, err := p.pagesInWindow(ctx, t.ProjectID, t.Pages, after.shifted(-yearDays))
	if err != nil {
		return "", 0, err
	}
	if b.Clicks >= minBaselineClicks {
		return models.ControlLastYear, relativeChange(b, before.days(), a, after.days()), nil
	}
	return models.ControlNone, 0, nil
}

func (p *Processor) measureTask(ctx context.Context, t *models.SEOTask, profiles []models.PageProfile, lastData, now time.Time) (*models.TaskMeasurement, error) {
	before, after := impactWindows(*t.DoneAt, lastData)
	m := &models.TaskMeasurement{
		UpdatedAt: now, BeforeStart: before.Start.Format(dayLayout), BeforeEnd: before.End.Format(dayLayout),
		AfterStart: after.Start.Format(dayLayout), AfterEnd: after.End.Format(dayLayout), AfterDays: after.days(),
		ControlKind: models.ControlNone, Verdict: models.VerdictMeasuring,
	}
	b, err := p.pagesInWindow(ctx, t.ProjectID, t.Pages, before)
	if err != nil {
		return nil, err
	}
	m.Before = b.daily(before.days())
	if m.AfterDays == 0 {
		return m, nil
	}
	a, err := p.pagesInWindow(ctx, t.ProjectID, t.Pages, after)
	if err != nil {
		return nil, err
	}
	m.After = a.daily(after.days())
	m.ClicksChange = relativeChange(b, before.days(), a, after.days())
	m.Movers = topMoves(b.PerPage, before.days(), a.PerPage, after.days())
	if m.ControlKind, m.ControlChange, err = p.comparison(ctx, t, profiles, before, after); err != nil {
		return nil, err
	}
	m.Verdict = impactVerdict(m.AfterDays, b.Clicks, m.ClicksChange, m.ControlKind, m.ControlChange)
	return m, nil
}

// applyVerdict moves a confirmed task on once its measurement has finished. A
// task that did not help returns once, because the first fix may have missed.
// After that it closes: a problem a second attempt didn't move is not one to
// keep on the list.
func applyVerdict(t *models.SEOTask, verdict string) {
	if t.Status != models.TaskStatusVerified {
		return
	}
	switch verdict {
	case models.VerdictImproved, models.VerdictInconclusive:
		t.Status = models.TaskStatusClosed
	case models.VerdictNoChange, models.VerdictWorse:
		if t.ImpactReopens > 0 {
			t.Status = models.TaskStatusClosed
			return
		}
		t.ImpactReopens++
		if verdict == models.VerdictWorse {
			reopen(t, "Clicks on these pages moved worse than the comparison group after the change. Review whether it did harm.")
		} else {
			reopen(t, "The change went live, but clicks on these pages moved no better than the comparison group. The cause may lie elsewhere.")
		}
	}
}

// regressed reports whether a closed task that had helped lost its gain since.
func (p *Processor) regressed(ctx context.Context, t *models.SEOTask, profiles []models.PageProfile, lastData, now time.Time) (bool, error) {
	if t.Status != models.TaskStatusClosed || t.Measurement == nil || t.Measurement.Verdict != models.VerdictImproved {
		return false, nil
	}
	if t.DoneAt == nil || now.Sub(*t.DoneAt) > watchDays*24*time.Hour {
		return false, nil
	}
	_, after := impactWindows(*t.DoneAt, lastData)
	recent := timeWindow{lastData.AddDate(0, 0, -(impactWindowDays - 1)), lastData}
	if !recent.Start.After(after.End) {
		return false, nil
	}
	a, err := p.pagesInWindow(ctx, t.ProjectID, t.Pages, after)
	if err != nil || a.Clicks < minBaselineClicks {
		return false, err
	}
	r, err := p.pagesInWindow(ctx, t.ProjectID, t.Pages, recent)
	if err != nil {
		return false, err
	}
	change := relativeChange(a, after.days(), r, recent.days())
	if change > -regressionDrop {
		return false, nil
	}
	kind, control, err := p.comparison(ctx, t, profiles, after, recent)
	if err != nil {
		return false, err
	}
	return kind != models.ControlNone && change-control <= -impactBand, nil
}

func (p *Processor) latestSearchDay(ctx context.Context, projectOID bson.ObjectID) (time.Time, error) {
	var row struct {
		Date string `bson:"date"`
	}
	err := p.db.Collection("gsc_rows").FindOne(ctx, bson.M{"project_id": projectOID},
		options.FindOne().SetSort(bson.D{{Key: "date", Value: -1}}).SetProjection(bson.M{"date": 1})).Decode(&row)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return time.Parse(dayLayout, row.Date)
}

// HandleTasksMeasure updates the measurement of a project's finished tasks.
func (p *Processor) HandleTasksMeasure(ctx context.Context, task *asynq.Task) error {
	var payload MeasureTasksPayload
	if err := json.Unmarshal(task.Payload(), &payload); err != nil {
		return fmt.Errorf("%w: unmarshal tasks:measure payload: %v", asynq.SkipRetry, err)
	}
	projectOID, err := bson.ObjectIDFromHex(payload.ProjectID)
	if err != nil {
		return fmt.Errorf("%w: invalid project_id: %v", asynq.SkipRetry, err)
	}
	lastData, err := p.latestSearchDay(ctx, projectOID)
	if err != nil {
		return err
	}
	if lastData.IsZero() {
		return nil
	}
	cursor, err := p.db.Collection("seo_tasks").Find(ctx, bson.M{"project_id": projectOID, "done_at": bson.M{"$exists": true},
		"status": bson.M{"$in": []models.TaskStatus{models.TaskStatusDone, models.TaskStatusVerified, models.TaskStatusClosed}}})
	if err != nil {
		return err
	}
	var tasks []models.SEOTask
	if err = cursor.All(ctx, &tasks); err != nil {
		return err
	}
	profiles, err := p.loadProfiles(ctx, projectOID)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for i := range tasks {
		t := &tasks[i]
		back, regErr := p.regressed(ctx, t, profiles, lastData, now)
		if regErr != nil {
			return regErr
		}
		if back {
			reopen(t, "These pages lost the gain they had after the change, by more than the comparison group.")
		} else if t.Status != models.TaskStatusClosed {
			m, mErr := p.measureTask(ctx, t, profiles, lastData, now)
			if mErr != nil {
				return mErr
			}
			t.Measurement = m
			applyVerdict(t, m.Verdict)
		} else if t.Measurement != nil {
			t.Measurement.UpdatedAt = now
		}
		if _, err = p.db.Collection("seo_tasks").UpdateOne(ctx, bson.M{"_id": t.ID}, bson.M{"$set": bson.M{
			"measurement": t.Measurement, "status": t.Status, "reopen_reason": t.ReopenReason,
			"impact_reopens": t.ImpactReopens, "updated_at": now,
		}}); err != nil {
			return err
		}
	}
	return RebalanceTasks(ctx, p.db, projectOID)
}

// StartTaskMeasureScheduler checks hourly for finished tasks whose measurement
// is a day old.
func StartTaskMeasureScheduler(db *mongo.Database, asynqClient *asynq.Client, logger *zap.Logger) {
	ticker := time.NewTicker(1 * time.Hour)
	go func() {
		for range ticker.C {
			enqueueTaskMeasurements(db, asynqClient, logger)
		}
	}()
	logger.Info("task measurement scheduler started")
}

func enqueueTaskMeasurements(db *mongo.Database, asynqClient *asynq.Client, logger *zap.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var projects []bson.ObjectID
	err := db.Collection("seo_tasks").Distinct(ctx, "project_id", bson.M{
		"done_at": bson.M{"$exists": true},
		"status":  bson.M{"$in": []models.TaskStatus{models.TaskStatusDone, models.TaskStatusVerified, models.TaskStatusClosed}},
		"$or": bson.A{
			bson.M{"measurement": bson.M{"$exists": false}},
			bson.M{"measurement.updated_at": bson.M{"$lt": time.Now().UTC().Add(-measureEvery)}},
		},
	}).Decode(&projects)
	if err != nil {
		logger.Error("task measurement: failed to list projects", zap.Error(err))
		return
	}
	for _, id := range projects {
		body, marshalErr := json.Marshal(MeasureTasksPayload{ProjectID: id.Hex()})
		if marshalErr != nil {
			continue
		}
		_, err = asynqClient.EnqueueContext(ctx, asynq.NewTask(TypeMeasureTasks, body, asynq.Unique(time.Hour), asynq.MaxRetry(3)))
		if err != nil && !errors.Is(err, asynq.ErrDuplicateTask) {
			logger.Error("task measurement: failed to enqueue", zap.String("project_id", id.Hex()), zap.Error(err))
		}
	}
}
