package worker

import (
	"encoding/json"
	"fmt"

	"github.com/hibiken/asynq"

	"github.com/creever/crawler-api/models"
)

// Task type identifiers used as asynq task type names.
const (
	TypeCrawlSEO    = "crawl:seo"
	TypeCrawlRender = "crawl:render"
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
