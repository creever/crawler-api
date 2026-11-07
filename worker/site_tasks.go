package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/hibiken/asynq"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.uber.org/zap"

	"github.com/creever/crawler-api/models"
)

const (
	maxTaskPages     = 2000
	maxTaskExamples  = 8
	maxTaskEvidence  = 12
	verifySamplePage = 10
	// suggestionMergeWindow limits merging to the current wave of analyses.
	suggestionMergeWindow = 30 * 24 * time.Hour
)

// resolveAgainst resolves a possibly relative URL against a page.
func resolveAgainst(base, ref string) string {
	b, err := url.Parse(base)
	if err != nil {
		return ref
	}
	r, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return b.ResolveReference(r).String()
}

func pageURLs(pages []models.PageProfile) []string {
	out := make([]string, 0, len(pages))
	for _, pr := range pages {
		out = append(out, pr.SourceURL)
	}
	sort.Strings(out)
	if len(out) > maxTaskPages {
		out = out[:maxTaskPages]
	}
	return out
}

func taskMetrics(pages []string, stats map[string]pageStat, sampled int) models.TaskMetrics {
	m := models.TaskMetrics{Pages: len(pages), PagesSampled: sampled}
	for _, page := range pages {
		s := stats[normalizePageURL(page)]
		m.Clicks += s.Clicks
		m.Impressions += s.Impressions
		m.Sessions += s.Sessions
	}
	return m
}

// taskFromFinding turns a detected pattern into a task draft.
func taskFromFinding(projectOID bson.ObjectID, f siteFinding, stats map[string]pageStat, sampled int, language string) models.SEOTask {
	pages := pageURLs(f.Pages)
	return models.SEOTask{
		ProjectID: projectOID, Key: taskKey(f.Issue, f.KeyPart), Source: models.TaskSourceDetector,
		Issue: f.Issue, Scope: f.Scope, PageGroup: f.Group, Title: f.Title, Problem: f.Problem, Goal: f.Goal,
		Language: language, Confidence: f.Confidence, Pages: pages, ConfirmedPages: pages,
		Metrics: taskMetrics(pages, stats, sampled), Examples: f.Examples, Avoid: f.Avoid, Checks: f.Checks,
	}
}

