package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/hibiken/asynq"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.uber.org/zap"

	"github.com/creever/crawler-api/models"
)

// Processor holds the dependencies needed to execute crawl tasks.
type Processor struct {
	db          *mongo.Database
	logger      *zap.Logger
	crawlerAddr string
	httpClient  *http.Client
	asynqClient *asynq.Client
}

// NewProcessor creates a Processor wired to the given MongoDB database,
// crawler base URL (e.g. "http://crawler"), and asynq client for re-enqueuing
// child tasks during discovery.
func NewProcessor(db *mongo.Database, logger *zap.Logger, crawlerAddr string, asynqClient *asynq.Client) *Processor {
	return &Processor{
		db:          db,
		logger:      logger,
		crawlerAddr: crawlerAddr,
		httpClient:  &http.Client{Timeout: 60 * time.Second},
		asynqClient: asynqClient,
	}
}

// Register mounts the task handlers onto the provided ServeMux.
func (p *Processor) Register(mux *asynq.ServeMux) {
	mux.HandleFunc(TypeCrawlSEO, p.HandleCrawlSEO)
	mux.HandleFunc(TypeCrawlRender, p.HandleCrawlRender)
	mux.HandleFunc(TypeCrawlDiscover, p.HandleCrawlDiscover)
}

// NewServer creates a pre-configured asynq.Server backed by Redis at redisAddr.
func NewServer(redisAddr string, logger *zap.Logger) *asynq.Server {
	return asynq.NewServer(
		asynq.RedisClientOpt{Addr: redisAddr},
		asynq.Config{
			Concurrency: 5,
			ErrorHandler: asynq.ErrorHandlerFunc(func(ctx context.Context, task *asynq.Task, err error) {
				logger.Error("asynq task failed",
					zap.String("type", task.Type()),
					zap.Error(err),
				)
			}),
		},
	)
}

// HandleCrawlSEO processes a crawl:seo task.
// It updates the queue-entry status, invokes the SEO crawler, stores the
// result in seo_data, and finally marks the entry as done (or failed).
func (p *Processor) HandleCrawlSEO(ctx context.Context, t *asynq.Task) error {
	var payload CrawlPayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		return fmt.Errorf("%w: unmarshal crawl:seo payload: %v", asynq.SkipRetry, err)
	}

	p.logger.Info("processing crawl:seo task",
		zap.String("queue_entry_id", payload.QueueEntryID),
		zap.Strings("seed_urls", payload.Config.SeedURLs),
	)

	p.setQueueStatus(ctx, payload.QueueEntryID, models.QueueStatusProcessing, "")

	result, err := p.runSEOCrawl(ctx, payload.Config)
	if err != nil {
		p.setQueueStatus(ctx, payload.QueueEntryID, models.QueueStatusFailed, err.Error())
		return err
	}

	if err = p.storeSEOResult(ctx, result); err != nil {
		p.setQueueStatus(ctx, payload.QueueEntryID, models.QueueStatusFailed, err.Error())
		return err
	}

	p.setQueueStatus(ctx, payload.QueueEntryID, models.QueueStatusDone, "")
	p.logger.Info("crawl:seo task completed", zap.String("queue_entry_id", payload.QueueEntryID))
	return nil
}

// HandleCrawlRender processes a crawl:render task.
// It updates the queue-entry status, invokes the render crawler, stores the
// full HTML in cache_entries, and finally marks the entry as done (or failed).
func (p *Processor) HandleCrawlRender(ctx context.Context, t *asynq.Task) error {
	var payload CrawlPayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		return fmt.Errorf("%w: unmarshal crawl:render payload: %v", asynq.SkipRetry, err)
	}

	p.logger.Info("processing crawl:render task",
		zap.String("queue_entry_id", payload.QueueEntryID),
		zap.Strings("seed_urls", payload.Config.SeedURLs),
	)

	p.setQueueStatus(ctx, payload.QueueEntryID, models.QueueStatusProcessing, "")

	result, err := p.runRenderCrawl(ctx, payload.Config)
	if err != nil {
		p.setQueueStatus(ctx, payload.QueueEntryID, models.QueueStatusFailed, err.Error())
		return err
	}

	if err = p.storeRenderResult(ctx, result); err != nil {
		p.setQueueStatus(ctx, payload.QueueEntryID, models.QueueStatusFailed, err.Error())
		return err
	}

	p.setQueueStatus(ctx, payload.QueueEntryID, models.QueueStatusDone, "")
	p.logger.Info("crawl:render task completed", zap.String("queue_entry_id", payload.QueueEntryID))
	return nil
}

// ---------------------------------------------------------------------------
// Crawler HTTP calls
// ---------------------------------------------------------------------------

