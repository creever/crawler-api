package handlers

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/hibiken/asynq"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/creever/crawler-api/models"
	"github.com/creever/crawler-api/worker"
)

const (
	prerenderStatsMaxDays = 90
	cacheRefreshMax       = 50
	prerenderTopPages     = 15
)

// PrerenderHandler serves a project's prerender analytics, cache and render
// queue.
type PrerenderHandler struct {
	db          *mongo.Database
	asynqClient *asynq.Client
}

// NewPrerenderHandler creates a PrerenderHandler.
func NewPrerenderHandler(db *mongo.Database, asynqClient *asynq.Client) *PrerenderHandler {
	return &PrerenderHandler{db: db, asynqClient: asynqClient}
}

// prerenderDay is one day of requests by requester type.
type prerenderDay struct {
	Date   string `json:"date"`
	Human  int64  `json:"human"`
	Bot    int64  `json:"bot"`
	AI     int64  `json:"ai"`
	Misses int64  `json:"misses"`
}

// prerenderAgent is one named crawler's requests.
type prerenderAgent struct {
	Agent    string `json:"agent"`
	Type     string `json:"type"`
	Requests int64  `json:"requests"`
}

// prerenderPage is a page bots ask for, with what the site crawl and Search
// Console know about it.
type prerenderPage struct {
	URL         string             `json:"url"`
	Requests    int64              `json:"requests"`
	AI          int64              `json:"ai"`
	Misses      int64              `json:"misses"`
	Status      int                `json:"status"`
	Cached      bool               `json:"cached"`
	Read        bool               `json:"read"`
	Issues      []models.PageIssue `json:"issues"`
	Impressions float64            `json:"impressions"`
	Clicks      float64            `json:"clicks"`
}

// cacheSummary counts a project's cache entries.
type cacheSummary struct {
	Entries int64 `json:"entries"`
	Fresh   int64 `json:"fresh"`
	Stale   int64 `json:"stale"`
	Errors  int64 `json:"errors"`
	Bytes   int64 `json:"bytes"`
}

// prerenderStats is the prerender picture of a project over a period.
type prerenderStats struct {
	Days        int              `json:"days"`
	Total       int64            `json:"total"`
	Hits        int64            `json:"hits"`
	Stale       int64            `json:"stale"`
	Misses      int64            `json:"misses"`
	Errors      int64            `json:"errors"`
	HitRate     float64          `json:"hit_rate"` // hits and stale answers over all, 0..1
	AvgMissMs   float64          `json:"avg_miss_ms"`
	AvgHitMs    float64          `json:"avg_hit_ms"`
	ErrorStatus int64            `json:"error_status"` // answers with a 4xx/5xx page status
	ByRequester map[string]int64 `json:"by_requester"`
	Daily       []prerenderDay   `json:"daily"`
	Agents      []prerenderAgent `json:"agents"`
	TopPages    []prerenderPage  `json:"top_pages"`
	Cache       cacheSummary     `json:"cache"`
}

