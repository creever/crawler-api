package worker

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/creever/crawler-api/models"
)

func profile(url, group, title, meta string) models.PageProfile {
	return models.PageProfile{
		URL: normalizePageURL(url), SourceURL: url, StatusCode: 200, Title: title, MetaDescription: meta,
		H1: title, Group: group, Canonical: url,
	}
}

func findingsOf(list []siteFinding, issue string) []siteFinding {
	var out []siteFinding
	for _, f := range list {
		if f.Issue == issue {
			out = append(out, f)
		}
	}
	return out
}

// A shop: products and listings at any URL depth share one meta sentence.
func shopProfiles() []models.PageProfile {
	var out []models.PageProfile
	const sentence = "Discover our %s collection today. We welcome every old and new customer!"
	for i := 0; i < 6; i++ {
		name := fmt.Sprintf("Boot%c", 'A'+i)
		out = append(out, profile("https://shop.example/boot-"+name+"-1234", models.PageKindProduct, name+" | Shop", fmt.Sprintf(sentence, name)))
	}
	for _, path := range []string{"nike", "nike/shoes", "nike/men/shoes"} {
		out = append(out, profile("https://shop.example/"+path, models.PageKindListing, path+" store | Shop", fmt.Sprintf(sentence, path)))
	}
	return out
}

func TestSharedWordingIsOneFindingAcrossPageTypesAndDepths(t *testing.T) {
	found := findingsOf(detectFindings(shopProfiles()), models.IssueTemplatedMeta)
	if len(found) != 1 {
		t.Fatalf("got %d findings, want one: %+v", len(found), found)
	}
	f := found[0]
	if len(f.Pages) != 9 {
		t.Fatalf("got %d pages, want 9", len(f.Pages))
	}
	if !strings.Contains(f.Problem, "today we welcome every old and new customer") {
		t.Fatalf("the phrase was not extended to the full wording: %s", f.Problem)
	}
}

func TestOrdinaryTitleSuffixIsNoFinding(t *testing.T) {
	var pages []models.PageProfile
	for i := 0; i < 8; i++ {
		pages = append(pages, profile(fmt.Sprintf("https://x.example/a/p%d", i), "other:/a",
			fmt.Sprintf("Page %d of the site | The Very Long Brand Name Of This Site", i),
			strings.Repeat(fmt.Sprintf("w%d ", i), 12)))
	}
	for _, f := range detectFindings(pages) {
		if f.Issue == models.IssueDuplicateTitle || f.Issue == models.IssueTemplatedMeta {
			t.Fatalf("a shared brand suffix must not be a finding: %s", f.Title)
		}
	}
}

// A marketing site without schema data, shaped like Talentview.
func TestDuplicatesOnASiteWithoutSchema(t *testing.T) {
	pages := []models.PageProfile{
		profile("https://t.example/", models.PageKindHome, "Launchpad | T", "T is the place for students."),
		profile("https://t.example/registration/a", "other:/registration", "Launchpad | T", "T is the place for students."),
		profile("https://t.example/registration/b", "other:/registration", "Launchpad | T", "T is the place for students."),
		profile("https://t.example/news", "page:/news", "News | T", "All our news."),
	}
	titles := findingsOf(detectFindings(pages), models.IssueDuplicateTitle)
	total := 0
	for _, f := range titles {
		total += len(f.Pages)
	}
	if len(titles) == 0 || total != 3 {
		t.Fatalf("got %d findings over %d pages, want 3 pages", len(titles), total)
	}
}

// A single-page app: every route serves one title and no description.
func TestAppShellIsCaughtWithoutSiteRules(t *testing.T) {
	var pages []models.PageProfile
	for i := 0; i < 9; i++ {
		pages = append(pages, profile(fmt.Sprintf("https://app.example/section/%d", i), "other:/section", "app-ui", ""))
	}
	list := detectFindings(pages)
	if len(findingsOf(list, models.IssueDuplicateTitle)) != 1 || len(findingsOf(list, models.IssueMissingMeta)) != 1 {
		t.Fatalf("findings: %+v", list)
	}
}

