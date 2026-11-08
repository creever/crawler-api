package models

import (
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// RequesterType classifies the entity that made the prerender request.
type RequesterType string

const (
	RequesterTypeHuman RequesterType = "human"
	RequesterTypeBot   RequesterType = "bot"
	RequesterTypeAI    RequesterType = "ai"
)

// How a prerender request was answered.
const (
	CacheStateHit   = "hit"   // a fresh render from the cache
	CacheStateStale = "stale" // an old render, refreshed in the background
	CacheStateMiss  = "miss"  // rendered while the caller waited
	CacheStateError = "error" // no render could be made
)

// PrerenderData is one request to /prerender.
// MongoDB collection: prerender_data (kept for 90 days)
type PrerenderData struct {
	ID         bson.ObjectID `bson:"_id,omitempty"    json:"id,omitempty"`
	ProjectID  bson.ObjectID `bson:"project_id"       json:"project_id"`
	URL        string        `bson:"url"              json:"url"`
	// Key is the normalized URL: the spellings of a page count as one.
	Key        string        `bson:"key"              json:"key"`
	StatusCode int           `bson:"status_code"      json:"status_code"`
	// RenderTimeMs is how long the caller waited for the answer.
	RenderTimeMs  int64         `bson:"render_time_ms"   json:"render_time_ms"`
	FromCache     bool          `bson:"from_cache"       json:"from_cache"`
	Cache         string        `bson:"cache"            json:"cache"`
	UserAgent     string        `bson:"user_agent"       json:"user_agent"`
	RequesterType RequesterType `bson:"requester_type"   json:"requester_type"`
	// Agent names the crawler, e.g. Googlebot or GPTBot; empty for others.
	Agent    string    `bson:"agent"     json:"agent"`
	CachedAt time.Time `bson:"cached_at" json:"cached_at"` // when the request came
}

// PrerenderListResponse is a page of the request log.
type PrerenderListResponse struct {
	Total  int64           `json:"total"`
	Limit  int64           `json:"limit"`
	Offset int64           `json:"offset"`
	Data   []PrerenderData `json:"data"`
}
