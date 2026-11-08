package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hibiken/asynq"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.uber.org/zap"
	"golang.org/x/net/html"
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
// its cache entry. A site the crawl found to need JavaScript is rendered with
// it whatever the project says: without it the bot would get an empty shell.
func (p *Processor) renderAndStore(ctx context.Context, project *models.Project, rawURL, source string) (*models.CacheEntry, error) {
	useJS := project.UseJS
	if !useJS {
		if audit, err := LatestAudit(ctx, p.db, project.ID); err == nil && audit != nil && audit.Rendering != nil && audit.Rendering.NeedsJS {
			useJS = true
		}
	}
	page, err := p.fetchPage(ctx, rawURL, useJS)
	if err != nil {
		return nil, err
	}
	return p.storeRender(ctx, project.ID, rawURL, page, useJS, source, time.Now().UTC())
}

// botHTML is the HTML to keep for bots: a JavaScript render no longer needs
// its scripts, and a bot that runs them would only redo the work.
func botHTML(page *renderedPage, rendered bool) string {
	if rendered {
		return stripScripts(page.HTML)
	}
	return page.HTML
}

// stripScripts removes executable scripts and the hints that preload them.
// Structured data (JSON-LD) stays: it is what bots read. The rest of the
// markup is copied as it came.
func stripScripts(src string) string {
	z := html.NewTokenizer(strings.NewReader(src))
	var b strings.Builder
	b.Grow(len(src))
	inScript := false
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			return b.String()
		}
		// TagName and TagAttr rewrite the buffer Raw points into.
		raw := append([]byte(nil), z.Raw()...)
		switch tt {
		case html.StartTagToken, html.SelfClosingTagToken:
			name, _ := z.TagName()
			tag := string(name)
			attrs := tagAttrs(z)
			if tag == "script" && !isDataScript(attrs["type"]) {
				inScript = tt == html.StartTagToken
				continue
			}
			if tag == "link" && preloadsScript(attrs) {
				continue
			}
		case html.EndTagToken:
			if name, _ := z.TagName(); inScript && string(name) == "script" {
				inScript = false
				continue
			}
		}
		if !inScript {
			b.Write(raw)
		}
	}
}

// tagAttrs reads the current tag's attributes, names in lower case. z.Raw must
// be taken before this call.
func tagAttrs(z *html.Tokenizer) map[string]string {
	attrs := map[string]string{}
	for {
		key, val, more := z.TagAttr()
		if len(key) > 0 {
			attrs[strings.ToLower(string(key))] = string(val)
		}
		if !more {
			return attrs
		}
	}
}

// isDataScript reports whether a script type is data rather than code.
func isDataScript(typ string) bool {
	return strings.EqualFold(strings.TrimSpace(typ), "application/ld+json")
}

func preloadsScript(attrs map[string]string) bool {
	rel := strings.ToLower(attrs["rel"])
	return rel == "modulepreload" || (rel == "preload" && strings.EqualFold(attrs["as"], "script"))
}

// cacheFields are what a render writes to its cache entry.
func cacheFields(page *renderedPage, htmlBody, rawURL, source string, now time.Time) bson.M {
	return bson.M{
		"url": rawURL, "full_html": htmlBody, "status_code": page.Status, "final_url": page.FinalURL,
		"redirects": page.Redirects, "render_ms": page.RenderMs, "html_bytes": len(htmlBody), "source": source,
		"cached_at": now, "expires_at": now.Add(cacheFreshFor), "purge_at": now.Add(cacheKeepFor),
	}
}

