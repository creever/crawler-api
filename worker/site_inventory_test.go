package worker

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.uber.org/zap"

	"github.com/creever/crawler-api/models"
)

func TestRobotsRules(t *testing.T) {
	rules := parseRobots(`
User-agent: Googlebot
Disallow: /only-google

User-agent: Bingbot
User-agent: *
Crawl-delay: 4
Disallow: /admin/
Disallow: /*?sort=
Disallow: /*.pdf$
Allow: /admin/public
Sitemap: https://x.example/sitemap.xml
Sitemap: https://x.example/sitemap.xml
`)
	if rules.Delay != 4*time.Second || len(rules.Sitemaps) != 1 {
		t.Fatalf("rules: %+v", rules)
	}
	for u, want := range map[string]bool{
		"https://x.example/":                  true,
		"https://x.example/only-google":       true,
		"https://x.example/admin/users":       false,
		"https://x.example/admin/public/page": true,
		"https://x.example/shoes?sort=price":  false,
		"https://x.example/doc.pdf":           false,
		"https://x.example/doc.pdf?x=1":       true,
	} {
		if got := rules.allowed(u); got != want {
			t.Errorf("allowed(%s) = %v", u, got)
		}
	}
	if !(robotsRules{}).allowed("https://x.example/anything") {
		t.Fatal("no rules allow everything")
	}
}

func gzipped(t *testing.T, s string) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	if _, err := zw.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestParseSitemap(t *testing.T) {
	index := `<?xml version="1.0"?><sitemapindex xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
<sitemap><loc> https://x.example/s1.xml.gz </loc></sitemap></sitemapindex>`
	pages, children, err := parseSitemap([]byte(index))
	if err != nil || len(pages) != 0 || len(children) != 1 || children[0] != "https://x.example/s1.xml.gz" {
		t.Fatalf("index: %v %v %v", pages, children, err)
	}
	set := `<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9"><url><loc>https://x.example/a</loc></url><url><loc>https://x.example/b</loc></url></urlset>`
	pages, children, err = parseSitemap(gzipped(t, set))
	if err != nil || len(pages) != 2 || len(children) != 0 {
		t.Fatalf("gzipped urlset: %v %v %v", pages, children, err)
	}
	if _, _, err = parseSitemap([]byte("<html>not a sitemap")); err == nil {
		t.Fatal("broken XML must be an error")
	}
}

func TestOrderFrontier(t *testing.T) {
	old := time.Now().Add(-30 * 24 * time.Hour)
	older := old.Add(-time.Hour)
	urls := []models.SiteURL{
		{URL: "a", SourceURL: "A", LastFetchedAt: &old},
		{URL: "b", SourceURL: "B"},
		{URL: "c", SourceURL: "C", InSitemap: true},
		{URL: "d", SourceURL: "D"},
		{URL: "e", SourceURL: "E", LastFetchedAt: &older},
	}
	got := strings.Join(orderFrontier(urls, map[string]float64{"d": 50}), "")
	if got != "DCBEA" {
		t.Fatalf("order %s: unread with traffic, unread in the sitemap, other unread, then oldest read", got)
	}
}

// prof builds a read page; links may use other spellings of the same URLs.
func prof(url string, status int, links ...string) models.PageProfile {
	return models.PageProfile{URL: url, SourceURL: url, StatusCode: status, FinalURL: url, Links: links}
}

func TestLinkGraph(t *testing.T) {
	home := "https://x.example/"
	profiles := []models.PageProfile{
		prof("https://x.example/", 200, "https://x.example/a", "https://x.example/gone", "https://x.example/old"),
		prof("https://x.example/a", 200, "http://www.x.example/b/", "https://x.example/a", "https://x.example/unread"),
		prof("https://x.example/b", 200, "https://x.example/"),
		prof("https://x.example/gone", 404),
		prof("https://x.example/old", 200),
		prof("https://x.example/island", 200),
	}
	profiles[4].FinalURL = "https://x.example/new"
	g := buildLinkGraph(profiles, home)
	if g["https://x.example/b"].Depth != 2 || g["https://x.example/a"].Inbound != 1 || g["https://x.example/"].Inbound != 1 {
		t.Fatalf("depth/inbound: %+v", g)
	}
	if g["https://x.example/island"].Depth != -1 || g["https://x.example/island"].Inbound != 0 {
		t.Fatalf("a page nothing links to: %+v", g["https://x.example/island"])
	}
	if s := g["https://x.example/"]; s.BrokenLinks != 1 || s.RedirectLinks != 1 {
		t.Fatalf("home links: %+v", s)
	}
	if g["https://x.example/a"].Inbound != 1 {
		t.Fatal("a link to itself does not count")
	}
}

