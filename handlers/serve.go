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
	// prerenderTimeout is the maximum time to wait for a render task to complete.
	prerenderTimeout = 120 * time.Second
	// prerenderPollInterval is how often we check the queue entry status.
	prerenderPollInterval = 500 * time.Millisecond
	// prerenderPriority is the default priority assigned to prerender queue entries.
	prerenderPriority = 10
)

// errPrerenderTimeout is returned when the render task does not finish within prerenderTimeout.
var errPrerenderTimeout = errors.New("prerender timed out waiting for render task to complete")

// ServeHandler provides the synchronous prerender endpoint used as an nginx
// proxy backend.  Incoming requests are answered with the fully-rendered HTML
// of the requested URL, either served from the cache or freshly rendered on
// demand via the existing queue + crawler pipeline.
type ServeHandler struct {
	db          *mongo.Database
	asynqClient *asynq.Client
}

// NewServeHandler creates a new ServeHandler.
func NewServeHandler(db *mongo.Database, asynqClient *asynq.Client) *ServeHandler {
	return &ServeHandler{db: db, asynqClient: asynqClient}
}

// Serve godoc
// @Summary      Synchronous prerender endpoint for nginx proxy
// @Description  Returns the fully-rendered HTML for the given URL.
//
//	The response is served from cache when a valid (non-expired) entry
//	exists.  Otherwise a crawl:render task (and a crawl:seo task for
//	analytics) is queued via the standard pipeline and the handler
//	blocks until the render completes or times out (~120 s).
//
// @Tags         prerender
// @Produce      html
// @Param        url  query  string  true  "Fully-qualified URL to prerender"
// @Success      200  {string}  string  "Rendered HTML"
// @Failure      400  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Failure      504  {object}  map[string]string
// @Router       /prerender [get]
func (h *ServeHandler) Serve(c *gin.Context) {
	start := time.Now()
	userAgent := c.GetHeader("User-Agent")

	rawURL := c.Query("url")
	if rawURL == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "url query parameter is required"})
		return
	}

	parsedURL, err := url.Parse(rawURL)
	if err != nil || parsedURL.Host == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid url: must be a fully-qualified URL"})
		return
	}

	// Use a context with a hard deadline that covers the entire render cycle.
	ctx, cancel := context.WithTimeout(c.Request.Context(), prerenderTimeout)
	defer cancel()

	// 1. Find the project that owns this URL.
	project, err := h.findProjectByHost(ctx, parsedURL.Host)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "no active project found matching host: " + parsedURL.Host})
		return
	}

	// 2. Return immediately if a valid cache entry already exists.
	if cached, cacheErr := h.lookupCache(ctx, rawURL); cacheErr == nil {
		pageStatus := cachePageStatus(cached)
		c.Header("X-Prerender-Status", strconv.Itoa(pageStatus))
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(cached.FullHTML))
		go h.logPrerenderRequest(project.ID, rawURL, userAgent, true, pageStatus, time.Since(start))
		return
	}

	// 3. Check whether a render is already in flight for this URL to avoid
	//    queueing duplicate tasks.
	existingID, found := h.findInFlightRender(ctx, project.ID, rawURL)
	if found {
		entry, waitErr := h.waitForRender(ctx, existingID, rawURL)
		if waitErr != nil {
			h.respondRenderError(c, waitErr)
			go h.logPrerenderRequest(project.ID, rawURL, userAgent, false, renderErrorStatus(waitErr), time.Since(start))
			return
		}
		pageStatus := cachePageStatus(entry)
		c.Header("X-Prerender-Status", strconv.Itoa(pageStatus))
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(entry.FullHTML))
		go h.logPrerenderRequest(project.ID, rawURL, userAgent, false, pageStatus, time.Since(start))
		return
	}

	// 4. Enqueue a fresh render task (and a background SEO task for analytics).
	renderEntryID, enqErr := h.enqueueRender(ctx, project, rawURL)
	if enqErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to enqueue render task: " + enqErr.Error()})
		return
	}

	// Fire-and-forget SEO task so requests are tracked in the analytics DB.
	h.enqueueSEO(ctx, project, rawURL) //nolint:errcheck

	// 5. Wait for the render task to finish, then return the HTML.
	entry, waitErr := h.waitForRender(ctx, renderEntryID, rawURL)
	if waitErr != nil {
		h.respondRenderError(c, waitErr)
		go h.logPrerenderRequest(project.ID, rawURL, userAgent, false, renderErrorStatus(waitErr), time.Since(start))
		return
	}
	pageStatus := cachePageStatus(entry)
	c.Header("X-Prerender-Status", strconv.Itoa(pageStatus))
	c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(entry.FullHTML))
	go h.logPrerenderRequest(project.ID, rawURL, userAgent, false, pageStatus, time.Since(start))
}

