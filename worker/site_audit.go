package worker

import (
	"context"
	"fmt"
	"sort"
	"unicode/utf8"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/creever/crawler-api/models"
)

// Audit issue codes, per page.
const (
	AuditHTTPError         = "http_error"
	AuditNoindexInSearch   = "noindex_in_search" // noindex on a page in the sitemap or with search traffic
	AuditNoindex           = "noindex"
	AuditRedirected        = "redirected_in_sitemap"
	AuditRedirectChain     = "redirect_chain"
	AuditCanonicalElse     = "canonical_elsewhere"
	AuditMissingTitle      = "missing_title"
	AuditDuplicateTitle    = "duplicate_title"
	AuditTitleTooLong      = "title_too_long"
	AuditMissingMeta       = "missing_meta"
	AuditDuplicateMeta     = "duplicate_meta"
	AuditMetaTooLong       = "meta_too_long"
	AuditMissingH1         = "missing_h1"
	AuditMultipleH1        = "multiple_h1"
	AuditThinContent       = "thin_content"
	AuditOrphan            = "orphan"
	AuditDeep              = "deep"
	AuditBrokenLinks       = "broken_links"
	AuditRedirectLinks     = "links_to_redirects"
	AuditMissingSchema     = "missing_schema"
	AuditSlowRender        = "slow_render"
	AuditUnavailable       = "unavailable_product"
	thinContentWords       = 150
	deepClicks             = 4
	slowRenderMs           = 5000
	graphReliableShare     = 0.8
	redirectChainThreshold = 2
)

// auditSeverity sets how much each issue weighs. Critical issues keep a page
// out of search or empty its listing, and they count against the score.
var auditSeverity = map[string]string{
	AuditHTTPError: models.SeverityCritical, AuditNoindexInSearch: models.SeverityCritical,
	AuditMissingTitle: models.SeverityCritical,
	AuditRedirected:   models.SeverityWarning, AuditRedirectChain: models.SeverityWarning,
	AuditCanonicalElse: models.SeverityWarning, AuditDuplicateTitle: models.SeverityWarning,
	AuditMissingMeta: models.SeverityWarning, AuditMissingH1: models.SeverityWarning,
	AuditOrphan: models.SeverityWarning, AuditBrokenLinks: models.SeverityWarning,
	AuditNoindex: models.SeverityNotice, AuditTitleTooLong: models.SeverityNotice,
	AuditDuplicateMeta: models.SeverityNotice, AuditMetaTooLong: models.SeverityNotice,
	AuditMultipleH1: models.SeverityNotice, AuditThinContent: models.SeverityNotice,
	AuditDeep: models.SeverityNotice, AuditRedirectLinks: models.SeverityNotice,
	AuditMissingSchema: models.SeverityNotice, AuditSlowRender: models.SeverityNotice,
	AuditUnavailable: models.SeverityNotice,
}

// graphStats is what the link graph says about one page.
type graphStats struct {
	Inbound, Depth, BrokenLinks, RedirectLinks int
}

// buildLinkGraph counts, for each page read, the pages linking to it, its
// clicks from the home page, and its links to broken or redirecting pages.
// Only pages read take part, so on a partly read site depth is an upper bound
// and inbound a lower one.
func buildLinkGraph(profiles []models.PageProfile, homeURL string) map[string]graphStats {
	byURL := make(map[string]*models.PageProfile, len(profiles))
	for i := range profiles {
		byURL[profiles[i].URL] = &profiles[i]
	}
	stats := make(map[string]graphStats, len(profiles))
	inbound := make(map[string]map[string]bool)
	links := make(map[string][]string, len(profiles))
	for _, pr := range profiles {
		for _, l := range pr.Links {
			links[pr.URL] = append(links[pr.URL], normalizePageURL(l))
		}
	}
	for _, pr := range profiles {
		s := stats[pr.URL]
		for _, link := range links[pr.URL] {
			target, ok := byURL[link]
			if !ok || link == pr.URL {
				continue
			}
			if inbound[link] == nil {
				inbound[link] = make(map[string]bool)
			}
			inbound[link][pr.URL] = true
			switch {
			case target.StatusCode >= 400:
				s.BrokenLinks++
			case target.FinalURL != "" && normalizePageURL(target.FinalURL) != target.URL:
				s.RedirectLinks++
			}
		}
		s.Depth = -1
		stats[pr.URL] = s
	}
	for page, from := range inbound {
		s := stats[page]
		s.Inbound = len(from)
		stats[page] = s
	}
	home := normalizePageURL(homeURL)
	if _, ok := byURL[home]; !ok {
		return stats
	}
	s := stats[home]
	s.Depth = 0
	stats[home] = s
	queue := []string{home}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, link := range links[cur] {
			next, ok := stats[link]
			if !ok || next.Depth >= 0 {
				continue
			}
			next.Depth = stats[cur].Depth + 1
			stats[link] = next
			queue = append(queue, link)
		}
	}
	return stats
}

