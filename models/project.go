package models

import (
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Project represents a website project being crawled
type Project struct {
	ID          bson.ObjectID `bson:"_id,omitempty"      json:"id,omitempty"`
	Name        string        `bson:"name"               json:"name"               binding:"required"`
	URL         string        `bson:"url"                json:"url"                binding:"required"`
	Description string        `bson:"description"        json:"description"`
	Active      bool          `bson:"active"             json:"active"`
	UseJS       bool          `bson:"use_js"             json:"use_js"`
	MaxPages    int           `bson:"max_pages"          json:"max_pages"`
	CreatedAt   time.Time     `bson:"created_at"         json:"created_at"`
	UpdatedAt   time.Time     `bson:"updated_at"         json:"updated_at"`
}

// ProjectUpdateInput contains the fields that can be updated on a Project
type ProjectUpdateInput struct {
	Name        string    `bson:"name,omitempty"        json:"name"`
	URL         string    `bson:"url,omitempty"         json:"url"`
	Description string    `bson:"description,omitempty" json:"description"`
	Active      *bool     `bson:"active,omitempty"      json:"active"`
	UseJS       *bool     `bson:"use_js,omitempty"      json:"use_js"`
	MaxPages    *int      `bson:"max_pages,omitempty"   json:"max_pages"`
	UpdatedAt   time.Time `bson:"updated_at"            json:"-"`
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
