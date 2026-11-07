package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hibiken/asynq"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.uber.org/zap"

	"github.com/creever/crawler-api/models"
)

const (
	// suggestDefaultLimit caps how many pages get analyzed per run.
	suggestDefaultLimit = 10
	// suggestLanguage is the language AI suggestions are generated in.
	suggestLanguage = "hu"
	// analysisMaxTokens leaves room for a diagnosis plus a full category text.
	analysisMaxTokens = 4000
	// analysisRenderBudget caps Chrome renders per run on the small crawler box.
	analysisRenderBudget = 20
	// analysisOppsPerLimit is how many ranked opportunities are read per page
	// of the limit, since several opportunities can fall on one page.
	analysisOppsPerLimit = 10

	// recentChangeWindow keeps a page out of bulk runs after a change was
	// applied, so its impact can be measured.
	recentChangeWindow = 28 * 24 * time.Hour
	// unchangedPageWindow keeps an unchanged page out of bulk runs after it was
	// analyzed.
	unchangedPageWindow = 30 * 24 * time.Hour
)

// aiCallFn sends a prompt to the configured LLM provider.
type aiCallFn func(ctx context.Context, system, user string, maxTokens int) (string, error)

// suggestionAI returns the project's LLM caller with its provider and model.
func (p *Processor) suggestionAI(ctx context.Context, projectOID bson.ObjectID) (aiCallFn, string, string, error) {
	provider, apiKey, model, err := p.resolveSuggestionAI(ctx, projectOID)
	if err != nil {
		return nil, "", "", err
	}
	call := func(ctx context.Context, system, user string, maxTokens int) (string, error) {
		switch provider {
		case models.BlogAIProviderMistral:
			return p.callMistralAPI(ctx, apiKey, model, system, user, maxTokens)
		case models.BlogAIProviderGemini:
			return p.callGeminiTextAPI(ctx, apiKey, system, user)
		default:
			return p.callClaudeAPI(ctx, apiKey, model, system, user, maxTokens)
		}
	}
	return call, string(provider), model, nil
}

// analysisTarget is one page with all of its opportunity signals.
type analysisTarget struct {
	Page string
	Opps []models.Opportunity
}

// groupOpportunitiesByPage merges ranked opportunities into one target per
// page, keeping rank order. Spellings of one URL count as the same page.
func groupOpportunitiesByPage(opps []models.Opportunity) []analysisTarget {
	index := make(map[string]int)
	var targets []analysisTarget
	for _, o := range opps {
		key := normalizePageURL(o.Page)
		if i, ok := index[key]; ok {
			targets[i].Opps = append(targets[i].Opps, o)
			continue
		}
		index[key] = len(targets)
		targets = append(targets, analysisTarget{Page: o.Page, Opps: []models.Opportunity{o}})
	}
	return targets
}

// analysisOutcome is the result of analyzing one page.
type analysisOutcome struct {
	Analysis models.PageAnalysis
	Actions  []analysisAction
}

func outcomeWith(status models.AnalysisStatus, reason string) *analysisOutcome {
	return &analysisOutcome{Analysis: models.PageAnalysis{Status: status, StatusReason: reason}}
}

// gateAnalysis decides whether a page can go to the LLM at all. It returns nil
// when it can. A broken or deindexed page is a finding by itself, so it needs
// no LLM; a page with no readable content must not reach the LLM.
func gateAnalysis(page string, snap *models.PageSnapshot, snapErr error) *analysisOutcome {
	if page == "" {
		return outcomeWith(models.AnalysisStatusFailed, "Az opportunity nem tartalmaz céloldalt.")
	}
	if snapErr != nil {
		return outcomeWith(models.AnalysisStatusFailed, "Az oldal nem olvasható: "+snapErr.Error())
	}
	if snap.StatusCode >= 400 {
		o := technicalOutcome(snap, fmt.Sprintf("Az oldal HTTP %d hibával válaszol.", snap.StatusCode),
			"Az oldal hibát ad vissza, ezért a keresőből kieshet, a tartalma pedig nem elemezhető.",
			models.SuggestionKindTechnical, "Állítsd helyre az oldalt, vagy irányítsd át 301-gyel a legközelebbi releváns oldalra.")
		return &o
	}
	if strings.Contains(strings.ToLower(snap.Robots), "noindex") {
		o := technicalOutcome(snap, "Az oldal noindex jelölést kapott.",
			"A noindex miatt a Google nem tartja indexben az oldalt, így minden más javítás hatástalan.",
			models.SuggestionKindNoindex, "Ellenőrizd, hogy a noindex szándékos-e. Ha nem, távolítsd el a meta robots jelölést.")
		return &o
	}
	if snap.LowConfidence {
		return outcomeWith(models.AnalysisStatusInsufficientContext,
			fmt.Sprintf("Az oldalon csak %d tartalmi szó ismerhető fel, ez nem elég megalapozott elemzéshez. Ellenőrizd a renderelést.", snap.ContentWordCount))
	}
	return nil
}