// buildPrerenderStats sums the request log of a project over the last days.
func buildPrerenderStats(ctx context.Context, db *mongo.Database, projectID bson.ObjectID, days int, now time.Time) (*prerenderStats, error) {
	since := now.AddDate(0, 0, -days+1).Truncate(24 * time.Hour)
	match := bson.D{{Key: "$match", Value: bson.M{"project_id": projectID, "cached_at": bson.M{"$gte": since}}}}
	col := db.Collection("prerender_data")
	stats := &prerenderStats{Days: days, ByRequester: map[string]int64{"human": 0, "bot": 0, "ai": 0}}

	var groups []struct {
		ID struct {
			Day   string `bson:"day"`
			Type  string `bson:"type"`
			Cache string `bson:"cache"`
			Agent string `bson:"agent"`
			Error bool   `bson:"error"`
		} `bson:"_id"`
		N  int64   `bson:"n"`
		Ms float64 `bson:"ms"`
	}
	cursor, err := col.Aggregate(ctx, mongo.Pipeline{match,
		{{Key: "$group", Value: bson.M{
			"_id": bson.M{
				"day":   bson.M{"$dateToString": bson.M{"format": "%Y-%m-%d", "date": "$cached_at"}},
				"type":  "$requester_type",
				"cache": bson.M{"$ifNull": bson.A{"$cache", bson.M{"$cond": bson.A{"$from_cache", models.CacheStateHit, models.CacheStateMiss}}}},
				"agent": bson.M{"$ifNull": bson.A{"$agent", ""}},
				"error": bson.M{"$gte": bson.A{"$status_code", 400}},
			},
			"n":  bson.M{"$sum": 1},
			"ms": bson.M{"$sum": "$render_time_ms"},
		}}},
	})
	if err != nil {
		return nil, err
	}
	if err = cursor.All(ctx, &groups); err != nil {
		return nil, err
	}

	daily := make(map[string]*prerenderDay)
	for d := since; !d.After(now); d = d.AddDate(0, 0, 1) {
		key := d.Format("2006-01-02")
		daily[key] = &prerenderDay{Date: key}
	}
	agents := make(map[string]*prerenderAgent)
	var missMs, hitMs float64
	for _, g := range groups {
		stats.Total += g.N
		stats.ByRequester[g.ID.Type] += g.N
		if g.ID.Error {
			stats.ErrorStatus += g.N
		}
		switch g.ID.Cache {
		case models.CacheStateHit:
			stats.Hits += g.N
			hitMs += g.Ms
		case models.CacheStateStale:
			stats.Stale += g.N
			hitMs += g.Ms
		case models.CacheStateError:
			stats.Errors += g.N
		default:
			stats.Misses += g.N
			missMs += g.Ms
		}
		if day := daily[g.ID.Day]; day != nil {
			switch models.RequesterType(g.ID.Type) {
			case models.RequesterTypeAI:
				day.AI += g.N
			case models.RequesterTypeBot:
				day.Bot += g.N
			default:
				day.Human += g.N
			}
			if g.ID.Cache == models.CacheStateMiss {
				day.Misses += g.N
			}
		}
		if g.ID.Agent != "" {
			a := agents[g.ID.Agent]
			if a == nil {
				a = &prerenderAgent{Agent: g.ID.Agent, Type: g.ID.Type}
				agents[g.ID.Agent] = a
			}
			a.Requests += g.N
		}
	}
	if served := stats.Hits + stats.Stale; served > 0 {
		stats.AvgHitMs = hitMs / float64(served)
	}
	if stats.Misses > 0 {
		stats.AvgMissMs = missMs / float64(stats.Misses)
	}
	if stats.Total > 0 {
		stats.HitRate = float64(stats.Hits+stats.Stale) / float64(stats.Total)
	}
	for d := since; !d.After(now); d = d.AddDate(0, 0, 1) {
		stats.Daily = append(stats.Daily, *daily[d.Format("2006-01-02")])
	}
	stats.Agents = make([]prerenderAgent, 0, len(agents))
	for _, a := range agents {
		stats.Agents = append(stats.Agents, *a)
	}
	sort.Slice(stats.Agents, func(i, j int) bool { return stats.Agents[i].Requests > stats.Agents[j].Requests })

	if stats.TopPages, err = topPrerenderPages(ctx, db, projectID, match, now); err != nil {
		return nil, err
	}
	if stats.Cache, err = summarizeCache(ctx, db, projectID, now); err != nil {
		return nil, err
	}
	return stats, nil
}

