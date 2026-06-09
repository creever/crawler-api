package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/hibiken/asynq"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.uber.org/zap"

	"github.com/creever/crawler-api/models"
)

// HandleBlogGenerate processes a blog:generate task by running the full
// 4-step pipeline: Gemini SERP → KW clustering → strategy → writing.
func (p *Processor) HandleBlogGenerate(ctx context.Context, t *asynq.Task) error {
	var payload BlogGeneratePayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		return fmt.Errorf("%w: unmarshal blog:generate payload: %v", asynq.SkipRetry, err)
	}

	p.logger.Info("processing blog:generate task",
		zap.String("blog_post_id", payload.BlogPostID),
		zap.String("seed_keyword", payload.SeedKeyword),
	)

	postOID, err := bson.ObjectIDFromHex(payload.BlogPostID)
	if err != nil {
		return fmt.Errorf("%w: invalid blog_post_id: %v", asynq.SkipRetry, err)
	}
	configOID, err := bson.ObjectIDFromHex(payload.BlogConfigID)
	if err != nil {
		return fmt.Errorf("%w: invalid blog_config_id: %v", asynq.SkipRetry, err)
	}

	p.setBlogPostStatus(ctx, postOID, models.BlogPostStatusGenerating, "")

	// Load blog config to get API keys and publish settings.
	var cfg models.BlogConfig
	if err = p.db.Collection("blog_configs").FindOne(ctx, bson.M{"_id": configOID}).Decode(&cfg); err != nil {
		p.setBlogPostStatus(ctx, postOID, models.BlogPostStatusFailed, "blog config not found: "+err.Error())
		return fmt.Errorf("%w: load blog config: %v", asynq.SkipRetry, err)
	}

	// ── Step 1: Gemini Search Grounding (SERP) ──────────────────────────────
	p.logger.Info("blog:generate step 1 — Gemini SERP", zap.String("keyword", payload.SeedKeyword))
	serpSummary, err := p.fetchGeminiSERP(ctx, cfg.GeminiAPIKey, payload.SeedKeyword, payload.Language)
	if err != nil {
		p.logger.Warn("blog:generate SERP failed, continuing with empty context", zap.Error(err))
		serpSummary = fmt.Sprintf("Topic: %s. Language: %s.", payload.SeedKeyword, payload.Language)
	}

	// Build an AI caller for steps 2-4 based on the configured writing provider.
	callAI := p.aiCaller(cfg)
	p.logger.Info("blog:generate writing provider",
		zap.String("provider", string(cfg.WritingProvider)),
	)

	// ── Step 2: Keyword Clustering ───────────────────────────────────────────
	p.logger.Info("blog:generate step 2 — keyword clustering")
	keywords, err := p.clusterKeywords(ctx, callAI, payload.SeedKeyword, payload.Language, serpSummary)
	if err != nil {
		p.logger.Warn("blog:generate keyword clustering failed, using fallback", zap.Error(err))
		keywords = []models.BlogKeyword{
			{Keyword: payload.SeedKeyword, Volume: "2K/mo", Difficulty: "Medium", Intent: "Informational"},
		}
	}

	// ── Step 3: Content Strategy ─────────────────────────────────────────────
	p.logger.Info("blog:generate step 3 — content strategy")
	strategy, err := p.buildStrategy(ctx, callAI, payload.SeedKeyword, payload.Language, serpSummary, keywords)
	if err != nil {
		p.setBlogPostStatus(ctx, postOID, models.BlogPostStatusFailed, "strategy step failed: "+err.Error())
		return fmt.Errorf("blog:generate strategy: %w", err)
	}

	// ── Step 4: Write Blog Post ───────────────────────────────────────────────
	p.logger.Info("blog:generate step 4 — writing")
	content, err := p.writeBlogPost(ctx, callAI, payload, strategy, keywords, serpSummary)
	if err != nil {
		p.setBlogPostStatus(ctx, postOID, models.BlogPostStatusFailed, "writing step failed: "+err.Error())
		return fmt.Errorf("blog:generate writing: %w", err)
	}

	// ── Persist result ───────────────────────────────────────────────────────
	now := time.Now().UTC()
	update := bson.M{
		"status":           models.BlogPostStatusDone,
		"serp_summary":     serpSummary,
		"keywords":         keywords,
		"strategy":         strategy,
		"content":          content,
		"title":            strategy.SuggestedTitle,
		"meta_description": strategy.MetaDescription,
		"generated_at":     now,
		"updated_at":       now,
	}
	if _, err = p.db.Collection("blog_posts").UpdateOne(ctx, bson.M{"_id": postOID}, bson.M{"$set": update}); err != nil {
		return fmt.Errorf("blog:generate persist: %w", err)
	}

	// ── Publish to external CMS (draft) ─────────────────────────────────────
	if cfg.PublishEndpoint != "" {
		publishErr := p.publishDraft(ctx, cfg, strategy.SuggestedTitle, content, strategy.MetaDescription)
		now2 := time.Now().UTC()
		if publishErr != nil {
			p.logger.Warn("blog:generate publish failed", zap.Error(publishErr))
			_, _ = p.db.Collection("blog_posts").UpdateOne(ctx, bson.M{"_id": postOID}, bson.M{
				"$set": bson.M{"publish_error": publishErr.Error(), "updated_at": now2},
			})
		} else {
			_, _ = p.db.Collection("blog_posts").UpdateOne(ctx, bson.M{"_id": postOID}, bson.M{
				"$set": bson.M{
					"status":       models.BlogPostStatusPublished,
					"published_at": now2,
					"updated_at":   now2,
				},
			})
		}
	}

	// ── Update last_generated_at on config ───────────────────────────────────
	_, _ = p.db.Collection("blog_configs").UpdateOne(ctx, bson.M{"_id": configOID}, bson.M{
		"$set": bson.M{"last_generated_at": now, "updated_at": now},
	})

	p.logger.Info("blog:generate task completed",
		zap.String("blog_post_id", payload.BlogPostID),
		zap.String("title", strategy.SuggestedTitle),
	)
	return nil
}