// auditInput is what the audit knows of a page beyond its profile.
type auditInput struct {
	Graph         map[string]graphStats
	InSitemap     map[string]bool
	Impressions   map[string]float64
	GraphReliable bool // enough of the site was read for orphans and depth to mean something
	TitleCounts   map[string]int
	MetaCounts    map[string]int
}

var expectedSchema = map[string][]string{
	models.PageKindProduct: {"Product"},
	models.PageKindArticle: {"Article", "NewsArticle", "BlogPosting"},
}

func hasAnyType(types, want []string) bool {
	for _, t := range types {
		for _, w := range want {
			if t == w {
				return true
			}
		}
	}
	return false
}

// auditPage lists a page's issues. A page that fails, redirects or is kept out
// of search is not judged on its content: that would only repeat the cause.
func auditPage(pr models.PageProfile, in auditInput) []models.PageIssue {
	var codes []string
	add := func(code string) { codes = append(codes, code) }
	inSearch := in.InSitemap[pr.URL] || in.Impressions[pr.URL] > 0
	redirected := pr.FinalURL != "" && normalizePageURL(pr.FinalURL) != pr.URL
	g := in.Graph[pr.URL]

	switch {
	case pr.StatusCode >= 400:
		add(AuditHTTPError)
	case redirected:
		if in.InSitemap[pr.URL] {
			add(AuditRedirected)
		}
		if pr.Redirects >= redirectChainThreshold {
			add(AuditRedirectChain)
		}
	case pr.Noindex:
		if inSearch {
			add(AuditNoindexInSearch)
		} else {
			add(AuditNoindex)
		}
	default:
		if pr.Canonical != "" && normalizePageURL(resolveAgainst(pr.SourceURL, pr.Canonical)) != pr.URL {
			add(AuditCanonicalElse)
			break
		}
		switch {
		case pr.Title == "":
			add(AuditMissingTitle)
		case in.TitleCounts[normalizedText(pr.Title)] > 1:
			add(AuditDuplicateTitle)
		case utf8.RuneCountInString(pr.Title) > maxTitleChars:
			add(AuditTitleTooLong)
		}
		switch {
		case pr.MetaDescription == "":
			add(AuditMissingMeta)
		case in.MetaCounts[normalizedText(pr.MetaDescription)] > 1:
			add(AuditDuplicateMeta)
		case utf8.RuneCountInString(pr.MetaDescription) > maxMetaChars:
			add(AuditMetaTooLong)
		}
		switch {
		case pr.H1Count == 0:
			add(AuditMissingH1)
		case pr.H1Count > 1:
			add(AuditMultipleH1)
		}
		if pr.Kind != models.PageKindListing && pr.Kind != models.PageKindHome && pr.ContentWords < thinContentWords {
			add(AuditThinContent)
		}
		if want, ok := expectedSchema[pr.Kind]; ok && !hasAnyType(pr.SchemaTypes, want) {
			add(AuditMissingSchema)
		}
		if pr.Unavailable {
			add(AuditUnavailable)
		}
		if in.GraphReliable && inSearch && g.Inbound == 0 && g.Depth != 0 {
			add(AuditOrphan)
		}
		if in.GraphReliable && g.Depth > deepClicks {
			add(AuditDeep)
		}
	}
	if pr.StatusCode < 400 {
		if g.BrokenLinks > 0 {
			add(AuditBrokenLinks)
		}
		if g.RedirectLinks > 0 {
			add(AuditRedirectLinks)
		}
		if pr.RenderMs > slowRenderMs {
			add(AuditSlowRender)
		}
	}
	issues := make([]models.PageIssue, 0, len(codes))
	for _, c := range codes {
		issues = append(issues, models.PageIssue{Code: c, Severity: auditSeverity[c]})
	}
	return issues
}

