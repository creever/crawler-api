package models

import (
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// AnalysisStatus is the outcome of one page analysis.
type AnalysisStatus string

const (
	AnalysisStatusCompleted           AnalysisStatus = "completed"
	AnalysisStatusNoAction            AnalysisStatus = "no_action"
	AnalysisStatusInsufficientContext AnalysisStatus = "insufficient_context"
	AnalysisStatusFailed              AnalysisStatus = "failed"
)

// AnalysisEvidence is one claim backed by a fact of the context pack.
type AnalysisEvidence struct {
	Ref   string `bson:"ref"   json:"ref"`
	Claim string `bson:"claim" json:"claim"`
	// Fact is the source fact as it was sent to the model, copied here so a
	// reviewer can check the claim against it.
	Fact string `bson:"fact" json:"fact"`
}

// AnalysisCoverage records what the model actually saw.
type AnalysisCoverage struct {
	PagesRead    []string  `bson:"pages_read"    json:"pages_read"`
	SnapshotAt   time.Time `bson:"snapshot_at"   json:"snapshot_at"`
	Sections     []string  `bson:"sections"      json:"sections"`
	Truncated    []string  `bson:"truncated"     json:"truncated"`
	CharsSent    int       `bson:"chars_sent"    json:"chars_sent"`
	ContentWords int       `bson:"content_words" json:"content_words"`
	GSCQueries   int       `bson:"gsc_queries"   json:"gsc_queries"`
	HasGA4       bool      `bson:"has_ga4"       json:"has_ga4"`
}

// PageAnalysis is the diagnosis of one page, covering all of its opportunity
// signals. Its actions are stored as suggestions with AnalysisID set.
// MongoDB collection: page_analyses
type PageAnalysis struct {
	ID             bson.ObjectID     `bson:"_id,omitempty"   json:"id,omitempty"`
	ProjectID      bson.ObjectID     `bson:"project_id"      json:"project_id"`
	Page           string            `bson:"page"            json:"page"`
	OpportunityIDs []bson.ObjectID   `bson:"opportunity_ids" json:"opportunity_ids"`
	Signals        []OpportunityType `bson:"signals"         json:"signals"`
	Status         AnalysisStatus    `bson:"status"          json:"status"`
	// StatusReason explains a failed or insufficient_context analysis.
	StatusReason       string             `bson:"status_reason"       json:"status_reason"`
	PrimaryIssue       string             `bson:"primary_issue"       json:"primary_issue"`
	RootCause          string             `bson:"root_cause"          json:"root_cause"`
	Confidence         string             `bson:"confidence"          json:"confidence"` // high | medium | low
	Evidence           []AnalysisEvidence `bson:"evidence"            json:"evidence"`
	RuledOut           []string           `bson:"ruled_out"           json:"ruled_out"`
	NoActionReason     string             `bson:"no_action_reason"    json:"no_action_reason"`
	MissingData        []string           `bson:"missing_data"        json:"missing_data"`
	ValidationWarnings []string           `bson:"validation_warnings" json:"validation_warnings"`
	Coverage           AnalysisCoverage   `bson:"coverage"            json:"coverage"`
	Provider           string             `bson:"provider"            json:"provider"`
	Model              string             `bson:"model"               json:"model"`
	Fingerprint        string             `bson:"fingerprint"         json:"fingerprint"`
	// Prompt and RawResponse are stored for auditability.
	Prompt      string    `bson:"prompt"       json:"prompt"`
	RawResponse string    `bson:"raw_response" json:"raw_response"`
	CreatedAt   time.Time `bson:"created_at"   json:"created_at"`
}

// SuggestionRun records when a project's bulk analysis last started.
// MongoDB collection: suggestion_runs (one document per project)
type SuggestionRun struct {
	ProjectID bson.ObjectID `bson:"project_id" json:"project_id"`
	StartedAt time.Time     `bson:"started_at" json:"started_at"`
}
