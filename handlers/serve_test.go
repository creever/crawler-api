package handlers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.uber.org/zap"

	"github.com/creever/crawler-api/db"
	"github.com/creever/crawler-api/models"
	"github.com/creever/crawler-api/worker"
)

func TestAgentNames(t *testing.T) {
	cases := map[string]string{
		"Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko); compatible; GPTBot/1.2; +https://openai.com/gptbot": "GPTBot",
		"Mozilla/5.0 (compatible; ClaudeBot/1.0; +claudebot@anthropic.com)":                                      "ClaudeBot",
		"Mozilla/5.0 (Linux; Android 6.0.1) Chrome/120 Mobile Safari/537.36 (compatible; Googlebot/2.1)":         "Googlebot",
		"Mozilla/5.0 (compatible; PerplexityBot/1.0)":                                                            "PerplexityBot",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/120 Safari/537.36":                                     "",
	}
	for ua, want := range cases {
		if got := agentName(ua); got != want {
			t.Errorf("agentName(%q) = %q, want %q", ua, got, want)
		}
	}
	if classifyRequester("Mozilla/5.0 (compatible; ClaudeBot/1.0)") != models.RequesterTypeAI {
		t.Error("ClaudeBot is an AI crawler")
	}
}

func testDB(t *testing.T) *mongo.Database {
	t.Helper()
	uri := os.Getenv("TEST_MONGO_URI")
	if uri == "" {
		t.Skip("TEST_MONGO_URI not set")
	}
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatal(err)
	}
	database := client.Database(fmt.Sprintf("handlers_test_%d", time.Now().UnixNano()))
	t.Cleanup(func() {
		_ = database.Drop(context.Background())
		_ = client.Disconnect(context.Background())
	})
	db.EnsureIndexes(database, zap.NewNop())
	return database
}

func TestIntegrationServe(t *testing.T) {
	database := testDB(t)
	ctx := context.Background()
	renders := 0
	crawler := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		renders++
		if r.URL.Query().Get("url") == "https://shop.example/missing" {
			w.Header().Set("X-Prerender-Status", "404")
		}
		fmt.Fprintf(w, "<html><body>rendered %s</body></html>", r.URL.Query().Get("url"))
	}))
	defer crawler.Close()
	project := models.Project{ID: bson.NewObjectID(), URL: "https://shop.example", Active: true, UseJS: true}
	if _, err := database.Collection("projects").InsertOne(ctx, project); err != nil {
		t.Fatal(err)
	}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/prerender", NewServeHandler(database, nil, worker.NewProcessor(database, zap.NewNop(), crawler.URL, nil)).Serve)
	get := func(page string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/prerender?url="+url.QueryEscape(page), nil)
		req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; GPTBot/1.2)")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	first := get("https://www.shop.example/a/")
	if first.Code != 200 || first.Header().Get("X-Prerender-Cache") != models.CacheStateMiss || renders != 1 {
		t.Fatalf("miss: %d %v renders=%d", first.Code, first.Header(), renders)
	}
	second := get("https://shop.example/a")
	if second.Header().Get("X-Prerender-Cache") != models.CacheStateHit || renders != 1 || second.Body.String() != first.Body.String() {
		t.Fatalf("hit: %v renders=%d", second.Header(), renders)
	}
	missing := get("https://shop.example/missing")
	if missing.Header().Get("X-Prerender-Status") != "404" {
		t.Fatalf("the page's own status should pass through: %v", missing.Header())
	}
	if rec := get("https://other.example/a"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown host: %d", rec.Code)
	}

	// The request log names the crawler and how it was answered.
	deadline := time.Now().Add(2 * time.Second)
	for {
		n, _ := database.Collection("prerender_data").CountDocuments(ctx, bson.M{"project_id": project.ID, "agent": "GPTBot", "requester_type": "ai"})
		if n == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("logged %d of 3 requests", n)
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Spellings of one page are one row among the pages bots ask for.
	get("https://shop.example/a/")
	deadline = time.Now().Add(2 * time.Second)
	for {
		n, _ := database.Collection("prerender_data").CountDocuments(ctx, bson.M{"project_id": project.ID})
		if n == 4 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("logged %d of 4 requests", n)
		}
		time.Sleep(50 * time.Millisecond)
	}
	stats, err := buildPrerenderStats(ctx, database, project.ID, 14, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Total != 4 || stats.Hits != 2 || stats.Misses != 2 || stats.ByRequester["ai"] != 4 || stats.ErrorStatus != 1 ||
		len(stats.Agents) != 1 || stats.Agents[0].Agent != "GPTBot" || stats.Cache.Entries != 2 || len(stats.Daily) != 14 ||
		len(stats.TopPages) != 2 || stats.TopPages[0].Requests != 3 {
		t.Fatalf("stats: %+v", stats)
	}
}

func TestSecretsAreNeverReadBack(t *testing.T) {
	for key, secret := range map[string]bool{"smtp_password": true, "anthropic_api_key": true, "mcp_token_hash": true, "smtp_host": false, "ai_model": false} {
		got := maskSecret(key, "value")
		if secret != (got == settingMask) {
			t.Errorf("%s: %q", key, got)
		}
	}
	if maskSecret("smtp_password", "") != "" {
		t.Error("an unset secret stays empty")
	}
}
