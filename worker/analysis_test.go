package worker

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/creever/crawler-api/models"
)

const testPage = "https://cipomarket.hu/noi/bokacsizma"

func testInput(t *testing.T) contextInput {
	t.Helper()
	return contextInput{
		Brand: "Cipomarket", Page: testPage, Snapshot: cipomarketSnapshot(t),
		Opportunities: []models.Opportunity{{
			Type: models.OppStrikingDistance, Query: "női bokacsizma", Page: testPage,
			Metrics: map[string]float64{"impressions": 900, "position": 8.4},
		}},
		Queries: []pageQueryStat{
			{Query: "női bokacsizma", Impressions: 900, Clicks: 12, Position: 8.4, PrevImpressions: 700, PrevClicks: 10, PrevPosition: 9},
			{Query: "bélelt bokacsizma", Impressions: 300, Clicks: 2, Position: 12},
		},
		Devices: []pageDeviceStat{{Device: "MOBILE", Impressions: 800, Clicks: 9, Position: 9}},
		GA4:     &ga4PageStat{Sessions: 420, Conversions: 6, Revenue: 210000},
	}
}

func TestContextPackHoldsRealPageContent(t *testing.T) {
	pack := buildContextPack(testInput(t))
	for _, want := range []string{
		"A télről, hidegről kinek ne jutna", // existing category text, at the very end of the page
		"Skechers női bokacsizma",           // listing sample
		"Items on this page: 60",
		"bélelt bokacsizma", // second query
		"Sessions 420",
	} {
		if !strings.Contains(pack.Text, want) {
			t.Errorf("context lacks %q", want)
		}
	}
	for _, unwanted := range []string{"Népszerű márkák", "Bejelentkezés", `"Rieker"`} {
		if strings.Contains(pack.Text, unwanted) {
			t.Errorf("context contains site chrome %q", unwanted)
		}
	}
	if _, ok := pack.Facts["C1"]; !ok {
		t.Error("category text has no fact ID")
	}
	if pack.Coverage.GSCQueries != 2 || !pack.Coverage.HasGA4 {
		t.Errorf("coverage = %+v", pack.Coverage)
	}
}

func TestContextPackCapsSignalsPerType(t *testing.T) {
	in := testInput(t)
	in.Opportunities = nil
	for i := 0; i < 12; i++ {
		in.Opportunities = append(in.Opportunities, models.Opportunity{Type: models.OppCannibalization, Query: "q", Page: testPage, Details: strings.Repeat("x", 2000)})
	}
	in.Opportunities = append(in.Opportunities, models.Opportunity{Type: models.OppLosingClicks, Page: testPage})
	pack := buildContextPack(in)
	if _, ok := pack.Facts["S6"]; !ok {
		t.Error("the other signal type must keep its place")
	}
	if _, ok := pack.Facts["S7"]; ok {
		t.Error("signals were not capped")
	}
	if !strings.Contains(pack.Text, "cannibalization: 12 signals in total") || !containsString(pack.Coverage.Truncated, "signals: cannibalization") {
		t.Errorf("truncation not reported: %v", pack.Coverage.Truncated)
	}
	if len(pack.Facts["S1"]) > 400 {
		t.Errorf("signal details not clipped: %d", len(pack.Facts["S1"]))
	}
}

func TestContextPackWarnsAboutConversions(t *testing.T) {
	if !strings.Contains(buildContextPack(testInput(t)).Text, "misattributed") {
		t.Error("GA4 conversion warning missing")
	}
}