// ---------------------------------------------------------------------------
// Project lookup
// ---------------------------------------------------------------------------

// findProjectByHost returns the first active project whose base URL hostname
// matches host.
func (h *ServeHandler) findProjectByHost(ctx context.Context, host string) (*models.Project, error) {
	cursor, err := h.db.Collection("projects").Find(ctx, bson.M{"active": true})
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx) //nolint:errcheck

	var projects []models.Project
	if err = cursor.All(ctx, &projects); err != nil {
		return nil, err
	}

	for i := range projects {
		p := &projects[i]
		pURL, parseErr := url.Parse(p.URL)
		if parseErr != nil {
			continue
		}
		if pURL.Host == host {
			return p, nil
		}
	}
	return nil, errors.New("no matching project")
}

// ---------------------------------------------------------------------------
// Cache helpers
// ---------------------------------------------------------------------------

// lookupCache returns a non-expired cache entry for rawURL, or an error if
// none is found.
func (h *ServeHandler) lookupCache(ctx context.Context, rawURL string) (*models.CacheEntry, error) {
	var entry models.CacheEntry
	err := h.db.Collection("cache_entries").FindOne(ctx, bson.M{
		"url":        rawURL,
		"expires_at": bson.M{"$gt": time.Now().UTC()},
	}).Decode(&entry)
	if err != nil {
		return nil, err
	}
	return &entry, nil
}

// ---------------------------------------------------------------------------
// In-flight detection
// ---------------------------------------------------------------------------

// findInFlightRender looks for an existing pending or processing render queue
// entry for the given URL within the project.  If found it returns the entry
// ObjectID and true.
func (h *ServeHandler) findInFlightRender(ctx context.Context, projectID bson.ObjectID, rawURL string) (bson.ObjectID, bool) {
	var entry models.QueueEntry
	err := h.db.Collection("crawl_queue").FindOne(ctx, bson.M{
		"project_id": projectID,
		"url":        rawURL,
		"task_type":  models.QueueTaskTypeRender,
		"status":     bson.M{"$in": []models.QueueStatus{models.QueueStatusPending, models.QueueStatusProcessing}},
	}).Decode(&entry)
	if err != nil {
		return bson.ObjectID{}, false
	}
	return entry.ID, true
}

// ---------------------------------------------------------------------------
// Task enqueueing
// ---------------------------------------------------------------------------

// enqueueRender creates a crawl:render queue entry and dispatches the asynq
// task.  It returns the new entry's ObjectID so the caller can poll its status.
func (h *ServeHandler) enqueueRender(ctx context.Context, project *models.Project, rawURL string) (bson.ObjectID, error) {
	config := models.ProjectConfig{
		ProjectID: project.ID.Hex(),
		SeedURLs:  []string{rawURL},
		UseJS:     project.UseJS,
		MaxPages:  project.MaxPages,
	}

	entry := models.QueueEntry{
		ID:         bson.NewObjectID(),
		ProjectID:  project.ID,
		URL:        rawURL,
		TaskType:   models.QueueTaskTypeRender,
		Priority:   prerenderPriority,
		Status:     models.QueueStatusPending,
		EnqueuedAt: time.Now().UTC(),
	}

	task, err := worker.NewCrawlRenderTask(entry.ID.Hex(), config)
	if err != nil {
		return bson.ObjectID{}, err
	}

	info, err := h.asynqClient.EnqueueContext(ctx, task)
	if err != nil {
		return bson.ObjectID{}, err
	}
	entry.AsynqTaskID = info.ID

	if _, err = h.db.Collection("crawl_queue").InsertOne(ctx, entry); err != nil {
		return bson.ObjectID{}, err
	}
	return entry.ID, nil
}

