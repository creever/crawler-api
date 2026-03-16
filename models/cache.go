package models

import (
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// SEOInfo holds extracted SEO metadata embedded inside a cached page entry.
type SEOInfo struct {
	Title           string   `bson:"title"            json:"title"`
	MetaDescription string   `bson:"meta_description" json:"meta_description"`
	H1Tags          []string `bson:"h1_tags"          json:"h1_tags"`
	H2Tags          []string `bson:"h2_tags"          json:"h2_tags"`
	CanonicalURL    string   `bson:"canonical_url"    json:"canonical_url"`
	MetaRobots      string   `bson:"meta_robots"      json:"meta_robots"`
	OGTitle         string   `bson:"og_title"         json:"og_title"`
	OGDescription   string   `bson:"og_description"   json:"og_description"`
	OGImage         string   `bson:"og_image"         json:"og_image"`
	SchemaTypes     []string `bson:"schema_types"     json:"schema_types"`
	WordCount       int      `bson:"word_count"       json:"word_count"`
	InternalLinks   int      `bson:"internal_links"   json:"internal_links"`
	ExternalLinks   int      `bson:"external_links"   json:"external_links"`
}

// CacheEntry represents a prerendered and cached page, combining the full
// HTML output with extracted SEO information.
type CacheEntry struct {
	ID        bson.ObjectID `bson:"_id,omitempty" json:"id,omitempty"`
	ProjectID bson.ObjectID `bson:"project_id"    json:"project_id"    binding:"required"`
	URL       string        `bson:"url"           json:"url"           binding:"required"`
	FullHTML  string        `bson:"full_html"     json:"full_html"`
	SEO       SEOInfo       `bson:"seo"           json:"seo"`
	CachedAt  time.Time     `bson:"cached_at"     json:"cached_at"`
	ExpiresAt time.Time     `bson:"expires_at"    json:"expires_at"`
	UpdatedAt time.Time     `bson:"updated_at"    json:"updated_at"`
}

// CacheEntryUpdateInput holds the fields that can be refreshed on an existing
// cache entry (HTML content, SEO data, and expiry).
type CacheEntryUpdateInput struct {
	FullHTML  string    `bson:"full_html,omitempty"  json:"full_html"`
	SEO       *SEOInfo  `bson:"seo,omitempty"        json:"seo"`
	ExpiresAt time.Time `bson:"expires_at,omitempty" json:"expires_at"`
	UpdatedAt time.Time `bson:"updated_at"           json:"-"`
}
