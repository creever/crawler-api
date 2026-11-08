package worker

import (
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/creever/crawler-api/models"
)

func TestExpectedCTR(t *testing.T) {
	cases := []struct {
		pos      float64
		expected float64
	}{
		{1, 0.08},
		{3, 0.08},
		{4, 0.05},
		{5, 0.05},
		{6, 0.02},
		{10, 0.02},
		{11, 0.01},
		{20, 0.01},
	}
	for _, c := range cases {
		if got := expectedCTR(c.pos); got != c.expected {
			t.Errorf("expectedCTR(%v) = %v, want %v", c.pos, got, c.expected)
		}
	}
}

func TestExtractJSON(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		expected string
	}{
		{"plain", `{"a": 1}`, `{"a": 1}`},
		{"fenced", "```json\n{\"a\": 1}\n```", `{"a": 1}`},
		{"fenced no lang", "```\n{\"a\": 1}\n```", `{"a": 1}`},
		{"with prose", "Here you go:\n{\"a\": 1}\nHope that helps", `{"a": 1}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := extractJSON(c.raw); got != c.expected {
				t.Errorf("extractJSON(%q) = %q, want %q", c.raw, got, c.expected)
			}
		})
	}
}

func TestParseFloat(t *testing.T) {
	if got := parseFloat("12.5"); got != 12.5 {
		t.Errorf("parseFloat(12.5) = %v", got)
	}
	if got := parseFloat("0"); got != 0 {
		t.Errorf("parseFloat(0) = %v", got)
	}
	if got := parseFloat("not-a-number"); got != 0 {
		t.Errorf("parseFloat(garbage) = %v, want 0", got)
	}
}

func TestScoresShareOneUnit(t *testing.T) {
	// 10 000 impressions at position 8 with an expected CTR: moving to the top
	// three earns 6 percentage points, reached with 13/16 likelihood.
	if got := strikingGain(10000, 8); got < 487 || got > 488 {
		t.Fatalf("striking gain = %v", got)
	}
	if strikingGain(10000, 3) != 0 {
		t.Fatal("a page already in the top three gains nothing")
	}
	if got := lowCTRGain(10000, 0.005, 8); got < 149 || got > 151 {
		t.Fatalf("low CTR gain = %v", got)
	}
	if got := cannibalizationGain(10000); got != 100 {
		t.Fatalf("cannibalization gain = %v", got)
	}
	if crawlIssueGain(40, models.SeverityCritical) != 40 || crawlIssueGain(40, models.SeverityWarning) != 10 {
		t.Fatal("crawl issue gain should put all clicks at risk for critical, a quarter for warning")
	}
}

func TestOpportunityKeyIsStable(t *testing.T) {
	a := models.Opportunity{ID: bson.NewObjectID(), Type: models.OppLowCTR, Query: "q", Page: "https://x.hu/a", Score: 1}
	b := models.Opportunity{ID: bson.NewObjectID(), Type: models.OppLowCTR, Query: "q", Page: "https://x.hu/a", Score: 9}
	if opportunityKey(a) != opportunityKey(b) {
		t.Fatal("the same finding must keep its key across runs")
	}
	b.Type = models.OppStrikingDistance
	if opportunityKey(a) == opportunityKey(b) {
		t.Fatal("different types must not share a key")
	}
}

func TestInterleaveGivesEveryTypeATurn(t *testing.T) {
	mk := func(typ models.OpportunityType, n int) []models.Opportunity {
		out := make([]models.Opportunity, n)
		for i := range out {
			out[i] = models.Opportunity{Type: typ}
		}
		return out
	}
	got := interleaveByType([][]models.Opportunity{mk(models.OppStrikingDistance, 3), mk(models.OppLowCTR, 1), nil, mk(models.OppLosingClicks, 2)})
	want := []models.OpportunityType{models.OppStrikingDistance, models.OppLowCTR, models.OppLosingClicks, models.OppStrikingDistance, models.OppLosingClicks, models.OppStrikingDistance}
	if len(got) != len(want) {
		t.Fatalf("got %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Type != want[i] {
			t.Fatalf("position %d: got %s, want %s", i, got[i].Type, want[i])
		}
	}
}

func TestCrawlIssueOpportunityUsesTheWorstSeverity(t *testing.T) {
	pg := oppPage{ID: "https://x.hu/a", Clicks: 20, Impressions: 900}
	if _, ok := crawlIssueOpportunity(bson.NewObjectID(), pg, []models.PageIssue{{Code: "thin_content", Severity: models.SeverityNotice}}, time.Now()); ok {
		t.Fatal("notices alone are no opportunity")
	}
	o, ok := crawlIssueOpportunity(bson.NewObjectID(), pg, []models.PageIssue{
		{Code: "missing_h1", Severity: models.SeverityWarning},
		{Code: "http_error", Severity: models.SeverityCritical},
		{Code: "duplicate_title", Severity: models.SeverityWarning},
	}, time.Now())
	if !ok || o.Score != 20 || o.Page != pg.ID || o.Details != "critical: missing_h1, http_error, duplicate_title" {
		t.Fatalf("got %+v", o)
	}
}
