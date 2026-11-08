package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/hibiken/asynq"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/creever/crawler-api/models"
	"github.com/creever/crawler-api/worker"
)

const (
	// prerenderTimeout caps how long a bot waits for a page that is not cached.
	prerenderTimeout = 60 * time.Second
)

// ServeHandler answers /prerender: the rendered HTML of a page for a bot.
type ServeHandler struct {
	db          *mongo.Database
	asynqClient *asynq.Client
	renderer    *worker.Processor
}

// NewServeHandler creates a ServeHandler.
func NewServeHandler(db *mongo.Database, asynqClient *asynq.Client, renderer *worker.Processor) *ServeHandler {
	return &ServeHandler{db: db, asynqClient: asynqClient, renderer: renderer}
}

// Serve godoc
// @Summary      Prerendered HTML of a page, for bots
// @Description  A fresh render comes from the cache. An old one is served at once and refreshed in the background. A page not in the cache is rendered while the caller waits. X-Prerender-Status carries the page's own HTTP status; X-Prerender-Cache says hit, stale or miss.
// @Tags         prerender
// @Produce      html
// @Param        url  query  string  true  "Fully-qualified page URL"
// @Router       /prerender [get]
func (h *ServeHandler) Serve(c *gin.Context) {
	start := time.Now()
	userAgent := c.GetHeader("User-Agent")
	rawURL := c.Query("url")
	parsedURL, err := url.Parse(rawURL)
	if rawURL == "" || err != nil || parsedURL.Host == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "url must be a fully-qualified URL"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), prerenderTimeout)
	defer cancel()

	project, err := h.findProjectByHost(ctx, parsedURL.Host)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "no active project found matching host: " + parsedURL.Host})
		return
	}

	state := models.CacheStateHit
	entry, err := worker.LookupCache(ctx, h.db, project.ID, rawURL)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "cache lookup failed"})
		return
	}
	switch {
	case entry != nil && time.Now().After(entry.ExpiresAt):
		state = models.CacheStateStale
		go h.refresh(project.ID, rawURL)
	case entry == nil:
		state = models.CacheStateMiss
		entry, err = h.renderer.RenderToCache(ctx, project, rawURL, models.CacheSourceBot)
		if err != nil {
			status := http.StatusBadGateway
			if errors.Is(err, context.DeadlineExceeded) {
				status = http.StatusGatewayTimeout
			}
			c.JSON(status, gin.H{"error": "render failed: " + err.Error()})
			go h.logRequest(project.ID, rawURL, userAgent, models.CacheStateError, status, time.Since(start))
			return
		}
	}
	if state != models.CacheStateMiss {
		go worker.MarkServed(context.Background(), h.db, entry.ID, time.Now().UTC())
	}
	pageStatus := entry.StatusCode
	if pageStatus == 0 {
		pageStatus = http.StatusOK
	}
	c.Header("X-Prerender-Status", strconv.Itoa(pageStatus))
	c.Header("X-Prerender-Cache", state)
	c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(entry.FullHTML))
	go h.logRequest(project.ID, rawURL, userAgent, state, pageStatus, time.Since(start))
}

// refresh queues a new render of a stale page.
func (h *ServeHandler) refresh(projectID bson.ObjectID, rawURL string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = worker.EnqueueRender(ctx, h.db, h.asynqClient, projectID, rawURL, models.CacheSourceStale)
}

// findProjectByHost returns the active project of a host; www. is ignored.
func (h *ServeHandler) findProjectByHost(ctx context.Context, host string) (*models.Project, error) {
	cursor, err := h.db.Collection("projects").Find(ctx, bson.M{"active": true})
	if err != nil {
		return nil, err
	}
	var projects []models.Project
	if err = cursor.All(ctx, &projects); err != nil {
		return nil, err
	}
	want := strings.TrimPrefix(strings.ToLower(host), "www.")
	for i := range projects {
		if pURL, parseErr := url.Parse(projects[i].URL); parseErr == nil &&
			strings.TrimPrefix(strings.ToLower(pURL.Host), "www.") == want {
			return &projects[i], nil
		}
	}
	return nil, errors.New("no matching project")
}

