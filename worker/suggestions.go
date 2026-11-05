package worker

import (
	"context"
	"encoding/json"
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
	// suggestDefaultLimit caps how many opportunities get suggestions per run.
	suggestDefaultLimit = 10
	// suggestLanguage is the language AI suggestions are generated in.
	suggestLanguage = "hu"
)

// HandleSuggestionsGenerate processes a suggestions:generate task: it takes
// the project's top opportunities, asks Claude for improved title / meta
// description (and category text when relevant) in Hungarian, and stores the
// proposals as pending suggestions for human review.
func (p *Processor) HandleSuggestionsGenerate(ctx context.Context, t *asynq.Task) error {
	var payload GenSuggestPayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		return fmt.Errorf("%w: unmarshal suggestions:generate payload: %v", asynq.SkipRetry, err)
	}
	projectOID, err := bson.ObjectIDFromHex(payload.ProjectID)
	if err != nil {
		return fmt.Errorf("%w: invalid project_id: %v", asynq.SkipRetry, err)
	}

	limit := payload.Limit
	if limit <= 0 {
		limit = suggestDefaultLimit
	}

	apiKey, err := p.resolveAnthropicKey(ctx, projectOID)
	if err != nil {
		return fmt.Errorf("%w: %v", asynq.SkipRetry, err)
	}

	// Targets: either the single requested opportunity, or the top actionable
	// opportunities deduplicated by page.
	var targets []models.Opportunity
	if payload.OpportunityID != "" {
		oppOID, oidErr := bson.ObjectIDFromHex(payload.OpportunityID)
		if oidErr != nil {
			return fmt.Errorf("%w: invalid opportunity_id: %v", asynq.SkipRetry, oidErr)
		}
		var opp models.Opportunity
		if err = p.db.Collection("opportunities").FindOne(ctx, bson.M{
			"_id": oppOID, "project_id": projectOID,
		}).Decode(&opp); err != nil {
			return fmt.Errorf("%w: opportunity not found: %v", asynq.SkipRetry, err)
		}
		targets = append(targets, opp)
	} else {
		cursor, err := p.db.Collection("opportunities").Find(ctx, bson.M{
			"project_id": projectOID,
			"type": bson.M{"$in": []models.OpportunityType{
				models.OppStrikingDistance, models.OppLowCTR, models.OppCannibalization,
			}},
		}, options.Find().SetSort(bson.D{{Key: "rank", Value: 1}}).SetLimit(int64(limit*3)))
		if err != nil {
			return fmt.Errorf("query opportunities: %w", err)
		}
		var opps []models.Opportunity
		if err = cursor.All(ctx, &opps); err != nil {
			cursor.Close(ctx) //nolint:errcheck
			return fmt.Errorf("decode opportunities: %w", err)
		}
		cursor.Close(ctx) //nolint:errcheck

		seenPages := make(map[string]bool)
		for _, o := range opps {
			if o.Page == "" || seenPages[o.Page] {
				continue
			}
			seenPages[o.Page] = true
			targets = append(targets, o)
			if len(targets) >= limit {
				break
			}
		}
	}
	if len(targets) == 0 {
		p.logger.Info("suggestions:generate nothing to do", zap.String("project_id", payload.ProjectID))
		return nil
	}

	now := time.Now().UTC()
	col := p.db.Collection("suggestions")
	generated := 0

	for _, opp := range targets {
		var seo models.SEOData
		_ = p.db.Collection("seo_data").FindOne(ctx, bson.M{
			"project_id": projectOID, "url": opp.Page,
		}).Decode(&seo)

		system, user := buildSuggestionPrompt(opp, seo)
		raw, callErr := p.callClaudeAPI(ctx, apiKey, system, user, 1024)
		if callErr != nil {
			p.logger.Warn("suggestions:generate claude call failed",
				zap.String("page", opp.Page), zap.Error(callErr))
			continue
		}

		var parsed struct {
			Title           string `json:"title"`
			MetaDescription string `json:"meta_description"`
			CategoryText    string `json:"category_text"`
			Reason          string `json:"reason"`
		}
		if jsonErr := json.Unmarshal([]byte(extractJSON(raw)), &parsed); jsonErr != nil {
			p.logger.Warn("suggestions:generate unparseable response",
				zap.String("page", opp.Page), zap.Error(jsonErr))
			continue
		}

		reason := parsed.Reason
		if reason == "" {
			reason = fmt.Sprintf("%s (rank #%d, score %.0f): %s", opp.Type, opp.Rank, opp.Score, opp.Details)
		}

		docs := []models.Suggestion{}
		if strings.TrimSpace(parsed.Title) != "" {
			docs = append(docs, models.Suggestion{
				ID: bson.NewObjectID(), ProjectID: projectOID, OpportunityID: &opp.ID,
				Kind: models.SuggestionKindTitle, Page: opp.Page, Query: opp.Query,
				Language: suggestLanguage, CurrentValue: seo.Title, ProposedValue: parsed.Title,
				Status: models.SuggestionStatusPending, Reason: reason,
				Prompt: user, RawResponse: raw, CreatedAt: now, UpdatedAt: now,
			})
		}
		if strings.TrimSpace(parsed.MetaDescription) != "" {
			docs = append(docs, models.Suggestion{
				ID: bson.NewObjectID(), ProjectID: projectOID, OpportunityID: &opp.ID,
				Kind: models.SuggestionKindMeta, Page: opp.Page, Query: opp.Query,
				Language: suggestLanguage, CurrentValue: seo.MetaDescription, ProposedValue: parsed.MetaDescription,
				Status: models.SuggestionStatusPending, Reason: reason,
				Prompt: user, RawResponse: raw, CreatedAt: now, UpdatedAt: now,
			})
		}
		if strings.TrimSpace(parsed.CategoryText) != "" {
			docs = append(docs, models.Suggestion{
				ID: bson.NewObjectID(), ProjectID: projectOID, OpportunityID: &opp.ID,
				Kind: models.SuggestionKindCategoryText, Page: opp.Page, Query: opp.Query,
				Language: suggestLanguage, ProposedValue: parsed.CategoryText,
				Status: models.SuggestionStatusPending, Reason: reason,
				Prompt: user, RawResponse: raw, CreatedAt: now, UpdatedAt: now,
			})
		}

		if len(docs) > 0 {
			toInsert := make([]interface{}, 0, len(docs))
			for _, d := range docs {
				toInsert = append(toInsert, d)
			}
			if _, insErr := col.InsertMany(ctx, toInsert); insErr != nil {
				p.logger.Warn("suggestions:generate insert failed",
					zap.String("page", opp.Page), zap.Error(insErr))
				continue
			}
			generated += len(docs)
		}
	}

	p.logger.Info("suggestions:generate completed",
		zap.String("project_id", payload.ProjectID),
		zap.Int("pages", len(targets)),
		zap.Int("suggestions", generated),
	)
	return nil
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

