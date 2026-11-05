package models

import (
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// OpportunityType identifies the signal that produced an opportunity.
type OpportunityType string

const (
	OppStrikingDistance OpportunityType = "striking_distance" // pos 5–20, high impressions
	OppLowCTR           OpportunityType = "low_ctr"           // CTR well below expected for position
	OppLosingClicks     OpportunityType = "losing_clicks"     // clicks down 28d vs previous 28d
	OppCannibalization  OpportunityType = "cannibalization"   // 2+ pages ranking for the same query
	OppCrawlIssue       OpportunityType = "crawl_issue"       // crawl problem on a page with traffic
)

// Opportunity is one ranked organic-search opportunity for a project.
// MongoDB collection: opportunities (recomputed daily per project)
type Opportunity struct {
	ID        bson.ObjectID   `bson:"_id,omitempty" json:"id,omitempty"`
	ProjectID bson.ObjectID   `bson:"project_id"    json:"project_id"`
	Type      OpportunityType `bson:"type"        json:"type"`
	Query     string          `bson:"query"         json:"query"`
	Page      string          `bson:"page"          json:"page"`
	Score     float64         `bson:"score"         json:"score"`
	Rank      int             `bson:"rank"          json:"rank"`
	// Metrics carries the raw numbers behind the score (impressions, clicks,
	// ctr, position, prev_clicks, pages, ...).
	Metrics map[string]float64 `bson:"metrics"   json:"metrics"`
	// Details is a human-readable explanation (e.g. the crawl issue found).
	Details    string    `bson:"details"       json:"details"`
	ComputedAt time.Time `bson:"computed_at"   json:"computed_at"`
}