func hasCritical(issues []models.PageIssue) bool {
	for _, i := range issues {
		if i.Severity == models.SeverityCritical {
			return true
		}
	}
	return false
}

// textCounts counts how many indexable pages share each text.
func textCounts(profiles []models.PageProfile, value func(models.PageProfile) string) map[string]int {
	counts := make(map[string]int)
	for _, pr := range indexablePages(profiles) {
		if v := normalizedText(value(pr)); v != "  " && v != " " {
			counts[v]++
		}
	}
	return counts
}

// siteScore is the share of search impressions on pages without a critical
// issue, or the share of pages when the site has no search data.
func siteScore(profiles []models.PageProfile, issues map[string][]models.PageIssue, impressions map[string]float64) (float64, string) {
	var total, fine float64
	for _, pr := range profiles {
		total += impressions[pr.URL]
		if !hasCritical(issues[pr.URL]) {
			fine += impressions[pr.URL]
		}
	}
	if total > 0 {
		return fine / total, models.ScoreByImpressions
	}
	if len(profiles) == 0 {
		return 0, models.ScoreByPages
	}
	clean := 0
	for _, pr := range profiles {
		if !hasCritical(issues[pr.URL]) {
			clean++
		}
	}
	return float64(clean) / float64(len(profiles)), models.ScoreByPages
}

// auditSite rebuilds the link graph and the issues of every page read, and
// fills the audit summary.
func (p *Processor) auditSite(ctx context.Context, project *models.Project, audit *models.SiteAudit) error {
	all, err := p.loadProfiles(ctx, project.ID)
	if err != nil {
		return fmt.Errorf("load profiles: %w", err)
	}
	// Profiles of an older format are not judged; their pages are read again
	// before anything else.
	var profiles []models.PageProfile
	var stale []string
	for _, pr := range all {
		if pr.Version >= profileVersion {
			profiles = append(profiles, pr)
		} else {
			stale = append(stale, pr.URL)
		}
	}
	if len(stale) > 0 {
		if _, err = p.db.Collection("page_profiles").UpdateMany(ctx, bson.M{"project_id": project.ID, "url": bson.M{"$in": stale}},
			bson.M{"$set": bson.M{"issues": bson.A{}}}); err != nil {
			return err
		}
		if _, err = p.db.Collection("site_urls").UpdateMany(ctx, bson.M{"project_id": project.ID, "url": bson.M{"$in": stale}},
			bson.M{"$unset": bson.M{"last_fetched_at": ""}}); err != nil {
			return err
		}
	}
	urls, err := p.loadSiteURLs(ctx, project.ID)
	if err != nil {
		return fmt.Errorf("load site URLs: %w", err)
	}
	inSitemap := make(map[string]bool)
	reachable := 0
	for _, u := range urls {
		if u.InSitemap {
			inSitemap[u.URL] = true
			audit.URLsInSitemap++
		}
		if u.Disallowed {
			audit.URLsBlocked++
		} else if sampleableURL(u.SourceURL, project.URL) {
			reachable++
		}
	}
	audit.URLsKnown = len(urls)

	curStart, end, _, _ := analysisWindows()
	totals, err := p.gscPageTotals(ctx, project.ID, curStart, end, 0)
	if err != nil {
		return err
	}
	impressions := make(map[string]float64, len(totals))
	clicks := make(map[string]float64, len(totals))
	var allImpressions float64
	for _, r := range totals {
		key := normalizePageURL(r.Page)
		impressions[key] += r.Impressions
		clicks[key] += r.Clicks
		allImpressions += r.Impressions
	}

	in := auditInput{
		Graph: buildLinkGraph(profiles, project.URL), InSitemap: inSitemap, Impressions: impressions,
		GraphReliable: reachable > 0 && float64(len(profiles)) >= graphReliableShare*float64(reachable),
		TitleCounts:   textCounts(profiles, profileTitle), MetaCounts: textCounts(profiles, profileMeta),
	}
	issues := make(map[string][]models.PageIssue, len(profiles))
	audit.IssuesBySeverity = map[string]int{}
	audit.IssuesByCode = map[string]int{}
	groups := make(map[string]*models.SiteAuditGroup)
	var readImpressions float64
	var writes []mongo.WriteModel
	for _, pr := range profiles {
		list := auditPage(pr, in)
		issues[pr.URL] = list
		seenSeverity := map[string]bool{}
		for _, i := range list {
			audit.IssuesByCode[i.Code]++
			if !seenSeverity[i.Severity] {
				seenSeverity[i.Severity] = true
				audit.IssuesBySeverity[i.Severity]++
			}
		}
		if pr.StatusCode < 400 && !pr.Noindex {
			audit.Indexable++
		}
		readImpressions += impressions[pr.URL]
		g := groups[pr.Group]
		if g == nil {
			g = &models.SiteAuditGroup{Group: pr.Group}
			groups[pr.Group] = g
		}
		g.Pages++
		g.Impressions += impressions[pr.URL]
		g.Clicks += clicks[pr.URL]
		if hasCritical(list) {
			g.Critical++
		}
		gs := in.Graph[pr.URL]
		writes = append(writes, mongo.NewUpdateOneModel().SetFilter(bson.M{"_id": pr.ID}).SetUpdate(bson.M{"$set": bson.M{
			"in_sitemap": inSitemap[pr.URL], "inbound": gs.Inbound, "depth": gs.Depth,
			"broken_links": gs.BrokenLinks, "redirect_links": gs.RedirectLinks,
			"impressions": impressions[pr.URL], "clicks": clicks[pr.URL], "issues": list,
		}}))
	}
	for start := 0; start < len(writes); start += siteURLWriteSize {
		if _, err = p.db.Collection("page_profiles").BulkWrite(ctx, writes[start:min(start+siteURLWriteSize, len(writes))]); err != nil {
			return fmt.Errorf("store audit: %w", err)
		}
	}

	audit.PagesFetched = len(profiles)
	audit.Score, audit.ScoreBasis = siteScore(profiles, issues, impressions)
	if allImpressions > 0 {
		audit.SearchCoverage = readImpressions / allImpressions
	}
	audit.Groups = make([]models.SiteAuditGroup, 0, len(groups))
	for _, g := range groups {
		audit.Groups = append(audit.Groups, *g)
	}
	sort.Slice(audit.Groups, func(i, j int) bool {
		if audit.Groups[i].Impressions != audit.Groups[j].Impressions {
			return audit.Groups[i].Impressions > audit.Groups[j].Impressions
		}
		return audit.Groups[i].Pages > audit.Groups[j].Pages
	})
	return nil
}

