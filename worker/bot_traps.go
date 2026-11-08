package worker

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/creever/crawler-api/models"
)

const (
	botTrapWindow = 14 * 24 * time.Hour
	// botErrorMinRequests: a page bots asked for this often is no one-off.
	botErrorMinRequests = 3
	// botWasteMinRequests and botWasteMinShare keep a few stray requests from
	// becoming a task.
	botWasteMinRequests = 50
	botWasteMinShare    = 0.2
	// botTrapPagesListed caps the pages a task names.
	botTrapPagesListed = 50
)

// botPageStat is what bots asked of one page over the window.
type botPageStat struct {
	Key      string `bson:"_id"`
	URL      string `bson:"url"`
	Requests int    `bson:"n"`
	Status   int    `bson:"status"`
}

// botTraps is where bots spend requests for nothing.
type botTraps struct {
	Errors        []botPageStat
	ErrorRequests int
	Waste         []botPageStat
	WasteRequests int
	Total         int
}

// onlyPagination reports whether a query string only turns the page.
func onlyPagination(q url.Values) bool {
	for k := range q {
		switch strings.ToLower(k) {
		case "page", "p", "pg", "paged", "offset", "start":
		default:
			return false
		}
	}
	return true
}

// wastedOn reports why a working page is not worth a bot's request, or "".
func wastedOn(s botPageStat, prof *models.PageProfile) string {
	if u, err := url.Parse(s.URL); err == nil && u.RawQuery != "" && !onlyPagination(u.Query()) {
		// A query URL with search traffic of its own earns its requests.
		if prof == nil || prof.Impressions == 0 {
			return "query"
		}
	}
	if prof == nil {
		return ""
	}
	if prof.Noindex {
		return "noindex"
	}
	if prof.Canonical != "" && normalizePageURL(prof.Canonical) != prof.URL {
		return "canonical"
	}
	return ""
}

// findBotTraps sorts the pages bots asked for into errors and waste.
func findBotTraps(stats []botPageStat, profiles map[string]*models.PageProfile) botTraps {
	var t botTraps
	for _, s := range stats {
		t.Total += s.Requests
		if s.Status >= 400 {
			if s.Requests >= botErrorMinRequests {
				t.Errors = append(t.Errors, s)
				t.ErrorRequests += s.Requests
			}
			continue
		}
		if wastedOn(s, profiles[s.Key]) != "" {
			t.Waste = append(t.Waste, s)
			t.WasteRequests += s.Requests
		}
	}
	strongest := func(list []botPageStat) {
		sort.Slice(list, func(i, j int) bool {
			if list[i].Requests != list[j].Requests {
				return list[i].Requests > list[j].Requests
			}
			return list[i].Key < list[j].Key
		})
	}
	strongest(t.Errors)
	strongest(t.Waste)
	return t
}

func (t botTraps) wasteIsATask() bool {
	return t.WasteRequests >= botWasteMinRequests && float64(t.WasteRequests) >= botWasteMinShare*float64(t.Total)
}

func listedURLs(list []botPageStat) []string {
	urls := make([]string, 0, botTrapPagesListed)
	for i, s := range list {
		if i == botTrapPagesListed {
			break
		}
		urls = append(urls, s.URL)
	}
	return urls
}

func botErrorsTask(projectID bson.ObjectID, t botTraps) models.SEOTask {
	pages := listedURLs(t.Errors)
	return models.SEOTask{
		ProjectID: projectID, Key: taskKey(models.IssueBotErrors, "site"), Source: models.TaskSourceDetector,
		Issue: models.IssueBotErrors, Scope: models.TaskScopeSite, Confidence: "high", Language: defaultLanguage,
		Title:   "Bots keep asking for pages that fail",
		Problem: fmt.Sprintf("In the last 14 days bots asked for %d failing URLs %d times. These requests use up crawl budget, and each one shows the crawler a broken site.", len(t.Errors), t.ErrorRequests),
		Goal:    "Each URL answers usefully: a page that moved has a 301 to its new address, a page gone for good answers 410, and links or sitemap entries pointing at it are removed.",
		Avoid:   []string{"Do not redirect every failing URL to the home page: search engines treat that as a soft 404."},
		Pages:   pages, ConfirmedPages: pages,
		Metrics: models.TaskMetrics{Pages: len(t.Errors), PagesSampled: len(pages)},
	}
}

func botWasteTask(projectID bson.ObjectID, t botTraps) models.SEOTask {
	pages := listedURLs(t.Waste)
	share := float64(t.WasteRequests) / float64(t.Total) * 100
	return models.SEOTask{
		ProjectID: projectID, Key: taskKey(models.IssueBotWaste, "site"), Source: models.TaskSourceDetector,
		Issue: models.IssueBotWaste, Scope: models.TaskScopeSite, Confidence: "medium", Language: defaultLanguage,
		Title:   "Bots spend their crawl on pages that cannot rank",
		Problem: fmt.Sprintf("%.0f%% of the bot requests of the last 14 days (%d of %d) went to URLs that cannot rank: query-string variants without search traffic, noindex pages, or pages whose canonical points elsewhere.", share, t.WasteRequests, t.Total),
		Goal:    "Crawlers spend their requests on pages meant for search: internal links stop producing these variants, and patterns of no value are disallowed in robots.txt.",
		Avoid:   []string{"Do not disallow a URL that must drop out of the index: a crawler that cannot read it cannot see its noindex.", "Do not block query parameters that change what the page shows and earn search traffic."},
		Pages:   pages, ConfirmedPages: pages,
		Metrics: models.TaskMetrics{Pages: len(t.Waste), PagesSampled: len(pages)},
	}
}

