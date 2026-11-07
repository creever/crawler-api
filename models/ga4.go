package models

import (
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// GA4Row is one landing-page × date row from the GA4 Data API.
// MongoDB collection: ga4_rows
type GA4Row struct {
	ID          bson.ObjectID `bson:"_id,omitempty" json:"id,omitempty"`
	ProjectID   bson.ObjectID `bson:"project_id"   json:"project_id"`
	Page        string        `bson:"page"         json:"page"` // landing page path
	Date        string        `bson:"date"         json:"date"` // YYYYMMDD, as the GA4 API returns it
	Sessions    int           `bson:"sessions"     json:"sessions"`
	Conversions float64       `bson:"conversions"  json:"conversions"`
	Revenue     float64       `bson:"revenue"      json:"revenue"` // in the GA4 property's currency
	SyncedAt    time.Time     `bson:"synced_at"    json:"synced_at"`
}

// GA4SyncState tracks the sync progress of one project's GA4 property.
// MongoDB collection: ga4_sync_state (one document per project)
type GA4SyncState struct {
	ID        bson.ObjectID `bson:"_id,omitempty"  json:"id,omitempty"`
	ProjectID bson.ObjectID `bson:"project_id"     json:"project_id"`
	LastDate  string        `bson:"last_date"      json:"last_date"`
	LastRunAt time.Time     `bson:"last_run_at"    json:"last_run_at"`
	LastError string        `bson:"last_error,omitempty" json:"last_error,omitempty"`
	TotalRows int64         `bson:"total_rows"     json:"total_rows"`
	UpdatedAt time.Time     `bson:"updated_at"     json:"updated_at"`
}
