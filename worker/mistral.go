package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/creever/crawler-api/models"
)

const mistralModel = "mistral-large-latest"

type mistralRequest struct {
	Model       string       `json:"model"`
	MaxTokens   int          `json:"max_tokens"`
	Messages    []mistralMsg `json:"messages"`
	Temperature float64      `json:"temperature"`
}

type mistralMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type mistralResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// callMistralAPI calls the Mistral chat completions API.
func (p *Processor) callMistralAPI(ctx context.Context, apiKey, systemPrompt, userMessage string, maxTokens int) (string, error) {
	if apiKey == "" {
		return "", fmt.Errorf("mistral API key not configured")
	}
	if maxTokens <= 0 {
		maxTokens = 1024
	}

	reqBody := mistralRequest{
		Model:     mistralModel,
		MaxTokens: maxTokens,
		Messages: []mistralMsg{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userMessage},
		},
		Temperature: 0.4,
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal mistral request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.mistral.ai/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build mistral request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("mistral request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	var mr mistralResponse
	if err = json.NewDecoder(resp.Body).Decode(&mr); err != nil {
		return "", fmt.Errorf("decode mistral response: %w", err)
	}
	if mr.Error != nil {
		return "", fmt.Errorf("mistral API error: %s", mr.Error.Message)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("mistral API HTTP %s", resp.Status)
	}
	if len(mr.Choices) == 0 || strings.TrimSpace(mr.Choices[0].Message.Content) == "" {
		return "", fmt.Errorf("mistral returned no content")
	}
	return mr.Choices[0].Message.Content, nil
}

// resolveSuggestionAI picks the LLM provider + key for suggestion generation.
// Provider comes from settings "ai_provider" (claude|mistral|gemini), falling
// back to the project's active blog config writing_provider, default claude.
// The key is read from the matching blog-config field or the global
// "<provider>_api_key" setting.
func (p *Processor) resolveSuggestionAI(ctx context.Context, projectOID bson.ObjectID) (models.BlogAIProvider, string, error) {
	provider := models.BlogAIProvider("")

	var s models.Setting
	if err := p.db.Collection("settings").FindOne(ctx, bson.M{"key": "ai_provider"}).Decode(&s); err == nil && s.Value != "" {
		provider = models.BlogAIProvider(s.Value)
	}
	if provider == "" {
		var cfg models.BlogConfig
		if err := p.db.Collection("blog_configs").FindOne(ctx, bson.M{"project_id": projectOID, "active": true}).Decode(&cfg); err == nil && cfg.WritingProvider != "" {
			provider = cfg.WritingProvider
		}
	}
	if provider == "" {
		provider = models.BlogAIProviderClaude
	}

	key := ""
	// Project blog config key first.
	var cfg models.BlogConfig
	if err := p.db.Collection("blog_configs").FindOne(ctx, bson.M{"project_id": projectOID, "active": true}).Decode(&cfg); err == nil {
		switch provider {
		case models.BlogAIProviderMistral:
			key = cfg.MistralAPIKey
		case models.BlogAIProviderGemini:
			key = cfg.GeminiAPIKey
		default:
			key = cfg.AnthropicAPIKey
		}
	}
	// Global settings fallback: "<provider>_api_key".
	if key == "" {
		settingKey := string(provider) + "_api_key"
		var ks models.Setting
		if err := p.db.Collection("settings").FindOne(ctx, bson.M{"key": settingKey}).Decode(&ks); err == nil {
			key = ks.Value
		}
	}
	if key == "" {
		return "", "", fmt.Errorf("no %s API key configured for project (blog config %s_api_key or settings %s)", provider, provider, provider+"_api_key")
	}
	return provider, key, nil
}
