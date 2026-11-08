package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/hibiken/asynq"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"

	"github.com/creever/crawler-api/models"
)

const (
	// cacheFreshFor is how long a render is served without a refresh.
	cacheFreshFor = 24 * time.Hour
	// cacheKeepFor is how long a render stays after its last refresh: a stale
	// render answers at once while a new one is made in the background.
	cacheKeepFor = 30 * 24 * time.Hour
)

// renders makes concurrent requests for one page share a single render.
var renders singleflight.Group

// CacheKey is the cache key of a page URL.
func CacheKey(rawURL string) string { return normalizePageURL(rawURL) }

// LookupCache returns the project's cached render of a page, fresh or stale,
// or nil. An entry without HTML is never returned unless it records an error
// status, which needs no body.
func LookupCache(ctx context.Context, db *mongo.Database, projectID bson.ObjectID, rawURL string) (*models.CacheEntry, error) {
	var entry models.CacheEntry
	err := db.Collection("cache_entries").FindOne(ctx, bson.M{
		"project_id": projectID,
		"key":        CacheKey(rawURL),
		"$or":        bson.A{bson.M{"full_html": bson.M{"$ne": ""}}, bson.M{"status_code": bson.M{"$gte": 400}}},
	}).Decode(&entry)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &entry, nil
}

// MarkServed counts a bot request answered from the cache.
func MarkServed(ctx context.Context, db *mongo.Database, id bson.ObjectID, at time.Time) {
	_, _ = db.Collection("cache_entries").UpdateOne(ctx, bson.M{"_id": id},
		bson.M{"$inc": bson.M{"hits": 1}, "$set": bson.M{"last_served_at": at}})
}

// RenderToCache renders a page now and stores it in the cache. Concurrent
// calls for the same page wait for one render. The queue entry records it for
// the Rendering Queue view.
func (p *Processor) RenderToCache(ctx context.Context, project *models.Project, rawURL, source string) (*models.CacheEntry, error) {
	key := project.ID.Hex() + "|" + CacheKey(rawURL)
	v, err, _ := renders.Do(key, func() (interface{}, error) {
		entryID := bson.NewObjectID()
		now := time.Now().UTC()
		_, _ = p.db.Collection("crawl_queue").InsertOne(ctx, models.QueueEntry{
			ID: entryID, ProjectID: project.ID, URL: rawURL, TaskType: models.QueueTaskTypeRender,
			Status: models.QueueStatusProcessing, Source: source, EnqueuedAt: now,
		})
		entry, renderErr := p.renderAndStore(ctx, project, rawURL, source)
		if renderErr != nil {
			p.setQueueStatus(context.Background(), entryID.Hex(), models.QueueStatusFailed, renderErr.Error())
			return nil, renderErr
		}
		p.setQueueStatus(context.Background(), entryID.Hex(), models.QueueStatusDone, "")
		return entry, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*models.CacheEntry), nil
}

// renderAndStore renders a page the way the project serves bots and upserts
// its cache entry.
func (p *Processor) renderAndStore(ctx context.Context, project *models.Project, rawURL, source string) (*models.CacheEntry, error) {
	page, err := p.fetchPage(ctx, rawURL, project.UseJS)
	if err != nil {
		return nil, err
	}
	return p.storeRender(ctx, project.ID, rawURL, page, source, time.Now().UTC())
}

// storeRender upserts the render of a page into the cache.
func (p *Processor) storeRender(ctx context.Context, projectID bson.ObjectID, rawURL string, page *renderedPage, source string, now time.Time) (*models.CacheEntry, error) {
	entry := &models.CacheEntry{
		ProjectID: projectID, Key: CacheKey(rawURL), URL: rawURL, FullHTML: page.HTML,
		StatusCode: page.Status, FinalURL: page.FinalURL, Redirects: page.Redirects, RenderMs: page.RenderMs,
		HTMLBytes: len(page.HTML), Source: source, CachedAt: now,
		ExpiresAt: now.Add(cacheFreshFor), PurgeAt: now.Add(cacheKeepFor),
	}
	set := bson.M{
		"url": entry.URL, "full_html": entry.FullHTML, "status_code": entry.StatusCode, "final_url": entry.FinalURL,
		"redirects": entry.Redirects, "render_ms": entry.RenderMs, "html_bytes": entry.HTMLBytes, "source": source,
		"cached_at": now, "expires_at": entry.ExpiresAt, "purge_at": entry.PurgeAt,
	}
	err := p.db.Collection("cache_entries").FindOneAndUpdate(ctx,
		bson.M{"project_id": projectID, "key": entry.Key},
		bson.M{"$set": set, "$setOnInsert": bson.M{"hits": 0}},
		options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After),
	).Decode(entry)
	if err != nil {
		return nil, fmt.Errorf("store render of %s: %w", rawURL, err)
	}
	return entry, nil
}

