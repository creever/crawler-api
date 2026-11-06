package models

import (
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// SuggestionStatus is the review lifecycle state of an AI suggestion.
type SuggestionStatus string

const (
	SuggestionStatusPending  SuggestionStatus = "pending"
	SuggestionStatusApproved SuggestionStatus = "approved"
	SuggestionStatusRejected SuggestionStatus = "rejected"
	SuggestionStatusEdited   SuggestionStatus = "edited" // approved with manual edits
)

// Suggestion kinds.
const (
	SuggestionKindTitle        = "title"
	SuggestionKindMeta         = "meta_description"
	SuggestionKindCategoryText = "category_text"
)

// Suggestion is one AI-generated fix proposal for a page, awaiting review.
// MongoDB collection: suggestions
type Suggestion struct {
	ID            bson.ObjectID  `bson:"_id,omitempty"    json:"id,omitempty"`
	ProjectID     bson.ObjectID  `bson:"project_id"       json:"project_id"`
	OpportunityID *bson.ObjectID `bson:"opportunity_id,omitempty" json:"opportunity_id,omitempty"`
	Kind          string         `bson:"kind"             json:"kind"` // title | meta_description | category_text
	Page          string         `bson:"page"             json:"page"`
	Query         string         `bson:"query"            json:"query"`
	Language      string         `bson:"language"         json:"language"` // e.g. "hu"
	CurrentValue  string         `bson:"current_value"    json:"current_value"`
	ProposedValue string         `bson:"proposed_value"   json:"proposed_value"`
	// FinalValue is what gets applied: ProposedValue on approve, or the
	// edited text when the reviewer modified it (status "edited").
	FinalValue string           `bson:"final_value"     json:"final_value"`
	Status     SuggestionStatus `bson:"status"           json:"status"`
	Reason     string           `bson:"reason"           json:"reason"`
	// Prompt and RawResponse are stored for auditability.
	Prompt      string         `bson:"prompt"           json:"prompt"`
	RawResponse string         `bson:"raw_response"     json:"raw_response"`
	ReviewedBy  *bson.ObjectID `bson:"reviewed_by,omitempty" json:"reviewed_by,omitempty"`
	// AppliedAt is set when the suggestion was pushed to the site via an adapter.
	AppliedAt *time.Time `bson:"applied_at,omitempty" json:"applied_at,omitempty"`
	CreatedAt time.Time  `bson:"created_at"       json:"created_at"`
	UpdatedAt time.Time  `bson:"updated_at"       json:"updated_at"`
}
