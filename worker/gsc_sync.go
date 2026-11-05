package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/hibiken/asynq"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.uber.org/zap"

	"github.com/creever/crawler-api/models"
)

const (
	// gscMaxRowLimit is the Search Analytics API hard cap per request.
	gscMaxRowLimit = 25000
	// gscBucketDays is the date-bucket size; weekly buckets keep even large
	// properties well under the 25k row cap per request.
	gscBucketDays = 7
	// gscBackfillMonths is how much history the first sync (backfill) fetches.
	gscBackfillMonths = 16
	// gscBulkChunk is the max number of upserts per BulkWrite call.
	gscBulkChunk = 5000
	// gscDataLagDays: GSC data has a ~3-day processing lag.
	gscDataLagDays = 3
)

// gscAnalyticsRow is one raw row from the Search Analytics API.
type gscAnalyticsRow struct {
	Keys        []string `json:"keys"`
	Clicks      float64  `json:"clicks"`
	Impressions float64  `json:"impressions"`
	CTR         float64  `json:"ctr"`
	Position    float64  `json:"position"`
}

// gscSearchAnalytics performs one Search Analytics API request. The keys in
// each returned row follow the order of dimensions.
func (p *Processor) gscSearchAnalytics(ctx context.Context, accessToken, propertyURL, startDate, endDate string, dimensions []string, rowLimit, startRow int, orderField string) ([]gscAnalyticsRow, error) {
	if orderField == "" {
		orderField = "clicks"
	}
	reqBody, _ := json.Marshal(map[string]interface{}{
		"startDate":  startDate,
		"endDate":    endDate,
		"dimensions": dimensions,
		"rowLimit":   rowLimit,
		"startRow":   startRow,
		"orderBy": []map[string]string{
			{"fieldName": orderField, "sortOrder": "DESCENDING"},
		},
	})

	apiURL := fmt.Sprintf(
		"https://searchconsole.googleapis.com/webmasters/v3/sites/%s/searchAnalytics/query",
		url.PathEscape(propertyURL),
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("build GSC analytics request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GSC analytics request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("GSC API HTTP %s: %s", resp.Status, truncate(string(b), 300))
	}

	var result struct {
		Rows []gscAnalyticsRow `json:"rows"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode GSC response: %w", err)
	}
	return result.Rows, nil
}

// HandleGSCSync processes a gsc:sync task: it fetches query × page × date ×
// device rows for the project's GSC property and upserts them into gsc_rows.
// The date range is split into weekly buckets, and each bucket is paginated
// with startRow up to the 25k row API cap.
func (p *Processor) HandleGSCSync(ctx context.Context, t *asynq.Task) error {
	var payload GSCSyncPayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		return fmt.Errorf("%w: unmarshal gsc:sync payload: %v", asynq.SkipRetry, err)
	}

	projectOID, err := bson.ObjectIDFromHex(payload.ProjectID)
	if err != nil {
		return fmt.Errorf("%w: invalid project_id: %v", asynq.SkipRetry, err)
	}

	var project models.Project
	if err = p.db.Collection("projects").FindOne(ctx, bson.M{"_id": projectOID}).Decode(&project); err != nil {
		return fmt.Errorf("%w: project not found: %v", asynq.SkipRetry, err)
	}
	if project.GSCPropertyURL == "" || project.GSCServiceAccountJSON == "" {
		return fmt.Errorf("%w: project %s has no GSC configuration", asynq.SkipRetry, payload.ProjectID)
	}

	now := time.Now().UTC()
	end := now.AddDate(0, 0, -gscDataLagDays)
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
		var state models.GSCSyncState
		stateErr := p.db.Collection("gsc_sync_state").FindOne(ctx, bson.M{"project_id": projectOID}).Decode(&state)
		if stateErr == nil && state.LastDate != "" {
			start, _ = time.Parse("2006-01-02", state.LastDate)
			start = start.AddDate(0, 0, 1)
		} else {
			// First run for this project: backfill the full history window.
			start = end.AddDate(0, -gscBackfillMonths, 0)
		}
	}

	p.logger.Info("processing gsc:sync task",
		zap.String("project_id", payload.ProjectID),
		zap.String("start", start.Format("2006-01-02")),
		zap.String("end", end.Format("2006-01-02")),
	)

	if start.After(end) {
		p.upsertGSCSyncState(ctx, projectOID, end.Format("2006-01-02"), now, "", 0)
		p.logger.Info("gsc:sync nothing to do, already up to date", zap.String("project_id", payload.ProjectID))
		return nil
	}

	var sa serviceAccountKey
	if err = json.Unmarshal([]byte(project.GSCServiceAccountJSON), &sa); err != nil {
		return fmt.Errorf("%w: parse service account credentials: %v", asynq.SkipRetry, err)
	}
	accessToken, err := p.gscGetAccessToken(ctx, sa)
	if err != nil {
		p.upsertGSCSyncState(ctx, projectOID, "", now, err.Error(), 0)
		return fmt.Errorf("get GSC access token: %w", err)
	}

	col := p.db.Collection("gsc_rows")
	var totalRows int64
	for bucketStart := start; !bucketStart.After(end); bucketStart = bucketStart.AddDate(0, 0, gscBucketDays) {
		bucketEnd := bucketStart.AddDate(0, 0, gscBucketDays-1)
		if bucketEnd.After(end) {
			bucketEnd = end
		}
		n, bucketErr := p.syncGSCBucket(ctx, col, projectOID, accessToken, project.GSCPropertyURL, bucketStart, bucketEnd)
		if bucketErr != nil {
			p.upsertGSCSyncState(ctx, projectOID, "", now, bucketErr.Error(), totalRows)
			return bucketErr
		}
		totalRows += n
	}

	p.upsertGSCSyncState(ctx, projectOID, end.Format("2006-01-02"), now, "", totalRows)
	p.logger.Info("gsc:sync task completed",
		zap.String("project_id", payload.ProjectID),
		zap.Int64("rows", totalRows),
	)
	return nil
}

// syncGSCBucket fetches one date bucket with startRow pagination and upserts
// all rows. It returns the number of rows written.
func (p *Processor) syncGSCBucket(ctx context.Context, col *mongo.Collection, projectOID bson.ObjectID, accessToken, propertyURL string, start, end time.Time) (int64, error) {
	dims := []string{"query", "page", "date", "device"}
	startStr := start.Format("2006-01-02")
	endStr := end.Format("2006-01-02")

	var total int64
	for startRow := 0; ; startRow += gscMaxRowLimit {
		rows, err := p.gscSearchAnalytics(ctx, accessToken, propertyURL, startStr, endStr, dims, gscMaxRowLimit, startRow, "clicks")
		if err != nil {
			return total, fmt.Errorf("bucket %s..%s startRow %d: %w", startStr, endStr, startRow, err)
		}
		if len(rows) == 0 {
			break
		}
		if err = p.upsertGSCRows(ctx, col, projectOID, rows); err != nil {
			return total, fmt.Errorf("bucket %s..%s startRow %d: %w", startStr, endStr, startRow, err)
		}
		total += int64(len(rows))
		if len(rows) < gscMaxRowLimit {
			break
		}
	}
	return total, nil
}

// upsertGSCRows bulk-upserts raw analytics rows into gsc_rows, keyed on
// project × query × page × date × device.
func (p *Processor) upsertGSCRows(ctx context.Context, col *mongo.Collection, projectOID bson.ObjectID, rows []gscAnalyticsRow) error {
	now := time.Now().UTC()
	writeModels := make([]mongo.WriteModel, 0, len(rows))
	for _, row := range rows {
		if len(row.Keys) != 4 {
			continue
		}
		filter := bson.M{
			"project_id": projectOID,
			"query":      row.Keys[0],
			"page":       row.Keys[1],
			"date":       row.Keys[2],
			"device":     row.Keys[3],
		}
		writeModels = append(writeModels, mongo.NewUpdateOneModel().
			SetFilter(filter).
			SetUpdate(bson.M{"$set": bson.M{
				"clicks":      int(row.Clicks),
				"impressions": int(row.Impressions),
				"ctr":         row.CTR,
				"position":    row.Position,
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
			return fmt.Errorf("bulk write gsc_rows: %w", err)
		}
	}
	return nil
}

// upsertGSCSyncState records the outcome of a sync run for the project.
func (p *Processor) upsertGSCSyncState(ctx context.Context, projectOID bson.ObjectID, lastDate string, runAt time.Time, lastErr string, totalRows int64) {
	update := bson.M{"$set": bson.M{
		"last_run_at": runAt,
		"last_error":  lastErr,
		"total_rows":  totalRows,
		"updated_at":  runAt,
	}}
	if lastDate != "" {
		update["$set"].(bson.M)["last_date"] = lastDate
	}

	tctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if _, err := p.db.Collection("gsc_sync_state").UpdateOne(
		tctx,
		bson.M{"project_id": projectOID},
		bson.M{
			"$set":         update["$set"],
			"$setOnInsert": bson.M{"_id": bson.NewObjectID(), "project_id": projectOID},
		},
		options.UpdateOne().SetUpsert(true),
	); err != nil {
		p.logger.Error("upsertGSCSyncState: failed", zap.String("project_id", projectOID.Hex()), zap.Error(err))
	}
}
