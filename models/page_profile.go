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
	Group       string    `bson:"group"       json:"group"`
	Unavailable bool      `bson:"unavailable" json:"unavailable"`
	FetchedAt   time.Time `bson:"fetched_at"  json:"fetched_at"`
	ExpiresAt   time.Time `bson:"expires_at"  json:"expires_at"`
}