func TestValidateRejectsFalseLengthClaim(t *testing.T) {
	in := testInput(t)
	pack := buildContextPack(in)
	r := goodResponse()
	r.Diagnosis.Evidence = append(r.Diagnosis.Evidence, struct {
		Ref   string `json:"ref"`
		Claim string `json:"claim"`
	}{"P3", "A meta leírás túl hosszú."})
	v := validateAnalysis(&r, pack, in.Snapshot)
	if len(v.Evidence) != 1 || !strings.Contains(strings.Join(v.Problems, ";"), "contradicts") {
		t.Errorf("evidence = %+v, problems = %v", v.Evidence, v.Problems)
	}

	over := in
	snap := *in.Snapshot
	snap.MetaDescription = strings.Repeat("a", 200)
	over.Snapshot = &snap
	v = validateAnalysis(&r, buildContextPack(over), &snap)
	if strings.Contains(strings.Join(v.Problems, ";"), "contradicts") {
		t.Errorf("a true claim was rejected: %v", v.Problems)
	}
}

func TestContextPackReportsTruncation(t *testing.T) {
	in := testInput(t)
	in.Snapshot.Prose = []string{strings.Repeat("hosszú szöveg ", 1000)}
	in.Snapshot.CategoryText = ""
	pack := buildContextPack(in)
	if !containsString(pack.Coverage.Truncated, "page text") {
		t.Errorf("truncated = %v", pack.Coverage.Truncated)
	}
}

func TestGateBlocksLLM(t *testing.T) {
	ok := &models.PageSnapshot{StatusCode: 200, ContentWordCount: 100}
	cases := []struct {
		name   string
		page   string
		snap   *models.PageSnapshot
		err    error
		status models.AnalysisStatus
		kind   string
	}{
		{name: "no page", status: models.AnalysisStatusFailed},
		{name: "render failed", page: testPage, err: errors.New("crawler down"), status: models.AnalysisStatusFailed},
		{name: "404", page: testPage, snap: &models.PageSnapshot{StatusCode: 404}, status: models.AnalysisStatusCompleted, kind: models.SuggestionKindTechnical},
		{name: "noindex", page: testPage, snap: &models.PageSnapshot{StatusCode: 200, Robots: "NOINDEX, follow"}, status: models.AnalysisStatusCompleted, kind: models.SuggestionKindNoindex},
		{name: "empty shell", page: testPage, snap: &models.PageSnapshot{StatusCode: 200, LowConfidence: true}, status: models.AnalysisStatusInsufficientContext},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := gateAnalysis(c.page, c.snap, c.err)
			if o == nil {
				t.Fatal("expected the gate to stop the page")
			}
			if o.Analysis.Status != c.status {
				t.Errorf("status = %s, want %s", o.Analysis.Status, c.status)
			}
			if c.kind != "" && (len(o.Actions) != 1 || o.Actions[0].Kind != c.kind) {
				t.Errorf("actions = %+v", o.Actions)
			}
		})
	}
	if o := gateAnalysis(testPage, ok, nil); o != nil {
		t.Errorf("healthy page was stopped: %+v", o)
	}
}

func goodResponse() analysisResponse {
	var r analysisResponse
	r.Diagnosis.PrimaryIssue = "A kategóriaszöveg nem a bokacsizmáról szól."
	r.Diagnosis.RootCause = "A szöveg általánosan a csizmát tárgyalja."
	r.Diagnosis.Confidence = "high"
	r.Diagnosis.Evidence = append(r.Diagnosis.Evidence, struct {
		Ref   string `json:"ref"`
		Claim string `json:"claim"`
	}{"[C1]", "A szöveg a csizmáról szól."})
	r.Actions = []analysisAction{{
		Kind: models.SuggestionKindCategoryText, Priority: 1, Effort: "M",
		Proposed: "A női bokacsizma kínálatunkban Skechers modelleket is találsz.", Rationale: "Pontosabb.",
		EvidenceRefs: []string{"C1", "Q1"},
	}}
	return r
}

func TestValidateAcceptsGoodAnswer(t *testing.T) {
	in := testInput(t)
	pack := buildContextPack(in)
	r := goodResponse()
	v := validateAnalysis(&r, pack, in.Snapshot)
	if len(v.Problems) != 0 || len(v.Actions) != 1 {
		t.Fatalf("problems = %v, actions = %d", v.Problems, len(v.Actions))
	}
	if v.Evidence[0].Ref != "C1" || v.Evidence[0].Fact == "" {
		t.Errorf("evidence = %+v", v.Evidence)
	}
}

