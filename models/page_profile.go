package models

import (
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Page kinds, read from the page itself. A site needs no configuration.
const (
	PageKindHome    = "home"
	PageKindListing = "listing"
	PageKindProduct = "product"
	PageKindArticle = "article"
	PageKindOther   = "other"
)

// PageProfile is the short, long-lived summary of a page. Snapshots expire
// within a day; profiles stay, so a project's picture of the site grows run by
// run and shared patterns can be found across it.
// MongoDB collection: page_profiles (one per project × URL, TTL on expires_at)
type PageProfile struct {
	ID              bson.ObjectID `bson:"_id,omitempty"    json:"id,omitempty"`
	ProjectID       bson.ObjectID `bson:"project_id"       json:"project_id"`
	URL             string        `bson:"url"              json:"url"` // normalized
	SourceURL       string        `bson:"source_url"       json:"source_url"`
	StatusCode      int           `bson:"status_code"      json:"status_code"`
	Title           string        `bson:"title"            json:"title"`
	MetaDescription string        `bson:"meta_description" json:"meta_description"`
	H1              string        `bson:"h1"               json:"h1"`
	CategoryText    string        `bson:"category_text"    json:"category_text"` // clipped
	Canonical       string        `bson:"canonical"        json:"canonical"`
	Robots          string        `bson:"robots"           json:"robots"`
	Kind            string        `bson:"kind"             json:"kind"`
	// Group is the page type used to merge findings, e.g. "product" or
	// "other:/news". A "page:" group holds a single page and never merges.
	Group       string `bson:"group"       json:"group"`
	Unavailable bool   `bson:"unavailable" json:"unavailable"`
	FinalURL    string `bson:"final_url"   json:"final_url"`
	Redirects   int    `bson:"redirects"   json:"redirects"`
	// Noindex is set by meta robots or the X-Robots-Tag header.
	Noindex      bool     `bson:"noindex"       json:"noindex"`
	SchemaTypes  []string `bson:"schema_types"  json:"schema_types"`
	H1Count      int      `bson:"h1_count"      json:"h1_count"`
	ContentWords int      `bson:"content_words" json:"content_words"`
	RenderMs     int64    `bson:"render_ms"     json:"render_ms"`
	Links        []string `bson:"links"         json:"-"`

	// Set by the site crawl from the link graph, the URL inventory and search
	// data; a page read alone keeps the previous values.
	InSitemap     bool        `bson:"in_sitemap"     json:"in_sitemap"`
	Inbound       int         `bson:"inbound"        json:"inbound"`
	Depth         int         `bson:"depth"          json:"depth"` // clicks from the home page, -1 when not reached
	BrokenLinks   int         `bson:"broken_links"   json:"broken_links"`
	RedirectLinks int         `bson:"redirect_links" json:"redirect_links"`
	Impressions   float64     `bson:"impressions"    json:"impressions"`
	Clicks        float64     `bson:"clicks"         json:"clicks"`
	Issues        []PageIssue `bson:"issues"         json:"issues"`

	// Version is the profile format. An older profile lacks fields the audit
	// reads, so it is not audited until the page is read again.
	Version   int       `bson:"version" json:"version"`
	FetchedAt time.Time `bson:"fetched_at"  json:"fetched_at"`
	ExpiresAt time.Time `bson:"expires_at"  json:"expires_at"`
}