func technicalOutcome(snap *models.PageSnapshot, issue, cause, kind, proposed string) analysisOutcome {
	fact := fmt.Sprintf("HTTP status: %d, meta robots: %q", snap.StatusCode, snap.Robots)
	return analysisOutcome{
		Analysis: models.PageAnalysis{
			Status: models.AnalysisStatusCompleted, PrimaryIssue: issue, RootCause: cause, Confidence: "high",
			Evidence: []models.AnalysisEvidence{{Ref: "P1", Claim: issue, Fact: fact}},
		},
		Actions: []analysisAction{{Kind: kind, Priority: 1, Effort: "S", Proposed: proposed, Rationale: cause, EvidenceRefs: []string{"P1"}}},
	}
}

func parseAnalysis(raw string) (*analysisResponse, error) {
	var resp analysisResponse
	if err := json.Unmarshal([]byte(extractJSON(raw)), &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// runLLMAnalysis asks the model for a diagnosis and validates the answer. A
// rejected answer gets one repair attempt; what still fails is dropped, and
// the reasons are kept as warnings for the reviewer.
func runLLMAnalysis(ctx context.Context, ai aiCallFn, pack *contextPack, opps []models.Opportunity, snap *models.PageSnapshot) analysisOutcome {
	system, user := buildAnalysisPrompt(pack, opps)
	outcome := analysisOutcome{Analysis: models.PageAnalysis{Prompt: user}}

	raw, err := ai(ctx, system, user, analysisMaxTokens)
	if err != nil {
		outcome.Analysis.Status = models.AnalysisStatusFailed
		outcome.Analysis.StatusReason = "Az AI-hívás sikertelen: " + err.Error()
		return outcome
	}
	outcome.Analysis.RawResponse = raw

	resp, parseErr := parseAnalysis(raw)
	var checked validatedAnalysis
	problems := []string{}
	if parseErr != nil {
		problems = append(problems, "the answer is not valid JSON: "+parseErr.Error())
	} else {
		checked = validateAnalysis(resp, pack, snap)
		problems = checked.Problems
	}

	if len(problems) > 0 {
		if repaired, repairErr := ai(ctx, system, buildRepairPrompt(user, raw, problems), analysisMaxTokens); repairErr == nil {
			if r2, err2 := parseAnalysis(repaired); err2 == nil {
				c2 := validateAnalysis(r2, pack, snap)
				if resp == nil || len(c2.Problems) <= len(problems) {
					resp, checked, problems = r2, c2, c2.Problems
					outcome.Analysis.RawResponse = repaired
				}
			}
		}
	}

	if resp == nil {
		outcome.Analysis.Status = models.AnalysisStatusFailed
		outcome.Analysis.StatusReason = "A modell válasza nem értelmezhető: " + parseErr.Error()
		return outcome
	}
	if strings.TrimSpace(resp.Diagnosis.PrimaryIssue) == "" && len(checked.Actions) == 0 && resp.NoActionReason == "" {
		outcome.Analysis.Status = models.AnalysisStatusFailed
		outcome.Analysis.StatusReason = "A modell nem adott diagnózist."
		outcome.Analysis.ValidationWarnings = problems
		return outcome
	}

	a := &outcome.Analysis
	a.PrimaryIssue = strings.TrimSpace(resp.Diagnosis.PrimaryIssue)
	a.RootCause = strings.TrimSpace(resp.Diagnosis.RootCause)
	a.Confidence = normalizeConfidence(resp.Diagnosis.Confidence)
	a.Evidence = checked.Evidence
	a.RuledOut = resp.Diagnosis.RuledOut
	a.MissingData = resp.MissingData
	a.ValidationWarnings = problems
	outcome.Actions = checked.Actions

	a.Status = models.AnalysisStatusCompleted
	if len(checked.Actions) == 0 && strings.TrimSpace(resp.NoActionReason) != "" {
		a.Status = models.AnalysisStatusNoAction
		a.NoActionReason = strings.TrimSpace(resp.NoActionReason)
	}
	return outcome
}

func normalizeConfidence(c string) string {
	switch c = strings.ToLower(strings.TrimSpace(c)); c {
	case "high", "medium", "low":
		return c
	}
	return "low"
}

func normalizeEffort(e string) string {
	switch e = strings.ToUpper(strings.TrimSpace(e)); e {
	case "S", "M", "L":
		return e
	}
	return "M"
}

// currentValue returns what a suggestion would replace on the page.
func currentValue(kind string, snap *models.PageSnapshot) string {
	if snap == nil {
		return ""
	}
	switch kind {
	case models.SuggestionKindTitle:
		return snap.Title
	case models.SuggestionKindMeta:
		return snap.MetaDescription
	case models.SuggestionKindCategoryText:
		return snap.CategoryText
	case models.SuggestionKindH1:
		for _, h := range snap.Headings {
			if h.Level == 1 && h.Role == models.HeadingRoleContent {
				return h.Text
			}
		}
	case models.SuggestionKindCanonical:
		return snap.Canonical
	case models.SuggestionKindNoindex:
		return snap.Robots
	}
	return ""
}

// buildSuggestions turns validated actions into pending suggestions.
func buildSuggestions(a *models.PageAnalysis, actions []analysisAction, target analysisTarget, snap *models.PageSnapshot, now time.Time) []models.Suggestion {
	query := ""
	var oppID *bson.ObjectID
	if len(target.Opps) > 0 {
		query = target.Opps[0].Query
		oppID = &target.Opps[0].ID
	}
	out := make([]models.Suggestion, 0, len(actions))
	for i, act := range actions {
		priority := act.Priority
		if priority <= 0 {
			priority = i + 1
		}
		out = append(out, models.Suggestion{
			ID: bson.NewObjectID(), ProjectID: a.ProjectID, OpportunityID: oppID, AnalysisID: &a.ID,
			Kind: act.Kind, Target: strings.TrimSpace(act.Target), Priority: priority, Effort: normalizeEffort(act.Effort),
			EvidenceRefs: act.EvidenceRefs, Page: target.Page, Query: query, Language: suggestLanguage,
			CurrentValue: currentValue(act.Kind, snap), ProposedValue: act.Proposed,
			Status: models.SuggestionStatusPending, Reason: strings.TrimSpace(act.Rationale),
			CreatedAt: now, UpdatedAt: now,
		})
	}
	return out
}

// analyzeTarget reads a page and its context, and runs the gate and the LLM.
// It returns the snapshot too, which may be nil, and stored-independent output.
func (p *Processor) analyzeTarget(ctx context.Context, project *models.Project, target analysisTarget, ai aiCallFn, pacer *renderPacer) (analysisOutcome, *models.PageSnapshot, error) {
	var snap *models.PageSnapshot
	var snapErr error
	if target.Page != "" {
		snap, snapErr = p.getSnapshot(ctx, project.ID, target.Page, project.UseJS, pacer)
		if errors.Is(snapErr, errRenderBudget) || ctx.Err() != nil {
			return analysisOutcome{}, nil, snapErr
		}
	}
	if gated := gateAnalysis(target.Page, snap, snapErr); gated != nil {
		return *gated, snap, nil
	}

	input := contextInput{Brand: project.Name, Page: target.Page, Opportunities: target.Opps, Snapshot: snap}
	var missing []string
	queries, devices, competing, err := p.pageSearchData(ctx, project.ID, target.Page)
	if err != nil {
		p.logger.Warn("analysis: search data unavailable", zap.String("page", target.Page), zap.Error(err))
		missing = append(missing, "Search Console adatok")
	}
	input.Queries, input.Devices = queries, devices
	if input.GA4, err = p.pageGA4(ctx, project.ID, target.Page); err != nil {
		p.logger.Warn("analysis: GA4 data unavailable", zap.String("page", target.Page), zap.Error(err))
		missing = append(missing, "GA4 adatok")
	}

	for _, rel := range pickRelatedPages(target.Page, target.Opps, competing) {
		relSnap, relErr := p.getSnapshot(ctx, project.ID, rel.URL, project.UseJS, pacer)
		if relErr != nil || relSnap.StatusCode >= 400 {
			input.RelatedFailed = append(input.RelatedFailed, rel.URL)
			continue
		}
		rel.Snapshot = relSnap
		input.Related = append(input.Related, rel)
	}

	pack := buildContextPack(input)
	outcome := runLLMAnalysis(ctx, ai, pack, target.Opps, snap)
	outcome.Analysis.Coverage = pack.Coverage
	outcome.Analysis.MissingData = append(outcome.Analysis.MissingData, missing...)
	return outcome, snap, nil
}

// finishAnalysis fills the fields common to every outcome.
func finishAnalysis(a *models.PageAnalysis, projectOID bson.ObjectID, target analysisTarget, snap *models.PageSnapshot, provider, model string, now time.Time) {
	a.ID = bson.NewObjectID()
	a.ProjectID = projectOID
	a.Page = target.Page
	a.Provider, a.Model = provider, model
	a.CreatedAt = now
	a.OpportunityIDs = make([]bson.ObjectID, 0, len(target.Opps))
	for _, o := range target.Opps {
		a.OpportunityIDs = append(a.OpportunityIDs, o.ID)
		found := false
		for _, s := range a.Signals {
			found = found || s == o.Type
		}
		if !found {
			a.Signals = append(a.Signals, o.Type)
		}
	}
	if snap != nil {
		a.Fingerprint = snap.Fingerprint
		if a.Coverage.SnapshotAt.IsZero() {
			a.Coverage.SnapshotAt = snap.FetchedAt
			a.Coverage.ContentWords = snap.ContentWordCount
		}
	}
}

// storeAnalysis persists an analysis and its suggestions.
func (p *Processor) storeAnalysis(ctx context.Context, a *models.PageAnalysis, suggestions []models.Suggestion) error {
	if _, err := p.db.Collection("page_analyses").InsertOne(ctx, a); err != nil {
		return fmt.Errorf("insert analysis: %w", err)
	}
	if len(suggestions) == 0 {
		return nil
	}
	docs := make([]interface{}, 0, len(suggestions))
	for _, s := range suggestions {
		docs = append(docs, s)
	}
	if _, err := p.db.Collection("suggestions").InsertMany(ctx, docs); err != nil {
		return fmt.Errorf("insert suggestions: %w", err)
	}
	return nil
}

// pageIsBusy reports whether a bulk run should leave a page alone: a reviewer
// still has its suggestions, or a recent change is being measured.
func (p *Processor) pageIsBusy(ctx context.Context, projectOID bson.ObjectID, page string, now time.Time) (bool, error) {
	pending, err := p.db.Collection("suggestions").CountDocuments(ctx, bson.M{
		"project_id": projectOID, "page": page, "status": models.SuggestionStatusPending,
	}, options.Count().SetLimit(1))
	if err != nil {
		return false, err
	}
	if pending > 0 {
		return true, nil
	}
	applied, err := p.db.Collection("applied_changes").CountDocuments(ctx, bson.M{
		"project_id": projectOID, "page": page, "status": "applied",
		"applied_at": bson.M{"$gte": now.Add(-recentChangeWindow)},
	}, options.Count().SetLimit(1))
	if err != nil {
		return false, err
	}
	return applied > 0, nil
}

// pageIsUnchanged reports whether the page's content is the same as at its
// last finished analysis. It may render the page, which the later analysis
// then reuses from the snapshot cache.
func (p *Processor) pageIsUnchanged(ctx context.Context, project *models.Project, page string, pacer *renderPacer, now time.Time) (bool, error) {
	last, err := p.latestAnalysisFingerprint(ctx, project.ID, page, now.Add(-unchangedPageWindow))
	if err != nil || last == "" {
		return false, err
	}
	snap, err := p.getSnapshot(ctx, project.ID, page, project.UseJS, pacer)
	if err != nil {
		return false, err
	}
	return snap.Fingerprint == last, nil
}

// loadTargets returns the pages to analyze. A single requested opportunity is
// analyzed with the page's other signals; a bulk run covers the ranked
// opportunities of all signal types.
func (p *Processor) loadTargets(ctx context.Context, projectOID bson.ObjectID, payload GenSuggestPayload, limit int) ([]analysisTarget, error) {
	if payload.OpportunityID != "" {
		oppOID, err := bson.ObjectIDFromHex(payload.OpportunityID)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid opportunity_id: %v", asynq.SkipRetry, err)
		}
		var opp models.Opportunity
		if err = p.db.Collection("opportunities").FindOne(ctx, bson.M{
			"_id": oppOID, "project_id": projectOID,
		}).Decode(&opp); err != nil {
			return nil, fmt.Errorf("%w: opportunity not found: %v", asynq.SkipRetry, err)
		}
		opps := []models.Opportunity{opp}
		if opp.Page != "" {
			siblings, sibErr := p.findOpportunities(ctx, bson.M{"project_id": projectOID, "page": opp.Page, "_id": bson.M{"$ne": opp.ID}}, 0)
			if sibErr != nil {
				return nil, sibErr
			}
			opps = append(opps, siblings...)
		}
		return []analysisTarget{{Page: opp.Page, Opps: opps}}, nil
	}

	opps, err := p.findOpportunities(ctx, bson.M{"project_id": projectOID, "page": bson.M{"$ne": ""}}, int64(limit*analysisOppsPerLimit))
	if err != nil {
		return nil, err
	}
	return groupOpportunitiesByPage(opps), nil
}

func (p *Processor) findOpportunities(ctx context.Context, filter bson.M, limit int64) ([]models.Opportunity, error) {
	opts := options.Find().SetSort(bson.D{{Key: "rank", Value: 1}})
	if limit > 0 {
		opts = opts.SetLimit(limit)
	}
	cursor, err := p.db.Collection("opportunities").Find(ctx, filter, opts)
	if err != nil {
		return nil, fmt.Errorf("query opportunities: %w", err)
	}
	defer cursor.Close(ctx) //nolint:errcheck
	var opps []models.Opportunity
	if err = cursor.All(ctx, &opps); err != nil {
		return nil, fmt.Errorf("decode opportunities: %w", err)
	}
	return opps, nil
}

// HandleSuggestionsGenerate processes a suggestions:generate task. For each
// target page it reads the page, gathers its search data, has the LLM diagnose
// it, validates the answer, and stores the analysis with its pending
// suggestions for human review.
func (p *Processor) HandleSuggestionsGenerate(ctx context.Context, t *asynq.Task) error {
	var payload GenSuggestPayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		return fmt.Errorf("%w: unmarshal suggestions:generate payload: %v", asynq.SkipRetry, err)
	}
	projectOID, err := bson.ObjectIDFromHex(payload.ProjectID)
	if err != nil {
		return fmt.Errorf("%w: invalid project_id: %v", asynq.SkipRetry, err)
	}

	var project models.Project
	if err = p.db.Collection("projects").FindOne(ctx, bson.M{"_id": projectOID}).Decode(&project); err != nil {
		return fmt.Errorf("%w: project not found: %v", asynq.SkipRetry, err)
	}

	limit := payload.Limit
	if limit <= 0 {
		limit = suggestDefaultLimit
	}
	bulk := payload.OpportunityID == ""
	if bulk {
		// Marked first: a run may end with nothing to store, and the scheduler
		// must still count it as today's run.
		if _, err = p.db.Collection("suggestion_runs").UpdateOne(ctx, bson.M{"project_id": projectOID},
			bson.M{"$set": bson.M{"started_at": time.Now().UTC()}}, options.UpdateOne().SetUpsert(true),
		); err != nil {
			return fmt.Errorf("mark suggestion run: %w", err)
		}
	}

	ai, provider, model, err := p.suggestionAI(ctx, projectOID)
	if err != nil {
		return fmt.Errorf("%w: %v", asynq.SkipRetry, err)
	}
	targets, err := p.loadTargets(ctx, projectOID, payload, limit)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		p.logger.Info("suggestions:generate nothing to do", zap.String("project_id", payload.ProjectID))
		return nil
	}

	pacer := &renderPacer{delay: p.siteCrawlDelay(ctx, project.URL), remaining: analysisRenderBudget}
	analyzed, generated := 0, 0
	for _, target := range targets {
		if analyzed >= limit {
			break
		}
		now := time.Now().UTC()
		if bulk {
			busy, busyErr := p.pageIsBusy(ctx, projectOID, target.Page, now)
			if busyErr != nil {
				return fmt.Errorf("check page state: %w", busyErr)
			}
			if busy {
				continue
			}
			unchanged, chErr := p.pageIsUnchanged(ctx, &project, target.Page, pacer, now)
			if errors.Is(chErr, errRenderBudget) {
				break
			}
			if chErr == nil && unchanged {
				continue
			}
		}

		outcome, snap, targetErr := p.analyzeTarget(ctx, &project, target, ai, pacer)
		if errors.Is(targetErr, errRenderBudget) {
			p.logger.Info("suggestions:generate render budget used up", zap.String("project_id", payload.ProjectID))
			break
		}
		if targetErr != nil {
			return targetErr
		}

		a := outcome.Analysis
		finishAnalysis(&a, projectOID, target, snap, provider, model, now)
		suggestions := buildSuggestions(&a, outcome.Actions, target, snap, now)
		if err = p.storeAnalysis(ctx, &a, suggestions); err != nil {
			p.logger.Warn("suggestions:generate store failed", zap.String("page", target.Page), zap.Error(err))
			continue
		}
		analyzed++
		generated += len(suggestions)
		p.logger.Info("page analyzed",
			zap.String("page", target.Page),
			zap.String("status", string(a.Status)),
			zap.Int("suggestions", len(suggestions)),
			zap.Int("warnings", len(a.ValidationWarnings)),
		)
	}

	p.logger.Info("suggestions:generate completed",
		zap.String("project_id", payload.ProjectID),
		zap.Int("pages", analyzed),
		zap.Int("suggestions", generated),
	)
	return nil
}