func TestNonIndexablePagesAreLeftOut(t *testing.T) {
	a := profile("https://x.example/a", "listing", "Same title", "")
	b := profile("https://x.example/a?sort=price", "listing", "Same title", "")
	b.Canonical = "https://x.example/a"
	c := profile("https://x.example/c", "listing", "Same title", "")
	c.Robots = "noindex, follow"
	if got := findingsOf(detectFindings([]models.PageProfile{a, b, c}), models.IssueDuplicateTitle); len(got) != 0 {
		t.Fatalf("pages that do not compete in search were counted: %+v", got)
	}
}

func TestRelativeCanonicalIsItsOwnPage(t *testing.T) {
	a := profile("https://x.example/a/b", "other:/a", "T", "M")
	a.Canonical = "/a/b"
	if len(indexablePages([]models.PageProfile{a})) != 1 {
		t.Fatal("a relative self-canonical must count as indexable")
	}
}

func TestUnavailablePagesAreOneTaskThatForbidsRedirects(t *testing.T) {
	var pages []models.PageProfile
	for i := 0; i < 4; i++ {
		p := profile(fmt.Sprintf("https://s.example/p-%d", i), "product", fmt.Sprintf("T%d", i), fmt.Sprintf("Unique %d", i))
		p.Unavailable = true
		pages = append(pages, p)
	}
	found := findingsOf(detectFindings(pages), models.IssueUnavailable)
	if len(found) != 1 || !strings.Contains(strings.Join(found[0].Avoid, " "), "redirect") {
		t.Fatalf("findings: %+v", found)
	}
}

func TestTaskKeyIsStableAndFindingsDoNotRepeat(t *testing.T) {
	first := detectFindings(shopProfiles())
	second := detectFindings(shopProfiles())
	if len(first) != len(second) {
		t.Fatal("detection is not deterministic")
	}
	seen := make(map[string]bool)
	for i := range first {
		k1, k2 := taskKey(first[i].Issue, first[i].KeyPart), taskKey(second[i].Issue, second[i].KeyPart)
		if k1 != k2 {
			t.Fatalf("key changed between runs for %s", first[i].Title)
		}
		if seen[k1] {
			t.Fatalf("two findings share a key: %s", first[i].Title)
		}
		seen[k1] = true
	}
}

func TestPageGroups(t *testing.T) {
	product := &models.PageSnapshot{SchemaTypes: []string{"Product", "Offer"}}
	article := &models.PageSnapshot{SchemaTypes: []string{"NewsArticle"}}
	listing := &models.PageSnapshot{Listing: &models.ListingSummary{ItemCount: 24}}
	cases := []struct {
		url  string
		snap *models.PageSnapshot
		want string
	}{
		{"https://x.example/", product, "home"},
		{"https://x.example/shoe-123", product, "product"},
		{"https://x.example/a/b/c/shoe", product, "product"},
		{"https://x.example/nike", listing, "listing"},
		{"https://x.example/nike/men/shoes", listing, "listing"},
		{"https://x.example/adidas/men/shoes", &models.PageSnapshot{Listing: &models.ListingSummary{ItemCount: 5}, Coverage: models.SnapshotCoverage{ListingChars: 900, ProseChars: 100}}, "listing"},
		{"https://x.example/shoe-77", &models.PageSnapshot{SchemaTypes: []string{"Product"}, Listing: &models.ListingSummary{ItemCount: 5}, Coverage: models.SnapshotCoverage{ListingChars: 900, ProseChars: 100}}, "product"},
		{"https://x.example/about/team", &models.PageSnapshot{Listing: &models.ListingSummary{ItemCount: 5}, Coverage: models.SnapshotCoverage{ListingChars: 300, ProseChars: 900}}, "other:/about"},
		{"https://x.example/2026/hello", article, "article"},
		{"https://x.example/news/hello", &models.PageSnapshot{}, "other:/news"},
		{"https://x.example/about", &models.PageSnapshot{}, "page:/about"},
	}
	for _, c := range cases {
		if got := snapshotGroup(c.url, c.snap); got != c.want {
			t.Errorf("snapshotGroup(%s) = %q, want %q", c.url, got, c.want)
		}
	}
}

