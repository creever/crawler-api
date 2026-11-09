package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.uber.org/zap"

	"github.com/creever/crawler-api/models"
)

func TestNeedsJS(t *testing.T) {
	cases := []struct {
		probe models.RenderingProbe
		want  bool
	}{
		{models.RenderingProbe{HTMLWords: 950, JSWords: 1000, HTMLLinks: 80, JSLinks: 82}, false},
		{models.RenderingProbe{HTMLWords: 40, JSWords: 1000, HTMLLinks: 2, JSLinks: 80}, true},
		{models.RenderingProbe{HTMLWords: 1000, JSWords: 1000, HTMLLinks: 10, JSLinks: 80}, true},  // links come from JS
		{models.RenderingProbe{HTMLWords: 950, JSWords: 954, HTMLLinks: 260, JSLinks: 350}, false}, // a script-built menu
		{models.RenderingProbe{}, false},
	}
	for i, c := range cases {
		if got := needsJS(&c.probe); got != c.want {
			t.Errorf("case %d: needsJS = %v, want %v", i, got, c.want)
		}
	}
}

func TestIntegrationPrerenderCache(t *testing.T) {
	database := integrationDB(t)
	ctx := context.Background()
	var calls atomic.Int32
	crawler := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		time.Sleep(50 * time.Millisecond)
		w.Header().Set("X-Render-Ms", "50")
		fmt.Fprintf(w, "<html><body>render of %s js=%s</body></html>", r.URL.Query().Get("url"), r.URL.Query().Get("js"))
	}))
	defer crawler.Close()
	p := NewProcessor(database, zap.NewNop(), crawler.URL, nil)
	project := &models.Project{ID: bson.NewObjectID(), URL: "https://shop.example/", UseJS: true, Active: true}

	// Concurrent requests for one page share a single render.
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := p.RenderToCache(ctx, project, "https://shop.example/a/", models.CacheSourceBot); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := calls.Load(); n != 1 {
		t.Fatalf("5 concurrent misses rendered %d times", n)
	}
	// Another spelling of the page updates the same entry.
	if _, err := p.RenderToCache(ctx, project, "https://www.shop.example/a", models.CacheSourceManual); err != nil {
		t.Fatal(err)
	}
	if n, _ := database.Collection("cache_entries").CountDocuments(ctx, bson.M{"project_id": project.ID}); n != 1 {
		t.Fatalf("one page should have one cache entry, got %d", n)
	}
	entry, err := LookupCache(ctx, database, project.ID, "http://shop.example/a")
	if err != nil || entry == nil || !strings.Contains(entry.FullHTML, "js=true") || entry.Source != models.CacheSourceManual {
		t.Fatalf("lookup: %v %+v", err, entry)
	}
	if done, _ := database.Collection("crawl_queue").CountDocuments(ctx, bson.M{"project_id": project.ID, "status": models.QueueStatusDone}); done != 2 {
		t.Fatalf("each render should be on the queue as done, got %d", done)
	}

	// An entry without HTML is never served.
	if _, err = database.Collection("cache_entries").InsertOne(ctx, models.CacheEntry{
		ID: bson.NewObjectID(), ProjectID: project.ID, Key: CacheKey("https://shop.example/empty"), URL: "https://shop.example/empty", StatusCode: 200,
	}); err != nil {
		t.Fatal(err)
	}
	if got, _ := LookupCache(ctx, database, project.ID, "https://shop.example/empty"); got != nil {
		t.Fatal("an empty entry must not be served")
	}
	// Another project's render of the same URL is not this project's.
	if got, _ := LookupCache(ctx, database, bson.NewObjectID(), "https://shop.example/a"); got != nil {
		t.Fatal("cache lookups must stay inside the project")
	}

	// The site crawl refreshes pages bots ask for, and only those.
	page := &renderedPage{HTML: "<html>crawl render</html>", Status: 200, FinalURL: "https://shop.example/a"}
	p.refreshCachedRender(ctx, project.ID, "https://shop.example/a", page)
	p.refreshCachedRender(ctx, project.ID, "https://shop.example/never-asked", page)
	entry, _ = LookupCache(ctx, database, project.ID, "https://shop.example/a")
	if entry.Source != models.CacheSourceCrawl || entry.FullHTML != page.HTML || !entry.ExpiresAt.After(time.Now()) {
		t.Fatalf("crawl refresh: %+v", entry)
	}
	if got, _ := LookupCache(ctx, database, project.ID, "https://shop.example/never-asked"); got != nil {
		t.Fatal("the crawl must not fill the cache with pages no bot asked for")
	}
}

