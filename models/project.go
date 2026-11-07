package models

import (
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Project represents a website project being crawled
type Project struct {
	ID          bson.ObjectID `bson:"_id,omitempty"      json:"id,omitempty"`
	OwnerID     bson.ObjectID `bson:"owner_id"           json:"owner_id"`
	Name        string        `bson:"name"               json:"name"               binding:"required"`
	URL         string        `bson:"url"                json:"url"                binding:"required"`
	Description string        `bson:"description"        json:"description"`
	Active      bool          `bson:"active"             json:"active"`
	UseJS       bool          `bson:"use_js"             json:"use_js"`
	MaxPages    int           `bson:"max_pages"          json:"max_pages"`
	// Google Search Console integration (optional).
	// GSCPropertyURL: "sc-domain:example.com" or "https://example.com/"
	GSCPropertyURL string `bson:"gsc_property_url" json:"gsc_property_url"`
	// GSCServiceAccountJSON: full Google service account JSON key as a string.
	GSCServiceAccountJSON string `bson:"gsc_service_account_json" json:"gsc_service_account_json"`
	// Site adapter used to push approved changes to the target site.
	// AdapterType: e.g. "cipomarket" (empty = no adapter configured).
	AdapterType string `bson:"adapter_type"     json:"adapter_type"`
	// AdapterEndpoint / AdapterAPIKey configure the adapter's target API.
	AdapterEndpoint string `bson:"adapter_endpoint" json:"adapter_endpoint"`
	AdapterAPIKey   string `bson:"adapter_api_key"  json:"adapter_api_key"`
	// GA4PropertyID is the numeric GA4 property ID (e.g. "123456789"); uses the
	// same service account as GSC with the analytics.readonly scope.
	GA4PropertyID string `bson:"ga4_property_id"  json:"ga4_property_id"`
	// Language is the site's language code (e.g. "en"). Empty means it is read
	// from each page's <html lang>.
	Language string `bson:"language"  json:"language"`
	// Currency labels GA4 revenue (e.g. "EUR"). Empty leaves it unlabelled.
	Currency  string    `bson:"currency"  json:"currency"`
	CreatedAt time.Time `bson:"created_at"       json:"created_at"`
	UpdatedAt time.Time `bson:"updated_at"               json:"updated_at"`
}

// ProjectUpdateInput contains the fields that can be updated on a Project
type ProjectUpdateInput struct {
	Name        string `bson:"name,omitempty"        json:"name"`
	URL         string `bson:"url,omitempty"         json:"url"`
	Description string `bson:"description,omitempty" json:"description"`
	Active      *bool  `bson:"active,omitempty"      json:"active"`
	UseJS       *bool  `bson:"use_js,omitempty"      json:"use_js"`
	MaxPages    *int   `bson:"max_pages,omitempty"   json:"max_pages"`
	// GSCPropertyURL: "sc-domain:example.com" or "https://example.com/"
	GSCPropertyURL *string `bson:"gsc_property_url,omitempty" json:"gsc_property_url"`
	// GSCServiceAccountJSON: full Google service account JSON key as a string.
	GSCServiceAccountJSON *string   `bson:"gsc_service_account_json,omitempty" json:"gsc_service_account_json"`
	AdapterType           *string   `bson:"adapter_type,omitempty"     json:"adapter_type"`
	AdapterEndpoint       *string   `bson:"adapter_endpoint,omitempty" json:"adapter_endpoint"`
	AdapterAPIKey         *string   `bson:"adapter_api_key,omitempty"  json:"adapter_api_key"`
	GA4PropertyID         *string   `bson:"ga4_property_id,omitempty"  json:"ga4_property_id"`
	Language              *string   `bson:"language,omitempty"         json:"language"`
	Currency              *string   `bson:"currency,omitempty"         json:"currency"`
	UpdatedAt             time.Time `bson:"updated_at"              json:"-"`
}

// ProjectConfig describes what the crawler should collect for a specific
// project. The API returns this on GET /projects/:id/config.
type ProjectConfig struct {
	// ProjectID is the unique identifier for the project.
	ProjectID string `json:"project_id"`
	// SeedURLs are the starting points for the crawl.
	SeedURLs []string `json:"seed_urls"`
	// UseJS enables JavaScript rendering via chromedp (required for SPAs).
	UseJS bool `json:"use_js"`
	// MaxPages caps the number of pages the crawler visits per run.
	MaxPages int `json:"max_pages"`
}