// ---------------------------------------------------------------------------
// Step 1 — Gemini Search Grounding
// ---------------------------------------------------------------------------

type geminiRequest struct {
	Contents []geminiContent `json:"contents"`
	Tools    []geminiTool    `json:"tools"`
}

type geminiContent struct {
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text string `json:"text"`
}

type geminiTool struct {
	GoogleSearch map[string]interface{} `json:"google_search"`
}

type geminiResponse struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
		GroundingMetadata *struct {
			GroundingChunks []struct {
				Web *struct {
					URI   string `json:"uri"`
					Title string `json:"title"`
				} `json:"web"`
			} `json:"groundingChunks"`
		} `json:"groundingMetadata"`
	} `json:"candidates"`
}

// fetchGeminiSERP calls the Gemini 2.0 Flash API with Search Grounding and
// returns a text summary of the SERP findings for the given keyword.
func (p *Processor) fetchGeminiSERP(ctx context.Context, apiKey, keyword, language string) (string, error) {
	if apiKey == "" {
		return "", fmt.Errorf("gemini API key not configured")
	}

	prompt := fmt.Sprintf(`Search the web for: "%s"

I need you to find and report:
1. What are the top People Also Ask (PAA) questions for this keyword?
2. What related searches appear for this topic?
3. What types of content are currently ranking (guides, lists, product pages, etc)?
4. What subtopics and angles are covered by top results?
5. Any notable content gaps you can identify?

Language/market context: %s. Please search and give me a detailed SERP analysis.`, keyword, language)

	reqBody := geminiRequest{
		Contents: []geminiContent{{Parts: []geminiPart{{Text: prompt}}}},
		Tools:    []geminiTool{{GoogleSearch: map[string]interface{}{}}},
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal gemini request: %w", err)
	}

	endpoint := fmt.Sprintf(
		"https://generativelanguage.googleapis.com/v1beta/models/gemini-2.0-flash:generateContent?key=%s",
		apiKey,
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build gemini request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("gemini request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("gemini returned HTTP %s: %s", resp.Status, string(b))
	}

	var gemResp geminiResponse
	if err = json.NewDecoder(resp.Body).Decode(&gemResp); err != nil {
		return "", fmt.Errorf("decode gemini response: %w", err)
	}

	if len(gemResp.Candidates) == 0 {
		return "", fmt.Errorf("gemini returned no candidates")
	}

	var parts []string
	for _, part := range gemResp.Candidates[0].Content.Parts {
		if part.Text != "" {
			parts = append(parts, part.Text)
		}
	}
	return strings.Join(parts, "\n"), nil
}

// ---------------------------------------------------------------------------
// AI caller dispatcher
// ---------------------------------------------------------------------------

// aiCallFunc is a provider-agnostic function for a single LLM call.
type aiCallFunc func(ctx context.Context, system, user string, maxTokens int) (string, error)

// aiCaller returns a dispatch function that routes to Claude or Gemini
// depending on BlogConfig.WritingProvider.
func (p *Processor) aiCaller(cfg models.BlogConfig) aiCallFunc {
	if cfg.WritingProvider == models.BlogAIProviderGemini {
		return func(ctx context.Context, system, user string, maxTokens int) (string, error) {
			return p.callGeminiTextAPI(ctx, cfg.GeminiAPIKey, system, user)
		}
	}
	return func(ctx context.Context, system, user string, maxTokens int) (string, error) {
		return p.callClaudeAPI(ctx, cfg.AnthropicAPIKey, system, user, maxTokens)
	}
}

// ---------------------------------------------------------------------------
// Step 2 — Keyword Clustering
// ---------------------------------------------------------------------------

func (p *Processor) clusterKeywords(ctx context.Context, call aiCallFunc, keyword, language, serpSummary string) ([]models.BlogKeyword, error) {
	system := `You are an expert SEO strategist. Based on the seed keyword and SERP research data, return ONLY a valid JSON array (no markdown, no backticks) of 8-10 keyword opportunities.

Each object must have exactly:
- keyword (string, in the same language as the seed keyword)
- volume (string estimate: "200/mo", "1.2K/mo", "5K/mo" etc.)
- difficulty ("Low", "Medium", or "High")
- intent (exactly one of: "Informational", "Commercial", "Transactional", "Navigational")

Prioritize keywords that naturally emerge from the SERP data. Mix intents. Return only a valid JSON array, nothing else.`

	userMsg := fmt.Sprintf("Seed keyword: %q\nLanguage: %s\n\nSERP research data:\n%s",
		keyword, language, truncate(serpSummary, 2000))

	raw, err := call(ctx, system, userMsg, 1200)
	if err != nil {
		return nil, err
	}

	cleaned := strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(raw, "```json", ""), "```", ""))
	var keywords []models.BlogKeyword
	if err = json.Unmarshal([]byte(cleaned), &keywords); err != nil {
		return nil, fmt.Errorf("parse keyword JSON: %w (raw: %.200s)", err, cleaned)
	}
	return keywords, nil
}

