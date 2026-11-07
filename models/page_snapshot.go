package models

import (
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Heading roles: only "content" headings describe what the page is about.
const (
	HeadingRoleContent = "content"
	HeadingRoleFilter  = "filter"
	HeadingRoleNav     = "nav"
)

// PageHeading is one H1-H3 of a page with the region it was found in.
type PageHeading struct {
	Level int    `bson:"level" json:"level"`
	Text  string `bson:"text"  json:"text"`
	Role  string `bson:"role"  json:"role"`
}

// PageLink is one link found in the page's content regions.
type PageLink struct {
	Href   string `bson:"href"   json:"href"`
	Anchor string `bson:"anchor" json:"anchor"`
}

// ListingSummary describes the repeated product/item cards of a listing page.
// ItemCount covers the rendered page only; PageCount tells how many pages the
// listing has when it is paginated.
type ListingSummary struct {
	ItemCount   int      `bson:"item_count"   json:"item_count"`
	PageCount   int      `bson:"page_count"   json:"page_count"`
	PriceMin    float64  `bson:"price_min"    json:"price_min"`
	PriceMax    float64  `bson:"price_max"    json:"price_max"`
	Currency    string   `bson:"currency"     json:"currency"`
	Brands      []string `bson:"brands"       json:"brands"`
	SampleItems []string `bson:"sample_items" json:"sample_items"`
}

// SnapshotCoverage reports how the page's visible text was split into regions.
type SnapshotCoverage struct {
	TotalChars   int `bson:"total_chars"   json:"total_chars"`
	ProseChars   int `bson:"prose_chars"   json:"prose_chars"`
	ListingChars int `bson:"listing_chars" json:"listing_chars"`
	ChromeChars  int `bson:"chrome_chars"  json:"chrome_chars"` // nav, filters, footer
}

// PageSnapshot is the structured content of one rendered page. Raw HTML is
// deliberately not stored.
// MongoDB collection: page_snapshots (one per project × URL, TTL on expires_at)
type PageSnapshot struct {
	ID              bson.ObjectID `bson:"_id,omitempty"    json:"id,omitempty"`
	ProjectID       bson.ObjectID `bson:"project_id"       json:"project_id"`
	URL             string        `bson:"url"              json:"url"` // normalized
	SourceURL       string        `bson:"source_url"       json:"source_url"`
	StatusCode      int           `bson:"status_code"      json:"status_code"`
	Title           string        `bson:"title"            json:"title"`
	MetaDescription string        `bson:"meta_description" json:"meta_description"`
	Canonical       string        `bson:"canonical"        json:"canonical"`
	Robots          string        `bson:"robots"           json:"robots"`
	Hreflang        []string      `bson:"hreflang"         json:"hreflang"`
	OGTitle         string        `bson:"og_title"         json:"og_title"`
	OGDescription   string        `bson:"og_description"   json:"og_description"`
	SchemaTypes     []string      `bson:"schema_types"     json:"schema_types"`
	Headings        []PageHeading `bson:"headings"         json:"headings"`
	Prose           []string      `bson:"prose"            json:"prose"`
	// CategoryText is the existing descriptive text of a listing page. It is
	// also the current value of a category_text suggestion.
	CategoryText     string           `bson:"category_text"      json:"category_text"`
	Listing          *ListingSummary  `bson:"listing,omitempty"  json:"listing,omitempty"`
	ContentLinks     []PageLink       `bson:"content_links"      json:"content_links"`
	ImagesTotal      int              `bson:"images_total"       json:"images_total"`
	ImagesMissingAlt int              `bson:"images_missing_alt" json:"images_missing_alt"`
	ContentWordCount int              `bson:"content_word_count" json:"content_word_count"`
	TotalWordCount   int              `bson:"total_word_count"   json:"total_word_count"`
	LowConfidence    bool             `bson:"low_confidence"     json:"low_confidence"`
	Coverage         SnapshotCoverage `bson:"coverage"           json:"coverage"`
	// Fingerprint changes when the SEO-relevant content of the page changes.
	Fingerprint string `bson:"fingerprint" json:"fingerprint"`
	// Version is the extractor version that produced this snapshot.
	Version   int       `bson:"version"     json:"version"`
	FetchedAt time.Time `bson:"fetched_at"  json:"fetched_at"`
	ExpiresAt time.Time `bson:"expires_at"  json:"expires_at"`
}
