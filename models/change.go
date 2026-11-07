package models

import (
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// AppliedChange logs one change that reached the site, together with the
// "before" GSC snapshot used for impact tracking. A task records one per page.
// MongoDB collection: applied_changes
type AppliedChange struct {
	ID           bson.ObjectID  `bson:"_id,omitempty"   json:"id,omitempty"`
	ProjectID    bson.ObjectID  `bson:"project_id"      json:"project_id"`
	SuggestionID *bson.ObjectID `bson:"suggestion_id,omitempty" json:"suggestion_id,omitempty"`
	TaskID       *bson.ObjectID `bson:"task_id,omitempty"       json:"task_id,omitempty"`
	Kind         string         `bson:"kind"            json:"kind"` // a suggestion kind, or a task's issue
	Page         string         `bson:"page"            json:"page"`
	Value        string         `bson:"value"           json:"value"`   // the text that was applied
	Adapter      string         `bson:"adapter"         json:"adapter"` // "task", or the name of a former site adapter
	Status       string         `bson:"status"          json:"status"`  // applied | failed
	Error        string         `bson:"error,omitempty" json:"error,omitempty"`
	AppliedBy    *bson.ObjectID `bson:"applied_by,omitempty" json:"applied_by,omitempty"`
	AppliedAt    time.Time      `bson:"applied_at"      json:"applied_at"`
	// BeforeSnapshot holds the page's GSC aggregates for the 28 days before
	// AppliedAt, captured at apply time (GSC revises numbers retroactively).
	BeforeSnapshot PageMetrics `bson:"before_snapshot" json:"before_snapshot"`
}

// PageMetrics is a clicks/impressions/CTR/position aggregate for one page.
type PageMetrics struct {
	Clicks      float64 `bson:"clicks"      json:"clicks"`
	Impressions float64 `bson:"impressions" json:"impressions"`
	CTR         float64 `bson:"ctr"         json:"ctr"`
	Position    float64 `bson:"position"    json:"position"`
}
