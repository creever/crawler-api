package models

import (
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// GSCRow is a single query × page × date × device row from the Google Search
// Console Search Analytics API.
// MongoDB collection: gsc_rows
type GSCRow struct {
	ID          bson.ObjectID `bson:"_id,omitempty" json:"id,omitempty"`
	ProjectID   bson.ObjectID `bson:"project_id"    json:"project_id"`
	Query       string        `bson:"query"         json:"query"`
	Page        string        `bson:"page"          json:"page"`
	Date        string        `bson:"date"          json:"date"` // YYYY-MM-DD (UTC)
	Device      string        `bson:"device"        json:"device"`
	Clicks      int           `bson:"clicks"        json:"clicks"`
	Impressions int           `bson:"impressions"   json:"impressions"`
	CTR         float64       `bson:"ctr"           json:"ctr"`      // 0–1
	Position    float64       `bson:"position"      json:"position"` // 1-based average position
	SyncedAt    time.Time     `bson:"synced_at"     json:"synced_at"`
}

// GSCSyncState tracks the sync progress of one project's GSC property.
// MongoDB collection: gsc_sync_state (one document per project)
type GSCSyncState struct {
	ID        bson.ObjectID `bson:"_id,omitempty"  json:"id,omitempty"`
	ProjectID bson.ObjectID `bson:"project_id"     json:"project_id"`
	// LastDate is the most recent date (YYYY-MM-DD) that has been synced.
	LastDate string `bson:"last_date"      json:"last_date"`
	// LastRunAt is when the last sync run finished (successfully or not).
	LastRunAt time.Time `bson:"last_run_at"    json:"last_run_at"`
	LastError string    `bson:"last_error,omitempty" json:"last_error,omitempty"`
	// TotalRows is the number of rows upserted in the last run.
	TotalRows int64     `bson:"total_rows"     json:"total_rows"`
	UpdatedAt time.Time `bson:"updated_at"     json:"updated_at"`
}