// topPrerenderPages lists the pages bots ask for most, joined with the site
// crawl's profile and the page's search reach.
func topPrerenderPages(ctx context.Context, db *mongo.Database, projectID bson.ObjectID, match bson.D, now time.Time) ([]prerenderPage, error) {
	var rows []struct {
		URL    string `bson:"url"`
		N      int64  `bson:"n"`
		AI     int64  `bson:"ai"`
		Misses int64  `bson:"misses"`
		Status int    `bson:"status"`
	}
	cursor, err := db.Collection("prerender_data").Aggregate(ctx, mongo.Pipeline{match,
		{{Key: "$match", Value: bson.M{"requester_type": bson.M{"$ne": models.RequesterTypeHuman}}}},
		{{Key: "$sort", Value: bson.M{"cached_at": 1}}},
		{{Key: "$group", Value: bson.M{
			"_id":    bson.M{"$ifNull": bson.A{"$key", "$url"}},
			"url":    bson.M{"$last": "$url"},
			"n":      bson.M{"$sum": 1},
			"ai":     bson.M{"$sum": bson.M{"$cond": bson.A{bson.M{"$eq": bson.A{"$requester_type", models.RequesterTypeAI}}, 1, 0}}},
			"misses": bson.M{"$sum": bson.M{"$cond": bson.A{bson.M{"$eq": bson.A{"$cache", models.CacheStateMiss}}, 1, 0}}},
			"status": bson.M{"$last": "$status_code"},
		}}},
		{{Key: "$sort", Value: bson.D{{Key: "n", Value: -1}, {Key: "_id", Value: 1}}}},
		{{Key: "$limit", Value: prerenderTopPages}},
	})
	if err != nil {
		return nil, err
	}
	if err = cursor.All(ctx, &rows); err != nil {
		return nil, err
	}
	pages := make([]prerenderPage, 0, len(rows))
	keys := make(bson.A, 0, len(rows))
	for _, r := range rows {
		pages = append(pages, prerenderPage{URL: r.URL, Requests: r.N, AI: r.AI, Misses: r.Misses, Status: r.Status, Issues: []models.PageIssue{}})
		keys = append(keys, worker.NormalizePageURL(r.URL))
	}
	if len(pages) == 0 {
		return pages, nil
	}
	var profiles []models.PageProfile
	pc, err := db.Collection("page_profiles").Find(ctx, bson.M{"project_id": projectID, "url": bson.M{"$in": keys}, "version": bson.M{"$gte": 1}},
		options.Find().SetProjection(bson.M{"url": 1, "issues": 1, "impressions": 1, "clicks": 1}))
	if err != nil {
		return nil, err
	}
	if err = pc.All(ctx, &profiles); err != nil {
		return nil, err
	}
	byKey := make(map[string]models.PageProfile, len(profiles))
	for _, pr := range profiles {
		byKey[pr.URL] = pr
	}
	cached := make(map[string]bool)
	cc, err := db.Collection("cache_entries").Find(ctx, bson.M{"project_id": projectID, "key": bson.M{"$in": keys}},
		options.Find().SetProjection(bson.M{"key": 1}))
	if err != nil {
		return nil, err
	}
	var entries []struct {
		Key string `bson:"key"`
	}
	if err = cc.All(ctx, &entries); err != nil {
		return nil, err
	}
	for _, e := range entries {
		cached[e.Key] = true
	}
	for i := range pages {
		key := worker.NormalizePageURL(pages[i].URL)
		pages[i].Cached = cached[key]
		if pr, ok := byKey[key]; ok {
			pages[i].Read = true
			pages[i].Impressions, pages[i].Clicks = pr.Impressions, pr.Clicks
			if pr.Issues != nil {
				pages[i].Issues = pr.Issues
			}
		}
	}
	return pages, nil
}