func TestSampleSpreadsOverURLShapes(t *testing.T) {
	var candidates []string
	for i := 0; i < 50; i++ {
		candidates = append(candidates, fmt.Sprintf("https://x.example/shoe-%d123", i))
	}
	candidates = append(candidates, "https://x.example/blog/one", "https://x.example/a/b/c")
	got := pickSample(candidates, map[string]bool{normalizePageURL(candidates[0]): true}, 3)
	if len(got) != 3 || got[0] != candidates[1] || got[1] != "https://x.example/blog/one" || got[2] != "https://x.example/a/b/c" {
		t.Fatalf("got %v", got)
	}
}

func TestSampleableURL(t *testing.T) {
	site := "https://www.x.example"
	for url, want := range map[string]bool{
		"https://x.example/a":          true,
		"https://x.example/a?sort=1":   false,
		"https://x.example/a/ar:1-100": false,
		"https://other.example/a":      false,
	} {
		if got := sampleableURL(url, site); got != want {
			t.Errorf("sampleableURL(%s) = %v", url, got)
		}
	}
}

func suggestion(page, issue, scope, group string) models.Suggestion {
	return models.Suggestion{ID: bson.NewObjectID(), Page: page, Issue: issue, Scope: scope, PageGroup: group, Kind: "title"}
}

func TestMergeNeedsASecondPageOrASharedScope(t *testing.T) {
	alone := suggestion("https://x.example/p1", models.IssueThinContent, "page", "product")
	plan := planMerge([]models.Suggestion{alone}, nil)
	if len(plan.Groups) != 0 {
		t.Fatal("one page-level finding must stay a suggestion")
	}
	second := suggestion("https://x.example/p2", models.IssueThinContent, "page", "product")
	if plan = planMerge([]models.Suggestion{alone, second}, nil); len(plan.Groups) != 1 {
		t.Fatal("two pages of a type with one problem must merge")
	}
	shared := suggestion("https://x.example/p3", models.IssueTemplatedTitle, "page_type", "product")
	if plan = planMerge([]models.Suggestion{shared}, nil); len(plan.Groups) != 1 {
		t.Fatal("a problem the model calls shared must become a group task")
	}
}

func TestPageSpecificIssuesNeverMerge(t *testing.T) {
	for _, issue := range []string{models.IssueCannibal, models.IssueIntentMismatch, models.IssueOther} {
		list := []models.Suggestion{
			suggestion("https://x.example/p1", issue, "page_type", "product"),
			suggestion("https://x.example/p2", issue, "page_type", "product"),
		}
		if plan := planMerge(list, nil); len(plan.Groups) != 0 {
			t.Fatalf("%s must stay per page", issue)
		}
	}
	lone := []models.Suggestion{
		suggestion("https://x.example/about", models.IssueThinContent, "page_type", "page:/about"),
		suggestion("https://x.example/faq", models.IssueThinContent, "page_type", "page:/faq"),
	}
	if plan := planMerge(lone, nil); len(plan.Groups) != 0 {
		t.Fatal("pages without a page type must not merge")
	}
}

func TestProposalsJoinTheDetectedPattern(t *testing.T) {
	task := models.SEOTask{Issue: models.IssueDuplicateMeta, Status: models.TaskStatusOpen, Pages: []string{"https://x.example/p1/"}}
	done := models.SEOTask{Issue: models.IssueTemplatedMeta, Status: models.TaskStatusVerified, Pages: []string{"https://x.example/p1"}}
	s := suggestion("https://x.example/p1", models.IssueTemplatedMeta, "page", "product")
	plan := planMerge([]models.Suggestion{s}, []models.SEOTask{done, task})
	if len(plan.Attach[1]) != 1 || len(plan.Attach[0]) != 0 {
		t.Fatalf("plan: %+v", plan)
	}
}

func draftWith(pages ...string) models.SEOTask {
	return models.SEOTask{Source: models.TaskSourceDetector, Title: "New", Pages: pages, ConfirmedPages: pages, Metrics: models.TaskMetrics{Pages: len(pages)}}
}

