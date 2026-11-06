package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/hibiken/asynq"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.uber.org/zap"

	"github.com/creever/crawler-api/models"
)

const (
	// ga4DataLagDays: GA4 standard reports lag ~24-48h.
	ga4DataLagDays = 2
	// ga4PageSize is the runReport pagination size.
	ga4PageSize = 10000
	// ga4BackfillMonths mirrors the GSC backfill window.
	ga4BackfillMonths = 16
)

// ga4SyncPayload is the JSON payload for a ga4:sync task.
type ga4SyncPayload struct {
	ProjectID string `json:"project_id"`
	StartDate string `json:"start_date,omitempty"`
	EndDate   string `json:"end_date,omitempty"`
}

// NewGA4SyncTask creates an asynq task for syncing GA4 landing-page metrics.
func NewGA4SyncTask(projectID, startDate, endDate string) (*asynq.Task, error) {
	payload, err := json.Marshal(ga4SyncPayload{ProjectID: projectID, StartDate: startDate, EndDate: endDate})
	if err != nil {
		return nil, fmt.Errorf("marshal ga4:sync payload: %w", err)
	}
	return asynq.NewTask("ga4:sync", payload), nil
}

// HandleGA4Sync processes a ga4:sync task: it pulls landing-page × date
// sessions/conversions/revenue from the GA4 Data API and upserts them into
// ga4_rows.
func (p *Processor) HandleGA4Sync(ctx context.Context, t *asynq.Task) error {
	var payload ga4SyncPayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		return fmt.Errorf("%w: unmarshal ga4:sync payload: %v", asynq.SkipRetry, err)
	}
	projectOID, err := bson.ObjectIDFromHex(payload.ProjectID)
	if err != nil {
		return fmt.Errorf("%w: invalid project_id: %v", asynq.SkipRetry, err)
	}

	var project models.Project
	if err = p.db.Collection("projects").FindOne(ctx, bson.M{"_id": projectOID}).Decode(&project); err != nil {
		return fmt.Errorf("%w: project not found: %v", asynq.SkipRetry, err)
	}
	if project.GA4PropertyID == "" || project.GSCServiceAccountJSON == "" {
		return fmt.Errorf("%w: project %s has no GA4 configuration (ga4_property_id / gsc_service_account_json)", asynq.SkipRetry, payload.ProjectID)
	}

	now := time.Now().UTC()
	end := now.AddDate(0, 0, -ga4DataLagDays)
	if payload.EndDate != "" {
		if parsed, parseErr := time.Parse("2006-01-02", payload.EndDate); parseErr == nil {
			end = parsed
		}
	}

	var start time.Time
	switch {
	case payload.StartDate != "":
		start, err = time.Parse("2006-01-02", payload.StartDate)
		if err != nil {
			return fmt.Errorf("%w: invalid start_date: %v", asynq.SkipRetry, err)
		}
	default:
		var state models.GA4SyncState
		stateErr := p.db.Collection("ga4_sync_state").FindOne(ctx, bson.M{"project_id": projectOID}).Decode(&state)
		if stateErr == nil && state.LastDate != "" {
			start, _ = time.Parse("2006-01-02", state.LastDate)
			start = start.AddDate(0, 0, 1)
		} else {
			start = end.AddDate(0, -ga4BackfillMonths, 0)
		}
	}

	p.logger.Info("processing ga4:sync task",
		zap.String("project_id", payload.ProjectID),
		zap.String("start", start.Format("2006-01-02")),
		zap.String("end", end.Format("2006-01-02")),
	)

	if start.After(end) {
		p.upsertGA4SyncState(ctx, projectOID, end.Format("2006-01-02"), now, "", 0)
		return nil
	}

	var sa serviceAccountKey
	if err = json.Unmarshal([]byte(project.GSCServiceAccountJSON), &sa); err != nil {
		return fmt.Errorf("%w: parse service account credentials: %v", asynq.SkipRetry, err)
	}
	accessToken, err := p.googleAccessToken(ctx, sa, "https://www.googleapis.com/auth/analytics.readonly")
	if err != nil {
		p.upsertGA4SyncState(ctx, projectOID, "", now, err.Error(), 0)
		return fmt.Errorf("get GA4 access token: %w", err)
	}

	col := p.db.Collection("ga4_rows")
	var totalRows int64
	for offset := 0; ; offset += ga4PageSize {
		rows, rowCount, err := p.ga4RunReport(ctx, accessToken, project.GA4PropertyID,
			start.Format("2006-01-02"), end.Format("2006-01-02"), offset)
		if err != nil {
			p.upsertGA4SyncState(ctx, projectOID, "", now, err.Error(), totalRows)
			return err
		}
		if len(rows) == 0 {
			break
		}
		if err = p.upsertGA4Rows(ctx, col, projectOID, rows); err != nil {
			p.upsertGA4SyncState(ctx, projectOID, "", now, err.Error(), totalRows)
			return err
		}
		totalRows += int64(len(rows))
		if rowCount < ga4PageSize {
			break
		}
	}

	p.upsertGA4SyncState(ctx, projectOID, end.Format("2006-01-02"), now, "", totalRows)
	p.logger.Info("ga4:sync task completed",
		zap.String("project_id", payload.ProjectID),
		zap.Int64("rows", totalRows),
	)
	return nil
}

