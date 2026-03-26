package models

import (
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// SEOData holds SEO metrics collected by the bot for a specific URL
type SEOData struct {
	ID               bson.ObjectID `bson:"_id,omitempty"        json:"id,omitempty"`
	ProjectID        bson.ObjectID `bson:"project_id"           json:"project_id"           binding:"required"`
	URL              string        `bson:"url"                  json:"url"                  binding:"required"`
	StatusCode       int           `bson:"status_code"          json:"status_code"`
	Title            string        `bson:"title"                json:"title"`
	MetaDescription  string        `bson:"meta_description"     json:"meta_description"`
	H1Tags           []string      `bson:"h1_tags"              json:"h1_tags"`
	H2Tags           []string      `bson:"h2_tags"              json:"h2_tags"`
	CanonicalURL     string        `bson:"canonical_url"        json:"canonical_url"`
	MetaRobots       string        `bson:"meta_robots"          json:"meta_robots"`
	OGTitle          string        `bson:"og_title"             json:"og_title"`
	OGDescription    string        `bson:"og_description"       json:"og_description"`
	OGImage          string        `bson:"og_image"             json:"og_image"`
	SchemaTypes      []string      `bson:"schema_types"         json:"schema_types"`
	WordCount        int           `bson:"word_count"           json:"word_count"`
	InternalLinks    int           `bson:"internal_links"       json:"internal_links"`
	ExternalLinks    int           `bson:"external_links"       json:"external_links"`
	ImagesWithoutAlt int           `bson:"images_without_alt"   json:"images_without_alt"`
	LoadTimeMs       int64         `bson:"load_time_ms"         json:"load_time_ms"`
	CrawledAt        time.Time     `bson:"crawled_at"           json:"crawled_at"`
}

// SEOSummary is a lightweight summary used by the dashboard
type SEOSummary struct {
	TotalPages  int64   `json:"total_pages"`
	AvgLoadTime float64 `json:"avg_load_time_ms"`
	PagesWithH1 int64   `json:"pages_with_h1"`
	Pages4xx    int64   `json:"pages_4xx"`
	Pages5xx    int64   `json:"pages_5xx"`
}

// ProjectSeoIssueSummary holds counts of common SEO issues found across a project's pages.
type ProjectSeoIssueSummary struct {
	MissingTitle       int `json:"missing_title"`
	MissingDescription int `json:"missing_description"`
	MissingH1          int `json:"missing_h1"`
	MissingCanonical   int `json:"missing_canonical"`
	ImagesWithoutAlt   int `json:"images_without_alt"`
	PagesWithErrors    int `json:"pages_with_errors"`
}

// ProjectSeoTopPage is a lightweight summary of a single page used in the top-pages list.
type ProjectSeoTopPage struct {
	URL           string  `json:"url"`
	Title         *string `json:"title"`
	WordCount     int     `json:"word_count"`
	InternalLinks int     `json:"internal_links"`
	LoadTimeMs    int64   `json:"load_time_ms"`
}

// ProjectSeoSummary is the per-project SEO analytics summary returned by
// GET /api/v1/projects/:id/seo-summary.
type ProjectSeoSummary struct {
	ProjectID         string                 `json:"project_id"`
	TotalPagesAnalyzed int                   `json:"total_pages_analyzed"`
	AvgWordCount      float64                `json:"avg_word_count"`
	AvgInternalLinks  float64                `json:"avg_internal_links"`
	AvgExternalLinks  float64                `json:"avg_external_links"`
	AvgLoadTimeMs     float64                `json:"avg_load_time_ms"`
	Issues            ProjectSeoIssueSummary `json:"issues"`
	TopPages          []ProjectSeoTopPage    `json:"top_pages"`
}
