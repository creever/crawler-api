package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/hibiken/asynq"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.uber.org/zap"

	"github.com/creever/crawler-api/models"
)

// Processor holds the dependencies needed to execute crawl tasks.
type Processor struct {
	db     *mongo.Database
	logger *zap.Logger
}

// NewProcessor creates a Processor wired to the given MongoDB database.
func NewProcessor(db *mongo.Database, logger *zap.Logger) *Processor {
	return &Processor{db: db, logger: logger}
}

// Register mounts the task handlers onto the provided ServeMux.
func (p *Processor) Register(mux *asynq.ServeMux) {
	mux.HandleFunc(TypeCrawlSEO, p.HandleCrawlSEO)
	mux.HandleFunc(TypeCrawlRender, p.HandleCrawlRender)
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
// Crawl stubs — replace with real crawler calls (e.g. colly / chromedp).
// ---------------------------------------------------------------------------

// runSEOCrawl performs the SEO extraction for every seed URL in the config.
// TODO: Replace with real crawler implementation.
func (p *Processor) runSEOCrawl(_ context.Context, config models.ProjectConfig) ([]models.SEOResultPayload, error) {
	now := time.Now().UTC()
	results := make([]models.SEOResultPayload, 0, len(config.SeedURLs))
	for _, u := range config.SeedURLs {
		results = append(results, models.SEOResultPayload{
			ProjectID: config.ProjectID,
			CrawledAt: now,
			URL:       u,
		})
	}
	return results, nil
}

// runRenderCrawl pre-renders every seed URL via JavaScript and returns the HTML.
// TODO: Replace with real chromedp/Playwright implementation.
func (p *Processor) runRenderCrawl(_ context.Context, config models.ProjectConfig) ([]models.RenderPayload, error) {
	now := time.Now().UTC()
	results := make([]models.RenderPayload, 0, len(config.SeedURLs))
	for _, u := range config.SeedURLs {
		results = append(results, models.RenderPayload{
			ProjectID: config.ProjectID,
			CrawledAt: now,
			URL:       u,
			HTML:      "",
		})
	}
	return results, nil
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
			ID:        bson.NewObjectID(),
			ProjectID: projectOID,
			URL:       r.URL,
			FullHTML:  r.HTML,
			CachedAt:  r.CrawledAt,
			ExpiresAt: now.Add(24 * time.Hour),
			UpdatedAt: now,
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
