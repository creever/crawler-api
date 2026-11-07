package models

import (
	"fmt"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// TaskStatus is the lifecycle state of a task.
type TaskStatus string

const (
	TaskStatusOpen      TaskStatus = "open"
	TaskStatusClaimed   TaskStatus = "claimed"   // a harness is working on it
	TaskStatusDone      TaskStatus = "done"      // the harness reports it deployed
	TaskStatusVerified  TaskStatus = "verified"  // a recrawl confirmed the fix
	TaskStatusReopened  TaskStatus = "reopened"  // a recrawl found it unfixed
	TaskStatusResolved  TaskStatus = "resolved"  // the pattern disappeared by itself
	TaskStatusDismissed TaskStatus = "dismissed" // a person decided not to do it
)

// Task scopes.
const (
	TaskScopePage  = "page"
	TaskScopeGroup = "page_group"
	TaskScopeSite  = "site"
)

// Task sources.
const (
	TaskSourceDetector = "detector" // pattern found in code across page profiles
	TaskSourceAnalysis = "analysis" // merged from several page analyses
	TaskSourceReview   = "review"   // created from a suggestion marked Apply on site
)

// Issue codes. The model picks one per action, the detectors use the same list,
// and findings merge on them.
const (
	IssueTemplatedTitle = "templated_title"
	IssueTemplatedMeta  = "templated_meta"
	IssueDuplicateTitle = "duplicate_title"
	IssueDuplicateMeta  = "duplicate_meta"
	IssueDuplicateText  = "duplicate_text"
	IssueMissingTitle   = "missing_title"
	IssueMissingMeta    = "missing_meta"
	IssueMissingH1      = "missing_h1"
	IssueTitleTooLong   = "title_too_long"
	IssueMetaTooLong    = "meta_too_long"
	IssueWeakHeading    = "weak_heading"
	IssueThinContent    = "thin_content"
	IssueUnavailable    = "unavailable_pages"
	IssueInternalLinks  = "internal_linking"
	IssueStructuredData = "structured_data"
	IssueHTTPError      = "http_error"
	IssueTechnical      = "technical"
	IssueIntentMismatch = "intent_mismatch"
	IssueCannibal       = "cannibalization"
	IssueOther          = "other"
)

// ModelIssues are the codes offered to the model.
var ModelIssues = []string{
	IssueTemplatedTitle, IssueTemplatedMeta, IssueDuplicateText, IssueWeakHeading, IssueThinContent,
	IssueUnavailable, IssueInternalLinks, IssueStructuredData, IssueTechnical,
	IssueIntentMismatch, IssueCannibal, IssueOther,
}

// IsModelIssue reports whether code is an issue the model may use.
func IsModelIssue(code string) bool {
	for _, c := range ModelIssues {
		if c == code {
			return true
		}
	}
	return false
}

// IsPageSpecificIssue reports whether an issue belongs to one page even when
// several pages have it, so it never merges into a group task.
func IsPageSpecificIssue(code string) bool {
	return code == "" || code == IssueIntentMismatch || code == IssueCannibal || code == IssueOther
}

// Check types a recrawl can evaluate from outside.
const (
	CheckNotContains = "not_contains" // the field no longer has Value
	CheckMaxLength   = "max_length"   // the field is at most Limit characters
	CheckNotEmpty    = "not_empty"    // the field has a value
	CheckUnique      = "unique"       // no two checked pages share the field's value
	CheckChangedFrom = "changed_from" // Page's field is no longer Value
	CheckStatusOK    = "status_ok"    // the page answers below HTTP 400
)

// TaskCheck is one condition a recrawl tests after the harness reports done.
type TaskCheck struct {
	Type  string `bson:"type"            json:"type"`
	Field string `bson:"field,omitempty" json:"field,omitempty"` // title | meta_description | h1 | category_text
	Value string `bson:"value,omitempty" json:"value,omitempty"`
	Limit int    `bson:"limit,omitempty" json:"limit,omitempty"`
	Page  string `bson:"page,omitempty"  json:"page,omitempty"`
}

// Describe states the check in plain words.
func (c TaskCheck) Describe() string {
	switch c.Type {
	case CheckNotContains:
		return fmt.Sprintf("%s no longer contains %q", c.Field, c.Value)
	case CheckMaxLength:
		return fmt.Sprintf("%s is at most %d characters", c.Field, c.Limit)
	case CheckNotEmpty:
		return fmt.Sprintf("%s is present", c.Field)
	case CheckUnique:
		return fmt.Sprintf("%s differs from page to page", c.Field)
	case CheckChangedFrom:
		return fmt.Sprintf("%s of %s is no longer %q", c.Field, c.Page, c.Value)
	case CheckStatusOK:
		return "the pages answer below HTTP 400"
	}
	return c.Type
}

// TaskExample shows the problem, and a proposal when there is one, on a real page.
type TaskExample struct {
	Page     string `bson:"page"               json:"page"`
	Field    string `bson:"field"              json:"field"`
	Current  string `bson:"current,omitempty"  json:"current,omitempty"`
	Proposed string `bson:"proposed,omitempty" json:"proposed,omitempty"`
}

// TaskMetrics sums the known pages of a task. Pages are the ones actually read,
// so a template issue may touch more pages than these.
type TaskMetrics struct {
	Pages        int     `bson:"pages"         json:"pages"`
	PagesSampled int     `bson:"pages_sampled" json:"pages_sampled"` // pages read on the site so far
	Impressions  float64 `bson:"impressions"   json:"impressions"`
	Clicks       float64 `bson:"clicks"        json:"clicks"`
	Sessions     float64 `bson:"sessions"      json:"sessions"`
}

// TaskVerification is the outcome of the recrawl after a task was done.
type TaskVerification struct {
	At       time.Time `bson:"at"                 json:"at"`
	Checked  int       `bson:"checked"            json:"checked"` // pages read
	Failures []string  `bson:"failures,omitempty" json:"failures,omitempty"`
	Note     string    `bson:"note,omitempty"     json:"note,omitempty"`
}

// SEOTask is one unit of work for an implementing harness: one problem, however
// many pages it touches. Key stays the same across runs, so a repeated finding
// updates the task instead of creating another.
// MongoDB collection: seo_tasks
type SEOTask struct {
	ID         bson.ObjectID `bson:"_id,omitempty" json:"id,omitempty"`
	ProjectID  bson.ObjectID `bson:"project_id"    json:"project_id"`
	Key        string        `bson:"key"           json:"key"`
	Status     TaskStatus    `bson:"status"        json:"status"`
	Source     string        `bson:"source"        json:"source"`
	Issue      string        `bson:"issue"         json:"issue"`
	Scope      string        `bson:"scope"         json:"scope"`
	PageGroup  string        `bson:"page_group"    json:"page_group"`
	Title      string        `bson:"title"         json:"title"`
	Problem    string        `bson:"problem"       json:"problem"`
	Goal       string        `bson:"goal"          json:"goal"`
	Language   string        `bson:"language"      json:"language"`
	Confidence string        `bson:"confidence"    json:"confidence"` // high | medium | low
	// Pages lists every known affected page; ConfirmedPages are those an
	// analysis actually read and found the problem on.
	Pages          []string           `bson:"pages"           json:"pages"`
	ConfirmedPages []string           `bson:"confirmed_pages" json:"confirmed_pages"`
	Metrics        TaskMetrics        `bson:"metrics"         json:"metrics"`
	Evidence       []AnalysisEvidence `bson:"evidence"        json:"evidence"`
	Examples       []TaskExample      `bson:"examples"        json:"examples"`
	Avoid          []string           `bson:"avoid"           json:"avoid"`
	Checks         []TaskCheck        `bson:"checks"          json:"checks"`
	SuggestionIDs  []bson.ObjectID    `bson:"suggestion_ids"  json:"suggestion_ids"`
	AnalysisIDs    []bson.ObjectID    `bson:"analysis_ids"    json:"analysis_ids"`

	ClaimedBy    string            `bson:"claimed_by,omitempty"   json:"claimed_by,omitempty"`
	ClaimedAt    *time.Time        `bson:"claimed_at,omitempty"   json:"claimed_at,omitempty"`
	DoneAt       *time.Time        `bson:"done_at,omitempty"      json:"done_at,omitempty"`
	DoneSummary  string            `bson:"done_summary,omitempty" json:"done_summary,omitempty"`
	Verification *TaskVerification `bson:"verification,omitempty" json:"verification,omitempty"`
	LastSeenAt   time.Time         `bson:"last_seen_at"           json:"last_seen_at"`
	CreatedAt    time.Time         `bson:"created_at"             json:"created_at"`
	UpdatedAt    time.Time         `bson:"updated_at"             json:"updated_at"`
}

// IsActive reports whether a harness can still act on the task.
func (t *SEOTask) IsActive() bool {
	return t.Status == TaskStatusOpen || t.Status == TaskStatusClaimed || t.Status == TaskStatusReopened
}

const briefPages = 20

// Brief renders the task as the text an implementing harness works from.
func (t *SEOTask) Brief() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", t.Title)
	fmt.Fprintf(&b, "Task ID: %s\nIssue: %s\nScope: %s\nConfidence: %s\n\n", t.ID.Hex(), t.Issue, t.Scope, t.Confidence)
	b.WriteString("This was found by probing the public site from outside. The source code was not read. ")
	if t.Scope != TaskScopePage {
		b.WriteString("The pages behaving alike suggests one shared cause, but that is an inference: confirm it, find where the output comes from, and fix it once at the source. ")
	}
	b.WriteString("The change should fit the codebase; the examples show intent, not exact code.\n")

	fmt.Fprintf(&b, "\n## Problem\n%s\n", t.Problem)
	fmt.Fprintf(&b, "\n## Goal\n%s\n", t.Goal)
	fmt.Fprintf(&b, "\n## Reach\n%d pages may be affected, out of %d pages read on the site so far. The problem was confirmed on %d of them", t.Metrics.Pages, t.Metrics.PagesSampled, len(t.ConfirmedPages))
	if len(t.ConfirmedPages) < t.Metrics.Pages {
		b.WriteString("; the others are the same type of page and were assumed to share it, so check them first")
	}
	fmt.Fprintf(&b, ". Together the pages have %.0f impressions, %.0f clicks and %.0f sessions in 28 days.\n", t.Metrics.Impressions, t.Metrics.Clicks, t.Metrics.Sessions)

	if len(t.Evidence) > 0 {
		b.WriteString("\n## Evidence\n")
		for _, e := range t.Evidence {
			fmt.Fprintf(&b, "- %s", e.Claim)
			if e.Fact != "" {
				fmt.Fprintf(&b, " (%s)", e.Fact)
			}
			b.WriteByte('\n')
		}
	}
	if len(t.Examples) > 0 {
		b.WriteString("\n## Examples\n")
		for _, e := range t.Examples {
			fmt.Fprintf(&b, "- %s, %s\n", e.Page, e.Field)
			if e.Current != "" {
				fmt.Fprintf(&b, "  - now: %s\n", e.Current)
			}
			if e.Proposed != "" {
				fmt.Fprintf(&b, "  - proposed: %s\n", e.Proposed)
			}
		}
	}
	if len(t.Avoid) > 0 {
		b.WriteString("\n## Avoid\n")
		for _, a := range t.Avoid {
			fmt.Fprintf(&b, "- %s\n", a)
		}
	}
	b.WriteString("\n## Affected pages\n")
	for i, p := range t.Pages {
		if i == briefPages {
			fmt.Fprintf(&b, "- and %d more (see the task's pages list)\n", len(t.Pages)-briefPages)
			break
		}
		fmt.Fprintf(&b, "- %s\n", p)
	}
	b.WriteString("\n## Done when\n")
	if len(t.Checks) == 0 {
		b.WriteString("- This cannot be checked from outside. A person reviews the result.\n")
	}
	for _, c := range t.Checks {
		fmt.Fprintf(&b, "- %s\n", c.Describe())
	}
	b.WriteString("\nReport the task done only after the change is live on the public site: a recrawl of a sample of these pages follows at once, and a failed check reopens the task.\n")
	return b.String()
}