// storeRender upserts the render of a page into the cache.
func (p *Processor) storeRender(ctx context.Context, projectID bson.ObjectID, rawURL string, page *renderedPage, rendered bool, source string, now time.Time) (*models.CacheEntry, error) {
	entry := &models.CacheEntry{}
	err := p.db.Collection("cache_entries").FindOneAndUpdate(ctx,
		bson.M{"project_id": projectID, "key": CacheKey(rawURL)},
		bson.M{"$set": cacheFields(page, botHTML(page, rendered), rawURL, source, now), "$setOnInsert": bson.M{"hits": 0}},
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
	_, err := p.db.Collection("cache_entries").UpdateOne(ctx,
		bson.M{"project_id": projectID, "key": CacheKey(rawURL)},
		bson.M{"$set": cacheFields(page, botHTML(page, true), rawURL, models.CacheSourceCrawl, time.Now().UTC())})
	if err != nil {
		p.logger.Warn("prerender cache not refreshed from the crawl", zap.String("url", rawURL), zap.Error(err))
	}
}

const (
	// warmCachePages is how many of a project's strongest search pages are
	// kept ready in the cache.
	warmCachePages = 200
	// prerenderInUseWindow: a project with no bot request this recent has not
	// connected /prerender, so there is nobody to warm the cache for.
	prerenderInUseWindow = 30 * 24 * time.Hour
)

// warmSet names the pages the crawl should store in the cache as it reads
// them: the strongest search pages, on projects that use /prerender.
func (p *Processor) warmSet(ctx context.Context, project *models.Project, impressions map[string]float64, now time.Time) map[string]bool {
	inUse, err := p.db.Collection("prerender_data").CountDocuments(ctx, bson.M{
		"project_id": project.ID, "cached_at": bson.M{"$gte": now.Add(-prerenderInUseWindow)},
		"requester_type": bson.M{"$ne": models.RequesterTypeHuman},
	}, options.Count().SetLimit(1))
	if err != nil || inUse == 0 {
		return nil
	}
	return topImpressionKeys(impressions, warmCachePages)
}

// topImpressionKeys returns the n pages with the most impressions.
func topImpressionKeys(impressions map[string]float64, n int) map[string]bool {
	keys := make([]string, 0, len(impressions))
	for k := range impressions {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if impressions[keys[i]] != impressions[keys[j]] {
			return impressions[keys[i]] > impressions[keys[j]]
		}
		return keys[i] < keys[j]
	})
	if len(keys) > n {
		keys = keys[:n]
	}
	out := make(map[string]bool, len(keys))
	for _, k := range keys {
		out[k] = true
	}
	return out
}

// warmCache stores a JavaScript render the crawl made anyway. Server errors
// and empty answers are not worth serving.
func (p *Processor) warmCache(ctx context.Context, projectID bson.ObjectID, rawURL string, page *renderedPage) {
	if page.Status >= 400 || page.HTML == "" {
		return
	}
	if _, err := p.storeRender(ctx, projectID, rawURL, page, true, models.CacheSourceWarm, time.Now().UTC()); err != nil {
		p.logger.Warn("prerender cache not warmed", zap.String("url", rawURL), zap.Error(err))
	}
}

// taskRefreshRenders caps the renders one finished task can queue.
const taskRefreshRenders = 50

// refreshTaskPages makes bots get the changed pages of a finished task: the
// cached pages turn stale at once, and the most served are rendered now.
func (p *Processor) refreshTaskPages(ctx context.Context, task *models.SEOTask, now time.Time) {
	if len(task.Pages) == 0 {
		return
	}
	p.purgeProjectCDN(ctx, task.ProjectID, task.Pages, CDNTriggerTask)
	keys := make(bson.A, 0, len(task.Pages))
	for _, page := range task.Pages {
		keys = append(keys, CacheKey(page))
	}
	col := p.db.Collection("cache_entries")
	filter := bson.M{"project_id": task.ProjectID, "key": bson.M{"$in": keys}}
	if _, err := col.UpdateMany(ctx, filter, bson.M{"$set": bson.M{"expires_at": now}}); err != nil {
		p.logger.Warn("task pages not expired in the cache", zap.Error(err))
		return
	}
	if p.asynqClient == nil {
		return
	}
	cursor, err := col.Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "hits", Value: -1}}).SetLimit(taskRefreshRenders).SetProjection(bson.M{"url": 1}))
	if err != nil {
		p.logger.Warn("task pages not read from the cache", zap.Error(err))
		return
	}
	var entries []models.CacheEntry
	if err = cursor.All(ctx, &entries); err != nil {
		return
	}
	for _, e := range entries {
		if err = EnqueueRender(ctx, p.db, p.asynqClient, task.ProjectID, e.URL, models.CacheSourceTask); err != nil {
			p.logger.Warn("task page not queued for a render", zap.String("url", e.URL), zap.Error(err))
		}
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
	if source == models.CacheSourceManual {
		p.purgeProjectCDN(ctx, projectOID, []string{payload.URL}, CDNTriggerManual)
	}
	return nil
}