func TestRepeatedFindingUpdatesTheTask(t *testing.T) {
	now := time.Now()
	task := models.SEOTask{Status: models.TaskStatusClaimed, Title: "Old", Pages: []string{"a"}}
	mergeIntoTask(&task, draftWith("a", "b"), now, now)
	if task.Status != models.TaskStatusClaimed || len(task.Pages) != 2 || task.Title != "New" {
		t.Fatalf("task: %+v", task)
	}
}

func TestDismissedTaskStaysDismissed(t *testing.T) {
	now := time.Now()
	task := models.SEOTask{Status: models.TaskStatusDismissed, Title: "Old", Pages: []string{"a"}, DismissedImpressions: 100}
	d := draftWith("a", "b")
	d.Metrics.Impressions = 150
	mergeIntoTask(&task, d, now, now)
	if task.Status != models.TaskStatusDismissed || task.Title != "Old" {
		t.Fatalf("a little growth must not bring it back: %+v", task)
	}
	d.Metrics.Impressions = 200
	mergeIntoTask(&task, d, now, now)
	if task.Status != models.TaskStatusReopened || !strings.Contains(task.ReopenReason, "dismissed at 100") {
		t.Fatalf("twice the reach must bring it back: %+v", task)
	}
}

func TestFinishedTaskIgnoresAnAIRepeatingItself(t *testing.T) {
	now := time.Now()
	for _, status := range []models.TaskStatus{models.TaskStatusDone, models.TaskStatusVerified, models.TaskStatusClosed} {
		task := models.SEOTask{Status: status, DoneAt: &now, Title: "Old"}
		d := draftWith("a")
		d.Source = models.TaskSourceAnalysis
		mergeIntoTask(&task, d, time.Time{}, now.Add(time.Hour))
		if task.Status != status || task.Title != "Old" {
			t.Fatalf("%s: %+v", status, task)
		}
	}
}

func TestAReviewerBringsAFinishedTaskBack(t *testing.T) {
	now := time.Now()
	task := models.SEOTask{Status: models.TaskStatusClosed, DoneAt: &now}
	d := draftWith("a")
	d.Source = models.TaskSourceReview
	mergeIntoTask(&task, d, time.Time{}, now)
	if task.Status != models.TaskStatusReopened || task.ReopenReason == "" {
		t.Fatalf("task: %+v", task)
	}
	proposed := models.SEOTask{Status: models.TaskStatusProposed}
	mergeIntoTask(&proposed, d, time.Time{}, now)
	if proposed.Status != models.TaskStatusOpen {
		t.Fatal("accepting a finding of a proposed task approves that task")
	}
}

func TestTasksStartByTheirSource(t *testing.T) {
	for source, want := range map[string]models.TaskStatus{
		models.TaskSourceDetector: models.TaskStatusQueued,
		models.TaskSourceAnalysis: models.TaskStatusProposed,
		models.TaskSourceReview:   models.TaskStatusOpen,
	} {
		if got := initialTaskStatus(source); got != want {
			t.Errorf("%s starts as %s", source, got)
		}
	}
}

func TestPatternIsGoneOnlyOnRereadPages(t *testing.T) {
	seen := time.Now()
	pages := []string{"https://x.example/a", "https://x.example/b", "https://x.example/c", "https://x.example/d"}
	task := &models.SEOTask{Pages: pages, LastSeenAt: seen, Checks: []models.TaskCheck{{Type: models.CheckNotEmpty, Field: "title"}}}
	byURL := make(map[string]models.PageProfile)
	for i, page := range pages[:3] {
		pr := profile(page, "product", fmt.Sprintf("T%d", i), "")
		pr.FetchedAt = seen.Add(time.Hour)
		byURL[pr.URL] = pr
	}
	if !patternGoneOnReread(task, byURL) {
		t.Fatal("three fixed reread pages show the pattern is gone")
	}
	stale := byURL[normalizePageURL(pages[2])]
	stale.FetchedAt = seen.Add(-time.Hour)
	byURL[stale.URL] = stale
	if patternGoneOnReread(task, byURL) {
		t.Fatal("a pattern that only fell below its threshold proves nothing")
	}
	stale.FetchedAt, stale.Title = seen.Add(time.Hour), ""
	byURL[stale.URL] = stale
	if patternGoneOnReread(task, byURL) {
		t.Fatal("a reread page still showing it keeps the task open")
	}
}