// ga4Row is one runReport row: dimensions [date, pagePath].
type ga4Row struct {
	Date        string
	Page        string
	Sessions    float64
	Conversions float64
	Revenue     float64
}

// ga4RunReport fetches one page of the GA4 report. Returns rows and the total
// row count of the report (for pagination).
func (p *Processor) ga4RunReport(ctx context.Context, accessToken, propertyID, startDate, endDate string, offset int) ([]ga4Row, int, error) {
	reqBody, _ := json.Marshal(map[string]interface{}{
		"dateRanges": []map[string]string{{"startDate": startDate, "endDate": endDate}},
		"dimensions": []map[string]string{{"name": "date"}, {"name": "pagePath"}},
		"metrics": []map[string]string{
			{"name": "sessions"},
			{"name": "conversions"},
			{"name": "totalRevenue"},
		},
		"limit":  ga4PageSize,
		"offset": offset,
		"orderBys": []map[string]interface{}{
			{"dimension": map[string]string{"dimensionName": "date"}, "desc": false},
		},
	})

	apiURL := fmt.Sprintf("https://analyticsdata.googleapis.com/v1beta/properties/%s:runReport", propertyID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(reqBody))
	if err != nil {
		return nil, 0, fmt.Errorf("build GA4 request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("GA4 request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, 0, fmt.Errorf("GA4 API HTTP %s: %s", resp.Status, truncate(string(b), 300))
	}

	var result struct {
		Rows []struct {
			DimensionValues []struct {
				Value string `json:"value"`
			} `json:"dimensionValues"`
			MetricValues []struct {
				Value string `json:"value"`
			} `json:"metricValues"`
		} `json:"rows"`
		RowCount int `json:"rowCount"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, 0, fmt.Errorf("decode GA4 response: %w", err)
	}

	rows := make([]ga4Row, 0, len(result.Rows))
	for _, r := range result.Rows {
		if len(r.DimensionValues) != 2 || len(r.MetricValues) != 3 {
			continue
		}
		rows = append(rows, ga4Row{
			Date:        r.DimensionValues[0].Value,
			Page:        r.DimensionValues[1].Value,
			Sessions:    parseFloat(r.MetricValues[0].Value),
			Conversions: parseFloat(r.MetricValues[1].Value),
			Revenue:     parseFloat(r.MetricValues[2].Value),
		})
	}
	return rows, result.RowCount, nil
}

func parseFloat(s string) float64 {
	var f float64
	_, _ = fmt.Sscanf(s, "%g", &f)
	return f
}

// upsertGA4Rows bulk-upserts GA4 rows keyed on project × page × date.
func (p *Processor) upsertGA4Rows(ctx context.Context, col *mongo.Collection, projectOID bson.ObjectID, rows []ga4Row) error {
	now := time.Now().UTC()
	writeModels := make([]mongo.WriteModel, 0, len(rows))
	for _, r := range rows {
		writeModels = append(writeModels, mongo.NewUpdateOneModel().
			SetFilter(bson.M{
				"project_id": projectOID,
				"page":       r.Page,
				"date":       r.Date,
			}).
			SetUpdate(bson.M{"$set": bson.M{
				"sessions":    int(r.Sessions),
				"conversions": r.Conversions,
				"revenue":     r.Revenue,
				"synced_at":   now,
			}}).
			SetUpsert(true))
	}
	for i := 0; i < len(writeModels); i += gscBulkChunk {
		end := i + gscBulkChunk
		if end > len(writeModels) {
			end = len(writeModels)
		}
		if _, err := col.BulkWrite(ctx, writeModels[i:end]); err != nil {
			return fmt.Errorf("bulk write ga4_rows: %w", err)
		}
	}
	return nil
}

// upsertGA4SyncState records the outcome of a GA4 sync run.
func (p *Processor) upsertGA4SyncState(ctx context.Context, projectOID bson.ObjectID, lastDate string, runAt time.Time, lastErr string, totalRows int64) {
	set := bson.M{
		"last_run_at": runAt,
		"last_error":  lastErr,
		"total_rows":  totalRows,
		"updated_at":  runAt,
	}
	if lastDate != "" {
		set["last_date"] = lastDate
	}

	tctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if _, err := p.db.Collection("ga4_sync_state").UpdateOne(
		tctx,
		bson.M{"project_id": projectOID},
		bson.M{
			"$set":         set,
			"$setOnInsert": bson.M{"_id": bson.NewObjectID(), "project_id": projectOID},
		},
		options.UpdateOne().SetUpsert(true),
	); err != nil {
		p.logger.Error("upsertGA4SyncState: failed", zap.String("project_id", projectOID.Hex()), zap.Error(err))
	}
}