func TestValidateRejects(t *testing.T) {
	in := testInput(t)
	pack := buildContextPack(in)
	cases := []struct {
		name   string
		action analysisAction
		want   string
	}{
		{"unknown fact", analysisAction{Kind: "title", Proposed: "Női bokacsizma", EvidenceRefs: []string{"Q99"}}, "unknown fact"},
		{"no evidence", analysisAction{Kind: "title", Proposed: "Női bokacsizma"}, "no evidence_refs"},
		{"long title", analysisAction{Kind: "title", Proposed: strings.Repeat("a", 61), EvidenceRefs: []string{"Q1"}}, "limit is 60"},
		{"long meta", analysisAction{Kind: "meta_description", Proposed: strings.Repeat("a", 161), EvidenceRefs: []string{"Q1"}}, "limit is 160"},
		{"same title", analysisAction{Kind: "title", Proposed: in.Snapshot.Title, EvidenceRefs: []string{"Q1"}}, "identical"},
		{"foreign brand", analysisAction{Kind: "title", Proposed: "Női bokacsizma és Nike újdonságok", EvidenceRefs: []string{"Q1"}}, "Nike"},
		{"ignores existing text", analysisAction{Kind: "category_text", Proposed: "Új szöveg a témáról.", EvidenceRefs: []string{"Q1"}}, "C1"},
		{"invented threshold", analysisAction{Kind: "meta_description", Proposed: "Női bokacsizma akciósan 15.000 Ft felett!", EvidenceRefs: []string{"Q1"}}, "15.000"},
		{"free shipping", analysisAction{Kind: "meta_description", Proposed: "Női bokacsizma ingyenes szállítással és ingyenes kiszállítás minden rendelésre.", EvidenceRefs: []string{"Q1"}}, "free shipping"},
		{"unknown kind", analysisAction{Kind: "magic", Proposed: "x", EvidenceRefs: []string{"Q1"}}, "unknown kind"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := goodResponse()
			r.Actions = []analysisAction{c.action}
			v := validateAnalysis(&r, pack, in.Snapshot)
			if len(v.Actions) != 0 {
				t.Errorf("action survived: %+v", v.Actions)
			}
			if !strings.Contains(strings.Join(v.Problems, "; "), c.want) {
				t.Errorf("problems = %v, want %q", v.Problems, c.want)
			}
		})
	}
}