func TestVerifiedTaskReopensOnlyOnLaterReads(t *testing.T) {
	at := time.Now()
	stale := models.SEOTask{Status: models.TaskStatusVerified, Verification: &models.TaskVerification{At: at}}
	mergeIntoTask(&stale, draftWith("a"), at.Add(-time.Hour), at)
	if stale.Status != models.TaskStatusVerified {
		t.Fatal("a profile from before the fix must not reopen the task")
	}
	fresh := models.SEOTask{Status: models.TaskStatusVerified, Verification: &models.TaskVerification{At: at}}
	mergeIntoTask(&fresh, draftWith("a"), at.Add(time.Hour), at)
	if fresh.Status != models.TaskStatusReopened {
		t.Fatal("a page read after the fix shows it did not hold")
	}
}

func snapOf(page, title, meta string) *models.PageSnapshot {
	return &models.PageSnapshot{URL: normalizePageURL(page), SourceURL: page, StatusCode: 200, Title: title, MetaDescription: meta}
}

func TestChecks(t *testing.T) {
	a := snapOf("https://x.example/a", "One", "Please look at our great offer now, friends!")
	b := snapOf("https://x.example/b", "One", "")
	cases := []struct {
		check models.TaskCheck
		fails int
	}{
		{models.TaskCheck{Type: models.CheckNotContains, Field: "meta_description", Value: "look at our great offer"}, 1},
		{models.TaskCheck{Type: models.CheckNotContains, Field: "meta_description", Value: "nothing like this here"}, 0},
		{models.TaskCheck{Type: models.CheckMaxLength, Field: "meta_description", Limit: 10}, 1},
		{models.TaskCheck{Type: models.CheckNotEmpty, Field: "meta_description"}, 1},
		{models.TaskCheck{Type: models.CheckUnique, Field: "title"}, 1},
		{models.TaskCheck{Type: models.CheckUnique, Field: "meta_description"}, 0},
		{models.TaskCheck{Type: models.CheckChangedFrom, Field: "title", Value: "One", Page: "https://x.example/a/"}, 1},
		{models.TaskCheck{Type: models.CheckChangedFrom, Field: "title", Value: "Old", Page: "https://x.example/a"}, 0},
		{models.TaskCheck{Type: models.CheckStatusOK}, 0},
	}
	for _, c := range cases {
		if got := evaluateChecks([]models.TaskCheck{c.check}, []*models.PageSnapshot{a, b}); len(got) != c.fails {
			t.Errorf("%s: %d failures, want %d: %v", c.check.Describe(), len(got), c.fails, got)
		}
	}
}

func TestVerifySampleKeepsNamedPagesAndSpreads(t *testing.T) {
	var pages []string
	for i := 0; i < 100; i++ {
		pages = append(pages, fmt.Sprintf("https://x.example/%d", i))
	}
	task := &models.SEOTask{Pages: pages, Checks: []models.TaskCheck{{Page: "https://x.example/named"}}}
	got := verifySample(task, 10)
	if len(got) != 10 || got[0] != "https://x.example/named" {
		t.Fatalf("got %v", got)
	}
	if got[9] == pages[8] {
		t.Fatal("the sample must spread over the pages, not take the first ones")
	}
}