// ---------------------------------------------------------------------------
// Step 3 — Content Strategy
// ---------------------------------------------------------------------------

func (p *Processor) buildStrategy(ctx context.Context, call aiCallFunc, keyword, language, serpSummary string, keywords []models.BlogKeyword) (models.BlogStrategy, error) {
	system := `You are an SEO content strategist. Using SERP data and keyword clusters, return ONLY valid JSON (no markdown) with this exact structure:
{
  "primary_intent": "string (1 sentence)",
  "target_audience": "string (1 sentence)",
  "content_gaps": ["gap1", "gap2", "gap3"],
  "paa_questions": ["PAA question 1", "PAA question 2", "PAA question 3", "PAA question 4"],
  "recommended_headings": ["H2 heading 1", "H2 heading 2", "H2 heading 3", "H2 heading 4", "H2 heading 5"],
  "suggested_title": "string (SEO-optimized, compelling)",
  "meta_description": "string (max 155 chars)",
  "geo_opportunity": "string (1-2 sentences: is there an AI citation opportunity based on the SERP data? why or why not)"
}

Base paa_questions on real questions from the SERP data. Return only valid JSON.`

	kwJSON, _ := json.Marshal(keywords)
	userMsg := fmt.Sprintf("Keyword: %q\nLanguage: %s\n\nSERP research:\n%s\n\nKeyword clusters:\n%s",
		keyword, language, truncate(serpSummary, 2000), string(kwJSON))

	raw, err := call(ctx, system, userMsg, 1200)
	if err != nil {
		return models.BlogStrategy{}, err
	}

	cleaned := strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(raw, "```json", ""), "```", ""))
	var strategy models.BlogStrategy
	if err = json.Unmarshal([]byte(cleaned), &strategy); err != nil {
		return models.BlogStrategy{}, fmt.Errorf("parse strategy JSON: %w (raw: %.200s)", err, cleaned)
	}
	return strategy, nil
}