// summarizeCache counts a project's cache entries by freshness.
func summarizeCache(ctx context.Context, db *mongo.Database, projectID bson.ObjectID, now time.Time) (cacheSummary, error) {
	var out []struct {
		Entries int64 `bson:"entries"`
		Fresh   int64 `bson:"fresh"`
		Errors  int64 `bson:"errors"`
		Bytes   int64 `bson:"bytes"`
	}
	cursor, err := db.Collection("cache_entries").Aggregate(ctx, mongo.Pipeline{
		{{Key: "$match", Value: bson.M{"project_id": projectID}}},
		{{Key: "$group", Value: bson.M{
			"_id":     nil,
			"entries": bson.M{"$sum": 1},
			"fresh":   bson.M{"$sum": bson.M{"$cond": bson.A{bson.M{"$gt": bson.A{"$expires_at", now}}, 1, 0}}},
			"errors":  bson.M{"$sum": bson.M{"$cond": bson.A{bson.M{"$gte": bson.A{"$status_code", 400}}, 1, 0}}},
			"bytes":   bson.M{"$sum": bson.M{"$ifNull": bson.A{"$html_bytes", 0}}},
		}}},
	})
	if err != nil {
		return cacheSummary{}, err
	}
	if err = cursor.All(ctx, &out); err != nil || len(out) == 0 {
		return cacheSummary{}, err
	}
	o := out[0]
	return cacheSummary{Entries: o.Entries, Fresh: o.Fresh, Stale: o.Entries - o.Fresh, Errors: o.Errors, Bytes: o.Bytes}, nil
}

// Stats godoc
// @Summary      Prerender analytics of a project
// @Tags         prerender
// @Produce      json
// @Param        id    path   string  true   "Project ID"
// @Param        days  query  int     false  "Days to cover (default 14, max 90)"
// @Router       /api/v1/projects/{id}/prerender/stats [get]
func (h *PrerenderHandler) Stats(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	id, ok := projectFromRequest(ctx, c, h.db)
	if !ok {
		return
	}
	days, _ := strconv.Atoi(c.DefaultQuery("days", "14"))
	if days <= 0 || days > prerenderStatsMaxDays {
		days = 14
	}
	stats, err := buildPrerenderStats(ctx, h.db, id, days, time.Now().UTC())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to build prerender stats"})
		return
	}
	c.JSON(http.StatusOK, stats)
}

// Requests godoc
// @Summary      Prerender request log of a project
// @Tags         prerender
// @Produce      json
// @Param        id              path   string  true   "Project ID"
// @Param        requester_type  query  string  false  "human | bot | ai"
// @Param        cache           query  string  false  "hit | stale | miss | error"
// @Param        agent           query  string  false  "Crawler name, e.g. GPTBot"
// @Param        limit           query  int     false  "Page size (max 200)"
// @Param        offset          query  int     false  "Offset"
// @Router       /api/v1/projects/{id}/prerender/requests [get]
func (h *PrerenderHandler) Requests(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id, ok := projectFromRequest(ctx, c, h.db)
	if !ok {
		return
	}
	filter := bson.M{"project_id": id}
	if t := c.Query("requester_type"); t != "" {
		filter["requester_type"] = t
	}
	if v := c.Query("cache"); v != "" {
		filter["cache"] = v
	}
	if v := c.Query("agent"); v != "" {
		filter["agent"] = v
	}
	limit, _ := strconv.ParseInt(c.DefaultQuery("limit", "50"), 10, 64)
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	offset, _ := strconv.ParseInt(c.DefaultQuery("offset", "0"), 10, 64)
	if offset < 0 {
		offset = 0
	}
	col := h.db.Collection("prerender_data")
	total, err := col.CountDocuments(ctx, filter)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to count requests"})
		return
	}
	cursor, err := col.Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "cached_at", Value: -1}}).SetSkip(offset).SetLimit(limit))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch requests"})
		return
	}
	data := []models.PrerenderData{}
	if err = cursor.All(ctx, &data); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to decode requests"})
		return
	}
	c.JSON(http.StatusOK, models.PrerenderListResponse{Total: total, Limit: limit, Offset: offset, Data: data})
}

// cacheRow is a cache entry without its HTML, for lists.
type cacheRow struct {
	models.CacheEntry
	State string `json:"state"` // fresh | stale
}