// recordBotTraps keeps the bot trap tasks in line with the request log.
func (p *Processor) recordBotTraps(ctx context.Context, project *models.Project, now time.Time) error {
	cursor, err := p.db.Collection("prerender_data").Aggregate(ctx, mongo.Pipeline{
		{{Key: "$match", Value: bson.M{
			"project_id": project.ID, "cached_at": bson.M{"$gte": now.Add(-botTrapWindow)},
			"requester_type": bson.M{"$ne": models.RequesterTypeHuman}, "cache": bson.M{"$ne": models.CacheStateError},
		}}},
		{{Key: "$sort", Value: bson.M{"cached_at": 1}}},
		{{Key: "$group", Value: bson.M{
			"_id": bson.M{"$ifNull": bson.A{"$key", "$url"}}, "url": bson.M{"$last": "$url"},
			"n": bson.M{"$sum": 1}, "status": bson.M{"$last": "$status_code"},
		}}},
	})
	if err != nil {
		return err
	}
	var stats []botPageStat
	if err = cursor.All(ctx, &stats); err != nil {
		return err
	}
	keys := make(bson.A, 0, len(stats))
	for _, s := range stats {
		keys = append(keys, s.Key)
	}
	profiles := make(map[string]*models.PageProfile, len(stats))
	if len(keys) > 0 {
		pc, findErr := p.db.Collection("page_profiles").Find(ctx, bson.M{"project_id": project.ID, "url": bson.M{"$in": keys}, "version": bson.M{"$gte": profileVersion}},
			options.Find().SetProjection(bson.M{"url": 1, "noindex": 1, "canonical": 1, "impressions": 1}))
		if findErr != nil {
			return findErr
		}
		var rows []models.PageProfile
		if err = pc.All(ctx, &rows); err != nil {
			return err
		}
		for i := range rows {
			profiles[rows[i].URL] = &rows[i]
		}
	}
	traps := findBotTraps(stats, profiles)
	changed := false
	if len(traps.Errors) > 0 {
		if _, err = UpsertTask(ctx, p.db, botErrorsTask(project.ID, traps), now, now); err != nil {
			return err
		}
		changed = true
	} else if err = p.resolveSiteTask(ctx, project.ID, models.IssueBotErrors, now); err != nil {
		return err
	}
	if traps.wasteIsATask() {
		if _, err = UpsertTask(ctx, p.db, botWasteTask(project.ID, traps), now, now); err != nil {
			return err
		}
		changed = true
	} else if err = p.resolveSiteTask(ctx, project.ID, models.IssueBotWaste, now); err != nil {
		return err
	}
	if changed {
		return RebalanceTasks(ctx, p.db, project.ID)
	}
	return nil
}

// jsOnlyTask reports content clients without JavaScript cannot see.
func jsOnlyTask(projectID bson.ObjectID, r *models.RenderingProbe) models.SEOTask {
	pages := append([]string{}, r.Pages...)
	return models.SEOTask{
		ProjectID: projectID, Key: taskKey(models.IssueJSOnlyContent, "site"), Source: models.TaskSourceDetector,
		Issue: models.IssueJSOnlyContent, Scope: models.TaskScopeSite, Confidence: "high", Language: defaultLanguage,
		Title:   "The content is invisible without JavaScript",
		Problem: fmt.Sprintf("Without JavaScript the sampled pages show %d of %d words and %d of %d links, and no bot request has reached /prerender for 30 days. AI crawlers such as GPTBot and ClaudeBot run no JavaScript, so they see an almost empty site.", r.HTMLWords, r.JSWords, r.HTMLLinks, r.JSLinks),
		Goal:    "A client without JavaScript gets the page's content and links: render on the server, or send crawlers to the prerender endpoint.",
		Avoid:   []string{"Do not show crawlers content that visitors do not get: the prerendered page must match the live one."},
		Pages:   pages, ConfirmedPages: pages,
		Metrics: models.TaskMetrics{Pages: len(pages), PagesSampled: len(pages)},
	}
}

// recordJSDependence opens the task while the content needs JavaScript and
// nothing delivers it to bots. A site whose bots go through /prerender has
// solved the problem.
func (p *Processor) recordJSDependence(ctx context.Context, project *models.Project, r *models.RenderingProbe, now time.Time) error {
	if r == nil {
		return nil
	}
	served, err := p.db.Collection("prerender_data").CountDocuments(ctx, bson.M{
		"project_id": project.ID, "cached_at": bson.M{"$gte": now.Add(-prerenderInUseWindow)},
		"requester_type": bson.M{"$ne": models.RequesterTypeHuman},
	}, options.Count().SetLimit(1))
	if err != nil {
		return err
	}
	if !r.NeedsJS || served > 0 {
		return p.resolveSiteTask(ctx, project.ID, models.IssueJSOnlyContent, now)
	}
	if _, err = UpsertTask(ctx, p.db, jsOnlyTask(project.ID, r), now, now); err != nil {
		return err
	}
	return RebalanceTasks(ctx, p.db, project.ID)
}