func codes(issues []models.PageIssue) string {
	var out []string
	for _, i := range issues {
		out = append(out, i.Code)
	}
	return strings.Join(out, ",")
}

func TestAuditPage(t *testing.T) {
	good := models.PageProfile{
		URL: "https://x.example/p", SourceURL: "https://x.example/p", FinalURL: "https://x.example/p", StatusCode: 200,
		Title: "Good", MetaDescription: "Text", H1Count: 1, Kind: models.PageKindProduct, ContentWords: 400, SchemaTypes: []string{"Product"},
	}
	in := auditInput{Graph: map[string]graphStats{good.URL: {Inbound: 3, Depth: 2}}, InSitemap: map[string]bool{}, Impressions: map[string]float64{}, GraphReliable: true}
	if got := auditPage(good, in); len(got) != 0 {
		t.Fatalf("a sound page has no issues: %s", codes(got))
	}

	broken := good
	broken.StatusCode, broken.Title = 404, ""
	if got := codes(auditPage(broken, in)); got != AuditHTTPError {
		t.Fatalf("an error page is judged on its status only: %s", got)
	}

	hidden := good
	hidden.Noindex = true
	if got := codes(auditPage(hidden, in)); got != AuditNoindex {
		t.Fatalf("a noindex page out of search: %s", got)
	}
	in.InSitemap[good.URL] = true
	if got := auditPage(hidden, in); codes(got) != AuditNoindexInSearch || got[0].Severity != models.SeverityCritical {
		t.Fatalf("noindex on a sitemap page is critical: %+v", got)
	}

	moved := good
	moved.FinalURL, moved.Redirects = "https://x.example/new", 2
	if got := codes(auditPage(moved, in)); got != AuditRedirected+","+AuditRedirectChain {
		t.Fatalf("redirect: %s", got)
	}

	weak := good
	weak.Title, weak.MetaDescription, weak.H1Count, weak.ContentWords, weak.SchemaTypes = "", "", 2, 40, nil
	in.Graph[good.URL] = graphStats{Inbound: 0, Depth: 6, BrokenLinks: 1}
	want := strings.Join([]string{AuditMissingTitle, AuditMissingMeta, AuditMultipleH1, AuditThinContent, AuditMissingSchema, AuditOrphan, AuditDeep, AuditBrokenLinks}, ",")
	if got := codes(auditPage(weak, in)); got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
	in.GraphReliable = false
	if strings.Contains(codes(auditPage(weak, in)), AuditOrphan) {
		t.Fatal("with too little of the site read, orphans cannot be told")
	}
}

func TestSiteScore(t *testing.T) {
	profiles := []models.PageProfile{{URL: "a"}, {URL: "b"}, {URL: "c"}, {URL: "d"}}
	issues := map[string][]models.PageIssue{"b": {{Code: AuditHTTPError, Severity: models.SeverityCritical}}, "c": {{Code: AuditMissingMeta, Severity: models.SeverityWarning}}}
	score, basis := siteScore(profiles, issues, map[string]float64{"a": 600, "b": 300, "c": 100})
	if basis != models.ScoreByImpressions || score < 0.699 || score > 0.701 {
		t.Fatalf("score %v by %s: 300 of 1000 impressions sit on a broken page", score, basis)
	}
	score, basis = siteScore(profiles, issues, map[string]float64{})
	if basis != models.ScoreByPages || score != 0.75 {
		t.Fatalf("without search data, pages count: %v %s", score, basis)
	}
}

