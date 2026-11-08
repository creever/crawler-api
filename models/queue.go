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

// QueueTaskType is the kind of work of a queue entry; only renders remain.
type QueueTaskType string

// QueueTaskTypeRender renders a page into the prerender cache.
const QueueTaskTypeRender QueueTaskType = "render"

// QueueEntry is one render into the prerender cache.
// MongoDB collection: crawl_queue (removed 30 days after it was queued)
type QueueEntry struct {
	ID          bson.ObjectID `bson:"_id,omitempty"           json:"id,omitempty"`
	ProjectID   bson.ObjectID `bson:"project_id"              json:"project_id"              binding:"required"`
	URL         string        `bson:"url"                     json:"url"                     binding:"required"`
	TaskType    QueueTaskType `bson:"task_type"               json:"task_type"`
	Priority    int           `bson:"priority"                json:"priority"`
	Status      QueueStatus   `bson:"status"                  json:"status"`
	AsynqTaskID string        `bson:"asynq_task_id,omitempty" json:"asynq_task_id,omitempty"`
	Error       string        `bson:"error,omitempty"         json:"error,omitempty"`
	// Source says why the render was made (see the CacheSource constants).
	Source      string     `bson:"source,omitempty" json:"source,omitempty"`
	EnqueuedAt  time.Time  `bson:"enqueued_at"             json:"enqueued_at"`
	ProcessedAt *time.Time `bson:"processed_at,omitempty"  json:"processed_at,omitempty"`
}
