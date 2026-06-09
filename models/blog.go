package models

import (
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// BlogPostStatus represents the lifecycle state of a generated blog post.
type BlogPostStatus string

const (
	BlogPostStatusPending    BlogPostStatus = "pending"
	BlogPostStatusGenerating BlogPostStatus = "generating"
	BlogPostStatusDone       BlogPostStatus = "done"
	BlogPostStatusFailed     BlogPostStatus = "failed"
	BlogPostStatusPublished  BlogPostStatus = "published"
)

// BlogAIProvider selects the LLM used for writing steps (keyword clustering,
// strategy, and article writing). Step 1 (SERP via Gemini Search Grounding)
// is always Gemini regardless of this setting.
type BlogAIProvider string

const (
	BlogAIProviderClaude BlogAIProvider = "claude" // default
	BlogAIProviderGemini BlogAIProvider = "gemini"
)

// BlogConfig stores per-project settings for the blog generation pipeline.
// MongoDB collection: blog_configs
type BlogConfig struct {
	ID              bson.ObjectID `bson:"_id,omitempty" json:"id,omitempty"`
	ProjectID       bson.ObjectID `bson:"project_id"    json:"project_id"`
	Active          bool          `bson:"active"        json:"active"`
	GeminiAPIKey    string        `bson:"gemini_api_key"    json:"gemini_api_key"`
	AnthropicAPIKey string        `bson:"anthropic_api_key" json:"anthropic_api_key"`
	// WritingProvider controls which LLM is used for steps 2-4.
	// "claude" (default) uses the Anthropic API; "gemini" uses the Gemini text API.
	WritingProvider BlogAIProvider `bson:"writing_provider" json:"writing_provider"`
	// PublishEndpoint is the custom CMS API URL that receives the draft post.
	PublishEndpoint  string   `bson:"publish_endpoint"   json:"publish_endpoint"`
	PublishAPIKey    string   `bson:"publish_api_key"    json:"publish_api_key"`
	PublishHeader    string   `bson:"publish_header"     json:"publish_header"` // e.g. "Authorization"
	DefaultLanguage  string   `bson:"default_language"   json:"default_language"`
	DefaultTone      string   `bson:"default_tone"       json:"default_tone"`
	DefaultWordCount int      `bson:"default_word_count" json:"default_word_count"`
	SeedKeywords     []string `bson:"seed_keywords"      json:"seed_keywords"`
	// ScheduleHours: 0 = manual only, >0 = auto-generate every N hours.
	ScheduleHours   int        `bson:"schedule_hours"              json:"schedule_hours"`
	LastGeneratedAt *time.Time `bson:"last_generated_at,omitempty" json:"last_generated_at,omitempty"`
	CreatedAt       time.Time  `bson:"created_at" json:"created_at"`
	UpdatedAt       time.Time  `bson:"updated_at" json:"updated_at"`
}

// BlogKeyword is one keyword opportunity produced by the clustering step.
type BlogKeyword struct {
	Keyword    string `bson:"keyword"    json:"keyword"`
	Volume     string `bson:"volume"     json:"volume"`     // e.g. "1.2K/mo"
	Difficulty string `bson:"difficulty" json:"difficulty"` // "Low" | "Medium" | "High"
	Intent     string `bson:"intent"     json:"intent"`     // "Informational" | "Commercial" | ...
}

// BlogStrategy holds the content plan produced by the strategy step.
type BlogStrategy struct {
	PrimaryIntent       string   `bson:"primary_intent"       json:"primary_intent"`
	TargetAudience      string   `bson:"target_audience"      json:"target_audience"`
	ContentGaps         []string `bson:"content_gaps"         json:"content_gaps"`
	PAAQuestions        []string `bson:"paa_questions"        json:"paa_questions"`
	RecommendedHeadings []string `bson:"recommended_headings" json:"recommended_headings"`
	SuggestedTitle      string   `bson:"suggested_title"      json:"suggested_title"`
	MetaDescription     string   `bson:"meta_description"     json:"meta_description"`
	GEOOpportunity      string   `bson:"geo_opportunity"      json:"geo_opportunity"`
}

// BlogPost is a generated blog post with full pipeline artefacts.
// MongoDB collection: blog_posts
type BlogPost struct {
	ID           bson.ObjectID `bson:"_id,omitempty"  json:"id,omitempty"`
	ProjectID    bson.ObjectID `bson:"project_id"     json:"project_id"`
	BlogConfigID bson.ObjectID `bson:"blog_config_id" json:"blog_config_id"`
	SeedKeyword  string        `bson:"seed_keyword"   json:"seed_keyword"`
	Language     string        `bson:"language"       json:"language"`
	Tone         string        `bson:"tone"           json:"tone"`
	WordCount    int           `bson:"word_count"     json:"word_count"`
	// Pipeline artefacts
	SerpSummary     string        `bson:"serp_summary"     json:"serp_summary"`
	Keywords        []BlogKeyword `bson:"keywords"         json:"keywords"`
	Strategy        BlogStrategy  `bson:"strategy"         json:"strategy"`
	Content         string        `bson:"content"          json:"content"`
	Title           string        `bson:"title"            json:"title"`
	MetaDescription string        `bson:"meta_description" json:"meta_description"`
	// Status tracking
	Status       BlogPostStatus `bson:"status"                  json:"status"`
	Error        string         `bson:"error,omitempty"         json:"error,omitempty"`
	PublishedAt  *time.Time     `bson:"published_at,omitempty"  json:"published_at,omitempty"`
	PublishError string         `bson:"publish_error,omitempty" json:"publish_error,omitempty"`
	GeneratedAt  *time.Time     `bson:"generated_at,omitempty"  json:"generated_at,omitempty"`
	CreatedAt    time.Time      `bson:"created_at"              json:"created_at"`
	UpdatedAt    time.Time      `bson:"updated_at"              json:"updated_at"`
}