func TestAuditIssuesBecomeTasks(t *testing.T) {
	var pages []models.PageProfile
	for i := 0; i < 4; i++ {
		pr := profile(fmt.Sprintf("https://s.example/p-%d", i), models.PageKindProduct, fmt.Sprintf("T%d", i), fmt.Sprintf("Own text %d.", i))
		pr.Kind = models.PageKindProduct
		pr.Issues = []models.PageIssue{{Code: AuditMissingSchema}, {Code: AuditOrphan}}
		pages = append(pages, pr)
	}
	list := detectFindings(pages)
	schema := findingsOf(list, models.IssueStructuredData)
	if len(schema) != 1 || len(schema[0].Pages) != 4 || schema[0].Checks[0].Type != models.CheckHasSchema {
		t.Fatalf("schema: %+v", schema)
	}
	if len(findingsOf(list, models.IssueOrphanPages)) != 1 {
		t.Fatal("orphans of one page type are one task")
	}
	snap := &models.PageSnapshot{URL: "u", SourceURL: "u", SchemaTypes: []string{"Organization"}}
	if len(evaluateChecks(schema[0].Checks, []*models.PageSnapshot{snap})) != 1 {
		t.Fatal("a page without the type fails the check")
	}
	snap.SchemaTypes = append(snap.SchemaTypes, "Product")
	if len(evaluateChecks(schema[0].Checks, []*models.PageSnapshot{snap})) != 0 {
		t.Fatal("a page with the type passes")
	}
}