// CacheList godoc
// @Summary      Cached renders of a project
// @Tags         prerender
// @Produce      json
// @Param        id     path   string  true   "Project ID"
// @Param        q      query  string  false  "URL contains"
// @Param        state  query  string  false  "fresh | stale | error"
// @Param        sort   query  string  false  "hits | recent | slow | url"
// @Param        page   query  int     false  "Page, from 1"
// @Param        size   query  int     false  "Page size (max 200)"
// @Router       /api/v1/projects/{id}/cache [get]
func (h *PrerenderHandler) CacheList(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id, ok := projectFromRequest(ctx, c, h.db)
	if !ok {
		return
	}
	now := time.Now().UTC()
	filter := bson.M{"project_id": id}
	switch c.Query("state") {
	case "fresh":
		filter["expires_at"] = bson.M{"$gt": now}
	case "stale":
		filter["expires_at"] = bson.M{"$lte": now}
	case "error":
		filter["status_code"] = bson.M{"$gte": 400}
	}
	if q := strings.TrimSpace(c.Query("q")); q != "" {
		filter["url"] = bson.M{"$regex": regexpQuote(q), "$options": "i"}
	}
	sortKey := bson.D{{Key: "hits", Value: -1}, {Key: "key", Value: 1}}
	switch c.Query("sort") {
	case "recent":
		sortKey = bson.D{{Key: "cached_at", Value: -1}}
	case "slow":
		sortKey = bson.D{{Key: "render_ms", Value: -1}}
	case "url":
		sortKey = bson.D{{Key: "key", Value: 1}}
	}
	size, _ := strconv.Atoi(c.DefaultQuery("size", "50"))
	if size <= 0 || size > 200 {
		size = 50
	}
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	if page < 1 {
		page = 1
	}
	col := h.db.Collection("cache_entries")
	total, err := col.CountDocuments(ctx, filter)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to count cache entries"})
		return
	}
	cursor, err := col.Find(ctx, filter, options.Find().SetSort(sortKey).SetSkip(int64((page-1)*size)).SetLimit(int64(size)).
		SetProjection(bson.M{"full_html": 0}))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch cache entries"})
		return
	}
	var entries []models.CacheEntry
	if err = cursor.All(ctx, &entries); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to decode cache entries"})
		return
	}
	rows := make([]cacheRow, 0, len(entries))
	for _, e := range entries {
		state := "fresh"
		if !e.ExpiresAt.After(now) {
			state = "stale"
		}
		rows = append(rows, cacheRow{CacheEntry: e, State: state})
	}
	summary, err := summarizeCache(ctx, h.db, id, now)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to summarize the cache"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"total": total, "page": page, "size": size, "items": rows, "summary": summary})
}

