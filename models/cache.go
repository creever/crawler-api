package models

import (
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Where a cached render came from.
const (
	CacheSourceBot    = "bot"    // a bot asked for a page not in the cache
	CacheSourceStale  = "stale"  // a bot was served an old render, which was then refreshed
	CacheSourceCrawl  = "crawl"  // the site crawl rendered the page with JavaScript anyway
	CacheSourceManual = "manual" // a person asked for a refresh
)

// CacheEntry is the prerendered HTML of one page, served to bots by
// /prerender. MongoDB collection: cache_entries, one per (project_id, key).
type CacheEntry struct {
	ID        bson.ObjectID `bson:"_id,omitempty" json:"id,omitempty"`
	ProjectID bson.ObjectID `bson:"project_id"    json:"project_id"`
	// Key is the normalized URL: the http/https, www and trailing-slash
	// spellings of a page share one entry.
	Key      string `bson:"key"       json:"key"`
	URL      string `bson:"url"       json:"url"`
	FullHTML string `bson:"full_html" json:"full_html,omitempty"`
	// StatusCode is the page's own HTTP status, sent to the caller in the
	// X-Prerender-Status header.
	StatusCode int    `bson:"status_code" json:"status_code"`
	FinalURL   string `bson:"final_url"   json:"final_url"`
	Redirects  int    `bson:"redirects"   json:"redirects"`
	RenderMs   int64  `bson:"render_ms"   json:"render_ms"`
	HTMLBytes  int    `bson:"html_bytes"  json:"html_bytes"`
	Source     string `bson:"source"      json:"source"`
	// Hits counts the bot requests served from this entry.
	Hits         int64      `bson:"hits"                     json:"hits"`
	LastServedAt *time.Time `bson:"last_served_at,omitempty" json:"last_served_at,omitempty"`
	CachedAt     time.Time  `bson:"cached_at"                json:"cached_at"`
	// ExpiresAt is when the render stops being fresh. A stale render is
	// still served while a new one is made, until PurgeAt removes it.
	ExpiresAt time.Time `bson:"expires_at" json:"expires_at"`
	PurgeAt   time.Time `bson:"purge_at"   json:"purge_at"`
}