func TestBriefTellsTheHarnessWhatItNeeds(t *testing.T) {
	task := models.SEOTask{
		ID: bson.NewObjectID(), Title: "Repeated wording", Scope: models.TaskScopeSite, Problem: "The problem.", Goal: "The goal.",
		Pages: []string{"https://x.example/a"}, Avoid: []string{"No redirects."},
		Examples: []models.TaskExample{{Page: "https://x.example/a", Field: "title", Current: "Old", Proposed: "New"}},
		Checks:   []models.TaskCheck{{Type: models.CheckMaxLength, Field: "title", Limit: 60}},
	}
	brief := task.Brief()
	for _, want := range []string{"The problem.", "The goal.", "confirm it", "No redirects.", "proposed: New", "title is at most 60", "live on the public site"} {
		if !strings.Contains(brief, want) {
			t.Errorf("brief lacks %q:\n%s", want, brief)
		}
	}
}

func TestTaskFromSuggestionUsesTheEditedText(t *testing.T) {
	s := suggestion("https://x.example/a", "", "", "product")
	s.ProposedValue, s.FinalValue, s.CurrentValue = "AI text", "Human text", "Old"
	task := TaskFromSuggestion(s, nil, pageStat{Impressions: 5})
	if task.Issue != models.IssueOther || task.Examples[0].Proposed != "Human text" || task.Goal != "Human text" {
		t.Fatalf("task: %+v", task)
	}
	if len(task.Checks) != 1 || task.Checks[0].Type != models.CheckChangedFrom {
		t.Fatalf("checks: %+v", task.Checks)
	}
}

// A small site must not get a task per section for what is one problem.
func TestSmallPageTypesShareOneTask(t *testing.T) {
	pages := []models.PageProfile{
		profile("https://t.example/", models.PageKindHome, "Launchpad | T", "Own text one."),
		profile("https://t.example/news/1", "other:/news", "Launchpad | T", "Own text two."),
		profile("https://t.example/news/2", "other:/news", "Launchpad | T", "Own text three."),
		profile("https://t.example/reg/a", "other:/reg", "Sign up | T", "Own text four."),
		profile("https://t.example/reg/b", "other:/reg", "Sign up | T", "Own text five."),
	}
	found := findingsOf(detectFindings(pages), models.IssueDuplicateTitle)
	if len(found) != 1 || len(found[0].Pages) != 5 || found[0].Scope != models.TaskScopeSite {
		t.Fatalf("findings: %+v", found)
	}
}

func TestBigPageTypeGetsItsOwnTask(t *testing.T) {
	var pages []models.PageProfile
	for i := 0; i < 6; i++ {
		pages = append(pages, profile(fmt.Sprintf("https://s.example/p-%d", i), models.PageKindProduct, "", fmt.Sprintf("Own text %d.", i)))
	}
	found := findingsOf(detectFindings(pages), models.IssueMissingTitle)
	if len(found) != 1 || found[0].Scope != models.TaskScopeGroup || found[0].Group != models.PageKindProduct {
		t.Fatalf("findings: %+v", found)
	}
}

func TestAStrayTwinJoinsItsFinding(t *testing.T) {
	var pages []models.PageProfile
	for i := 0; i < 5; i++ {
		pages = append(pages, profile(fmt.Sprintf("https://s.example/a/p-%d", i), "other:/a", "Same", fmt.Sprintf("Own text %d.", i)))
	}
	pages = append(pages, profile("https://s.example/", models.PageKindHome, "Same", "Home text."))
	found := findingsOf(detectFindings(pages), models.IssueDuplicateTitle)
	if len(found) != 1 || len(found[0].Pages) != 6 {
		t.Fatalf("findings: %+v", found)
	}
}

func TestAssetsAreNoPages(t *testing.T) {
	for _, u := range []string{"https://x.example/static/app.css", "https://x.example/logo.PNG", "https://x.example/doc.pdf"} {
		if sampleableURL(u, "https://x.example") {
			t.Errorf("%s must not be sampled", u)
		}
	}
}

func day(s string) time.Time {
	d, err := time.Parse(dayLayout, s)
	if err != nil {
		panic(err)
	}
	return d
}