// A whole site crawl against a small site: sitemap from robots.txt, a page
// only reachable by link, a blocked path, a broken page and an orphan.
func TestIntegrationSiteCrawl(t *testing.T) {
	database := integrationDB(t)
	ctx := context.Background()

	var site *httptest.Server
	page := func(title, h1 string, links ...string) string {
		var b strings.Builder
		fmt.Fprintf(&b, `<html lang="en"><head><title>%s</title><meta name="description" content="About %s"></head><body><h1>%s</h1><p>%s</p>`,
			title, title, h1, strings.Repeat("Plenty of text about this page. ", 20))
		for _, l := range links {
			fmt.Fprintf(&b, `<a href="%s">link to %s</a>`, l, l)
		}
		return b.String() + "</body></html>"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "User-agent: *\nCrawl-delay: 0.01\nDisallow: /private\nSitemap: %s/sitemap.xml\n", site.URL)
	})
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<sitemapindex><sitemap><loc>%s/pages.xml</loc></sitemap></sitemapindex>`, site.URL)
	})
	mux.HandleFunc("/pages.xml", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(gzipped(t, fmt.Sprintf(`<urlset><url><loc>%[1]s/</loc></url><url><loc>%[1]s/a</loc></url><url><loc>%[1]s/orphan</loc></url><url><loc>%[1]s/private/x</loc></url></urlset>`, site.URL)))
	})
	site = httptest.NewServer(mux)
	defer site.Close()

	pages := map[string]string{
		"/":       page("Home", "Home", "/a", "/linked-only", "/gone"),
		"/a":      page("A", "A", "/"),
		"/orphan": page("Orphan", "Orphan"),
		// Only a link leads here, and it repeats the home page's title.
		"/linked-only": page("Home", "Linked"),
	}
	crawler := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target := r.URL.Query().Get("url")
		path := strings.TrimPrefix(target, site.URL)
		if path == "" {
			path = "/"
		}
		w.Header().Set("X-Final-URL", target)
		body, ok := pages[path]
		if !ok {
			w.Header().Set("X-Prerender-Status", "404")
			body = "<html><head><title>Not found</title></head><body>gone</body></html>"
		}
		fmt.Fprint(w, body)
	}))
	defer crawler.Close()

	p := NewProcessor(database, zap.NewNop(), crawler.URL, nil)
	project := models.Project{ID: bson.NewObjectID(), Name: "Site", URL: site.URL + "/", Active: true}
	if _, err := database.Collection("projects").InsertOne(ctx, project); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(SiteCrawlPayload{ProjectID: project.ID.Hex()})
	if err := p.HandleSiteCrawl(ctx, asynq.NewTask(TypeSiteCrawl, body)); err != nil {
		t.Fatal(err)
	}

	var audit models.SiteAudit
	if err := database.Collection("site_audits").FindOne(ctx, bson.M{"project_id": project.ID}).Decode(&audit); err != nil {
		t.Fatal(err)
	}
	if audit.Status != "done" || audit.PagesFetched != 5 || audit.URLsInSitemap != 4 || audit.URLsBlocked != 1 {
		t.Fatalf("audit: %+v", audit)
	}
	profile := func(path string) models.PageProfile {
		t.Helper()
		var pr models.PageProfile
		if err := database.Collection("page_profiles").FindOne(ctx, bson.M{"url": normalizePageURL(site.URL + path)}).Decode(&pr); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		return pr
	}
	if got := codes(profile("/gone").Issues); got != AuditHTTPError {
		t.Errorf("/gone: %s", got)
	}
	if got := codes(profile("/orphan").Issues); !strings.Contains(got, AuditOrphan) {
		t.Errorf("/orphan: %s", got)
	}
	if got := codes(profile("/linked-only").Issues); !strings.Contains(got, AuditDuplicateTitle) {
		t.Errorf("/linked-only shares the home title: %s", got)
	}
	if pr := profile("/a"); pr.Depth != 1 || pr.Inbound != 1 || !pr.InSitemap {
		t.Errorf("/a: depth %d inbound %d sitemap %v", pr.Depth, pr.Inbound, pr.InSitemap)
	}
	if got := codes(profile("/").Issues); !strings.Contains(got, AuditBrokenLinks) {
		t.Errorf("home links to a broken page: %s", got)
	}
	var blocked models.SiteURL
	if err := database.Collection("site_urls").FindOne(ctx, bson.M{"url": normalizePageURL(site.URL + "/private/x")}).Decode(&blocked); err != nil || !blocked.Disallowed || blocked.LastFetchedAt != nil {
		t.Fatalf("a path robots.txt disallows is never read: %+v %v", blocked, err)
	}
	if audit.NotFound == nil || audit.NotFound.Soft || audit.NotFound.Status != 404 {
		t.Errorf("the site answers 404 for missing pages: %+v", audit.NotFound)
	}
	if audit.ScoreBasis != models.ScoreByPages || audit.Score != 0.8 {
		t.Errorf("one broken page of five: score %v by %s", audit.Score, audit.ScoreBasis)
	}

	// A page read before the current profile format is not judged, and it is
	// first in line to be read again.
	if _, err := database.Collection("page_profiles").UpdateOne(ctx, bson.M{"url": normalizePageURL(site.URL + "/a")},
		bson.M{"$set": bson.M{"version": 0, "h1_count": 0}}); err != nil {
		t.Fatal(err)
	}
	if err := p.auditSite(ctx, &project, &models.SiteAudit{}); err != nil {
		t.Fatal(err)
	}
	if pr := profile("/a"); len(pr.Issues) != 0 {
		t.Errorf("an old profile was judged: %s", codes(pr.Issues))
	}
	var again models.SiteURL
	_ = database.Collection("site_urls").FindOne(ctx, bson.M{"url": normalizePageURL(site.URL + "/a")}).Decode(&again)
	if again.LastFetchedAt != nil {
		t.Error("an old profile's page must be read again first")
	}

	// A second run reads nothing fresh again and keeps one inventory.
	if err := p.HandleSiteCrawl(ctx, asynq.NewTask(TypeSiteCrawl, body)); err != nil {
		t.Fatal(err)
	}
	n, _ := database.Collection("site_urls").CountDocuments(ctx, bson.M{"project_id": project.ID})
	var second models.SiteAudit
	_ = database.Collection("site_audits").FindOne(ctx, bson.M{"project_id": project.ID}, optionsNewest()).Decode(&second)
	if n != 6 || second.PagesReadNow != 1 || second.PagesFetched != 5 {
		t.Fatalf("second run: %d URLs, read %d, pages %d", n, second.PagesReadNow, second.PagesFetched)
	}
}

func optionsNewest() *options.FindOneOptionsBuilder {
	return options.FindOne().SetSort(bson.D{{Key: "started_at", Value: -1}})
}

func TestSoftNotFoundTask(t *testing.T) {
	probe := &models.NotFoundProbe{URL: "https://x.example/crawler-not-found-check-1", Status: 200, FinalURL: "https://x.example/", Soft: true}
	task := softNotFoundTask(bson.NewObjectID(), probe)
	if !strings.Contains(task.Problem, "redirects to https://x.example/") || task.Checks[0].Type != models.CheckStatusError {
		t.Fatalf("task: %+v", task)
	}
	page := func(status int, final string) *models.PageSnapshot {
		return &models.PageSnapshot{URL: normalizePageURL(probe.URL), SourceURL: probe.URL, StatusCode: status, FinalURL: final}
	}
	for _, c := range []struct {
		snap  *models.PageSnapshot
		fixed bool
	}{
		{page(404, probe.URL), true},
		{page(410, ""), true},
		{page(200, probe.URL), false},
		{page(404, "https://x.example/"), false},
	} {
		if got := len(evaluateChecks(task.Checks, []*models.PageSnapshot{c.snap})) == 0; got != c.fixed {
			t.Errorf("status %d at %q: fixed %v", c.snap.StatusCode, c.snap.FinalURL, got)
		}
	}
}
