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
	SuggestionKindH1           = "h1"
	SuggestionKindInternalLink = "internal_link"
	SuggestionKindCanonical    = "canonical"
	SuggestionKindRedirect     = "redirect_merge"
	SuggestionKindNoindex      = "noindex"
	SuggestionKindContentGap   = "content_gap"
	SuggestionKindTechnical    = "technical"
	SuggestionKindTemplateFix  = "template_fix"
)

// IsApplyableKind reports whether a suggestion kind can be pushed to the site
// through an adapter. All other kinds are manual tasks.
func IsApplyableKind(kind string) bool {
	switch kind {
	case SuggestionKindTitle, SuggestionKindMeta, SuggestionKindCategoryText:
		return true
	}
	return false
}

// IsSuggestionKind reports whether kind is a known suggestion kind.
func IsSuggestionKind(kind string) bool {
	switch kind {
	case SuggestionKindH1, SuggestionKindInternalLink, SuggestionKindCanonical,
		SuggestionKindRedirect, SuggestionKindNoindex, SuggestionKindContentGap,
		SuggestionKindTechnical, SuggestionKindTemplateFix:
		return true
	}
	return IsApplyableKind(kind)
}

// Suggestion is one AI-generated fix proposal for a page, awaiting review.
// MongoDB collection: suggestions
type Suggestion struct {
	ID            bson.ObjectID  `bson:"_id,omitempty"    json:"id,omitempty"`
	ProjectID     bson.ObjectID  `bson:"project_id"       json:"project_id"`
	OpportunityID *bson.ObjectID `bson:"opportunity_id,omitempty" json:"opportunity_id,omitempty"`
	// AnalysisID links the suggestion to the page analysis that produced it.
	// Empty on suggestions created before page analyses existed.
	AnalysisID *bson.ObjectID `bson:"analysis_id,omitempty" json:"analysis_id,omitempty"`
	Kind       string         `bson:"kind"             json:"kind"` // see the SuggestionKind constants
	// Target names what a manual action touches, e.g. the page to redirect to.
	Target   string `bson:"target,omitempty"   json:"target,omitempty"`
	Priority int    `bson:"priority,omitempty" json:"priority,omitempty"` // 1 = most important
	Effort   string `bson:"effort,omitempty"   json:"effort,omitempty"`   // S | M | L
	// EvidenceRefs are the context-pack facts the action is based on.
	EvidenceRefs  []string `bson:"evidence_refs,omitempty" json:"evidence_refs,omitempty"`
	Page          string   `bson:"page"             json:"page"`
	Query         string   `bson:"query"            json:"query"`
	Language      string   `bson:"language"         json:"language"` // e.g. "hu"
	CurrentValue  string   `bson:"current_value"    json:"current_value"`
	ProposedValue string   `bson:"proposed_value"   json:"proposed_value"`
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