// runSEOCrawl calls GET /crawl?url=...&js=... on the crawler bot for every
// seed URL in the config and returns the parsed SEO results.
func (p *Processor) runSEOCrawl(ctx context.Context, config models.ProjectConfig) ([]models.SEOResultPayload, error) {
	now := time.Now().UTC()
	results := make([]models.SEOResultPayload, 0, len(config.SeedURLs))
	for _, seedURL := range config.SeedURLs {
		result, err := p.fetchSEO(ctx, seedURL, config.UseJS)
		if err != nil {
			return nil, fmt.Errorf("SEO crawl for %s: %w", seedURL, err)
		}
		result.ProjectID = config.ProjectID
		result.CrawledAt = now
		results = append(results, *result)
	}
	return results, nil
}

// fetchSEO calls GET /crawl?url={u}&js={useJS} and decodes the JSON response
// into a SEOResultPayload.
func (p *Processor) fetchSEO(ctx context.Context, rawURL string, useJS bool) (*models.SEOResultPayload, error) {
	endpoint := fmt.Sprintf("%s/crawl?url=%s&js=%v",
		p.crawlerAddr,
		url.QueryEscape(rawURL),
		useJS,
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build SEO request: %w", err)
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("SEO request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("crawler /crawl returned HTTP %s for %s", resp.Status, rawURL)
	}

	var result models.SEOResultPayload
	if err = json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode SEO response: %w", err)
	}
	return &result, nil
}

// runRenderCrawl calls GET /render?url=...&js=... on the crawler bot for
// every seed URL in the config and returns the captured HTML payloads.
func (p *Processor) runRenderCrawl(ctx context.Context, config models.ProjectConfig) ([]models.RenderPayload, error) {
	now := time.Now().UTC()
	results := make([]models.RenderPayload, 0, len(config.SeedURLs))
	for _, seedURL := range config.SeedURLs {
		html, statusCode, err := p.fetchRender(ctx, seedURL, config.UseJS)
		if err != nil {
			return nil, fmt.Errorf("render crawl for %s: %w", seedURL, err)
		}
		results = append(results, models.RenderPayload{
			ProjectID:  config.ProjectID,
			CrawledAt:  now,
			URL:        seedURL,
			HTML:       html,
			StatusCode: statusCode,
		})
	}
	return results, nil
}

