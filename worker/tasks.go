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
)

// CrawlPayload is the JSON payload stored in every crawl task.
// It carries the queue-entry ID (so the worker can update its status in
// MongoDB) and the ProjectConfig that drives the crawler.
type CrawlPayload struct {
	QueueEntryID string              `json:"queue_entry_id"`
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
