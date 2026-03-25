package models

import (
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// DiscoveryStatus represents the lifecycle status of a discovery run.
type DiscoveryStatus string

const (
	DiscoveryStatusPending DiscoveryStatus = "pending"
	DiscoveryStatusRunning DiscoveryStatus = "running"
	DiscoveryStatusDone    DiscoveryStatus = "done"
	DiscoveryStatusFailed  DiscoveryStatus = "failed"
)

// Discovery represents a whole-website discovery run that crawls a project's
// root URL, collects all internal links, and enqueues SEO crawl jobs for each.
type Discovery struct {
	ID            bson.ObjectID   `bson:"_id,omitempty"          json:"id,omitempty"`
	ProjectID     bson.ObjectID   `bson:"project_id"             json:"project_id"`
	Status        DiscoveryStatus `bson:"status"                 json:"status"`
	PagesFound    int             `bson:"pages_found"            json:"pages_found"`
	QueueEntryIDs []string        `bson:"queue_entry_ids"        json:"queue_entry_ids"`
	Error         string          `bson:"error,omitempty"        json:"error,omitempty"`
	StartedAt     time.Time       `bson:"started_at"             json:"started_at"`
	FinishedAt    *time.Time      `bson:"finished_at,omitempty"  json:"finished_at,omitempty"`
}

// DiscoverySummary extends Discovery with a real-time progress view of the
// SEO crawl jobs that were enqueued during the discovery run.
type DiscoverySummary struct {
	Discovery
	PagesDone    int `json:"pages_done"`
	PagesFailed  int `json:"pages_failed"`
	PagesPending int `json:"pages_pending"`
}