func TestIntegrationSiteCrawlChoosesRenderingAndKnowsBotPages(t *testing.T) {
	database := integrationDB(t)
	ctx := context.Background()
	var site *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "User-agent: *\nCrawl-delay: 0.01\n")
	})
	site = httptest.NewServer(mux)
	defer site.Close()

	var plainReads, jsReads atomic.Int32
	crawler := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target := r.URL.Query().Get("url")
		w.Header().Set("X-Final-URL", target)
		if r.URL.Query().Get("js") != "true" {
			plainReads.Add(1)
			fmt.Fprint(w, `<html><head><title>App</title></head><body><div id="root"></div></body></html>`)
			return
		}
		jsReads.Add(1)
		fmt.Fprintf(w, `<html><head><title>Page</title></head><body><h1>Page</h1><p>%s</p><a href="%s/from-bot">x</a></body></html>`,
			strings.Repeat("Rendered words about the page. ", 30), site.URL)
	}))
	defer crawler.Close()

	p := NewProcessor(database, zap.NewNop(), crawler.URL, nil)
	project := models.Project{ID: bson.NewObjectID(), Name: "SPA", URL: site.URL + "/", Active: true, UseJS: false}
	if _, err := database.Collection("projects").InsertOne(ctx, project); err != nil {
		t.Fatal(err)
	}
	// A bot asked for a page nothing else names.
	if _, err := database.Collection("prerender_data").InsertOne(ctx, models.PrerenderData{
		ID: bson.NewObjectID(), ProjectID: project.ID, URL: site.URL + "/only-bots-know", StatusCode: 200,
		RequesterType: models.RequesterTypeAI, CachedAt: time.Now().UTC(),
	}); err != nil {
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
	r := audit.Rendering
	if r == nil || !r.NeedsJS || r.Mode != "js" || r.JSWords <= r.HTMLWords {
		t.Fatalf("rendering probe: %+v", r)
	}
	// Besides the probe's own plain reads and the 404 probe, every page is
	// read with JavaScript.
	if int(plainReads.Load()) != len(r.Pages)+1 {
		t.Fatalf("plain reads = %d, probe pages = %d", plainReads.Load(), len(r.Pages))
	}
	var botURL models.SiteURL
	if err := database.Collection("site_urls").FindOne(ctx, bson.M{"project_id": project.ID, "url": normalizePageURL(site.URL + "/only-bots-know")}).Decode(&botURL); err != nil {
		t.Fatalf("a page bots ask for should be a known URL: %v", err)
	}
	if n, _ := database.Collection("page_profiles").CountDocuments(ctx, bson.M{"project_id": project.ID, "url": normalizePageURL(site.URL + "/only-bots-know")}); n != 1 {
		t.Fatal("a page bots ask for should be read")
	}
}