func TestImpactWindowsCompareLikeWithLike(t *testing.T) {
	before, after := impactWindows(day("2026-10-01").Add(15*time.Hour), day("2026-10-20"))
	if before.Start != day("2026-09-03") || before.End != day("2026-09-30") || before.days() != 28 {
		t.Fatalf("before: %v", before)
	}
	if after.Start != day("2026-10-06") || after.End != day("2026-10-20") || after.days() != 15 {
		t.Fatalf("after: %v", after)
	}
	// Search data lags: the days it does not have yet must stay out of the window.
	before, after = impactWindows(day("2026-10-01"), day("2026-09-28"))
	if before.End != day("2026-09-28") || before.days() != 28 || after.days() != 0 {
		t.Fatalf("before %v after %v", before, after)
	}
}

func TestPartialWindowIsNoFall(t *testing.T) {
	before := windowTotals{Clicks: 280}
	after := windowTotals{Clicks: 60}
	if got := relativeChange(before, 28, after, 5); got < 0.19 || got > 0.21 {
		t.Fatalf("10 clicks a day growing to 12 is +20%%, got %v", got)
	}
}

func TestImpactVerdict(t *testing.T) {
	cases := []struct {
		days    int
		clicks  float64
		change  float64
		kind    string
		control float64
		want    string
	}{
		{20, 900, 0.5, models.ControlPageType, 0, models.VerdictMeasuring},
		{28, 10, 0.5, models.ControlPageType, 0, models.VerdictInconclusive},
		{28, 900, 0.5, models.ControlNone, 0, models.VerdictInconclusive},
		{28, 900, 0.2, models.ControlPageType, 0.05, models.VerdictImproved},
		{28, 900, 0.2, models.ControlLastYear, 0.25, models.VerdictNoChange},
		{28, 900, -0.2, models.ControlPageType, -0.15, models.VerdictNoChange},
		{28, 900, 0.1, models.ControlPageType, 0.3, models.VerdictWorse},
	}
	for _, c := range cases {
		if got := impactVerdict(c.days, c.clicks, c.change, c.kind, c.control); got != c.want {
			t.Errorf("%+v: got %s", c, got)
		}
	}
}

func TestVerdictsMoveTasks(t *testing.T) {
	improved := &models.SEOTask{Status: models.TaskStatusVerified}
	applyVerdict(improved, models.VerdictImproved)
	if improved.Status != models.TaskStatusClosed {
		t.Fatal("what helped closes")
	}
	flat := &models.SEOTask{Status: models.TaskStatusVerified}
	applyVerdict(flat, models.VerdictNoChange)
	if flat.Status != models.TaskStatusReopened || flat.ImpactReopens != 1 || flat.ReopenReason == "" {
		t.Fatalf("what did not help returns once: %+v", flat)
	}
	flat.Status = models.TaskStatusVerified
	applyVerdict(flat, models.VerdictWorse)
	if flat.Status != models.TaskStatusClosed {
		t.Fatal("a second attempt must not loop")
	}
	measuring := &models.SEOTask{Status: models.TaskStatusVerified}
	applyVerdict(measuring, models.VerdictMeasuring)
	if measuring.Status != models.TaskStatusVerified {
		t.Fatal("a measurement in progress decides nothing")
	}
	unconfirmed := &models.SEOTask{Status: models.TaskStatusDone}
	applyVerdict(unconfirmed, models.VerdictNoChange)
	if unconfirmed.Status != models.TaskStatusDone {
		t.Fatal("a task nobody confirmed is not judged")
	}
}

func TestTopMovesKeepsBothEnds(t *testing.T) {
	before := map[string]float64{"a": 280, "b": 280, "c": 280, "d": 280, "e": 280, "f": 280, "g": 280, "h": 280}
	after := map[string]float64{"a": 560, "b": 500, "c": 400, "d": 290, "e": 280, "f": 200, "g": 100, "h": 0}
	moves := topMoves(before, 28, after, 28)
	if len(moves) != 6 || moves[0].Page != "a" || moves[5].Page != "h" {
		t.Fatalf("moves: %+v", moves)
	}
}

func TestBriefExplainsAReturn(t *testing.T) {
	task := models.SEOTask{Title: "T", ReopenReason: "The recheck failed."}
	if !strings.Contains(task.Brief(), "Why this came back") {
		t.Fatal("a harness must see why a task returned")
	}
}