// CacheGet godoc
// @Summary      One cached render with what the site crawl knows of its page
// @Tags         prerender
// @Produce      json
// @Param        id   path  string  true  "Project ID"
// @Param        cid  path  string  true  "Cache entry ID"
// @Router       /api/v1/projects/{id}/cache/{cid} [get]
func (h *PrerenderHandler) CacheGet(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id, ok := projectFromRequest(ctx, c, h.db)
	if !ok {
		return
	}
	cid, err := bson.ObjectIDFromHex(c.Param("cid"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid cache entry id"})
		return
	}
	var entry models.CacheEntry
	if err = h.db.Collection("cache_entries").FindOne(ctx, bson.M{"_id": cid, "project_id": id}).Decode(&entry); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			c.JSON(http.StatusNotFound, gin.H{"error": "cache entry not found"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch cache entry"})
		}
		return
	}
	var profile *models.PageProfile
	var pr models.PageProfile
	if h.db.Collection("page_profiles").FindOne(ctx, bson.M{"project_id": id, "url": entry.Key, "version": bson.M{"$gte": 1}},
		options.FindOne().SetProjection(bson.M{"links": 0, "category_text": 0})).Decode(&pr) == nil {
		if pr.Issues == nil {
			pr.Issues = []models.PageIssue{}
		}
		profile = &pr
	}
	requests, _ := h.db.Collection("prerender_data").CountDocuments(ctx, bson.M{"project_id": id, "key": entry.Key})
	c.JSON(http.StatusOK, gin.H{"entry": entry, "profile": profile, "requests": requests})
}

// CacheRefresh godoc
// @Summary      Queue fresh renders of pages into the cache
// @Tags         prerender
// @Accept       json
// @Produce      json
// @Param        id    path  string  true  "Project ID"
// @Router       /api/v1/projects/{id}/cache/refresh [post]
func (h *PrerenderHandler) CacheRefresh(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	id, ok := projectFromRequest(ctx, c, h.db)
	if !ok {
		return
	}
	var body struct {
		URLs []string `json:"urls"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || len(body.URLs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "urls is required"})
		return
	}
	if len(body.URLs) > cacheRefreshMax {
		c.JSON(http.StatusBadRequest, gin.H{"error": "at most " + strconv.Itoa(cacheRefreshMax) + " URLs at once"})
		return
	}
	var project models.Project
	if err := h.db.Collection("projects").FindOne(ctx, bson.M{"_id": id}).Decode(&project); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch project"})
		return
	}
	projectHost := hostOf(project.URL)
	queued := 0
	for _, raw := range body.URLs {
		raw = strings.TrimSpace(raw)
		if hostOf(raw) == "" || hostOf(raw) != projectHost {
			c.JSON(http.StatusBadRequest, gin.H{"error": "not a page of this project: " + raw})
			return
		}
		if err := worker.EnqueueRender(ctx, h.db, h.asynqClient, id, raw, models.CacheSourceManual); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to queue " + raw})
			return
		}
		queued++
	}
	c.JSON(http.StatusAccepted, gin.H{"queued": queued})
}

// CacheDelete godoc
// @Summary      Remove a cached render
// @Tags         prerender
// @Param        id   path  string  true  "Project ID"
// @Param        cid  path  string  true  "Cache entry ID"
// @Router       /api/v1/projects/{id}/cache/{cid} [delete]
func (h *PrerenderHandler) CacheDelete(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id, ok := projectFromRequest(ctx, c, h.db)
	if !ok {
		return
	}
	cid, err := bson.ObjectIDFromHex(c.Param("cid"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid cache entry id"})
		return
	}
	var entry models.CacheEntry
	err = h.db.Collection("cache_entries").FindOneAndDelete(ctx, bson.M{"_id": cid, "project_id": id},
		options.FindOneAndDelete().SetProjection(bson.M{"url": 1})).Decode(&entry)
	if errors.Is(err, mongo.ErrNoDocuments) {
		c.JSON(http.StatusNotFound, gin.H{"error": "cache entry not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete cache entry"})
		return
	}
	var project models.Project
	if h.db.Collection("projects").FindOne(ctx, bson.M{"_id": id}).Decode(&project) == nil {
		_, _ = worker.PurgeCDN(ctx, h.db, &project, []string{entry.URL}, worker.CDNTriggerManual)
	}
	c.Status(http.StatusNoContent)
}

// RenderQueue godoc
// @Summary      Recent renders of a project
// @Tags         prerender
// @Produce      json
// @Param        id      path   string  true   "Project ID"
// @Param        status  query  string  false  "pending | processing | done | failed"
// @Router       /api/v1/projects/{id}/render-queue [get]
func (h *PrerenderHandler) RenderQueue(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id, ok := projectFromRequest(ctx, c, h.db)
	if !ok {
		return
	}
	filter := bson.M{"project_id": id}
	if s := c.Query("status"); s != "" {
		filter["status"] = s
	}
	cursor, err := h.db.Collection("crawl_queue").Find(ctx, filter,
		options.Find().SetSort(bson.D{{Key: "enqueued_at", Value: -1}}).SetLimit(200))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch the render queue"})
		return
	}
	entries := []models.QueueEntry{}
	if err = cursor.All(ctx, &entries); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to decode the render queue"})
		return
	}
	c.JSON(http.StatusOK, entries)
}

// hostOf returns a URL's lowercase host without www., or "".
func hostOf(raw string) string {
	key := worker.NormalizePageURL(raw)
	rest, ok := strings.CutPrefix(key, "https://")
	if !ok {
		return ""
	}
	host, _, _ := strings.Cut(rest, "/")
	host, _, _ = strings.Cut(host, "?")
	return host
}
