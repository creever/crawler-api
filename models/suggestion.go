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
	// SuggestionStatusApplyOnSite marks a decided suggestion for the site. It
	// changes nothing by itself: a task can be created from it later.
	SuggestionStatusApplyOnSite SuggestionStatus = "apply_on_site"
	// SuggestionStatusCovered means a task for several pages holds this
	// suggestion, so it left the review queue.
	SuggestionStatusCovered SuggestionStatus = "covered"
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

// IsApplyableKind reports whether a suggestion kind is complete publishable
// text. Other kinds describe a change and need someone to work it out.
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
	// Issue, Scope and PageGroup decide which findings merge into one task.
	Issue     string `bson:"issue,omitempty"      json:"issue,omitempty"`
	Scope     string `bson:"scope,omitempty"      json:"scope,omitempty"` // page | page_type
	PageGroup string `bson:"page_group,omitempty" json:"page_group,omitempty"`
	// TaskID is the task holding this suggestion, if any.
	TaskID *bson.ObjectID `bson:"task_id,omitempty" json:"task_id,omitempty"`
	Effort string         `bson:"effort,omitempty"   json:"effort,omitempty"` // S | M | L
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
	// AppliedAt is set on suggestions pushed through the former site adapters.
	AppliedAt *time.Time `bson:"applied_at,omitempty" json:"applied_at,omitempty"`
	CreatedAt time.Time  `bson:"created_at"       json:"created_at"`
	UpdatedAt time.Time  `bson:"updated_at"       json:"updated_at"`
}
