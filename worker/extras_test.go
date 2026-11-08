package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/creever/crawler-api/models"
)

func TestVitalsTargetsTakeTheStrongestPageOfEachType(t *testing.T) {
	got := vitalsTargets([]models.PageProfile{
		{URL: "a", Group: "product", StatusCode: 200}, {URL: "b", Group: "product", StatusCode: 200},
		{URL: "gone", Group: "listing", StatusCode: 404}, {URL: "hidden", Group: "listing", StatusCode: 200, Noindex: true},
		{URL: "c", Group: "listing", StatusCode: 200}, {URL: "d", Group: "article", StatusCode: 200},
	}, 2)
	if len(got) != 2 || got[0].URL != "a" || got[1].URL != "c" {
		t.Fatalf("%+v", got)
	}
}

func TestVitalsFromPageSpeed(t *testing.T) {
	var r psiResponse
	raw := `{"loadingExperience":{"overall_category":"AVERAGE","origin_fallback":true,"metrics":{
		"LARGEST_CONTENTFUL_PAINT_MS":{"percentile":3100},"INTERACTION_TO_NEXT_PAINT":{"percentile":180},"CUMULATIVE_LAYOUT_SHIFT_SCORE":{"percentile":12}}},
		"lighthouseResult":{"categories":{"performance":{"score":0.61}},"audits":{"largest-contentful-paint":{"numericValue":4200},"total-blocking-time":{"numericValue":310},"cumulative-layout-shift":{"numericValue":0.08}}}}`
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatal(err)
	}
	var v models.PageVitals
	vitalsFromPSI(&r, &v)
	if v.Field == nil || v.Field.LCPMs != 3100 || v.Field.INPMs != 180 || v.Field.CLS != 0.12 || v.FieldOverall != "AVERAGE" || !v.OriginFallback {
		t.Fatalf("field: %+v %+v", v.Field, v)
	}
	if v.LabScore != 0.61 || v.LabLCPMs != 4200 || v.LabTBTMs != 310 || v.LabCLS != 0.08 {
		t.Fatalf("lab: %+v", v)
	}
	var empty psiResponse
	var none models.PageVitals
	vitalsFromPSI(&empty, &none)
	if none.Field != nil {
		t.Fatal("no field data means no field values")
	}
}

func TestGrowingIssues(t *testing.T) {
	got := growingIssues(map[string]int{"a": 5, "b": 10, "c": 2}, map[string]int{"a": 1, "b": 10})
	if len(got) != 2 || got[0].Code != "a" || got[0].Added != 4 || got[1].Code != "c" {
		t.Fatalf("%+v", got)
	}
}

func TestRenderDigestEscapesAndSkipsEmptySections(t *testing.T) {
	d := RenderDigest(&DigestData{Project: "A<b>", HasAudit: true, Score: 0.9, HasPrev: true, PrevScore: 0.85, Critical: 1,
		Growing: []DigestIssue{{Code: "broken_links", Pages: 7, Added: 3}}, OpenTasks: 4, ToReview: 2})
	if strings.Contains(d.HTML, "A<b>") || !strings.Contains(d.HTML, "A&lt;b&gt;") {
		t.Fatalf("html not escaped: %s", d.HTML)
	}
	for _, want := range []string{"Score 90%, +5 points", "Growing: broken links on 7 pages (+3)", "4 open tasks, 2 waiting"} {
		if !strings.Contains(d.Text, want) {
			t.Fatalf("missing %q in\n%s", want, d.Text)
		}
	}
	if strings.Contains(d.Text, "Search,") || strings.Contains(d.Text, "Bots,") {
		t.Fatalf("empty sections should go:\n%s", d.Text)
	}
}

func TestDigestMessageCannotInjectHeaders(t *testing.T) {
	msg := string(digestMessage("a@x.hu", "b@x.hu\r\nBcc: evil@x.hu", &Digest{Subject: "S\r\nBcc: evil@x.hu", Text: "t", HTML: "<p>h</p>"}))
	if strings.Contains(msg, "\r\nBcc:") {
		t.Fatalf("header injected:\n%s", msg)
	}
}

func TestDigestDue(t *testing.T) {
	if !digestDue(time.Date(2026, 11, 9, 8, 0, 0, 0, time.UTC)) {
		t.Fatal("Monday 08:00 is due")
	}
	if digestDue(time.Date(2026, 11, 9, 6, 0, 0, 0, time.UTC)) || digestDue(time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)) {
		t.Fatal("only Monday after 07:00")
	}
}