// classifyRequester inspects the User-Agent header and returns the appropriate
// RequesterType: AI crawlers are detected first, then generic bots, with
// everything else treated as a human visitor.
func classifyRequester(ua string) models.RequesterType {
	lower := strings.ToLower(ua)

	// Well-known AI crawlers / LLM-related user agents.
	aiAgents := []string{
		"gptbot", "chatgpt-user", "oai-searchbot",
		"claudebot", "claude-web", "anthropic", "claude-user", "claude-searchbot", "perplexity-user", "google-extended", "bytespider", "amazonbot",
		"ccbot",
		"perplexitybot",
		"googleother",
		"bard",
		"cohere",
		"meta-externalagent",
	}
	for _, agent := range aiAgents {
		if strings.Contains(lower, agent) {
			return models.RequesterTypeAI
		}
	}

	// Generic bots / crawlers / spiders.
	botKeywords := []string{
		"bot", "crawler", "spider",
		"slurp",    // Yahoo
		"baidu",    // Baidu
		"yandex",   // Yandex
		"duckduck", // DuckDuckGo
		"sogou",    // Sogou
		"exabot",   // Exabot
		"facebot",
		"ia_archiver", // Wayback Machine
		"scraper",
		"curl", "wget", "python-requests", "go-http-client",
	}
	for _, kw := range botKeywords {
		if strings.Contains(lower, kw) {
			return models.RequesterTypeBot
		}
	}

	return models.RequesterTypeHuman
}

// knownAgents names well-known crawlers by a User-Agent fragment, most
// specific first.
var knownAgents = []struct{ match, name string }{
	{"chatgpt-user", "ChatGPT-User"}, {"oai-searchbot", "OAI-SearchBot"}, {"gptbot", "GPTBot"},
	{"claude-user", "Claude-User"}, {"claude-searchbot", "Claude-SearchBot"}, {"claudebot", "ClaudeBot"},
	{"perplexity-user", "Perplexity-User"}, {"perplexitybot", "PerplexityBot"},
	{"google-extended", "Google-Extended"}, {"googleother", "GoogleOther"}, {"google-inspectiontool", "Google-InspectionTool"},
	{"googlebot", "Googlebot"}, {"adsbot-google", "AdsBot-Google"}, {"bingbot", "Bingbot"},
	{"applebot", "Applebot"}, {"yandex", "YandexBot"}, {"duckduckbot", "DuckDuckBot"}, {"ccbot", "CCBot"},
	{"meta-externalagent", "Meta-ExternalAgent"}, {"facebookexternalhit", "Facebook"}, {"bytespider", "Bytespider"},
	{"amazonbot", "Amazonbot"}, {"ahrefsbot", "AhrefsBot"}, {"semrushbot", "SemrushBot"}, {"twitterbot", "Twitterbot"},
	{"linkedinbot", "LinkedInBot"}, {"slackbot", "Slackbot"},
}

// agentName returns the crawler a User-Agent belongs to, or "".
func agentName(ua string) string {
	lower := strings.ToLower(ua)
	for _, a := range knownAgents {
		if strings.Contains(lower, a.match) {
			return a.name
		}
	}
	return ""
}

// logRequest records a /prerender request. It runs in its own goroutine.
func (h *ServeHandler) logRequest(projectID bson.ObjectID, rawURL, userAgent, cache string, statusCode int, elapsed time.Duration) {
	record := models.PrerenderData{
		ID: bson.NewObjectID(), ProjectID: projectID, URL: rawURL, Key: worker.CacheKey(rawURL), StatusCode: statusCode,
		RenderTimeMs: elapsed.Milliseconds(), FromCache: cache == models.CacheStateHit || cache == models.CacheStateStale,
		Cache: cache, UserAgent: userAgent, RequesterType: classifyRequester(userAgent), Agent: agentName(userAgent),
		CachedAt: time.Now().UTC(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _ = h.db.Collection("prerender_data").InsertOne(ctx, record)
}
