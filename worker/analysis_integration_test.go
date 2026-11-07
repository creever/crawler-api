package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.uber.org/zap"

	"github.com/creever/crawler-api/db"
	"github.com/creever/crawler-api/models"
)

// llmStub answers Mistral calls and leaves other requests to the real client.
type llmStub struct {
	answer func(prompt string) string
	calls  []string
	next   http.RoundTripper
}

func (s *llmStub) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != "api.mistral.ai" {
		return s.next.RoundTrip(r)
	}
	var body struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return nil, err
	}
	prompt := body.Messages[len(body.Messages)-1].Content
	s.calls = append(s.calls, prompt)
	reply, _ := json.Marshal(map[string]interface{}{"choices": []interface{}{
		map[string]interface{}{"message": map[string]string{"content": s.answer(prompt)}},
	}})
	return &http.Response{StatusCode: 200, Status: "200 OK", Body: io.NopCloser(strings.NewReader(string(reply))), Header: http.Header{}}, nil
}

// integrationDB returns an empty database with the real indexes. It needs a
// MongoDB at TEST_MONGO_URI, and the test is skipped without one.
func integrationDB(t *testing.T) *mongo.Database {
	t.Helper()
	uri := os.Getenv("TEST_MONGO_URI")
	if uri == "" {
		t.Skip("TEST_MONGO_URI not set")
	}
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatal(err)
	}
	database := client.Database(fmt.Sprintf("analysis_test_%d", time.Now().UnixNano()))
	t.Cleanup(func() {
		_ = database.Drop(context.Background())
		_ = client.Disconnect(context.Background())
	})
	db.EnsureIndexes(database, zap.NewNop())
	return database
}

func gscRow(project bson.ObjectID, query, page string, day time.Time, clicks, impressions int, pos float64) interface{} {
	return models.GSCRow{
		ID: bson.NewObjectID(), ProjectID: project, Query: query, Page: page,
		Date: day.Format("2006-01-02"), Device: "MOBILE", Clicks: clicks, Impressions: impressions, Position: pos,
	}
}

