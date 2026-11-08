package worker

import (
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
	// vitalsPageTypes is how many page types get measured per run.
	vitalsPageTypes = 8
	vitalsEvery     = 7 * 24 * time.Hour
	vitalsCandidate = 2000
)

// pageSpeedAPI is a variable so that tests can point it at a stub.
var pageSpeedAPI = "https://www.googleapis.com/pagespeedonline/v5/runPagespeed"

// pageSpeedClient waits long: one measurement can take most of a minute.
var pageSpeedClient = &http.Client{Timeout: 120 * time.Second}

// SiteVitalsPayload names the project of a site:vitals task.
type SiteVitalsPayload struct {
	ProjectID string `json:"project_id"`
}

// NewSiteVitalsTask creates the task that measures a project's page types.
func NewSiteVitalsTask(projectID string) (*asynq.Task, error) {
	payload, err := json.Marshal(SiteVitalsPayload{ProjectID: projectID})
	if err != nil {
		return nil, err
	}
	return asynq.NewTask(TypeSiteVitals, payload, asynq.MaxRetry(0), asynq.Timeout(20*time.Minute), asynq.Unique(time.Hour)), nil
}

// vitalsTargets picks each page type's strongest page, strongest types first.
// Profiles must arrive sorted by impressions, strongest first.
func vitalsTargets(profiles []models.PageProfile, n int) []models.PageProfile {
	seen := make(map[string]bool)
	var out []models.PageProfile
	for _, pr := range profiles {
		if pr.StatusCode >= 400 || pr.Noindex || seen[pr.Group] {
			continue
		}
		seen[pr.Group] = true
		out = append(out, pr)
		if len(out) == n {
			break
		}
	}
	return out
}

type psiMetric struct {
	Percentile float64 `json:"percentile"`
}

type psiResponse struct {
	LoadingExperience struct {
		OverallCategory string               `json:"overall_category"`
		OriginFallback  bool                 `json:"origin_fallback"`
		Metrics         map[string]psiMetric `json:"metrics"`
	} `json:"loadingExperience"`
	LighthouseResult struct {
		Categories struct {
			Performance struct {
				Score float64 `json:"score"`
			} `json:"performance"`
		} `json:"categories"`
		Audits map[string]struct {
			NumericValue float64 `json:"numericValue"`
		} `json:"audits"`
	} `json:"lighthouseResult"`
}

// vitalsFromPSI turns a PageSpeed Insights answer into a measurement.
func vitalsFromPSI(r *psiResponse, v *models.PageVitals) {
	m := r.LoadingExperience.Metrics
	if len(m) > 0 {
		v.Field = &models.CoreVitals{
			LCPMs: m["LARGEST_CONTENTFUL_PAINT_MS"].Percentile,
			INPMs: m["INTERACTION_TO_NEXT_PAINT"].Percentile,
			// The API reports CLS multiplied by 100.
			CLS: m["CUMULATIVE_LAYOUT_SHIFT_SCORE"].Percentile / 100,
		}
		v.FieldOverall = r.LoadingExperience.OverallCategory
		v.OriginFallback = r.LoadingExperience.OriginFallback
	}
	a := r.LighthouseResult.Audits
	v.LabScore = r.LighthouseResult.Categories.Performance.Score
	v.LabLCPMs = a["largest-contentful-paint"].NumericValue
	v.LabTBTMs = a["total-blocking-time"].NumericValue
	v.LabCLS = a["cumulative-layout-shift"].NumericValue
}

func measurePage(ctx context.Context, pageURL, apiKey string, v *models.PageVitals) error {
	q := url.Values{"url": {pageURL}, "strategy": {"mobile"}, "category": {"performance"}}
	if apiKey != "" {
		q.Set("key", apiKey)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageSpeedAPI+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := pageSpeedClient.Do(req)
	if err != nil {
		// The URL of the request carries the key; keep it out of the record.
		var ue *url.Error
		if asURLError(err, &ue) {
			return ue.Err
		}
		return err
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode == http.StatusTooManyRequests {
		return fmt.Errorf("PageSpeed quota used up; set pagespeed_api_key in Settings")
	}
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return fmt.Errorf("PageSpeed answered HTTP %d", resp.StatusCode)
	}
	var parsed psiResponse
	if err = json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return fmt.Errorf("read PageSpeed answer: %w", err)
	}
	vitalsFromPSI(&parsed, v)
	return nil
}

