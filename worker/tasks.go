package worker

import (
	"encoding/json"
	"fmt"

	"github.com/hibiken/asynq"

	"github.com/creever/crawler-api/models"
)

// Task type identifiers used as asynq task type names.
const (
	TypeCrawlSEO      = "crawl:seo"
	TypeCrawlRender   = "crawl:render"
	TypeCrawlDiscover = "crawl:discover"
	TypeBlogGenerate  = "blog:generate"
	TypeGSCSync       = "gsc:sync"
	TypeComputeOpps   = "opportunities:compute"
	TypeGenSuggest    = "suggestions:generate"
	TypeVerifyTask    = "tasks:verify"
	TypeMeasureTasks  = "tasks:measure"
	TypeSiteCrawl     = "site:crawl"
)

// CrawlPayload is the JSON payload stored in every crawl task.
// It carries the queue-entry ID (so the worker can update its status in
// MongoDB) and the ProjectConfig that drives the crawler.
type CrawlPayload struct {
	QueueEntryID string               `json:"queue_entry_id"`
	Config       models.ProjectConfig `json:"config"`
}

// NewCrawlSEOTask creates an asynq task for an SEO-extraction crawl.
func NewCrawlSEOTask(queueEntryID string, config models.ProjectConfig) (*asynq.Task, error) {
	payload, err := json.Marshal(CrawlPayload{
		QueueEntryID: queueEntryID,
		Config:       config,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal crawl:seo payload: %w", err)
	}
	return asynq.NewTask(TypeCrawlSEO, payload), nil
}

// NewCrawlRenderTask creates an asynq task for a pre-render crawl.
func NewCrawlRenderTask(queueEntryID string, config models.ProjectConfig) (*asynq.Task, error) {
	payload, err := json.Marshal(CrawlPayload{
		QueueEntryID: queueEntryID,
		Config:       config,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal crawl:render payload: %w", err)
	}
	return asynq.NewTask(TypeCrawlRender, payload), nil
}

// DiscoverPayload is the JSON payload stored in a crawl:discover task.
// It carries the discovery-run ID (so the worker can update its status in
// MongoDB) and the ProjectConfig that drives the crawler.
type DiscoverPayload struct {
	DiscoveryID string               `json:"discovery_id"`
	Config      models.ProjectConfig `json:"config"`
}

// NewCrawlDiscoverTask creates an asynq task for a whole-site discovery run.
func NewCrawlDiscoverTask(discoveryID string, config models.ProjectConfig) (*asynq.Task, error) {
	payload, err := json.Marshal(DiscoverPayload{
		DiscoveryID: discoveryID,
		Config:      config,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal crawl:discover payload: %w", err)
	}
	return asynq.NewTask(TypeCrawlDiscover, payload), nil
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