func TestIntegrationDigestGoesToTheOwner(t *testing.T) {
	database := integrationDB(t)
	ctx := context.Background()
	owner := models.User{ID: bson.NewObjectID(), Email: "owner@x.hu"}
	project := &models.Project{ID: bson.NewObjectID(), OwnerID: owner.ID, Name: "Shop"}
	if _, err := database.Collection("users").InsertOne(ctx, owner); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := SendDigest(ctx, database, project, now); err != ErrEmailNotConfigured {
		t.Fatalf("without SMTP settings: %v", err)
	}
	for k, v := range map[string]string{"smtp_host": "mail.x.hu", "smtp_from": "seo@x.hu"} {
		if _, err := database.Collection("settings").InsertOne(ctx, models.Setting{Key: k, Value: v}); err != nil {
			t.Fatal(err)
		}
	}
	orig := sendMail
	defer func() { sendMail = orig }()
	var sentTo, sentMsg string
	sendMail = func(c *smtpConfig, to string, msg []byte) error { sentTo, sentMsg = to, string(msg); return nil }
	if err := SendDigest(ctx, database, project, now); err != nil {
		t.Fatal(err)
	}
	if sentTo != "owner@x.hu" || !strings.Contains(sentMsg, "Weekly SEO summary") {
		t.Fatalf("to=%q msg=%s", sentTo, sentMsg)
	}
	if last := LastDigestAt(ctx, database, project.ID); last == nil {
		t.Fatal("a sent summary is recorded")
	}
}

func TestIntegrationBunnyPurge(t *testing.T) {
	database := integrationDB(t)
	ctx := context.Background()
	var mu sync.Mutex
	var calls []string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery+" key="+r.Header.Get("AccessKey"))
		mu.Unlock()
		switch {
		case r.Header.Get("AccessKey") == "bad":
			w.WriteHeader(http.StatusUnauthorized)
		case strings.HasPrefix(r.URL.Path, "/pullzone/404"):
			w.WriteHeader(http.StatusNotFound)
		case strings.HasSuffix(r.URL.Path, "/purgeCache"):
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer stub.Close()
	orig := bunnyAPIBase
	bunnyAPIBase = stub.URL
	defer func() { bunnyAPIBase = orig }()

	none := &models.Project{ID: bson.NewObjectID()}
	if rec, err := PurgeCDN(ctx, database, none, []string{"https://x.hu/a"}, CDNTriggerTask); rec != nil || err != nil || len(calls) != 0 {
		t.Fatalf("a project without a CDN makes no calls: %v %v %v", rec, err, calls)
	}
	if err := CheckCDN(ctx, models.ProjectCDN{APIKey: "bad"}); err != ErrCDNKeyRefused {
		t.Fatalf("bad key: %v", err)
	}
	if err := CheckCDN(ctx, models.ProjectCDN{APIKey: "k", PullZoneID: 404}); err != ErrCDNZoneNotFound {
		t.Fatalf("bad zone: %v", err)
	}

	project := &models.Project{ID: bson.NewObjectID(), CDN: models.ProjectCDN{Provider: models.CDNProviderBunny, APIKey: "k", PullZoneID: 7}}
	calls = nil
	rec, err := PurgeCDN(ctx, database, project, []string{"https://x.hu/"}, CDNTriggerTask)
	if err != nil || rec.URLs != 1 || rec.Failed != 0 {
		t.Fatalf("%+v %v", rec, err)
	}
	if len(calls) != 1 || !strings.HasPrefix(calls[0], "POST /purge?") || !strings.Contains(calls[0], "exactPath=true") || !strings.HasSuffix(calls[0], "key=k") {
		t.Fatalf("purge call: %v", calls)
	}
	calls = nil
	if _, err = PurgeCDNZone(ctx, database, project); err != nil || len(calls) != 1 || !strings.HasPrefix(calls[0], "POST /pullzone/7/purgeCache") {
		t.Fatalf("zone: %v %v", err, calls)
	}
	if n, _ := database.Collection("cdn_purges").CountDocuments(ctx, bson.M{"project_id": project.ID}); n != 2 {
		t.Fatalf("purges recorded: %d", n)
	}
	// A failing URL is counted, and the rest still go.
	project.CDN.APIKey = "bad"
	rec, err = PurgeCDN(ctx, database, project, []string{"https://x.hu/a", "https://x.hu/b"}, CDNTriggerManual)
	if err == nil || rec.Failed != 2 || rec.Error == "" {
		t.Fatalf("%+v %v", rec, err)
	}
}