func asURLError(err error, target **url.Error) bool {
	ue, ok := err.(*url.Error)
	if ok {
		*target = ue
	}
	return ok
}

// settingValue reads a global setting, or "".
func settingValue(ctx context.Context, db *mongo.Database, key string) string {
	var s models.Setting
	if err := db.Collection("settings").FindOne(ctx, bson.M{"key": key}).Decode(&s); err != nil {
		return ""
	}
	return s.Value
}

// HandleSiteVitals measures the strongest page of each page type on mobile.
func (p *Processor) HandleSiteVitals(ctx context.Context, t *asynq.Task) error {
	var payload SiteVitalsPayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		return fmt.Errorf("%w: unmarshal site:vitals payload: %v", asynq.SkipRetry, err)
	}
	projectOID, err := bson.ObjectIDFromHex(payload.ProjectID)
	if err != nil {
		return fmt.Errorf("%w: invalid project_id: %v", asynq.SkipRetry, err)
	}
	cursor, err := p.db.Collection("page_profiles").Find(ctx, bson.M{"project_id": projectOID, "version": bson.M{"$gte": profileVersion}},
		options.Find().SetSort(bson.D{{Key: "impressions", Value: -1}}).SetLimit(vitalsCandidate).
			SetProjection(bson.M{"url": 1, "source_url": 1, "group": 1, "impressions": 1, "status_code": 1, "noindex": 1}))
	if err != nil {
		return err
	}
	var profiles []models.PageProfile
	if err = cursor.All(ctx, &profiles); err != nil {
		return err
	}
	apiKey := settingValue(ctx, p.db, "pagespeed_api_key")
	runAt := time.Now().UTC()
	for _, pr := range vitalsTargets(profiles, vitalsPageTypes) {
		v := models.PageVitals{ID: bson.NewObjectID(), ProjectID: projectOID, RunAt: runAt, URL: pr.SourceURL, Group: pr.Group, Impressions: pr.Impressions}
		if measureErr := measurePage(ctx, pr.SourceURL, apiKey, &v); measureErr != nil {
			v.Error = measureErr.Error()
			p.logger.Info("vitals: page not measured", zap.String("page", pr.SourceURL), zap.String("error", v.Error))
		}
		if _, err = p.db.Collection("page_vitals").InsertOne(context.WithoutCancel(ctx), v); err != nil {
			return err
		}
	}
	return nil
}

// enqueueVitalsIfDue measures speed after a crawl, at most once a week.
func (p *Processor) enqueueVitalsIfDue(ctx context.Context, projectID bson.ObjectID, now time.Time) {
	// Google turns down nearly every request without a key, so the automatic
	// run needs one. A manual run can still try.
	if p.asynqClient == nil || settingValue(ctx, p.db, "pagespeed_api_key") == "" {
		return
	}
	if n, err := p.db.Collection("page_vitals").CountDocuments(ctx, bson.M{
		"project_id": projectID, "run_at": bson.M{"$gte": now.Add(-vitalsEvery)}, "error": bson.M{"$exists": false},
	}, options.Count().SetLimit(1)); err != nil || n > 0 {
		return
	}
	task, err := NewSiteVitalsTask(projectID.Hex())
	if err != nil {
		return
	}
	if _, err = p.asynqClient.EnqueueContext(ctx, task); err != nil {
		p.logger.Info("vitals not queued", zap.Error(err))
	}
}

// LatestVitals returns the newest run of a project.
func LatestVitals(ctx context.Context, db *mongo.Database, projectID bson.ObjectID) ([]models.PageVitals, error) {
	col := db.Collection("page_vitals")
	var last models.PageVitals
	err := col.FindOne(ctx, bson.M{"project_id": projectID}, options.FindOne().SetSort(bson.D{{Key: "run_at", Value: -1}})).Decode(&last)
	if err == mongo.ErrNoDocuments {
		return []models.PageVitals{}, nil
	}
	if err != nil {
		return nil, err
	}
	cursor, err := col.Find(ctx, bson.M{"project_id": projectID, "run_at": last.RunAt}, options.Find().SetSort(bson.D{{Key: "impressions", Value: -1}}))
	if err != nil {
		return nil, err
	}
	rows := []models.PageVitals{}
	if err = cursor.All(ctx, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}
