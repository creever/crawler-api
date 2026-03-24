package models

import (
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// QueueStatus represents the processing status of a crawl queue entry.
type QueueStatus string

const (
	QueueStatusPending    QueueStatus = "pending"
	QueueStatusProcessing QueueStatus = "processing"
	QueueStatusDone       QueueStatus = "done"
	QueueStatusFailed     QueueStatus = "failed"
)

// QueueTaskType distinguishes between an SEO-extraction crawl and a
// pre-render (full-HTML) crawl.
type QueueTaskType string

const (
	QueueTaskTypeSEO    QueueTaskType = "seo"
	QueueTaskTypeRender QueueTaskType = "render"
)

// QueueEntry represents a URL that has been submitted for parsing and caching.
type QueueEntry struct {
	ID          bson.ObjectID  `bson:"_id,omitempty"           json:"id,omitempty"`
	ProjectID   bson.ObjectID  `bson:"project_id"              json:"project_id"              binding:"required"`
	DiscoveryID *bson.ObjectID `bson:"discovery_id,omitempty"  json:"discovery_id,omitempty"`
	URL         string         `bson:"url"                     json:"url"                     binding:"required"`
	TaskType    QueueTaskType  `bson:"task_type"               json:"task_type"`
	Priority    int            `bson:"priority"                json:"priority"`
	Status      QueueStatus    `bson:"status"                  json:"status"`
	AsynqTaskID string         `bson:"asynq_task_id,omitempty" json:"asynq_task_id,omitempty"`
	Error       string         `bson:"error,omitempty"         json:"error,omitempty"`
	EnqueuedAt  time.Time      `bson:"enqueued_at"             json:"enqueued_at"`
	ProcessedAt *time.Time     `bson:"processed_at,omitempty"  json:"processed_at,omitempty"`
}

// QueueEntryStatusUpdate is used to transition the status of a queue entry.
type QueueEntryStatusUpdate struct {
	Status      QueueStatus `bson:"status"                 json:"status"         binding:"required"`
	Error       string      `bson:"error,omitempty"        json:"error,omitempty"`
	ProcessedAt *time.Time  `bson:"processed_at,omitempty" json:"-"`
}
