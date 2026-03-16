package models

import (
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// SEOData holds SEO metrics collected by the bot for a specific URL
type SEOData struct {
	ID              bson.ObjectID `bson:"_id,omitempty"       json:"id,omitempty"`
	ProjectID       bson.ObjectID `bson:"project_id"          json:"project_id"          binding:"required"`
	URL             string             `bson:"url"                 json:"url"                 binding:"required"`
	StatusCode      int                `bson:"status_code"         json:"status_code"`
	Title           string             `bson:"title"               json:"title"`
	MetaDescription string             `bson:"meta_description"    json:"meta_description"`
	H1Tags          []string           `bson:"h1_tags"             json:"h1_tags"`
	H2Tags          []string           `bson:"h2_tags"             json:"h2_tags"`
	CanonicalURL    string             `bson:"canonical_url"       json:"canonical_url"`
	MetaRobots      string             `bson:"meta_robots"         json:"meta_robots"`
	OGTitle         string             `bson:"og_title"            json:"og_title"`
	OGDescription   string             `bson:"og_description"      json:"og_description"`
	OGImage         string             `bson:"og_image"            json:"og_image"`
	SchemaTypes     []string           `bson:"schema_types"        json:"schema_types"`
	WordCount       int                `bson:"word_count"          json:"word_count"`
	InternalLinks   int                `bson:"internal_links"      json:"internal_links"`
	ExternalLinks   int                `bson:"external_links"      json:"external_links"`
	LoadTimeMs      int64              `bson:"load_time_ms"        json:"load_time_ms"`
	CrawledAt       time.Time          `bson:"crawled_at"          json:"crawled_at"`
}

// SEOSummary is a lightweight summary used by the dashboard
type SEOSummary struct {
	TotalPages   int64   `json:"total_pages"`
	AvgLoadTime  float64 `json:"avg_load_time_ms"`
	PagesWithH1  int64   `json:"pages_with_h1"`
	Pages4xx     int64   `json:"pages_4xx"`
	Pages5xx     int64   `json:"pages_5xx"`
}