func TestActionAcceptsStructuredProposed(t *testing.T) {
	raw := `{"diagnosis":{"primary_issue":"x"},"actions":[
		{"kind":"internal_link","proposed":{"from":"/a","anchor":"Nike"},"target":{"url":"/b"},"evidence_refs":["Q1"]},
		{"kind":"title","proposed":"Sima szöveg","evidence_refs":["Q1"]},
		{"kind":"title","proposed":null,"evidence_refs":["Q1"]}]}`
	r, err := parseAnalysis(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Actions[0].Proposed; got != "anchor: Nike; from: /a" || !r.Actions[0].Structured {
		t.Errorf("proposed = %q, structured = %v", got, r.Actions[0].Structured)
	}
	if r.Actions[0].Target != "url: /b" {
		t.Errorf("target = %q", r.Actions[0].Target)
	}
	if r.Actions[1].Proposed != "Sima szöveg" || r.Actions[1].Structured {
		t.Errorf("plain text changed: %+v", r.Actions[1])
	}
	if r.Actions[2].Proposed != "" {
		t.Errorf("null = %q", r.Actions[2].Proposed)
	}
}

func TestStructuredTextIsNotPublishable(t *testing.T) {
	in := testInput(t)
	r := goodResponse()
	r.Actions = []analysisAction{
		{Kind: "title", Proposed: "cím: Női bokacsizma", Structured: true, EvidenceRefs: []string{"Q1"}},
		{Kind: "internal_link", Proposed: "from: /a; to: /b", Structured: true, EvidenceRefs: []string{"Q1"}},
	}
	v := validateAnalysis(&r, buildContextPack(in), in.Snapshot)
	if len(v.Actions) != 1 || v.Actions[0].Kind != "internal_link" {
		t.Errorf("actions = %+v, problems = %v", v.Actions, v.Problems)
	}
}

func TestHarmfulActions(t *testing.T) {
	const category = "https://cipomarket.hu/batz/papucs"
	listing := &models.PageSnapshot{SourceURL: category, CategoryText: "Szöveg", Listing: &models.ListingSummary{ItemCount: 60}}
	product := &models.PageSnapshot{SourceURL: "https://cipomarket.hu/cipo-nike-1", Prose: []string{"Nike cipő. A termék jelenleg nem elérhető."}}
	available := &models.PageSnapshot{SourceURL: "https://cipomarket.hu/cipo-nike-1", Prose: []string{"Nike cipő. Kosárba."}}
	cases := []struct {
		name string
		a    analysisAction
		snap *models.PageSnapshot
		want string
	}{
		{"301 on unavailable", analysisAction{Kind: "redirect_merge", Proposed: "301 a https://cipomarket.hu/nike/cipo oldalra"}, product, "unavailable"},
		{"canonical on unavailable", analysisAction{Kind: "canonical", Proposed: "A canonical mutasson a /nike/cipo oldalra"}, product, "unavailable"},
		{"listing into brand", analysisAction{Kind: "canonical", Proposed: "A canonicalt állítsd a https://cipomarket.hu/batz oldalra"}, listing, "/batz"},
		{"listing into target", analysisAction{Kind: "redirect_merge", Proposed: "Egyesítsd.", Target: "/batz/noi/papucs"}, listing, "/batz/noi/papucs"},
		{"subpage is no filter", analysisAction{Kind: "canonical", Proposed: "A /batz/papucs/noi canonicalja"}, listing, "/batz/papucs/noi"},
		{"filters to clean url", analysisAction{Kind: "canonical", Proposed: "A /batz/papucs/meret:41 canonical mutasson a https://cipomarket.hu/batz/papucs oldalra"}, listing, ""},
		{"self canonical check", analysisAction{Kind: "canonical", Proposed: "Ellenőrizd a canonicalt."}, listing, ""},
		{"available product", analysisAction{Kind: "redirect_merge", Proposed: "301 a /nike/cipo oldalra, a modell megszűnt"}, available, ""},
		{"other kinds pass", analysisAction{Kind: "internal_link", Proposed: "Linkelj a /batz oldalra"}, listing, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := harmfulAction(&c.a, c.snap)
			if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
				t.Errorf("harmfulAction = %q, want %q", got, c.want)
			}
		})
	}
}

func TestContextStatesUnavailability(t *testing.T) {
	in := testInput(t)
	if strings.Contains(buildContextPack(in).Text, "Availability:") {
		t.Error("a category page must not be marked unavailable")
	}
	snap := *in.Snapshot
	snap.Prose = []string{"A termék jelenleg nem elérhető."}
	in.Snapshot = &snap
	if !strings.Contains(buildContextPack(in).Text, "currently unavailable") {
		t.Error("availability fact missing")
	}
}

func TestUnsupportedNumbers(t *testing.T) {
	corpus := "ár 33.490 ft, kedvezmény -20%, 60000 ft-tól, 3-8 munkanap"
	cases := []struct {
		text string
		want []string
	}{
		{"Most 33 490 Ft, 20% kedvezménnyel", nil},
		{"Szállítás 3-8 nap", nil},
		{"Már 15.000 Ft felett", []string{"15.000"}},
		{"Akár 50% kedvezmény", []string{"50"}},
	}
	for _, c := range cases {
		got := unsupportedNumbers(c.text, corpus)
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("unsupportedNumbers(%q) = %v, want %v", c.text, got, c.want)
		}
	}
}