// refreshCachedRender stores a JavaScript render the site crawl made anyway,
// for a page bots already ask for. It costs no render of its own.
func (p *Processor) refreshCachedRender(ctx context.Context, projectID bson.ObjectID, rawURL string, page *renderedPage) {
	if page.Status >= 500 || page.HTML == "" {
		return
	}
	now := time.Now().UTC()
	_, err := p.db.Collection("cache_entries").UpdateOne(ctx,
		bson.M{"project_id": projectID, "key": CacheKey(rawURL)},
		bson.M{"$set": bson.M{
			"full_html": page.HTML, "status_code": page.Status, "final_url": page.FinalURL, "redirects": page.Redirects,
			"render_ms": page.RenderMs, "html_bytes": len(page.HTML), "source": models.CacheSourceCrawl,
			"cached_at": now, "expires_at": now.Add(cacheFreshFor), "purge_at": now.Add(cacheKeepFor),
		}})
	if err != nil {
		p.logger.Warn("prerender cache not refreshed from the crawl", zap.String("url", rawURL), zap.Error(err))
	}
}

// EnqueueRender queues a background render of a page into the cache. A page
// already waiting is not queued twice.
func EnqueueRender(ctx context.Context, db *mongo.Database, client *asynq.Client, projectID bson.ObjectID, rawURL, source string) error {
	entry := models.QueueEntry{
		ID: bson.NewObjectID(), ProjectID: projectID, URL: rawURL, TaskType: models.QueueTaskTypeRender,
		Status: models.QueueStatusPending, Source: source, EnqueuedAt: time.Now().UTC(),
	}
	task, err := NewCrawlRenderTask(entry.ID.Hex(), projectID.Hex(), rawURL)
	if err != nil {
		return err
	}
	info, err := client.EnqueueContext(ctx, task, asynq.TaskID("render:"+projectID.Hex()+":"+CacheKey(rawURL)))
	if errors.Is(err, asynq.ErrDuplicateTask) || errors.Is(err, asynq.ErrTaskIDConflict) {
		return nil
	}
	if err != nil {
		return err
	}
	entry.AsynqTaskID = info.ID
	_, err = db.Collection("crawl_queue").InsertOne(ctx, entry)
	return err
}

// HandleCrawlRender renders a page into the cache in the background.
func (p *Processor) HandleCrawlRender(ctx context.Context, t *asynq.Task) error {
	var payload RenderPayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		return fmt.Errorf("%w: unmarshal crawl:render payload: %v", asynq.SkipRetry, err)
	}
	projectOID, err := bson.ObjectIDFromHex(payload.ProjectID)
	if err != nil {
		return fmt.Errorf("%w: invalid project_id: %v", asynq.SkipRetry, err)
	}
	var project models.Project
	if err = p.db.Collection("projects").FindOne(ctx, bson.M{"_id": projectOID}).Decode(&project); err != nil {
		p.setQueueStatus(ctx, payload.QueueEntryID, models.QueueStatusFailed, "project not found")
		return fmt.Errorf("%w: project not found: %v", asynq.SkipRetry, err)
	}
	p.setQueueStatus(ctx, payload.QueueEntryID, models.QueueStatusProcessing, "")
	var source string
	if oid, idErr := bson.ObjectIDFromHex(payload.QueueEntryID); idErr == nil {
		var q models.QueueEntry
		if p.db.Collection("crawl_queue").FindOne(ctx, bson.M{"_id": oid}).Decode(&q) == nil {
			source = q.Source
		}
	}
	if source == "" {
		source = models.CacheSourceManual
	}
	if _, err = p.renderAndStore(ctx, &project, payload.URL, source); err != nil {
		p.setQueueStatus(ctx, payload.QueueEntryID, models.QueueStatusFailed, err.Error())
		return err
	}
	p.setQueueStatus(ctx, payload.QueueEntryID, models.QueueStatusDone, "")
	return nil
}