// PreviewAnalysis analyzes a page without storing the analysis or suggestions.
// Only the snapshot cache is written.
func (p *Processor) PreviewAnalysis(ctx context.Context, project *models.Project, page string, opps []models.Opportunity) (*models.PageAnalysis, []models.Suggestion, error) {
	ai, provider, model, err := p.suggestionAI(ctx, project.ID)
	if err != nil {
		return nil, nil, err
	}
	pacer := &renderPacer{delay: p.siteCrawlDelay(ctx, project.URL), remaining: analysisRenderBudget}
	target := analysisTarget{Page: page, Opps: opps}
	outcome, snap, err := p.analyzeTarget(ctx, project, target, ai, pacer)
	if err != nil {
		return nil, nil, err
	}
	now := time.Now().UTC()
	a := outcome.Analysis
	finishAnalysis(&a, project.ID, target, snap, provider, model, now)
	return &a, buildSuggestions(&a, outcome.Actions, target, snap, now), nil
}

// resolveAnthropicKey finds the Anthropic API key for a project: first the
// project's active blog config, then the global "anthropic_api_key" setting.
func (p *Processor) resolveAnthropicKey(ctx context.Context, projectOID bson.ObjectID) (string, error) {
	var cfg models.BlogConfig
	err := p.db.Collection("blog_configs").FindOne(ctx, bson.M{
		"project_id": projectOID, "active": true,
	}).Decode(&cfg)
	if err == nil && cfg.AnthropicAPIKey != "" {
		return cfg.AnthropicAPIKey, nil
	}
	var s models.Setting
	if sErr := p.db.Collection("settings").FindOne(ctx, bson.M{"key": "anthropic_api_key"}).Decode(&s); sErr == nil && s.Value != "" {
		return s.Value, nil
	}
	return "", fmt.Errorf("no Anthropic API key configured for project (blog config or settings)")
}

// extractJSON pulls the first JSON object out of an LLM response that may be
// wrapped in markdown fences.
func extractJSON(raw string) string {
	s := strings.TrimSpace(raw)
	if strings.HasPrefix(s, "```") {
		if idx := strings.Index(s, "\n"); idx >= 0 {
			s = s[idx+1:]
		}
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	}
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start >= 0 && end > start {
		return s[start : end+1]
	}
	return s
}