// LatestAudit returns the newest finished audit of a project, or nil.
func LatestAudit(ctx context.Context, db *mongo.Database, projectOID bson.ObjectID) (*models.SiteAudit, error) {
	var a models.SiteAudit
	err := db.Collection("site_audits").FindOne(ctx, bson.M{"project_id": projectOID, "status": "done"},
		options.FindOne().SetSort(bson.D{{Key: "started_at", Value: -1}})).Decode(&a)
	if err == mongo.ErrNoDocuments {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// PageQueryStat is one search query of a page, now and in the window before.
type PageQueryStat = pageQueryStat

// PageQueries returns a page's top search queries for the page explorer.
func PageQueries(ctx context.Context, db *mongo.Database, projectOID bson.ObjectID, page string) ([]PageQueryStat, error) {
	queries, _, _, err := (&Processor{db: db}).pageSearchData(ctx, projectOID, page)
	return queries, err
}

// AuditIssueTask maps an audit issue to the task issue that handles it.
var AuditIssueTask = map[string]string{
	AuditHTTPError: models.IssueHTTPError, AuditNoindexInSearch: models.IssueNoindex,
	AuditMissingTitle: models.IssueMissingTitle, AuditDuplicateTitle: models.IssueDuplicateTitle,
	AuditTitleTooLong: models.IssueTitleTooLong, AuditMissingMeta: models.IssueMissingMeta,
	AuditDuplicateMeta: models.IssueDuplicateMeta, AuditMetaTooLong: models.IssueMetaTooLong,
	AuditMissingH1: models.IssueMissingH1, AuditOrphan: models.IssueOrphanPages,
	AuditBrokenLinks: models.IssueBrokenLinks, AuditRedirected: models.IssueSitemap,
	AuditMissingSchema: models.IssueStructuredData, AuditThinContent: models.IssueThinContent,
	AuditUnavailable: models.IssueUnavailable,
}
