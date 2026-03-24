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

// PrerenderData holds information about a prerendered (JavaScript-rendered) URL
type PrerenderData struct {
	ID            bson.ObjectID `bson:"_id,omitempty"    json:"id,omitempty"`
	ProjectID     bson.ObjectID `bson:"project_id"       json:"project_id"       binding:"required"`
	URL           string        `bson:"url"              json:"url"              binding:"required"`
	StatusCode    int           `bson:"status_code"      json:"status_code"`
	RenderedHTML  string        `bson:"rendered_html"    json:"rendered_html"`
	RenderTimeMs  int64         `bson:"render_time_ms"   json:"render_time_ms"`
	FromCache     bool          `bson:"from_cache"       json:"from_cache"`
	UserAgent     string        `bson:"user_agent"       json:"user_agent"`
	RequesterType RequesterType `bson:"requester_type"   json:"requester_type"`
	CachedAt      time.Time     `bson:"cached_at"        json:"cached_at"`
	ExpiresAt     time.Time     `bson:"expires_at"       json:"expires_at"`
}

// PrerenderSummary is a lightweight summary used by the dashboard
type PrerenderSummary struct {
	TotalRequests int64   `json:"total_requests"`
	CacheHits     int64   `json:"cache_hits"`
	CacheMisses   int64   `json:"cache_misses"`
	HitRate       float64 `json:"hit_rate_percent"`
	AvgRenderMs   float64 `json:"avg_render_time_ms"`
}
