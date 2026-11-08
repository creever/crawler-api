package models

import (
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Issue severities. Only critical issues count against the site score.
const (
	SeverityCritical = "critical"
	SeverityWarning  = "warning"
	SeverityNotice   = "notice"
)

// PageIssue is one audit finding on a page.
type PageIssue struct {
	Code     string `bson:"code"     json:"code"`
	Severity string `bson:"severity" json:"severity"`
}

// Sources a URL can be known from.
const (
	URLSourceSeed    = "seed"
	URLSourceSitemap = "sitemap"
	URLSourceGSC     = "gsc"
	URLSourceLink    = "link"
	URLSourceBot     = "bot" // a bot asked /prerender for it
)

// SiteURL is one URL a project's site is known to have, read or not. It is the
// crawl frontier: profiles only exist for pages that were read.
// MongoDB collection: site_urls (one per project × normalized URL)
type SiteURL struct {
	ID            bson.ObjectID `bson:"_id,omitempty"            json:"id,omitempty"`
	ProjectID     bson.ObjectID `bson:"project_id"               json:"project_id"`
	URL           string        `bson:"url"                      json:"url"` // normalized
	SourceURL     string        `bson:"source_url"               json:"source_url"`
	Sources       []string      `bson:"sources"                  json:"sources"`
	InSitemap     bool          `bson:"in_sitemap"               json:"in_sitemap"`
	SitemapSeenAt *time.Time    `bson:"sitemap_seen_at,omitempty" json:"sitemap_seen_at,omitempty"`
	Disallowed    bool          `bson:"disallowed"               json:"disallowed"` // by robots.txt
	DiscoveredAt  time.Time     `bson:"discovered_at"            json:"discovered_at"`
	LastFetchedAt *time.Time    `bson:"last_fetched_at,omitempty" json:"last_fetched_at,omitempty"`
	LastError     string        `bson:"last_error,omitempty"     json:"last_error,omitempty"`
}

// SiteAuditGroup sums one page type of an audit.
type SiteAuditGroup struct {
	Group       string  `bson:"group"       json:"group"`
	Pages       int     `bson:"pages"       json:"pages"`
	Impressions float64 `bson:"impressions" json:"impressions"`
	Clicks      float64 `bson:"clicks"      json:"clicks"`
	Critical    int     `bson:"critical"    json:"critical"` // pages with a critical issue
}

// Score bases.
const (
	ScoreByImpressions = "impressions" // share of search impressions on pages without critical issues
	ScoreByPages       = "pages"       // share of pages, when there is no search data
)

// SiteAudit is the summary of one site crawl. Its history makes the trend.
// MongoDB collection: site_audits
type SiteAudit struct {
	ID         bson.ObjectID `bson:"_id,omitempty"         json:"id,omitempty"`
	ProjectID  bson.ObjectID `bson:"project_id"            json:"project_id"`
	Status     string        `bson:"status"                json:"status"` // running | done | failed
	Error      string        `bson:"error,omitempty"       json:"error,omitempty"`
	StartedAt  time.Time     `bson:"started_at"            json:"started_at"`
	FinishedAt *time.Time    `bson:"finished_at,omitempty" json:"finished_at,omitempty"`

	URLsKnown     int `bson:"urls_known"      json:"urls_known"`
	URLsInSitemap int `bson:"urls_in_sitemap" json:"urls_in_sitemap"`
	URLsBlocked   int `bson:"urls_blocked"    json:"urls_blocked"` // disallowed by robots.txt
	PagesFetched  int `bson:"pages_fetched"   json:"pages_fetched"`
	PagesReadNow  int `bson:"pages_read_now"  json:"pages_read_now"`
	Indexable     int `bson:"indexable"       json:"indexable"`

	// Score is 0..1: the share of ScoreBasis on pages without critical issues.
	Score      float64 `bson:"score"       json:"score"`
	ScoreBasis string  `bson:"score_basis" json:"score_basis"`
	// SearchCoverage is the share of the site's search impressions that fall on
	// pages read; below it the score speaks for part of the site only.
	SearchCoverage float64 `bson:"search_coverage"    json:"search_coverage"`
	// NotFound is how the site answered a URL that cannot exist. A site should
	// say 404 or 410; a 200, or a redirect to another page, is a soft 404.
	NotFound         *NotFoundProbe   `bson:"not_found,omitempty" json:"not_found,omitempty"`
	// Rendering compares a few pages read with and without JavaScript.
	Rendering        *RenderingProbe  `bson:"rendering,omitempty" json:"rendering,omitempty"`
	IssuesBySeverity map[string]int   `bson:"issues_by_severity" json:"issues_by_severity"` // pages per severity
	IssuesByCode     map[string]int   `bson:"issues_by_code"     json:"issues_by_code"`     // pages per issue
	Groups           []SiteAuditGroup `bson:"groups"             json:"groups"`
}

// RenderingProbe compares what a client without JavaScript gets with the
// rendered page. Crawlers of AI assistants and many other bots run no
// JavaScript: content that needs it reaches them only through prerendering.
type RenderingProbe struct {
	Pages     []string `bson:"pages"      json:"pages"`
	HTMLWords int      `bson:"html_words" json:"html_words"`
	JSWords   int      `bson:"js_words"   json:"js_words"`
	HTMLLinks int      `bson:"html_links" json:"html_links"`
	JSLinks   int      `bson:"js_links"   json:"js_links"`
	// NeedsJS is set when the plain HTML carries clearly less text or links.
	NeedsJS bool `bson:"needs_js" json:"needs_js"`
	// Mode is how this crawl read the site: "js" or "html".
	Mode string `bson:"mode" json:"mode"`
}

// NotFoundProbe is the answer to a URL that cannot exist on the site.
type NotFoundProbe struct {
	URL      string `bson:"url"       json:"url"`
	Status   int    `bson:"status"    json:"status"`
	FinalURL string `bson:"final_url" json:"final_url"`
	Soft     bool   `bson:"soft"      json:"soft"` // the site does not say the page is missing
}