// buildSuggestionPrompt builds the system + user prompts asking Claude for
// improved SEO texts in Hungarian, returned as strict JSON.
func buildSuggestionPrompt(opp models.Opportunity, seo models.SEOData) (string, string) {
	system := `You are an SEO copywriter for a Hungarian e-commerce website (webshop).
You write concise, click-worthy Hungarian texts.
Rules:
- title: max 60 characters, include the main keyword, no clickbait.
- meta_description: max 160 characters, include a call to action.
- category_text: only if the page is a category/listing page; 2-4 sentences of Hungarian descriptive text. Otherwise return an empty string.
Respond with a single JSON object and nothing else:
{"title": "...", "meta_description": "...", "category_text": "...", "reason": "one sentence why this helps"}`

	metrics := ""
	if opp.Metrics != nil {
		metrics = fmt.Sprintf("impressions=%.0f, clicks=%.0f, ctr=%.2f%%, position=%.1f",
			opp.Metrics["impressions"], opp.Metrics["clicks"], opp.Metrics["ctr"]*100, opp.Metrics["position"])
	}
	user := fmt.Sprintf(`Page: %s
Target query: %s
Opportunity type: %s (%s)
GSC metrics (last 28 days): %s
Current title: %q
Current meta description: %q
Current H1: %q

Generate improved Hungarian title, meta description and (if applicable) category text for this page.`,
		opp.Page, opp.Query, opp.Type, opp.Details, metrics, seo.Title, seo.MetaDescription, firstOrEmpty(seo.H1Tags))
	return system, user
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

func firstOrEmpty(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}