// enqueueSEO creates a crawl:seo queue entry for analytics tracking.
// Errors are intentionally ignored (best-effort).
func (h *ServeHandler) enqueueSEO(ctx context.Context, project *models.Project, rawURL string) error {
	config := models.ProjectConfig{
		ProjectID: project.ID.Hex(),
		SeedURLs:  []string{rawURL},
		UseJS:     project.UseJS,
		MaxPages:  project.MaxPages,
	}

	entry := models.QueueEntry{
		ID:         bson.NewObjectID(),
		ProjectID:  project.ID,
		URL:        rawURL,
		TaskType:   models.QueueTaskTypeSEO,
		Priority:   prerenderPriority,
		Status:     models.QueueStatusPending,
		EnqueuedAt: time.Now().UTC(),
	}

	task, err := worker.NewCrawlSEOTask(entry.ID.Hex(), config)
	if err != nil {
		return err
	}

	info, err := h.asynqClient.EnqueueContext(ctx, task)
	if err != nil {
		return err
	}
	entry.AsynqTaskID = info.ID

	_, err = h.db.Collection("crawl_queue").InsertOne(ctx, entry)
	return err
}

// ---------------------------------------------------------------------------
// Polling
// ---------------------------------------------------------------------------

// waitForRender polls the queue entry until it reaches a terminal state (done
// or failed) or the context deadline is exceeded.  On success it fetches and
// returns the cache entry (including the page status code).
func (h *ServeHandler) waitForRender(ctx context.Context, entryID bson.ObjectID, rawURL string) (*models.CacheEntry, error) {
	ticker := time.NewTicker(prerenderPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, errPrerenderTimeout
		case <-ticker.C:
			var current models.QueueEntry
			if err := h.db.Collection("crawl_queue").FindOne(ctx, bson.M{"_id": entryID}).Decode(&current); err != nil {
				return nil, err
			}

			switch current.Status {
			case models.QueueStatusDone:
				var cacheEntry models.CacheEntry
				if err := h.db.Collection("cache_entries").FindOne(ctx, bson.M{"url": rawURL}).Decode(&cacheEntry); err != nil {
					return nil, errors.New("render complete but cache entry not found")
				}
				return &cacheEntry, nil

			case models.QueueStatusFailed:
				msg := current.Error
				if msg == "" {
					msg = "render task failed"
				}
				return nil, errors.New(msg)
			}
			// Still pending or processing — keep polling.
		}
	}
}

// cachePageStatus returns the HTTP status code stored in a cache entry,
// defaulting to 200 when the field is unset (e.g. for entries created before
// X-Prerender-Status support was added).
func cachePageStatus(entry *models.CacheEntry) int {
	if entry.StatusCode == 0 {
		return http.StatusOK
	}
	return entry.StatusCode
}

// ---------------------------------------------------------------------------
// Error helpers
// ---------------------------------------------------------------------------

func (h *ServeHandler) respondRenderError(c *gin.Context, err error) {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errPrerenderTimeout) {
		c.JSON(http.StatusGatewayTimeout, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
}

// ---------------------------------------------------------------------------
// Request logging
// ---------------------------------------------------------------------------

// renderErrorStatus maps a render error to the corresponding HTTP status code.
func renderErrorStatus(err error) int {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errPrerenderTimeout) {
		return http.StatusGatewayTimeout
	}
	return http.StatusInternalServerError
}

// classifyRequester inspects the User-Agent header and returns the appropriate
// RequesterType: AI crawlers are detected first, then generic bots, with
// everything else treated as a human visitor.
func classifyRequester(ua string) models.RequesterType {
	lower := strings.ToLower(ua)

	// Well-known AI crawlers / LLM-related user agents.
	aiAgents := []string{
		"gptbot", "chatgpt-user", "oai-searchbot",
		"claudebot", "claude-web", "anthropic",
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
		"slurp",   // Yahoo
		"baidu",   // Baidu
		"yandex",  // Yandex
		"duckduck", // DuckDuckGo
		"sogou",   // Sogou
		"exabot",  // Exabot
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

// logPrerenderRequest inserts a PrerenderData record into prerender_data.
// It is intended to be called as a goroutine so it does not block the response.
func (h *ServeHandler) logPrerenderRequest(
	projectID bson.ObjectID,
	rawURL string,
	userAgent string,
	fromCache bool,
	statusCode int,
	elapsed time.Duration,
) {
	record := models.PrerenderData{
		ID:            bson.NewObjectID(),
		ProjectID:     projectID,
		URL:           rawURL,
		StatusCode:    statusCode,
		RenderTimeMs:  elapsed.Milliseconds(),
		FromCache:     fromCache,
		UserAgent:     userAgent,
		RequesterType: classifyRequester(userAgent),
		CachedAt:      time.Now().UTC(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, _ = h.db.Collection("prerender_data").InsertOne(ctx, record)
}