func TestUnsupportedNames(t *testing.T) {
	corpus := "skechers női bokacsizma rieker"
	cases := []struct {
		text string
		want []string
	}{
		{"Női bokacsizma | Rieker és Skechers", nil},
		{"Válogass a Skecherstől", nil},
		{"Női bokacsizma Nike modellekkel", []string{"Nike"}},
		{"Vásárolj most. Ingyenes kiszállítás", nil},
	}
	for _, c := range cases {
		got := unsupportedNames(c.text, corpus)
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("unsupportedNames(%q) = %v, want %v", c.text, got, c.want)
		}
	}
}

// scriptedAI answers each call from a list and counts the calls.
type scriptedAI struct {
	answers []string
	prompts []string
	err     error
}

func (s *scriptedAI) call(_ context.Context, _, user string, _ int) (string, error) {
	s.prompts = append(s.prompts, user)
	if s.err != nil {
		return "", s.err
	}
	i := len(s.prompts) - 1
	if i >= len(s.answers) {
		i = len(s.answers) - 1
	}
	return s.answers[i], nil
}

func toJSON(t *testing.T, r analysisResponse) string {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestLLMGoodAnswerNeedsOneCall(t *testing.T) {
	in := testInput(t)
	ai := &scriptedAI{answers: []string{"```json\n" + toJSON(t, goodResponse()) + "\n```"}}
	o := runLLMAnalysis(context.Background(), ai.call, buildContextPack(in), in.Opportunities, in.Snapshot)
	if len(ai.prompts) != 1 {
		t.Errorf("calls = %d", len(ai.prompts))
	}
	if o.Analysis.Status != models.AnalysisStatusCompleted || len(o.Actions) != 1 {
		t.Errorf("outcome = %+v", o)
	}
}

func TestLLMRepairsRejectedAnswer(t *testing.T) {
	in := testInput(t)
	bad := goodResponse()
	bad.Actions[0].EvidenceRefs = []string{"Q99"}
	ai := &scriptedAI{answers: []string{toJSON(t, bad), toJSON(t, goodResponse())}}
	o := runLLMAnalysis(context.Background(), ai.call, buildContextPack(in), in.Opportunities, in.Snapshot)
	if len(ai.prompts) != 2 || !strings.Contains(ai.prompts[1], "Q99") {
		t.Fatalf("repair prompt missing, calls = %d", len(ai.prompts))
	}
	if len(o.Actions) != 1 || len(o.Analysis.ValidationWarnings) != 0 {
		t.Errorf("outcome = %+v", o)
	}
}

func TestLLMDropsWhatRepairCannotFix(t *testing.T) {
	in := testInput(t)
	bad := goodResponse()
	bad.Actions[0].EvidenceRefs = []string{"Q99"}
	ai := &scriptedAI{answers: []string{toJSON(t, bad)}}
	o := runLLMAnalysis(context.Background(), ai.call, buildContextPack(in), in.Opportunities, in.Snapshot)
	if len(ai.prompts) != 2 {
		t.Errorf("calls = %d, want at most one repair", len(ai.prompts))
	}
	if len(o.Actions) != 0 || len(o.Analysis.ValidationWarnings) == 0 {
		t.Errorf("outcome = %+v", o)
	}
	if o.Analysis.PrimaryIssue == "" {
		t.Error("the diagnosis should survive a dropped action")
	}
}

func TestLLMNoActionIsValid(t *testing.T) {
	in := testInput(t)
	r := goodResponse()
	r.Actions = nil
	r.NoActionReason = "A visszaesés szezonális."
	ai := &scriptedAI{answers: []string{toJSON(t, r)}}
	o := runLLMAnalysis(context.Background(), ai.call, buildContextPack(in), in.Opportunities, in.Snapshot)
	if o.Analysis.Status != models.AnalysisStatusNoAction || o.Analysis.NoActionReason == "" {
		t.Errorf("outcome = %+v", o.Analysis)
	}
	if len(ai.prompts) != 1 {
		t.Errorf("calls = %d", len(ai.prompts))
	}
}

func TestLLMFailures(t *testing.T) {
	in := testInput(t)
	pack := buildContextPack(in)
	ai := &scriptedAI{err: errors.New("rate limited")}
	if o := runLLMAnalysis(context.Background(), ai.call, pack, in.Opportunities, in.Snapshot); o.Analysis.Status != models.AnalysisStatusFailed {
		t.Errorf("ai error status = %s", o.Analysis.Status)
	}
	ai = &scriptedAI{answers: []string{"ez nem JSON"}}
	if o := runLLMAnalysis(context.Background(), ai.call, pack, in.Opportunities, in.Snapshot); o.Analysis.Status != models.AnalysisStatusFailed {
		t.Errorf("garbage status = %s", o.Analysis.Status)
	}
}

func TestPromptForbidsHarmfulAdvice(t *testing.T) {
	for _, want := range []string{"Never point the canonical of a category", "do not recommend a 301", "is normal for a shop"} {
		if !strings.Contains(analysisSystemPrompt, want) {
			t.Errorf("system prompt lacks %q", want)
		}
	}
}

func TestPromptIsSignalSpecific(t *testing.T) {
	in := testInput(t)
	opps := []models.Opportunity{{Type: models.OppCannibalization}, {Type: models.OppCannibalization}, {Type: models.OppLosingClicks}}
	_, user := buildAnalysisPrompt(buildContextPack(in), opps)
	if strings.Count(user, "cannibalization: Read") != 1 || !strings.Contains(user, "losing_clicks: Compare") {
		t.Errorf("checks missing or repeated:\n%.600s", user)
	}
	if strings.Contains(user, "striking_distance: Does") {
		t.Error("unrelated signal check included")
	}
}

func TestGroupOpportunitiesByPage(t *testing.T) {
	got := groupOpportunitiesByPage([]models.Opportunity{
		{Page: "https://a.hu/x/", Type: models.OppLowCTR},
		{Page: "https://b.hu/y", Type: models.OppLowCTR},
		{Page: "https://www.a.hu/x", Type: models.OppStrikingDistance},
	})
	if len(got) != 2 || len(got[0].Opps) != 2 || got[0].Page != "https://a.hu/x/" {
		t.Errorf("targets = %+v", got)
	}
}

func TestPickRelatedPages(t *testing.T) {
	opps := []models.Opportunity{{
		Type: models.OppCannibalization, Query: "bokacsizma", Page: "https://a.hu/x",
		Pages: []string{"https://a.hu/x", "https://a.hu/y"},
	}}
	competing := []competingPage{
		{Page: "https://a.hu/y/", Queries: 3}, // same page as the cannibalizing one
		{Page: "https://a.hu/z", Queries: 2},
		{Page: "https://a.hu/v", Queries: 2},
		{Page: "https://a.hu/w", Queries: 1},
	}
	got := pickRelatedPages("https://a.hu/x", opps, competing)
	if len(got) != maxRelatedPages || got[0].URL != "https://a.hu/y" || got[1].URL != "https://a.hu/z" {
		t.Errorf("related = %+v", got)
	}
	if !strings.Contains(got[0].Reason, "bokacsizma") {
		t.Errorf("reason = %q", got[0].Reason)
	}
}

func TestBuildSuggestionsFillsCurrentValue(t *testing.T) {
	snap := &models.PageSnapshot{Title: "Régi cím", CategoryText: "Régi szöveg"}
	a := &models.PageAnalysis{ID: bson.NewObjectID(), ProjectID: bson.NewObjectID()}
	opp := models.Opportunity{ID: bson.NewObjectID(), Query: "bokacsizma"}
	got := buildSuggestions(a, []analysisAction{
		{Kind: models.SuggestionKindCategoryText, Proposed: "Új", EvidenceRefs: []string{"C1"}},
		{Kind: models.SuggestionKindInternalLink, Proposed: "Link", Target: " https://a.hu/x ", Priority: 2, Effort: "weird"},
	}, analysisTarget{Page: testPage, Opps: []models.Opportunity{opp}}, snap, time.Now())
	if got[0].CurrentValue != "Régi szöveg" || got[0].Priority != 1 || *got[0].AnalysisID != a.ID {
		t.Errorf("first = %+v", got[0])
	}
	if got[1].Target != "https://a.hu/x" || got[1].Effort != "M" || got[1].Status != models.SuggestionStatusPending {
		t.Errorf("second = %+v", got[1])
	}
}

func TestApplyableKinds(t *testing.T) {
	for _, k := range []string{"title", "meta_description", "category_text"} {
		if !models.IsApplyableKind(k) {
			t.Errorf("%s should be applyable", k)
		}
	}
	for _, k := range []string{"h1", "internal_link", "redirect_merge", "noindex", "unknown"} {
		if models.IsApplyableKind(k) {
			t.Errorf("%s must stay manual", k)
		}
	}
}

func TestPromptUsesTheSiteLanguage(t *testing.T) {
	system, _ := buildAnalysisPrompt(&contextPack{Language: "de"}, nil)
	if !strings.Contains(system, "output is in German") || strings.Contains(system, "Hungarian") {
		t.Fatalf("prompt does not name the site's language")
	}
	cases := []struct {
		project string
		page    string
		want    string
	}{{"hu", "en-GB", "hu"}, {"", "en-GB", "en"}, {"", "pt_BR", "pt"}, {"", "", "en"}}
	for _, c := range cases {
		if got := analysisLanguage(c.project, &models.PageSnapshot{Lang: c.page}); got != c.want {
			t.Errorf("analysisLanguage(%q, %q) = %q, want %q", c.project, c.page, got, c.want)
		}
	}
}

func TestPricesInOtherCurrencies(t *testing.T) {
	cases := []struct {
		text     string
		value    float64
		currency string
	}{
		{"36.999 Ft", 36999, "FT"},
		{"1 299 HUF", 1299, "HUF"},
		{"€12,99", 12, "€"},
		{"12,99 € kedvezmény", 12, "€"},
		{"$1,299.99", 1299, "$"},
		{"£45", 45, "£"},
		{"1.299,50 zł", 1299, "ZŁ"},
	}
	for _, c := range cases {
		v, cur, ok := cardPrice(c.text)
		if !ok || v != c.value || cur != c.currency {
			t.Errorf("cardPrice(%q) = %v %q %v, want %v %q", c.text, v, cur, ok, c.value, c.currency)
		}
	}
	if _, _, ok := cardPrice("12 kronor"); ok {
		t.Error("a word must not read as a currency")
	}
}

func TestStructuredAvailability(t *testing.T) {
	page := `<html lang="en-GB"><head><script type="application/ld+json">{"@type":"Product","offers":{"@type":"Offer","availability":"https://schema.org/OutOfStock"}}</script></head><body><h1>Red shoe</h1><p>A red shoe for every day of the week and for many other occasions, made from soft leather that lasts.</p></body></html>`
	snap, err := extractSnapshot("https://x.example/red-shoe", 200, page)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Lang != "en-GB" || snap.Availability != "out_of_stock" || !isUnavailablePage(snap) {
		t.Fatalf("lang %q, availability %q, unavailable %v", snap.Lang, snap.Availability, isUnavailablePage(snap))
	}
	mergeAvailability(snap, []string{"InStock"})
	if snap.Availability != "in_stock" {
		t.Fatal("one purchasable offer must win")
	}
}