func TestIntegrationBulkRun(t *testing.T) {
	database := integrationDB(t)
	ctx := context.Background()

	fixture, err := os.ReadFile("testdata/cipomarket_category.html")
	if err != nil {
		t.Fatal(err)
	}
	renders := map[string]int{}
	crawler := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target := r.URL.Query().Get("url")
		renders[target]++
		switch {
		case r.URL.Path == "/robots.txt":
			fmt.Fprint(w, "User-agent: *\nCrawl-delay: 0.01\n")
		case strings.Contains(target, "/torolt"):
			w.Header().Set("X-Prerender-Status", "404")
			fmt.Fprint(w, "<html><body>nincs</body></html>")
		case strings.Contains(target, "/ures"):
			fmt.Fprint(w, `<html><body><div id="app"></div></body></html>`)
		default:
			fmt.Fprint(w, string(fixture))
		}
	}))
	defer crawler.Close()

	stub := &llmStub{next: http.DefaultTransport, answer: func(string) string {
		r := goodResponse()
		b, _ := json.Marshal(r)
		return string(b)
	}}
	p := NewProcessor(database, zap.NewNop(), crawler.URL, nil)
	p.httpClient = &http.Client{Transport: stub}

	projectID := bson.NewObjectID()
	const base = "https://bolt.example"
	main, rival := base+"/noi/bokacsizma", base+"/noi/csizma"
	if _, err = database.Collection("projects").InsertOne(ctx, models.Project{ID: projectID, Name: "Cipomarket", URL: crawler.URL, Active: true}); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{"ai_provider": "mistral", "mistral_api_key": "test"} {
		if _, err = database.Collection("settings").InsertOne(ctx, bson.M{"key": k, "value": v}); err != nil {
			t.Fatal(err)
		}
	}

	day := time.Now().UTC().AddDate(0, 0, -gscDataLagDays-1)
	rows := []interface{}{
		// The stronger page and a weaker one compete for one query.
		gscRow(projectID, "bokacsizma", main, day, 12, 200, 8),
		gscRow(projectID, "bokacsizma", rival, day, 3, 150, 11),
		gscRow(projectID, "bélelt bokacsizma", main, day, 1, 60, 9),
		// Pages that must not reach the LLM.
		gscRow(projectID, "torolt", base+"/torolt", day, 4, 80, 7),
		gscRow(projectID, "ures", base+"/ures", day, 4, 80, 7),
	}
	if _, err = database.Collection("gsc_rows").InsertMany(ctx, rows); err != nil {
		t.Fatal(err)
	}
	if _, err = database.Collection("ga4_rows").InsertOne(ctx, models.GA4Row{
		ID: bson.NewObjectID(), ProjectID: projectID, Page: "/noi/bokacsizma", Date: day.Format(GA4DateLayout), Sessions: 50, Conversions: 2, Revenue: 40000,
	}); err != nil {
		t.Fatal(err)
	}

	payload, _ := json.Marshal(ComputeOppsPayload{ProjectID: projectID.Hex()})
	if err = p.HandleComputeOpportunities(ctx, asynq.NewTask(TypeComputeOpps, payload)); err != nil {
		t.Fatal(err)
	}
	var cannib models.Opportunity
	if err = database.Collection("opportunities").FindOne(ctx, bson.M{"project_id": projectID, "type": models.OppCannibalization}).Decode(&cannib); err != nil {
		t.Fatalf("no cannibalization opportunity: %v", err)
	}
	if cannib.Page != main || len(cannib.Pages) != 2 || cannib.Pages[1] != rival {
		t.Fatalf("cannibalization target = %q, pages = %v", cannib.Page, cannib.Pages)
	}

	run := func() {
		t.Helper()
		body, _ := json.Marshal(GenSuggestPayload{ProjectID: projectID.Hex()})
		if err := p.HandleSuggestionsGenerate(ctx, asynq.NewTask(TypeGenSuggest, body)); err != nil {
			t.Fatal(err)
		}
	}
	run()

	byPage := map[string]models.PageAnalysis{}
	cursor, err := database.Collection("page_analyses").Find(ctx, bson.M{"project_id": projectID})
	if err != nil {
		t.Fatal(err)
	}
	var analyses []models.PageAnalysis
	if err = cursor.All(ctx, &analyses); err != nil {
		t.Fatal(err)
	}
	for _, a := range analyses {
		byPage[a.Page] = a
	}

	if a := byPage[main]; a.Status != models.AnalysisStatusCompleted || !strings.Contains(a.Prompt, "A télről, hidegről") ||
		!strings.Contains(a.Prompt, "bélelt bokacsizma") || !strings.Contains(a.Prompt, "Sessions 50") ||
		!strings.Contains(a.Prompt, "competes for query") || len(a.Coverage.PagesRead) != 2 {
		t.Errorf("main page analysis: status=%s pages=%v prompt:\n%.1500s", a.Status, a.Coverage.PagesRead, a.Prompt)
	}
	if a := byPage[base+"/torolt"]; a.Status != models.AnalysisStatusCompleted || a.Prompt != "" {
		t.Errorf("404 page: %+v", a)
	}
	if a := byPage[base+"/ures"]; a.Status != models.AnalysisStatusInsufficientContext || a.Prompt != "" {
		t.Errorf("empty page: %+v", a)
	}
	for _, prompt := range stub.calls {
		if !strings.Contains(prompt, "Page under analysis") {
			t.Errorf("unexpected prompt %.200s", prompt)
		}
	}

	var sug models.Suggestion
	if err = database.Collection("suggestions").FindOne(ctx, bson.M{"project_id": projectID, "page": main}).Decode(&sug); err != nil {
		t.Fatal(err)
	}
	if sug.AnalysisID == nil || !strings.HasPrefix(sug.CurrentValue, "A télről") {
		t.Errorf("suggestion = %+v", sug)
	}

	// The second run must not repeat work: main has a pending suggestion, the
	// others are unchanged since their analysis or have no analysis to reuse.
	callsBefore := len(stub.calls)
	if _, err = database.Collection("suggestions").UpdateMany(ctx, bson.M{"project_id": projectID}, bson.M{"$set": bson.M{"status": models.SuggestionStatusPending}}); err != nil {
		t.Fatal(err)
	}
	run()
	if len(stub.calls) != callsBefore {
		t.Errorf("second run made %d LLM calls", len(stub.calls)-callsBefore)
	}

	// With the review finished and nothing changed on the page, it stays out too.
	if _, err = database.Collection("suggestions").UpdateMany(ctx, bson.M{"project_id": projectID}, bson.M{"$set": bson.M{"status": models.SuggestionStatusRejected}}); err != nil {
		t.Fatal(err)
	}
	run()
	if len(stub.calls) != callsBefore {
		t.Errorf("unchanged page was analyzed again")
	}

	count, err := database.Collection("suggestion_runs").CountDocuments(ctx, bson.M{"project_id": projectID})
	if err != nil || count != 1 {
		t.Errorf("suggestion_runs = %d, %v", count, err)
	}
}

func TestIntegrationSingleOpportunityWithoutPage(t *testing.T) {
	database := integrationDB(t)
	ctx := context.Background()
	stub := &llmStub{next: http.DefaultTransport, answer: func(string) string { return "{}" }}
	p := NewProcessor(database, zap.NewNop(), "http://127.0.0.1:1", nil)
	p.httpClient = &http.Client{Transport: stub, Timeout: 2 * time.Second}

	projectID, oppID := bson.NewObjectID(), bson.NewObjectID()
	_, _ = database.Collection("projects").InsertOne(ctx, models.Project{ID: projectID, Name: "X", URL: "https://x.example", Active: true})
	_, _ = database.Collection("settings").InsertOne(ctx, bson.M{"key": "ai_provider", "value": "mistral"})
	_, _ = database.Collection("settings").InsertOne(ctx, bson.M{"key": "mistral_api_key", "value": "test"})
	// A cannibalization document stored before pages were recorded.
	_, _ = database.Collection("opportunities").InsertOne(ctx, models.Opportunity{ID: oppID, ProjectID: projectID, Type: models.OppCannibalization, Query: "q"})

	body, _ := json.Marshal(GenSuggestPayload{ProjectID: projectID.Hex(), OpportunityID: oppID.Hex()})
	if err := p.HandleSuggestionsGenerate(ctx, asynq.NewTask(TypeGenSuggest, body)); err != nil {
		t.Fatal(err)
	}
	var a models.PageAnalysis
	if err := database.Collection("page_analyses").FindOne(ctx, bson.M{"project_id": projectID}).Decode(&a); err != nil {
		t.Fatal(err)
	}
	if a.Status != models.AnalysisStatusFailed || len(stub.calls) != 0 {
		t.Errorf("status = %s, llm calls = %d", a.Status, len(stub.calls))
	}
}