// ---------------------------------------------------------------------------
// Step 4 — Write Blog Post
// ---------------------------------------------------------------------------

func (p *Processor) writeBlogPost(ctx context.Context, call aiCallFunc, payload BlogGeneratePayload, strategy models.BlogStrategy, keywords []models.BlogKeyword, serpSummary string) (string, error) {
	system := `You are a senior SEO content writer. Write engaging, well-structured blog posts that rank well and provide genuine value to readers. Use proper heading hierarchy, natural keyword integration, and concise paragraphs. Never use placeholder text or filler content.`

	var kwList []string
	for _, k := range keywords {
		if len(kwList) >= 5 {
			break
		}
		kwList = append(kwList, k.Keyword)
	}

	headings := ""
	for i, h := range strategy.RecommendedHeadings {
		headings += fmt.Sprintf("%d. %s\n", i+1, h)
	}
	paaList := strings.Join(strategy.PAAQuestions, "\n")

	userMsg := fmt.Sprintf(`Write a %d-word, %s blog post about %q in %s.

Title: %s
Meta description: %s
Target audience: %s

Use these H2 headings (in order):
%s
Answer these PAA questions naturally within the content:
%s

Integrate these keywords naturally throughout:
%s

SERP context to ground the content:
%s

Write the complete blog post now, starting directly with the title. No preamble, no meta-commentary. Use short paragraphs, clear subheadings, and a helpful conversational style.`,
		payload.WordCount, strings.ToLower(payload.Tone), payload.SeedKeyword, payload.Language,
		strategy.SuggestedTitle, strategy.MetaDescription, strategy.TargetAudience,
		headings, paaList, strings.Join(kwList, ", "),
		truncate(serpSummary, 1500),
	)

	return call(ctx, system, userMsg, 4096)
}

// ---------------------------------------------------------------------------
// Claude API helper
// ---------------------------------------------------------------------------

type claudeRequest struct {
	Model     string           `json:"model"`
	MaxTokens int              `json:"max_tokens"`
	System    string           `json:"system"`
	Messages  []claudeMessage  `json:"messages"`
}

type claudeMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type claudeResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// callClaudeAPI sends a single-turn request to the Anthropic Messages API.
func (p *Processor) callClaudeAPI(ctx context.Context, apiKey, systemPrompt, userMessage string, maxTokens int) (string, error) {
	if apiKey == "" {
		return "", fmt.Errorf("anthropic API key not configured")
	}

	reqBody := claudeRequest{
		Model:     "claude-sonnet-4-6",
		MaxTokens: maxTokens,
		System:    systemPrompt,
		Messages:  []claudeMessage{{Role: "user", Content: userMessage}},
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal claude request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build claude request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("claude request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	var claudeResp claudeResponse
	if err = json.NewDecoder(resp.Body).Decode(&claudeResp); err != nil {
		return "", fmt.Errorf("decode claude response: %w", err)
	}
	if claudeResp.Error != nil {
		return "", fmt.Errorf("claude API error: %s", claudeResp.Error.Message)
	}

	var texts []string
	for _, block := range claudeResp.Content {
		if block.Type == "text" && block.Text != "" {
			texts = append(texts, block.Text)
		}
	}
	if len(texts) == 0 {
		return "", fmt.Errorf("claude returned no text content")
	}
	return strings.Join(texts, "\n"), nil
}

// ---------------------------------------------------------------------------
// Gemini text API helper (plain generation, no search grounding)
// ---------------------------------------------------------------------------

type geminiTextRequest struct {
	SystemInstruction *geminiContent `json:"system_instruction,omitempty"`
	Contents          []geminiContent `json:"contents"`
}

// callGeminiTextAPI calls Gemini 2.0 Flash for plain text generation (steps 2-4).
// It does NOT attach the Google Search tool — that is only used in fetchGeminiSERP.
func (p *Processor) callGeminiTextAPI(ctx context.Context, apiKey, systemPrompt, userMessage string) (string, error) {
	if apiKey == "" {
		return "", fmt.Errorf("gemini API key not configured")
	}

	reqBody := geminiTextRequest{
		Contents: []geminiContent{{Parts: []geminiPart{{Text: userMessage}}}},
	}
	if systemPrompt != "" {
		reqBody.SystemInstruction = &geminiContent{Parts: []geminiPart{{Text: systemPrompt}}}
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal gemini text request: %w", err)
	}

	endpoint := fmt.Sprintf(
		"https://generativelanguage.googleapis.com/v1beta/models/gemini-2.0-flash:generateContent?key=%s",
		apiKey,
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build gemini text request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("gemini text request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("gemini text returned HTTP %s: %s", resp.Status, string(b))
	}

	var gemResp geminiResponse
	if err = json.NewDecoder(resp.Body).Decode(&gemResp); err != nil {
		return "", fmt.Errorf("decode gemini text response: %w", err)
	}
	if len(gemResp.Candidates) == 0 {
		return "", fmt.Errorf("gemini text returned no candidates")
	}

	var parts []string
	for _, part := range gemResp.Candidates[0].Content.Parts {
		if part.Text != "" {
			parts = append(parts, part.Text)
		}
	}
	if len(parts) == 0 {
		return "", fmt.Errorf("gemini text returned no text content")
	}
	return strings.Join(parts, "\n"), nil
}

// ---------------------------------------------------------------------------
// Publish draft to external CMS
// ---------------------------------------------------------------------------

func (p *Processor) publishDraft(ctx context.Context, cfg models.BlogConfig, title, content, metaDescription string) error {
	payload := map[string]interface{}{
		"title":            title,
		"content":          content,
		"meta_description": metaDescription,
		"status":           "draft",
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal publish payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.PublishEndpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build publish request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.PublishHeader != "" && cfg.PublishAPIKey != "" {
		req.Header.Set(cfg.PublishHeader, cfg.PublishAPIKey)
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("publish request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("publish endpoint returned HTTP %s: %s", resp.Status, string(b))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Status helpers
// ---------------------------------------------------------------------------

func (p *Processor) setBlogPostStatus(ctx context.Context, id bson.ObjectID, status models.BlogPostStatus, errMsg string) {
	update := bson.M{"status": status, "updated_at": time.Now().UTC()}
	if errMsg != "" {
		update["error"] = errMsg
	}
	tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := p.db.Collection("blog_posts").UpdateOne(tctx, bson.M{"_id": id}, bson.M{"$set": update}); err != nil {
		p.logger.Error("setBlogPostStatus failed", zap.String("id", id.Hex()), zap.Error(err))
	}
}

// truncate returns at most n runes of s.
func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}
