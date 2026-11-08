package worker

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/hibiken/asynq"
)

// Task type identifiers used as asynq task type names.
const (
	TypeCrawlRender  = "crawl:render"
	TypeBlogGenerate = "blog:generate"
	TypeGSCSync      = "gsc:sync"
	TypeComputeOpps  = "opportunities:compute"
	TypeGenSuggest   = "suggestions:generate"
	TypeVerifyTask   = "tasks:verify"
	TypeMeasureTasks = "tasks:measure"
	TypeSiteCrawl    = "site:crawl"
	TypeSiteVitals   = "site:vitals"
	TypeDigestSend   = "digest:send"
)

// queueCritical is the asynq queue of cache refreshes.
const queueCritical = "critical"

// RenderPayload names the page a crawl:render task refreshes in the cache.
type RenderPayload struct {
	QueueEntryID string `json:"queue_entry_id"`
	ProjectID    string `json:"project_id"`
	URL          string `json:"url"`
}

// NewCrawlRenderTask creates the task that renders a page into the
// prerender cache. A page has at most one such task waiting.
func NewCrawlRenderTask(queueEntryID, projectID, pageURL string) (*asynq.Task, error) {
	payload, err := json.Marshal(RenderPayload{QueueEntryID: queueEntryID, ProjectID: projectID, URL: pageURL})
	if err != nil {
		return nil, fmt.Errorf("marshal crawl:render payload: %w", err)
	}
	return asynq.NewTask(TypeCrawlRender, payload, asynq.Queue(queueCritical), asynq.MaxRetry(1),
		asynq.Timeout(2*time.Minute), asynq.Unique(10*time.Minute)), nil
}

// BlogGeneratePayload is the JSON payload for a blog:generate task.
type BlogGeneratePayload struct {
	BlogPostID   string `json:"blog_post_id"`
	BlogConfigID string `json:"blog_config_id"`
	ProjectID    string `json:"project_id"`
	SeedKeyword  string `json:"seed_keyword"`
	Language     string `json:"language"`
	Tone         string `json:"tone"`
	WordCount    int    `json:"word_count"`
}

// NewBlogGenerateTask creates an asynq task for AI-driven blog post generation.
func NewBlogGenerateTask(p BlogGeneratePayload) (*asynq.Task, error) {
	payload, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("marshal blog:generate payload: %w", err)
	}
	return asynq.NewTask(TypeBlogGenerate, payload), nil
}

// GSCSyncPayload is the JSON payload for a gsc:sync task.
// Empty StartDate/EndDate means the worker derives the range from the
// project's sync state (daily: last synced date + 1 … today-3d).
type GSCSyncPayload struct {
	ProjectID string `json:"project_id"`
	StartDate string `json:"start_date,omitempty"` // YYYY-MM-DD
	EndDate   string `json:"end_date,omitempty"`   // YYYY-MM-DD
	Backfill  bool   `json:"backfill,omitempty"`   // fetch up to 16 months of history
}

// NewGSCSyncTask creates an asynq task for syncing GSC Search Analytics rows.
func NewGSCSyncTask(p GSCSyncPayload) (*asynq.Task, error) {
	payload, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("marshal gsc:sync payload: %w", err)
	}
	return asynq.NewTask(TypeGSCSync, payload), nil
}

// ComputeOppsPayload is the JSON payload for an opportunities:compute task.
type ComputeOppsPayload struct {
	ProjectID string `json:"project_id"`
}

// NewComputeOppsTask creates an asynq task for recomputing a project's
// organic-search opportunities from stored GSC rows.
func NewComputeOppsTask(projectID string) (*asynq.Task, error) {
	payload, err := json.Marshal(ComputeOppsPayload{ProjectID: projectID})
	if err != nil {
		return nil, fmt.Errorf("marshal opportunities:compute payload: %w", err)
	}
	return asynq.NewTask(TypeComputeOpps, payload), nil
}

// VerifyTaskPayload is the JSON payload for a tasks:verify task.
type VerifyTaskPayload struct {
	TaskID string `json:"task_id"`
}

// MeasureTasksPayload is the JSON payload for a tasks:measure task.
type MeasureTasksPayload struct {
	ProjectID string `json:"project_id"`
}

// GenSuggestPayload is the JSON payload for a suggestions:generate task.
type GenSuggestPayload struct {
	ProjectID string `json:"project_id"`
	Limit     int    `json:"limit,omitempty"` // max opportunities to process (default 10)
	// OpportunityID restricts generation to a single opportunity (ad-hoc
	// request from the UI).
	OpportunityID string `json:"opportunity_id,omitempty"`
}

// NewGenSuggestTask creates an asynq task for generating AI fix suggestions
// for a project's top opportunities.
func NewGenSuggestTask(p GenSuggestPayload) (*asynq.Task, error) {
	payload, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("marshal suggestions:generate payload: %w", err)
	}
	return asynq.NewTask(TypeGenSuggest, payload), nil
}
