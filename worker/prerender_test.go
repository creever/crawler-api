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
		{models.RenderingProbe{HTMLWords: 1000, JSWords: 1000, HTMLLinks: 10, JSLinks: 80}, true}, // links come from JS
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