// fetchRender calls GET /render?url={u}&js={useJS} and returns the full HTML
// body and the page status code.  The page status code is read from the
// X-Prerender-Status response header (set by the crawler to convey the
// original HTTP status of the crawled page without conflicting with the
// crawler's own 200 success response).  When the header is absent, 200 is
// assumed.
func (p *Processor) fetchRender(ctx context.Context, rawURL string, useJS bool) (string, int, error) {
	endpoint := fmt.Sprintf("%s/render?url=%s&js=%v",
		p.crawlerAddr,
		url.QueryEscape(rawURL),
		useJS,
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", 0, fmt.Errorf("build render request: %w", err)
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("render request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("crawler /render returned HTTP %s for %s", resp.Status, rawURL)
	}

	// Read the actual page status from X-Prerender-Status; default to 200.
	pageStatus := http.StatusOK
	if xps := resp.Header.Get("X-Prerender-Status"); xps != "" {
		if parsed, parseErr := strconv.Atoi(xps); parseErr == nil && parsed > 0 {
			pageStatus = parsed
		}
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", 0, fmt.Errorf("read render response: %w", err)
	}
	return string(body), pageStatus, nil
}

// ---------------------------------------------------------------------------
// Storage helpers
// ---------------------------------------------------------------------------

func (p *Processor) storeSEOResult(ctx context.Context, results []models.SEOResultPayload) error {
	if len(results) == 0 {
		return nil
	}
	col := p.db.Collection("seo_data")
	for _, r := range results {
		projectOID, err := bson.ObjectIDFromHex(r.ProjectID)
		if err != nil {
			p.logger.Warn("invalid project_id in SEO result, skipping", zap.String("project_id", r.ProjectID))
			continue
		}
		doc := models.SEOData{
			ID:              bson.NewObjectID(),
			ProjectID:       projectOID,
			URL:             r.URL,
			Title:           r.Title,
			MetaDescription: r.MetaDescription,
			H1Tags:          r.H1,
			H2Tags:          r.H2,
			CanonicalURL:    r.Canonical,
			MetaRobots:      r.Robots,
			OGTitle:         r.OGTitle,
			OGDescription:   r.OGDescription,
			OGImage:         r.OGImage,
			WordCount:       r.WordCount,
			InternalLinks:   len(r.InternalLinks),
			ExternalLinks:   len(r.ExternalLinks),
			CrawledAt:       r.CrawledAt,
		}
		if _, err = col.InsertOne(ctx, doc); err != nil {
			return fmt.Errorf("store seo result for %s: %w", r.URL, err)
		}
	}
	return nil
}

func (p *Processor) storeRenderResult(ctx context.Context, results []models.RenderPayload) error {
	if len(results) == 0 {
		return nil
	}
	col := p.db.Collection("cache_entries")
	now := time.Now().UTC()
	for _, r := range results {
		projectOID, err := bson.ObjectIDFromHex(r.ProjectID)
		if err != nil {
			p.logger.Warn("invalid project_id in render result, skipping", zap.String("project_id", r.ProjectID))
			continue
		}
		doc := models.CacheEntry{
			ID:         bson.NewObjectID(),
			ProjectID:  projectOID,
			URL:        r.URL,
			FullHTML:   r.HTML,
			StatusCode: r.StatusCode,
			CachedAt:   r.CrawledAt,
			ExpiresAt:  now.Add(24 * time.Hour),
			UpdatedAt:  now,
		}
		if _, err = col.InsertOne(ctx, doc); err != nil {
			return fmt.Errorf("store render result for %s: %w", r.URL, err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Queue-entry status helpers
// ---------------------------------------------------------------------------

func (p *Processor) setQueueStatus(ctx context.Context, queueEntryID string, status models.QueueStatus, errMsg string) {
	oid, err := bson.ObjectIDFromHex(queueEntryID)
	if err != nil {
		p.logger.Warn("setQueueStatus: invalid queue_entry_id", zap.String("id", queueEntryID))
		return
	}

	update := bson.M{"status": status}
	if errMsg != "" {
		update["error"] = errMsg
	}
	if status == models.QueueStatusDone || status == models.QueueStatusFailed {
		now := time.Now().UTC()
		update["processed_at"] = now
	}

	tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if _, err = p.db.Collection("crawl_queue").UpdateOne(
		tctx,
		bson.M{"_id": oid},
		bson.M{"$set": update},
	); err != nil {
		p.logger.Error("setQueueStatus: failed to update queue entry",
			zap.String("id", queueEntryID),
			zap.Error(err),
		)
	}
}

// ---------------------------------------------------------------------------
// Discovery task
// ---------------------------------------------------------------------------

// HandleCrawlDiscover processes a crawl:discover task.
// It crawls the project root URL to discover all internal links, caps the list
// at MaxPages (default 10), filters out off-host URLs, then enqueues a
// crawl:seo job for every discovered URL and records them on the Discovery doc.
func (p *Processor) HandleCrawlDiscover(ctx context.Context, t *asynq.Task) error {
	var payload DiscoverPayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		return fmt.Errorf("%w: unmarshal crawl:discover payload: %v", asynq.SkipRetry, err)
	}

	p.logger.Info("processing crawl:discover task",
		zap.String("discovery_id", payload.DiscoveryID),
		zap.Strings("seed_urls", payload.Config.SeedURLs),
	)

	if len(payload.Config.SeedURLs) == 0 {
		p.setDiscoveryStatus(ctx, payload.DiscoveryID, models.DiscoveryStatusFailed, "no seed URLs provided")
		return fmt.Errorf("%w: no seed URLs in discover payload", asynq.SkipRetry)
	}

	p.setDiscoveryStatus(ctx, payload.DiscoveryID, models.DiscoveryStatusRunning, "")

	seedURL := payload.Config.SeedURLs[0]

	// Fetch the root page to get its internal links.
	seoResult, err := p.fetchSEO(ctx, seedURL, payload.Config.UseJS)
	if err != nil {
		p.setDiscoveryStatus(ctx, payload.DiscoveryID, models.DiscoveryStatusFailed, err.Error())
		return err
	}

	// Determine the allowed host so we never crawl external sites.
	parsedSeed, err := url.Parse(seedURL)
	if err != nil {
		p.setDiscoveryStatus(ctx, payload.DiscoveryID, models.DiscoveryStatusFailed, "invalid seed URL: "+err.Error())
		return fmt.Errorf("%w: parse seed URL: %v", asynq.SkipRetry, err)
	}
	seedHost := parsedSeed.Host

	// Determine the effective page limit (default 10).
	maxPages := payload.Config.MaxPages
	if maxPages <= 0 {
		maxPages = 10
	}

	// Collect URLs: seed first, then internal links – deduplicated, same-host only.
	seen := make(map[string]struct{})
	var urls []string

	tryAdd := func(rawURL string) {
		if len(urls) >= maxPages {
			return
		}
		parsed, parseErr := url.Parse(rawURL)
		if parseErr != nil {
			return
		}
		// Resolve relative references against the seed URL.
		if !parsed.IsAbs() {
			parsed = parsedSeed.ResolveReference(parsed)
		}
		// Reject off-host URLs.
		if parsed.Host != seedHost {
			return
		}
		// Drop fragment so #section variants collapse to the same page.
		parsed.Fragment = ""
		normalized := parsed.String()
		if _, exists := seen[normalized]; exists {
			return
		}
		seen[normalized] = struct{}{}
		urls = append(urls, normalized)
	}

	tryAdd(seedURL)
	for _, link := range seoResult.InternalLinks {
		tryAdd(link)
	}

	// Parse the discovery and project ObjectIDs once.
	discoveryOID, err := bson.ObjectIDFromHex(payload.DiscoveryID)
	if err != nil {
		p.setDiscoveryStatus(ctx, payload.DiscoveryID, models.DiscoveryStatusFailed, "invalid discovery_id")
		return fmt.Errorf("%w: invalid discovery_id: %v", asynq.SkipRetry, err)
	}

	projectOID, err := bson.ObjectIDFromHex(payload.Config.ProjectID)
	if err != nil {
		p.setDiscoveryStatus(ctx, payload.DiscoveryID, models.DiscoveryStatusFailed, "invalid project_id")
		return fmt.Errorf("%w: invalid project_id: %v", asynq.SkipRetry, err)
	}

	// Enqueue a crawl:seo task for every discovered URL.
	now := time.Now().UTC()
	var queueEntryIDs []string

	for _, u := range urls {
		entryID := bson.NewObjectID()
		entry := models.QueueEntry{
			ID:          entryID,
			ProjectID:   projectOID,
			DiscoveryID: &discoveryOID,
			URL:         u,
			TaskType:    models.QueueTaskTypeSEO,
			Status:      models.QueueStatusPending,
			EnqueuedAt:  now,
		}

		seoConfig := models.ProjectConfig{
			ProjectID: payload.Config.ProjectID,
			SeedURLs:  []string{u},
			UseJS:     payload.Config.UseJS,
			MaxPages:  1,
		}

		task, taskErr := NewCrawlSEOTask(entryID.Hex(), seoConfig)
		if taskErr != nil {
			p.logger.Warn("discover: failed to build seo task", zap.String("url", u), zap.Error(taskErr))
			continue
		}

		info, enqErr := p.asynqClient.EnqueueContext(ctx, task)
		if enqErr != nil {
			p.logger.Warn("discover: failed to enqueue seo task", zap.String("url", u), zap.Error(enqErr))
			continue
		}
		entry.AsynqTaskID = info.ID

		if _, insErr := p.db.Collection("crawl_queue").InsertOne(ctx, entry); insErr != nil {
			p.logger.Warn("discover: failed to store queue entry", zap.String("url", u), zap.Error(insErr))
			continue
		}

		queueEntryIDs = append(queueEntryIDs, entryID.Hex())
	}

	// Persist the final state of the discovery document.
	finishedAt := time.Now().UTC()
	if _, updErr := p.db.Collection("discoveries").UpdateOne(
		ctx,
		bson.M{"_id": discoveryOID},
		bson.M{"$set": bson.M{
			"status":          models.DiscoveryStatusDone,
			"pages_found":     len(queueEntryIDs),
			"queue_entry_ids": queueEntryIDs,
			"finished_at":     finishedAt,
		}},
	); updErr != nil {
		p.logger.Error("discover: failed to update discovery",
			zap.String("id", payload.DiscoveryID),
			zap.Error(updErr),
		)
		return updErr
	}

	p.logger.Info("crawl:discover task completed",
		zap.String("discovery_id", payload.DiscoveryID),
		zap.Int("pages_found", len(queueEntryIDs)),
	)
	return nil
}

// setDiscoveryStatus updates the status (and optionally error + finished_at)
// of a Discovery document in MongoDB.
func (p *Processor) setDiscoveryStatus(ctx context.Context, discoveryID string, status models.DiscoveryStatus, errMsg string) {
	oid, err := bson.ObjectIDFromHex(discoveryID)
	if err != nil {
		p.logger.Warn("setDiscoveryStatus: invalid discovery_id", zap.String("id", discoveryID))
		return
	}

	update := bson.M{"status": status}
	if errMsg != "" {
		update["error"] = errMsg
	}
	if status == models.DiscoveryStatusFailed {
		now := time.Now().UTC()
		update["finished_at"] = now
	}

	tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if _, err = p.db.Collection("discoveries").UpdateOne(
		tctx,
		bson.M{"_id": oid},
		bson.M{"$set": update},
	); err != nil {
		p.logger.Error("setDiscoveryStatus: failed to update discovery",
			zap.String("id", discoveryID),
			zap.Error(err),
		)
	}
}