func uniqueStrings(lists ...[]string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, list := range lists {
		for _, s := range list {
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	return out
}

func uniqueIDs(lists ...[]bson.ObjectID) []bson.ObjectID {
	seen := make(map[bson.ObjectID]bool)
	var out []bson.ObjectID
	for _, list := range lists {
		for _, id := range list {
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	return out
}

func mergeExamples(lists ...[]models.TaskExample) []models.TaskExample {
	seen := make(map[string]bool)
	var withProposal, plain []models.TaskExample
	for _, list := range lists {
		for _, e := range list {
			key := e.Page + "\x00" + e.Field + "\x00" + e.Proposed
			if seen[key] {
				continue
			}
			seen[key] = true
			if e.Proposed != "" {
				withProposal = append(withProposal, e)
			} else {
				plain = append(plain, e)
			}
		}
	}
	out := append(withProposal, plain...)
	if len(out) > maxTaskExamples {
		out = out[:maxTaskExamples]
	}
	return out
}

func mergeEvidence(lists ...[]models.AnalysisEvidence) []models.AnalysisEvidence {
	seen := make(map[string]bool)
	var out []models.AnalysisEvidence
	for _, list := range lists {
		for _, e := range list {
			if key := e.Claim + "\x00" + e.Fact; !seen[key] && len(out) < maxTaskEvidence {
				seen[key] = true
				out = append(out, e)
			}
		}
	}
	return out
}

// mergeIntoTask folds a repeated finding into its existing task. A dismissed
// task stays dismissed. A pattern seen on a verified task means the fix did not
// hold, if its pages were read after that verification.
func mergeIntoTask(t *models.SEOTask, d models.SEOTask, newestRead time.Time, now time.Time) {
	t.LastSeenAt = now
	if t.Status == models.TaskStatusDismissed {
		return
	}
	if t.Status == models.TaskStatusResolved ||
		(t.Status == models.TaskStatusVerified && t.Verification != nil && newestRead.After(t.Verification.At)) {
		t.Status = models.TaskStatusReopened
	}
	if !t.IsActive() {
		return
	}
	t.Title, t.Problem, t.Goal = d.Title, d.Problem, d.Goal
	t.Confidence = d.Confidence
	if d.Source == models.TaskSourceDetector {
		t.Pages, t.Checks = d.Pages, d.Checks
		t.ConfirmedPages = uniqueStrings(d.ConfirmedPages)
		t.Metrics = d.Metrics
	} else {
		t.Pages = uniqueStrings(t.Pages, d.Pages)
		t.ConfirmedPages = uniqueStrings(t.ConfirmedPages, d.ConfirmedPages)
		t.Checks = mergeChecks(t.Checks, d.Checks)
		t.Metrics = d.Metrics
	}
	t.Examples = mergeExamples(d.Examples, t.Examples)
	t.Evidence = mergeEvidence(d.Evidence, t.Evidence)
	t.Avoid = uniqueStrings(t.Avoid, d.Avoid)
	t.SuggestionIDs = uniqueIDs(t.SuggestionIDs, d.SuggestionIDs)
	t.AnalysisIDs = uniqueIDs(t.AnalysisIDs, d.AnalysisIDs)
	t.UpdatedAt = now
}

func mergeChecks(lists ...[]models.TaskCheck) []models.TaskCheck {
	seen := make(map[models.TaskCheck]bool)
	var out []models.TaskCheck
	for _, list := range lists {
		for _, c := range list {
			if !seen[c] {
				seen[c] = true
				out = append(out, c)
			}
		}
	}
	return out
}

// UpsertTask stores a draft, or merges it into the task with the same key, and
// returns the stored task.
func UpsertTask(ctx context.Context, db *mongo.Database, draft models.SEOTask, newestRead, now time.Time) (*models.SEOTask, error) {
	col := db.Collection("seo_tasks")
	var existing models.SEOTask
	err := col.FindOne(ctx, bson.M{"project_id": draft.ProjectID, "key": draft.Key}).Decode(&existing)
	if errors.Is(err, mongo.ErrNoDocuments) {
		draft.ID = bson.NewObjectID()
		draft.Status = models.TaskStatusOpen
		draft.LastSeenAt, draft.CreatedAt, draft.UpdatedAt = now, now, now
		if _, err = col.InsertOne(ctx, draft); err != nil {
			return nil, fmt.Errorf("insert task: %w", err)
		}
		return &draft, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load task: %w", err)
	}
	mergeIntoTask(&existing, draft, newestRead, now)
	if _, err = col.ReplaceOne(ctx, bson.M{"_id": existing.ID}, existing); err != nil {
		return nil, fmt.Errorf("update task: %w", err)
	}
	return &existing, nil
}

// refreshSiteTasks turns the patterns found across the site's profiles into
// tasks. A pattern that disappeared closes its open task.
func (p *Processor) refreshSiteTasks(ctx context.Context, project *models.Project, now time.Time) error {
	profiles, err := p.loadProfiles(ctx, project.ID)
	if err != nil {
		return fmt.Errorf("load profiles: %w", err)
	}
	if len(profiles) == 0 {
		return nil
	}
	stats, err := p.sitePageStats(ctx, project.ID, profiles)
	if err != nil {
		return fmt.Errorf("page stats: %w", err)
	}
	language := analysisLanguage(project.Language, nil)
	findings := detectFindings(profiles)
	seen := make([]string, 0, len(findings))
	for _, f := range findings {
		draft := taskFromFinding(project.ID, f, stats, len(profiles), language)
		seen = append(seen, draft.Key)
		// Only a page read after a fix can show that the fix failed: an older
		// profile still holds the state from before it.
		newestRead := time.Time{}
		for _, pr := range f.Pages {
			if pr.FetchedAt.After(newestRead) {
				newestRead = pr.FetchedAt
			}
		}
		if _, err = UpsertTask(ctx, p.db, draft, newestRead, now); err != nil {
			return err
		}
	}
	_, err = p.db.Collection("seo_tasks").UpdateMany(ctx, bson.M{
		"project_id": project.ID, "source": models.TaskSourceDetector, "key": bson.M{"$nin": seen},
		"status": bson.M{"$in": []models.TaskStatus{models.TaskStatusOpen, models.TaskStatusReopened}},
	}, bson.M{"$set": bson.M{"status": models.TaskStatusResolved, "updated_at": now}})
	return err
}

// issueFamily maps a model's issue to the detector issues it describes, so
// proposals land on the task of the pattern they belong to.
func issueFamily(issue string) []string {
	switch issue {
	case models.IssueTemplatedMeta:
		return []string{models.IssueTemplatedMeta, models.IssueDuplicateMeta, models.IssueMetaTooLong, models.IssueMissingMeta}
	case models.IssueTemplatedTitle:
		return []string{models.IssueTemplatedTitle, models.IssueDuplicateTitle, models.IssueTitleTooLong, models.IssueMissingTitle}
	case models.IssueWeakHeading:
		return []string{models.IssueWeakHeading, models.IssueMissingH1}
	}
	return []string{issue}
}

// taskFor finds the active task already covering this suggestion's page.
func taskFor(s models.Suggestion, tasks []models.SEOTask) int {
	key := normalizePageURL(s.Page)
	for _, issue := range issueFamily(s.Issue) {
		for i := range tasks {
			if tasks[i].Issue != issue || !tasks[i].IsActive() {
				continue
			}
			for _, page := range tasks[i].Pages {
				if normalizePageURL(page) == key {
					return i
				}
			}
		}
	}
	return -1
}

// mergePlan says what happens to the unassigned suggestions of a project.
type mergePlan struct {
	Attach map[int][]models.Suggestion    // by index of the existing task
	Groups map[string][]models.Suggestion // new group tasks, by "issue|group"
}

// planMerge decides which suggestions belong together. One page's finding only
// joins others if the model says the page type shares it or a second page of
// the type has it too. Page-specific issues always stay alone.
func planMerge(suggestions []models.Suggestion, tasks []models.SEOTask) mergePlan {
	plan := mergePlan{Attach: make(map[int][]models.Suggestion), Groups: make(map[string][]models.Suggestion)}
	candidates := make(map[string][]models.Suggestion)
	for _, s := range suggestions {
		if models.IsPageSpecificIssue(s.Issue) {
			continue
		}
		if i := taskFor(s, tasks); i >= 0 {
			plan.Attach[i] = append(plan.Attach[i], s)
			continue
		}
		if s.PageGroup == "" || strings.HasPrefix(s.PageGroup, "page:") {
			continue
		}
		key := s.Issue + "|" + s.PageGroup
		candidates[key] = append(candidates[key], s)
	}
	for key, list := range candidates {
		pages := make(map[string]bool)
		shared := false
		for _, s := range list {
			pages[normalizePageURL(s.Page)] = true
			shared = shared || s.Scope == "page_type"
		}
		if len(pages) >= 2 || shared {
			plan.Groups[key] = list
		}
	}
	return plan
}

func suggestionExample(s models.Suggestion) models.TaskExample {
	proposed := s.FinalValue
	if proposed == "" {
		proposed = s.ProposedValue
	}
	return models.TaskExample{Page: s.Page, Field: s.Kind, Current: s.CurrentValue, Proposed: proposed}
}

func suggestionCheck(s models.Suggestion) []models.TaskCheck {
	switch s.Kind {
	case models.SuggestionKindTitle, models.SuggestionKindMeta, models.SuggestionKindCategoryText, models.SuggestionKindH1:
		if s.CurrentValue != "" {
			return []models.TaskCheck{{Type: models.CheckChangedFrom, Field: s.Kind, Value: s.CurrentValue, Page: s.Page}}
		}
	}
	return nil
}

func weakestConfidence(a, b string) string {
	rank := map[string]int{"low": 0, "medium": 1, "high": 2}
	if a == "" || rank[b] < rank[a] {
		return b
	}
	return a
}

var issueLabels = map[string]string{
	models.IssueTemplatedTitle: "Generic titles", models.IssueTemplatedMeta: "Generic meta descriptions",
	models.IssueDuplicateText: "Repeated page text", models.IssueWeakHeading: "Weak headings",
	models.IssueThinContent: "Thin content", models.IssueUnavailable: "Unavailable pages",
	models.IssueInternalLinks: "Internal linking", models.IssueStructuredData: "Structured data",
	models.IssueTechnical: "Technical problem",
}

// analysisDraft builds a task from suggestions found by page analyses.
func analysisDraft(projectOID bson.ObjectID, issue, group string, list []models.Suggestion, analyses map[bson.ObjectID]models.PageAnalysis, groupPages []models.PageProfile, stats map[string]pageStat, sampled int) models.SEOTask {
	d := models.SEOTask{
		ProjectID: projectOID, Key: taskKey(issue, group), Source: models.TaskSourceAnalysis, Issue: issue,
		Scope: models.TaskScopeGroup, PageGroup: group,
		Title: fmt.Sprintf("%s on %s", issueLabels[issue], groupLabel(group)),
	}
	var confirmed []string
	sort.SliceStable(list, func(i, j int) bool { return list[i].Priority < list[j].Priority })
	for _, s := range list {
		confirmed = append(confirmed, s.Page)
		d.Examples = mergeExamples(d.Examples, []models.TaskExample{suggestionExample(s)})
		d.Checks = mergeChecks(d.Checks, suggestionCheck(s))
		d.SuggestionIDs = append(d.SuggestionIDs, s.ID)
		if d.Goal == "" {
			d.Goal = "Fix this once for the whole page type instead of page by page. " + s.Reason
		}
		if s.AnalysisID == nil {
			continue
		}
		a, ok := analyses[*s.AnalysisID]
		if !ok {
			continue
		}
		d.AnalysisIDs = append(d.AnalysisIDs, a.ID)
		d.Evidence = mergeEvidence(d.Evidence, a.Evidence)
		d.Confidence = weakestConfidence(d.Confidence, a.Confidence)
		d.Language = a.Language
		if d.Problem == "" {
			d.Problem = strings.TrimSpace(a.PrimaryIssue + " " + a.RootCause)
		}
	}
	if d.Problem == "" {
		d.Problem = list[0].Reason
	}
	if d.Confidence == "" {
		d.Confidence = "low"
	}
	d.ConfirmedPages = uniqueStrings(confirmed)
	// One page cannot show that a whole page type shares its problem.
	if len(d.ConfirmedPages) < 2 {
		d.Confidence = weakestConfidence(d.Confidence, "low")
	}
	d.Pages = uniqueStrings(confirmed, pageURLs(groupPages))
	d.Metrics = taskMetrics(d.Pages, stats, sampled)
	return d
}

// mergeAnalysisFindings folds suggestions sharing one problem into tasks, so the
// harness and the reviewer see that problem once.
func (p *Processor) mergeAnalysisFindings(ctx context.Context, project *models.Project, now time.Time) error {
	cursor, err := p.db.Collection("suggestions").Find(ctx, bson.M{
		"project_id": project.ID, "status": models.SuggestionStatusPending,
		"task_id": bson.M{"$exists": false}, "issue": bson.M{"$exists": true},
		"created_at": bson.M{"$gte": now.Add(-suggestionMergeWindow)},
	})
	if err != nil {
		return err
	}
	var suggestions []models.Suggestion
	if err = cursor.All(ctx, &suggestions); err != nil {
		return err
	}
	if len(suggestions) == 0 {
		return nil
	}

	taskCursor, err := p.db.Collection("seo_tasks").Find(ctx, bson.M{"project_id": project.ID})
	if err != nil {
		return err
	}
	var tasks []models.SEOTask
	if err = taskCursor.All(ctx, &tasks); err != nil {
		return err
	}
	profiles, err := p.loadProfiles(ctx, project.ID)
	if err != nil {
		return err
	}
	stats, err := p.sitePageStats(ctx, project.ID, profiles)
	if err != nil {
		return err
	}
	analyses, err := p.analysesOf(ctx, suggestions)
	if err != nil {
		return err
	}

	plan := planMerge(suggestions, tasks)
	for _, i := range sortedInts(plan.Attach) {
		list := plan.Attach[i]
		d := analysisDraft(project.ID, tasks[i].Issue, tasks[i].PageGroup, list, analyses, nil, stats, len(profiles))
		// The pattern's own wording stays: proposals only add to it.
		d.Key, d.Source, d.Title, d.Problem, d.Goal, d.Confidence, d.Scope =
			tasks[i].Key, tasks[i].Source, tasks[i].Title, tasks[i].Problem, tasks[i].Goal, tasks[i].Confidence, tasks[i].Scope
		d.Checks = tasks[i].Checks
		d.Metrics = taskMetrics(uniqueStrings(tasks[i].Pages, d.Pages), stats, len(profiles))
		d.Source = models.TaskSourceAnalysis
		merged := tasks[i]
		mergeIntoTask(&merged, d, time.Time{}, now)
		merged.Source = tasks[i].Source
		if _, err = p.db.Collection("seo_tasks").ReplaceOne(ctx, bson.M{"_id": merged.ID}, merged); err != nil {
			return fmt.Errorf("update task: %w", err)
		}
		if err = p.coverSuggestions(ctx, list, merged.ID, now); err != nil {
			return err
		}
	}
	for _, key := range sortedKeys(plan.Groups) {
		issue, group, _ := strings.Cut(key, "|")
		list := plan.Groups[key]
		groupPages := where(profiles, func(pr models.PageProfile) bool { return pr.Group == group })
		d := analysisDraft(project.ID, issue, group, list, analyses, groupPages, stats, len(profiles))
		task, upErr := UpsertTask(ctx, p.db, d, time.Time{}, now)
		if upErr != nil {
			return upErr
		}
		if err = p.coverSuggestions(ctx, list, task.ID, now); err != nil {
			return err
		}
	}
	return nil
}

func sortedInts[V any](m map[int]V) []int {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	return keys
}

func (p *Processor) analysesOf(ctx context.Context, suggestions []models.Suggestion) (map[bson.ObjectID]models.PageAnalysis, error) {
	var ids []bson.ObjectID
	for _, s := range suggestions {
		if s.AnalysisID != nil {
			ids = append(ids, *s.AnalysisID)
		}
	}
	return LoadAnalyses(ctx, p.db, ids)
}

// LoadAnalyses returns analyses by ID, without their stored prompts.
func LoadAnalyses(ctx context.Context, db *mongo.Database, ids []bson.ObjectID) (map[bson.ObjectID]models.PageAnalysis, error) {
	out := make(map[bson.ObjectID]models.PageAnalysis, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	cursor, err := db.Collection("page_analyses").Find(ctx, bson.M{"_id": bson.M{"$in": ids}},
		options.Find().SetProjection(bson.M{"prompt": 0, "raw_response": 0}))
	if err != nil {
		return nil, err
	}
	var rows []models.PageAnalysis
	if err = cursor.All(ctx, &rows); err != nil {
		return nil, err
	}
	for _, a := range rows {
		out[a.ID] = a
	}
	return out, nil
}

// coverSuggestions takes merged suggestions out of the review queue.
func (p *Processor) coverSuggestions(ctx context.Context, list []models.Suggestion, taskID bson.ObjectID, now time.Time) error {
	ids := make([]bson.ObjectID, 0, len(list))
	for _, s := range list {
		ids = append(ids, s.ID)
	}
	_, err := p.db.Collection("suggestions").UpdateMany(ctx, bson.M{"_id": bson.M{"$in": ids}},
		bson.M{"$set": bson.M{"status": models.SuggestionStatusCovered, "task_id": taskID, "updated_at": now}})
	return err
}

// TaskFromSuggestion builds the task of one suggestion marked Apply on site.
func TaskFromSuggestion(s models.Suggestion, a *models.PageAnalysis, stats pageStat) models.SEOTask {
	issue := s.Issue
	if issue == "" {
		issue = models.IssueOther
	}
	proposed := s.FinalValue
	if proposed == "" {
		proposed = s.ProposedValue
	}
	d := models.SEOTask{
		ProjectID: s.ProjectID, Key: taskKey(issue, s.Kind, normalizePageURL(s.Page)), Source: models.TaskSourceReview,
		Issue: issue, Scope: models.TaskScopePage, PageGroup: s.PageGroup, Language: s.Language,
		Title:   fmt.Sprintf("%s: %s", capitalize(strings.ReplaceAll(s.Kind, "_", " ")), pagePath(s.Page)),
		Problem: s.Reason, Goal: proposed, Confidence: "medium",
		Pages: []string{s.Page}, ConfirmedPages: []string{s.Page},
		Metrics:       models.TaskMetrics{Pages: 1, PagesSampled: 1, Impressions: stats.Impressions, Clicks: stats.Clicks, Sessions: stats.Sessions},
		Examples:      []models.TaskExample{suggestionExample(s)},
		Checks:        suggestionCheck(s),
		SuggestionIDs: []bson.ObjectID{s.ID},
	}
	if a != nil {
		d.Problem = strings.TrimSpace(a.PrimaryIssue + " " + a.RootCause + " " + s.Reason)
		d.Evidence = a.Evidence
		d.Confidence = a.Confidence
		d.AnalysisIDs = []bson.ObjectID{a.ID}
	}
	return d
}

// PageStat returns one page's 28-day search totals.
func PageStat(ctx context.Context, db *mongo.Database, projectOID bson.ObjectID, page string) pageStat {
	curStart, end, _, _ := analysisWindows()
	cursor, err := db.Collection("gsc_rows").Aggregate(ctx, bson.A{
		bson.D{{Key: "$match", Value: bson.D{
			{Key: "project_id", Value: projectOID}, {Key: "page", Value: page}, {Key: "date", Value: dateRange(curStart, end)},
		}}},
		bson.D{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: nil},
			{Key: "clicks", Value: bson.D{{Key: "$sum", Value: "$clicks"}}},
			{Key: "impressions", Value: bson.D{{Key: "$sum", Value: "$impressions"}}},
		}}},
	})
	if err != nil {
		return pageStat{}
	}
	defer cursor.Close(ctx) //nolint:errcheck
	var rows []struct {
		Clicks      float64 `bson:"clicks"`
		Impressions float64 `bson:"impressions"`
	}
	if err = cursor.All(ctx, &rows); err != nil || len(rows) == 0 {
		return pageStat{}
	}
	return pageStat{Clicks: rows[0].Clicks, Impressions: rows[0].Impressions}
}

// snapshotField returns the text a check looks at.
func snapshotField(snap *models.PageSnapshot, field string) string {
	switch field {
	case "title":
		return snap.Title
	case "meta_description":
		return snap.MetaDescription
	case "h1":
		return contentH1(snap)
	case "category_text":
		return snap.CategoryText
	}
	return ""
}

// evaluateChecks tests checks on freshly read pages and lists what failed.
func evaluateChecks(checks []models.TaskCheck, snaps []*models.PageSnapshot) []string {
	var failures []string
	fail := func(c models.TaskCheck, page string) {
		failures = append(failures, fmt.Sprintf("%s: expected that %s", page, c.Describe()))
	}
	for _, c := range checks {
		if c.Type == models.CheckUnique {
			first := make(map[string]string)
			for _, s := range snaps {
				value := normalizedText(snapshotField(s, c.Field))
				if strings.TrimSpace(value) == "" {
					continue
				}
				if other, dup := first[value]; dup {
					fail(c, s.SourceURL+" and "+other)
				} else {
					first[value] = s.SourceURL
				}
			}
			continue
		}
		for _, s := range snaps {
			if c.Page != "" && normalizePageURL(c.Page) != s.URL {
				continue
			}
			value := snapshotField(s, c.Field)
			ok := true
			switch c.Type {
			case models.CheckNotContains:
				ok = !strings.Contains(normalizedText(value), " "+c.Value+" ")
			case models.CheckMaxLength:
				ok = len([]rune(value)) <= c.Limit
			case models.CheckNotEmpty:
				ok = strings.TrimSpace(value) != ""
			case models.CheckChangedFrom:
				ok = normalizedText(value) != normalizedText(c.Value)
			case models.CheckStatusOK:
				ok = s.StatusCode < 400
			}
			if !ok {
				fail(c, s.SourceURL)
			}
		}
	}
	return failures
}

// verifySample picks the pages to reread: those a check names first, then an
// even spread of the others.
func verifySample(t *models.SEOTask, n int) []string {
	var named []string
	for _, c := range t.Checks {
		if c.Page != "" {
			named = append(named, c.Page)
		}
	}
	named = uniqueStrings(named)
	if len(named) >= n {
		return named[:n]
	}
	picked := named
	room := n - len(named)
	step := float64(len(t.Pages)) / float64(room)
	if step < 1 {
		step = 1
	}
	for i := 0.0; int(i) < len(t.Pages) && room > 0; i += step {
		picked = uniqueStrings(picked, []string{t.Pages[int(i)]})
		room--
	}
	return picked
}

// NewVerifyTask creates the asynq task rechecking a done task.
func NewVerifyTask(taskID string) (*asynq.Task, error) {
	payload, err := json.Marshal(VerifyTaskPayload{TaskID: taskID})
	if err != nil {
		return nil, fmt.Errorf("marshal tasks:verify payload: %w", err)
	}
	return asynq.NewTask(TypeVerifyTask, payload, asynq.MaxRetry(3)), nil
}

// HandleTaskVerify rereads a sample of a done task's pages and confirms or
// reopens the task. Tasks with no checks stay done for a person to review.
func (p *Processor) HandleTaskVerify(ctx context.Context, t *asynq.Task) error {
	var payload VerifyTaskPayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		return fmt.Errorf("%w: unmarshal tasks:verify payload: %v", asynq.SkipRetry, err)
	}
	id, err := bson.ObjectIDFromHex(payload.TaskID)
	if err != nil {
		return fmt.Errorf("%w: invalid task id: %v", asynq.SkipRetry, err)
	}
	col := p.db.Collection("seo_tasks")
	var task models.SEOTask
	if err = col.FindOne(ctx, bson.M{"_id": id}).Decode(&task); err != nil {
		return fmt.Errorf("%w: task not found: %v", asynq.SkipRetry, err)
	}
	if task.Status != models.TaskStatusDone {
		return nil
	}
	now := time.Now().UTC()
	if len(task.Checks) == 0 {
		v := models.TaskVerification{At: now, Note: "This task cannot be checked from outside. A person reviews the result."}
		_, err = col.UpdateOne(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"verification": v, "updated_at": now}})
		return err
	}

	var project models.Project
	if err = p.db.Collection("projects").FindOne(ctx, bson.M{"_id": task.ProjectID}).Decode(&project); err != nil {
		return fmt.Errorf("%w: project not found: %v", asynq.SkipRetry, err)
	}
	pacer := &renderPacer{delay: p.siteCrawlDelay(ctx, project.URL), remaining: verifySamplePage}
	var snaps []*models.PageSnapshot
	for _, page := range verifySample(&task, verifySamplePage) {
		snap, readErr := p.readPage(ctx, project.ID, page, project.UseJS, pacer)
		if readErr != nil {
			p.logger.Warn("task verify: page not read", zap.String("page", page), zap.Error(readErr))
			continue
		}
		snaps = append(snaps, snap)
	}
	if len(snaps) == 0 {
		return fmt.Errorf("task %s: no page could be read", payload.TaskID)
	}

	failures := evaluateChecks(task.Checks, snaps)
	status := models.TaskStatusVerified
	if len(failures) > 0 {
		status = models.TaskStatusReopened
	}
	v := models.TaskVerification{At: time.Now().UTC(), Checked: len(snaps), Failures: failures}
	_, err = col.UpdateOne(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"status": status, "verification": v, "updated_at": v.At}})
	return err
}