func TestStripScriptsKeepsStructuredData(t *testing.T) {
	src := `<!doctype html><html><head><title>T &amp; S</title>` +
		`<link rel="modulepreload" href="/a.js"><link rel="preload" as="script" href="/b.js"><link rel="stylesheet" href="/s.css">` +
		`<script src="/app.js"></script><script>if (a < b) { document.write("</div>") }</script>` +
		`<SCRIPT TYPE="application/ld+json">{"@type":"Product"}</SCRIPT></head>` +
		`<body data-x="1"><div>Text <b>here</b></div><script type="module">boot()</script></body></html>`
	want := `<!doctype html><html><head><title>T &amp; S</title><link rel="stylesheet" href="/s.css">` +
		`<SCRIPT TYPE="application/ld+json">{"@type":"Product"}</SCRIPT></head>` +
		`<body data-x="1"><div>Text <b>here</b></div></body></html>`
	if got := stripScripts(src); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestRobotsForOneCrawler(t *testing.T) {
	body := "User-agent: *\nDisallow: /private\n\nUser-agent: GPTBot\nUser-agent: CCBot\nDisallow: /\n\nUser-agent: ClaudeBot\nDisallow:\n"
	rules := parseRobots(body)
	got := map[string]bool{}
	for _, a := range aiAccess(rules, "https://x.hu/") {
		got[a.Agent] = a.Allowed
	}
	if got["GPTBot"] || got["CCBot"] {
		t.Fatalf("a crawler disallowed by name is blocked: %v", got)
	}
	if !got["ClaudeBot"] || !got["PerplexityBot"] {
		t.Fatalf("an empty Disallow and the * group allow the home page: %v", got)
	}
	if parseRobotsFor(body, "ClaudeBot").allowed("https://x.hu/private") != true {
		t.Fatal("a crawler with its own group ignores the * group")
	}
	if rules.allowed("https://x.hu/private") {
		t.Fatal("the * group still applies to others")
	}
}

func TestFindBotTraps(t *testing.T) {
	prof := func(key string, canonical string, noindex bool, impressions float64) *models.PageProfile {
		return &models.PageProfile{URL: key, Canonical: canonical, Noindex: noindex, Impressions: impressions}
	}
	stats := []botPageStat{
		{Key: "https://x.hu/gone", URL: "https://x.hu/gone", Requests: 9, Status: 404},
		{Key: "https://x.hu/once", URL: "https://x.hu/once", Requests: 1, Status: 404},
		{Key: "https://x.hu/a?color=red", URL: "https://x.hu/a?color=red", Requests: 40, Status: 200},
		{Key: "https://x.hu/a?page=2", URL: "https://x.hu/a?page=2", Requests: 30, Status: 200},
		{Key: "https://x.hu/earns?x=1", URL: "https://x.hu/earns?x=1", Requests: 5, Status: 200},
		{Key: "https://x.hu/hidden", URL: "https://x.hu/hidden", Requests: 20, Status: 200},
		{Key: "https://x.hu/dup", URL: "https://x.hu/dup", Requests: 10, Status: 200},
		{Key: "https://x.hu/good", URL: "https://x.hu/good", Requests: 30, Status: 200},
	}
	profiles := map[string]*models.PageProfile{
		"https://x.hu/earns?x=1": prof("https://x.hu/earns?x=1", "", false, 100),
		"https://x.hu/hidden":    prof("https://x.hu/hidden", "", true, 0),
		"https://x.hu/dup":       prof("https://x.hu/dup", "https://x.hu/good", false, 0),
		"https://x.hu/good":      prof("https://x.hu/good", "https://x.hu/good", false, 50),
	}
	got := findBotTraps(stats, profiles)
	if len(got.Errors) != 1 || got.Errors[0].Key != "https://x.hu/gone" || got.ErrorRequests != 9 {
		t.Fatalf("errors: %+v", got.Errors)
	}
	if len(got.Waste) != 3 || got.WasteRequests != 70 || got.Waste[0].Requests != 40 || !got.wasteIsATask() {
		t.Fatalf("waste: %+v (%d)", got.Waste, got.WasteRequests)
	}
	small := findBotTraps(stats[2:3], nil)
	if small.wasteIsATask() {
		t.Fatal("a few requests are no task")
	}
}

func TestTopImpressionKeys(t *testing.T) {
	got := topImpressionKeys(map[string]float64{"a": 1, "b": 9, "c": 5}, 2)
	if len(got) != 2 || !got["b"] || !got["c"] {
		t.Fatalf("%v", got)
	}
}

func TestIntegrationFinishedTaskRefreshesItsCachedPages(t *testing.T) {
	database := integrationDB(t)
	ctx := context.Background()
	p := NewProcessor(database, zap.NewNop(), "http://127.0.0.1:1", nil)
	project := bson.NewObjectID()
	now := time.Now().UTC()
	fresh := now.Add(time.Hour)
	for _, page := range []string{"https://x.hu/a", "https://x.hu/other"} {
		if _, err := database.Collection("cache_entries").InsertOne(ctx, models.CacheEntry{
			ID: bson.NewObjectID(), ProjectID: project, Key: CacheKey(page), URL: page, FullHTML: "<html></html>", ExpiresAt: fresh, PurgeAt: fresh,
		}); err != nil {
			t.Fatal(err)
		}
	}
	p.refreshTaskPages(ctx, &models.SEOTask{ProjectID: project, Pages: []string{"https://www.x.hu/a/"}}, now)
	a, _ := LookupCache(ctx, database, project, "https://x.hu/a")
	other, _ := LookupCache(ctx, database, project, "https://x.hu/other")
	if a.ExpiresAt.After(now.Add(time.Second)) {
		t.Fatalf("the changed page should be stale: %v", a.ExpiresAt)
	}
	if !other.ExpiresAt.After(now) {
		t.Fatal("other pages stay fresh")
	}
}

func TestIntegrationWarmCacheOnlyForUsedPrerender(t *testing.T) {
	database := integrationDB(t)
	ctx := context.Background()
	p := NewProcessor(database, zap.NewNop(), "http://127.0.0.1:1", nil)
	project := &models.Project{ID: bson.NewObjectID(), URL: "https://x.hu/"}
	now := time.Now().UTC()
	impressions := map[string]float64{"https://x.hu/a": 10}
	if got := p.warmSet(ctx, project, impressions, now); got != nil {
		t.Fatalf("no bots use /prerender yet: %v", got)
	}
	if _, err := database.Collection("prerender_data").InsertOne(ctx, models.PrerenderData{ID: bson.NewObjectID(), ProjectID: project.ID, RequesterType: models.RequesterTypeBot, CachedAt: now}); err != nil {
		t.Fatal(err)
	}
	if got := p.warmSet(ctx, project, impressions, now); !got["https://x.hu/a"] {
		t.Fatalf("warm set: %v", got)
	}
	p.warmCache(ctx, project.ID, "https://x.hu/a", &renderedPage{HTML: "<html><script>x()</script><p>Hi</p></html>", Status: 200})
	p.warmCache(ctx, project.ID, "https://x.hu/gone", &renderedPage{HTML: "<html></html>", Status: 404})
	e, _ := LookupCache(ctx, database, project.ID, "https://x.hu/a")
	if e == nil || e.Source != models.CacheSourceWarm || strings.Contains(e.FullHTML, "<script") {
		t.Fatalf("warm entry: %+v", e)
	}
	if gone, _ := LookupCache(ctx, database, project.ID, "https://x.hu/gone"); gone != nil {
		t.Fatal("an error page is not warmed")
	}
}

func TestCrawlMode(t *testing.T) {
	cases := []struct {
		projectUsesJS, needsJS bool
		want                   string
	}{
		{false, false, renderModeHTML},
		{false, true, renderModeJS},
		{true, false, renderModeJS}, // the project's choice stands
		{true, true, renderModeJS},
	}
	for _, c := range cases {
		if got := crawlMode(c.projectUsesJS, c.needsJS); got != c.want {
			t.Errorf("crawlMode(%v, %v) = %q, want %q", c.projectUsesJS, c.needsJS, got, c.want)
		}
	}
}
