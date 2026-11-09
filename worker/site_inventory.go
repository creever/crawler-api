package worker

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/hibiken/asynq"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.uber.org/zap"

	"github.com/creever/crawler-api/models"
)

const (
	// siteCrawlBudget is how many pages one site crawl reads. A big site is
	// covered over several runs, the pages never read or read longest ago first.
	siteCrawlBudget = 500
	// siteRefreshAfter is how long a read page counts as current.
	siteRefreshAfter = 7 * 24 * time.Hour
	// siteCrawlTimeout bounds one run: 500 pages at a 4 s crawl delay, plus
	// rendering, take well under it.
	siteCrawlTimeout = 3 * time.Hour
	siteCrawlEvery   = 24 * time.Hour

	maxSitemapFiles  = 50
	maxSitemapURLs   = 50000
	maxSitemapBytes  = 50 << 20
	maxRobotsBytes   = 512 << 10
	siteURLWriteSize = 1000
)

// robotsRules is what robots.txt asks of one crawler.
type robotsRules struct {
	Delay    time.Duration
	Allow    []string
	Disallow []string
	Sitemaps []string
	// Raw is the file, so that other crawlers' rules can be read from it.
	Raw string
}

// parseRobots reads the "*" group and the sitemap lines of a robots.txt.
func parseRobots(body string) robotsRules {
	return parseRobotsFor(body, "*")
}

// robotsGroup is one group of a robots.txt.
type robotsGroup struct {
	agents          []string
	allow, disallow []string
}

func (g *robotsGroup) names(agent string) bool {
	for _, a := range g.agents {
		if strings.EqualFold(a, agent) {
			return true
		}
	}
	return false
}

// parseRobotsFor reads the rules that apply to one crawler: the groups naming
// it, or the "*" groups when none does.
func parseRobotsFor(body, agent string) robotsRules {
	rules := robotsRules{Delay: parseCrawlDelay(body), Raw: body}
	var groups []*robotsGroup
	var cur *robotsGroup
	lastWasAgent := false
	for _, line := range strings.Split(body, "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key, value = strings.ToLower(strings.TrimSpace(key)), strings.TrimSpace(value)
		switch key {
		case "user-agent":
			// Consecutive user-agent lines share one group.
			if !lastWasAgent || cur == nil {
				cur = &robotsGroup{}
				groups = append(groups, cur)
			}
			cur.agents = append(cur.agents, value)
			lastWasAgent = true
			continue
		case "sitemap":
			if value != "" {
				rules.Sitemaps = appendUnique(rules.Sitemaps, value)
			}
		case "allow":
			if cur != nil && value != "" {
				cur.allow = append(cur.allow, value)
			}
		case "disallow":
			if cur != nil && value != "" {
				cur.disallow = append(cur.disallow, value)
			}
		}
		lastWasAgent = false
	}
	named := false
	for _, g := range groups {
		named = named || g.names(agent)
	}
	target := agent
	if !named {
		target = "*"
	}
	for _, g := range groups {
		if g.names(target) {
			rules.Allow = append(rules.Allow, g.allow...)
			rules.Disallow = append(rules.Disallow, g.disallow...)
		}
	}
	return rules
}

// aiAgents are the robots.txt names of the crawlers behind AI assistants and
// AI training. Google-Extended and Applebot-Extended are permission tokens
// only: they never fetch a page themselves.
var aiAgents = []string{
	"GPTBot", "OAI-SearchBot", "ChatGPT-User", "ClaudeBot", "Claude-SearchBot", "Claude-User",
	"PerplexityBot", "Perplexity-User", "Google-Extended", "Applebot-Extended",
	"Meta-ExternalAgent", "Amazonbot", "Bytespider", "CCBot",
}

// aiAccess says which AI crawlers robots.txt lets at the site's home page.
func aiAccess(rules robotsRules, siteURL string) []models.AIAccess {
	out := make([]models.AIAccess, 0, len(aiAgents))
	for _, agent := range aiAgents {
		out = append(out, models.AIAccess{Agent: agent, Allowed: parseRobotsFor(rules.Raw, agent).allowed(siteURL)})
	}
	return out
}

// robotsMatch returns the length of the pattern if it matches path, else -1.
// Patterns support * and a closing $, as major search engines read them.
func robotsMatch(pattern, path string) int {
	expr := "^" + strings.ReplaceAll(regexp.QuoteMeta(pattern), `\*`, ".*")
	if strings.HasSuffix(expr, `\$`) {
		expr = strings.TrimSuffix(expr, `\$`) + "$"
	}
	re, err := regexp.Compile(expr)
	if err != nil || !re.MatchString(path) {
		return -1
	}
	return len(pattern)
}

// allowed applies the longest matching rule; on a tie, allow wins.
func (r robotsRules) allowed(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	if u.RawQuery != "" {
		path += "?" + u.RawQuery
	}
	best, allow := -1, true
	for _, p := range r.Disallow {
		if n := robotsMatch(p, path); n > best {
			best, allow = n, false
		}
	}
	for _, p := range r.Allow {
		if n := robotsMatch(p, path); n >= best && n >= 0 {
			best, allow = n, true
		}
	}
	return allow
}

// sitemapDoc covers both a urlset and a sitemap index.
type sitemapDoc struct {
	URLs []struct {
		Loc string `xml:"loc"`
	} `xml:"url"`
	Sitemaps []struct {
		Loc string `xml:"loc"`
	} `xml:"sitemap"`
}

// parseSitemap returns the page URLs and the child sitemaps of a sitemap,
// gzipped or not.
func parseSitemap(body []byte) (pages, children []string, err error) {
	if len(body) > 2 && body[0] == 0x1f && body[1] == 0x8b {
		zr, zErr := gzip.NewReader(bytes.NewReader(body))
		if zErr != nil {
			return nil, nil, zErr
		}
		if body, err = io.ReadAll(io.LimitReader(zr, maxSitemapBytes)); err != nil {
			return nil, nil, err
		}
	}
	var doc sitemapDoc
	if err = xml.Unmarshal(body, &doc); err != nil {
		return nil, nil, err
	}
	for _, u := range doc.URLs {
		if loc := strings.TrimSpace(u.Loc); loc != "" {
			pages = append(pages, loc)
		}
	}
	for _, s := range doc.Sitemaps {
		if loc := strings.TrimSpace(s.Loc); loc != "" {
			children = append(children, loc)
		}
	}
	return pages, children, nil
}

func (p *Processor) fetchBytes(ctx context.Context, rawURL string, limit int64) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close() //nolint:errcheck
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	return body, resp.StatusCode, err
}

func (p *Processor) siteRobots(ctx context.Context, siteURL string) robotsRules {
	u, err := url.Parse(siteURL)
	if err != nil || u.Host == "" {
		return robotsRules{Delay: defaultCrawlDelay}
	}
	body, status, err := p.fetchBytes(ctx, (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: "/robots.txt"}).String(), maxRobotsBytes)
	if err != nil || status != http.StatusOK {
		return robotsRules{Delay: defaultCrawlDelay}
	}
	return parseRobots(string(body))
}

// loadSitemaps walks the sitemaps named in robots.txt, or /sitemap.xml when
// none is named, through sitemap indexes. ok is false when no sitemap could
// be read, so sitemap membership is left as it was.
func (p *Processor) loadSitemaps(ctx context.Context, siteURL string, named []string) (pages []string, ok bool) {
	queue := named
	if len(queue) == 0 {
		if u, err := url.Parse(siteURL); err == nil && u.Host != "" {
			queue = []string{(&url.URL{Scheme: u.Scheme, Host: u.Host, Path: "/sitemap.xml"}).String()}
		}
	}
	seen := make(map[string]bool)
	for files := 0; len(queue) > 0 && files < maxSitemapFiles && len(pages) < maxSitemapURLs; files++ {
		next := queue[0]
		queue = queue[1:]
		if seen[next] {
			continue
		}
		seen[next] = true
		body, status, err := p.fetchBytes(ctx, next, maxSitemapBytes)
		if err != nil || status != http.StatusOK {
			p.logger.Info("sitemap not read", zap.String("url", next), zap.Int("status", status), zap.Error(err))
			continue
		}
		found, children, err := parseSitemap(body)
		if err != nil {
			p.logger.Info("sitemap not parsed", zap.String("url", next), zap.Error(err))
			continue
		}
		ok = true
		pages = append(pages, found...)
		queue = append(queue, children...)
	}
	if len(pages) > maxSitemapURLs {
		pages = pages[:maxSitemapURLs]
	}
	return pages, ok
}

// recordURLs adds URLs to the project's inventory under a source.
func (p *Processor) recordURLs(ctx context.Context, projectOID bson.ObjectID, urls []string, source string, now time.Time) error {
	var writes []mongo.WriteModel
	seen := make(map[string]bool)
	for _, raw := range urls {
		key := normalizePageURL(raw)
		if seen[key] {
			continue
		}
		seen[key] = true
		set := bson.M{}
		if source == models.URLSourceSitemap {
			set = bson.M{"in_sitemap": true, "sitemap_seen_at": now}
		}
		update := bson.M{
			"$addToSet":    bson.M{"sources": source},
			"$setOnInsert": bson.M{"source_url": raw, "discovered_at": now, "disallowed": false},
		}
		if len(set) > 0 {
			update["$set"] = set
		} else {
			update["$setOnInsert"].(bson.M)["in_sitemap"] = false
		}
		writes = append(writes, mongo.NewUpdateOneModel().
			SetFilter(bson.M{"project_id": projectOID, "url": key}).SetUpdate(update).SetUpsert(true))
	}
	for start := 0; start < len(writes); start += siteURLWriteSize {
		end := min(start+siteURLWriteSize, len(writes))
		if _, err := p.db.Collection("site_urls").BulkWrite(ctx, writes[start:end], options.BulkWrite().SetOrdered(false)); err != nil {
			return err
		}
	}
	return nil
}

// orderFrontier lists the URLs to read, best first: never read before read
// long ago, and among the unread, those with search traffic, then those in
// the sitemap. pickSample later spreads the budget over the site's sections.
func orderFrontier(urls []models.SiteURL, impressions map[string]float64) []string {
	sorted := append([]models.SiteURL(nil), urls...)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if (a.LastFetchedAt == nil) != (b.LastFetchedAt == nil) {
			return a.LastFetchedAt == nil
		}
		if a.LastFetchedAt != nil {
			return a.LastFetchedAt.Before(*b.LastFetchedAt)
		}
		if impressions[a.URL] != impressions[b.URL] {
			return impressions[a.URL] > impressions[b.URL]
		}
		if a.InSitemap != b.InSitemap {
			return a.InSitemap
		}
		return a.URL < b.URL
	})
	out := make([]string, 0, len(sorted))
	for _, u := range sorted {
		out = append(out, u.SourceURL)
	}
	return out
}

// trafficShare is the part of the budget kept for pages with search traffic.
// They carry the score; spreading the whole budget over the site's sections
// would leave them unread on a site with thousands of URLs.
const trafficShare = 0.8

// planCrawl picks the pages of one run: those with search traffic first, by
// impressions, then the rest spread over the site's sections.
func planCrawl(urls []models.SiteURL, impressions map[string]float64, fresh map[string]bool, budget int) []string {
	ordered := orderFrontier(urls, impressions)
	var plan []string
	taken := make(map[string]bool)
	byTraffic := append([]models.SiteURL(nil), urls...)
	sort.SliceStable(byTraffic, func(i, j int) bool { return impressions[byTraffic[i].URL] > impressions[byTraffic[j].URL] })
	limit := int(trafficShare * float64(budget))
	for _, u := range byTraffic {
		if len(plan) >= limit || impressions[u.URL] <= 0 {
			break
		}
		if !fresh[u.URL] {
			plan = append(plan, u.SourceURL)
			taken[u.URL] = true
		}
	}
	skip := make(map[string]bool, len(fresh)+len(taken))
	for k := range fresh {
		skip[k] = true
	}
	for k := range taken {
		skip[k] = true
	}
	return append(plan, pickSample(ordered, skip, budget-len(plan))...)
}

func (p *Processor) loadSiteURLs(ctx context.Context, projectOID bson.ObjectID) ([]models.SiteURL, error) {
	cursor, err := p.db.Collection("site_urls").Find(ctx, bson.M{"project_id": projectOID})
	if err != nil {
		return nil, err
	}
	var urls []models.SiteURL
	if err = cursor.All(ctx, &urls); err != nil {
		return nil, err
	}
	return urls, nil
}

// SiteCrawlPayload is the JSON payload for a site:crawl task.
type SiteCrawlPayload struct {
	ProjectID string `json:"project_id"`
}

// NewSiteCrawlTask creates the asynq task reading a project's site. One run
// per project at a time.
func NewSiteCrawlTask(projectID string) (*asynq.Task, error) {
	body, err := json.Marshal(SiteCrawlPayload{ProjectID: projectID})
	if err != nil {
		return nil, fmt.Errorf("marshal site:crawl payload: %w", err)
	}
	return asynq.NewTask(TypeSiteCrawl, body, asynq.Timeout(siteCrawlTimeout), asynq.MaxRetry(1), asynq.Unique(siteCrawlTimeout)), nil
}

// notFoundProbe asks for a URL that cannot exist. Only an error status says the
// page is missing; anything else lets broken links and removed pages pose as
// real pages in search.
func (p *Processor) notFoundProbe(ctx context.Context, project *models.Project, pacer *renderPacer) (*models.NotFoundProbe, error) {
	u, err := url.Parse(project.URL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("invalid project URL")
	}
	probe := (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: "/" + notFoundPath + "-" + bson.NewObjectID().Hex()}).String()
	if err = pacer.wait(ctx); err != nil {
		return nil, err
	}
	page, err := p.fetchPage(ctx, probe, project.UseJS)
	if err != nil {
		return nil, err
	}
	return &models.NotFoundProbe{URL: probe, Status: page.Status, FinalURL: page.FinalURL, Soft: page.Status < 400}, nil
}

// notFoundPath names the probe so it is easy to recognize in the site's logs.
const notFoundPath = "crawler-not-found-check"

// softNotFoundTask is the task of a site that hides missing pages, or nil.
func softNotFoundTask(projectOID bson.ObjectID, probe *models.NotFoundProbe) models.SEOTask {
	what := fmt.Sprintf("answers with HTTP %d", probe.Status)
	if normalizePageURL(probe.FinalURL) != normalizePageURL(probe.URL) {
		what = fmt.Sprintf("redirects to %s, which answers with HTTP %d", probe.FinalURL, probe.Status)
	}
	return models.SEOTask{
		ProjectID: projectOID, Key: taskKey(models.IssueSoft404, "site"), Source: models.TaskSourceDetector,
		Issue: models.IssueSoft404, Scope: models.TaskScopeSite, Confidence: "high", Language: defaultLanguage,
		Title:   "Missing pages do not answer 404",
		Problem: fmt.Sprintf("A URL that cannot exist (%s) %s. Removed pages, mistyped links and broken internal links then look like real pages to search engines, and they can be indexed as duplicates of the page they land on.", probe.URL, what),
		Goal:    "Any URL without a page answers 404, or 410 for a page removed for good, with a helpful error page. Pages that moved keep their own 301 to the new address.",
		Avoid:   []string{"Do not redirect all unknown URLs to the home page: search engines treat that as a soft 404 too."},
		Pages:   []string{probe.URL}, ConfirmedPages: []string{probe.URL},
		Metrics: models.TaskMetrics{Pages: 1, PagesSampled: 1},
		Checks:  []models.TaskCheck{{Type: models.CheckStatusError, Page: probe.URL}},
	}
}

// HandleSiteCrawl reads a project's site within the budget, then rebuilds its
// link graph, audit and pattern tasks.
func (p *Processor) HandleSiteCrawl(ctx context.Context, t *asynq.Task) error {
	var payload SiteCrawlPayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		return fmt.Errorf("%w: unmarshal site:crawl payload: %v", asynq.SkipRetry, err)
	}
	projectOID, err := bson.ObjectIDFromHex(payload.ProjectID)
	if err != nil {
		return fmt.Errorf("%w: invalid project_id: %v", asynq.SkipRetry, err)
	}
	var project models.Project
	if err = p.db.Collection("projects").FindOne(ctx, bson.M{"_id": projectOID}).Decode(&project); err != nil {
		return fmt.Errorf("%w: project not found: %v", asynq.SkipRetry, err)
	}

	audit := models.SiteAudit{ID: bson.NewObjectID(), ProjectID: projectOID, Status: "running", StartedAt: time.Now().UTC()}
	if _, err = p.db.Collection("site_audits").InsertOne(ctx, audit); err != nil {
		return err
	}
	run, crawlErr := p.crawlSite(ctx, &project, audit.StartedAt)
	audit.AIAccess = run.AIAccess
	audit.PagesReadNow = run.Read
	audit.NotFound = run.NotFound
	audit.Rendering = run.Rendering
	probe := run.NotFound
	if crawlErr == nil {
		crawlErr = p.auditSite(ctx, &project, &audit)
	}
	finished := time.Now().UTC()
	audit.FinishedAt = &finished
	audit.Status = "done"
	if crawlErr != nil {
		audit.Status, audit.Error = "failed", crawlErr.Error()
	}
	if _, err = p.db.Collection("site_audits").ReplaceOne(context.WithoutCancel(ctx), bson.M{"_id": audit.ID}, audit); err != nil {
		return err
	}
	if crawlErr != nil {
		return crawlErr
	}
	if err = p.refreshSiteTasks(ctx, &project, finished); err != nil {
		p.logger.Warn("site tasks not refreshed", zap.String("project_id", payload.ProjectID), zap.Error(err))
	}
	if err = p.recordNotFoundProbe(ctx, &project, probe, finished); err != nil {
		p.logger.Warn("soft 404 task not updated", zap.String("project_id", payload.ProjectID), zap.Error(err))
	}
	if err = p.recordJSDependence(ctx, &project, run.Rendering, finished); err != nil {
		p.logger.Warn("JavaScript task not updated", zap.String("project_id", payload.ProjectID), zap.Error(err))
	}
	if err = p.recordBotTraps(ctx, &project, finished); err != nil {
		p.logger.Warn("bot trap tasks not updated", zap.String("project_id", payload.ProjectID), zap.Error(err))
	}
	p.enqueueVitalsIfDue(ctx, project.ID, finished)
	p.logger.Info("site crawl completed", zap.String("project_id", payload.ProjectID), zap.Int("pages_read", run.Read),
		zap.Float64("score", audit.Score), zap.Int("pages", audit.PagesFetched))
	return nil
}

// recordNotFoundProbe keeps the soft 404 task in line with the latest probe: a
// soft answer opens or updates it, a real 404 closes an open one.
func (p *Processor) recordNotFoundProbe(ctx context.Context, project *models.Project, probe *models.NotFoundProbe, now time.Time) error {
	if probe == nil {
		return nil
	}
	if probe.Soft {
		if _, err := UpsertTask(ctx, p.db, softNotFoundTask(project.ID, probe), now, now); err != nil {
			return err
		}
		return RebalanceTasks(ctx, p.db, project.ID)
	}
	return p.resolveSiteTask(ctx, project.ID, models.IssueSoft404, now)
}

// resolveSiteTask closes a site-wide task whose problem is gone, unless
// someone is already working on it.
func (p *Processor) resolveSiteTask(ctx context.Context, projectID bson.ObjectID, issue string, now time.Time) error {
	_, err := p.db.Collection("seo_tasks").UpdateMany(ctx, bson.M{
		"project_id": projectID, "key": taskKey(issue, "site"),
		"status": bson.M{"$in": []models.TaskStatus{models.TaskStatusQueued, models.TaskStatusOpen, models.TaskStatusReopened}},
	}, bson.M{"$set": bson.M{"status": models.TaskStatusResolved, "updated_at": now}})
	return err
}

// siteCrawlRun is what a crawl did besides storing the pages it read.
type siteCrawlRun struct {
	Read      int
	NotFound  *models.NotFoundProbe
	Rendering *models.RenderingProbe
	AIAccess  []models.AIAccess
}

// crawlSite gathers the site's URLs and reads up to the budget. Links found on
// the pages read join the queue, so a site without a sitemap is still walked.
func (p *Processor) crawlSite(ctx context.Context, project *models.Project, now time.Time) (siteCrawlRun, error) {
	var run siteCrawlRun
	rules := p.siteRobots(ctx, project.URL)
	run.AIAccess = aiAccess(rules, project.URL)
	sitemapPages, sitemapOK := p.loadSitemaps(ctx, project.URL, rules.Sitemaps)
	if err := p.recordURLs(ctx, project.ID, sitemapPages, models.URLSourceSitemap, now); err != nil {
		return run, fmt.Errorf("record sitemap URLs: %w", err)
	}
	if sitemapOK {
		if _, err := p.db.Collection("site_urls").UpdateMany(ctx,
			bson.M{"project_id": project.ID, "in_sitemap": true, "sitemap_seen_at": bson.M{"$lt": now}},
			bson.M{"$set": bson.M{"in_sitemap": false}}); err != nil {
			return run, err
		}
	}
	if err := p.recordURLs(ctx, project.ID, []string{project.URL}, models.URLSourceSeed, now); err != nil {
		return run, err
	}
	known, err := p.siteCandidates(ctx, project)
	if err != nil {
		return run, err
	}
	if err = p.recordURLs(ctx, project.ID, known, models.URLSourceGSC, now); err != nil {
		return run, err
	}
	botPages, err := p.botRequestedURLs(ctx, project, now)
	if err != nil {
		return run, err
	}
	if err = p.recordURLs(ctx, project.ID, botPages, models.URLSourceBot, now); err != nil {
		return run, err
	}

	urls, err := p.loadSiteURLs(ctx, project.ID)
	if err != nil {
		return run, err
	}
	curStart, end, _, _ := analysisWindows()
	totals, err := p.gscPageTotals(ctx, project.ID, curStart, end, 0)
	if err != nil {
		return run, err
	}
	impressions := make(map[string]float64, len(totals))
	for _, r := range totals {
		impressions[normalizePageURL(r.Page)] += r.Impressions
	}
	knownURL := make(map[string]bool, len(urls))
	fresh := make(map[string]bool)
	var eligible []models.SiteURL
	var blocked []string
	for _, u := range urls {
		knownURL[u.URL] = true
		if !sampleableURL(u.SourceURL, project.URL) {
			continue
		}
		if !rules.allowed(u.SourceURL) {
			blocked = append(blocked, u.URL)
			continue
		}
		if u.LastFetchedAt != nil && now.Sub(*u.LastFetchedAt) < siteRefreshAfter {
			fresh[u.URL] = true
		}
		eligible = append(eligible, u)
	}
	if len(blocked) > 0 {
		if _, err = p.db.Collection("site_urls").UpdateMany(ctx, bson.M{"project_id": project.ID, "url": bson.M{"$in": blocked}},
			bson.M{"$set": bson.M{"disallowed": true}}); err != nil {
			return run, err
		}
	}

	queue := planCrawl(eligible, impressions, fresh, siteCrawlBudget)
	queued := make(map[string]bool, len(queue))
	for _, q := range queue {
		queued[normalizePageURL(q)] = true
	}
	pacer := &renderPacer{delay: rules.Delay, remaining: siteCrawlBudget + 1 + 2*renderingProbePages}
	run.NotFound, err = p.notFoundProbe(ctx, project, pacer)
	if err != nil {
		p.logger.Info("site crawl: 404 probe failed", zap.String("project_id", project.ID.Hex()), zap.Error(err))
	}
	run.Rendering = p.renderingProbe(ctx, project, queue, pacer)
	useJS := project.UseJS
	if run.Rendering != nil {
		useJS = run.Rendering.Mode == renderModeJS
	}
	var warm map[string]bool
	if useJS {
		warm = p.warmSet(ctx, project, impressions, now)
	}
	for len(queue) > 0 {
		page := queue[0]
		queue = queue[1:]
		snap, readErr := p.readPageThen(ctx, project.ID, page, useJS, pacer, func(rendered *renderedPage) {
			if warm[normalizePageURL(page)] {
				p.warmCache(ctx, project.ID, page, rendered)
			}
		})
		if errors.Is(readErr, errRenderBudget) || ctx.Err() != nil {
			break
		}
		at := time.Now().UTC()
		set := bson.M{"last_fetched_at": at, "last_error": ""}
		if readErr != nil {
			set["last_error"] = readErr.Error()
			p.logger.Info("site crawl: page not read", zap.String("page", page), zap.Error(readErr))
		}
		if _, err = p.db.Collection("site_urls").UpdateOne(ctx, bson.M{"project_id": project.ID, "url": normalizePageURL(page)},
			bson.M{"$set": set}); err != nil {
			return run, err
		}
		if readErr != nil {
			continue
		}
		run.Read++
		var found []string
		for _, link := range snap.Links {
			if !sampleableURL(link, project.URL) || !rules.allowed(link) {
				continue
			}
			found = append(found, link)
			key := normalizePageURL(link)
			if !knownURL[key] && !queued[key] {
				queued[key] = true
				queue = append(queue, link)
			}
			knownURL[key] = true
		}
		if err = p.recordURLs(ctx, project.ID, found, models.URLSourceLink, at); err != nil {
			return run, err
		}
	}
	return run, ctx.Err()
}

const (
	// renderingProbePages is how many pages are read both with and without
	// JavaScript to tell whether the site needs it.
	renderingProbePages = 3
	// renderingWordsShare: plain HTML with less than this share of the
	// rendered words needs JavaScript.
	renderingWordsShare = 0.7
	// renderingLinksShare is lower: a menu built by a script adds many links
	// to a page whose content is all in the HTML, and that should not send a
	// whole crawl through Chrome.
	renderingLinksShare = 0.5
	renderModeJS        = "js"
	renderModeHTML      = "html"
)

// renderingProbe reads the home page and the first pages of the queue with
// and without JavaScript. A site that needs JavaScript is crawled with it
// whatever the project says. Otherwise the plain HTML carries the content and
// the site is crawled without Chrome, which is several times cheaper, unless
// the project asks for JavaScript. Nil when the probe could not read a page
// both ways.
func (p *Processor) renderingProbe(ctx context.Context, project *models.Project, queue []string, pacer *renderPacer) *models.RenderingProbe {
	pages := []string{project.URL}
	for _, q := range queue {
		if len(pages) >= renderingProbePages {
			break
		}
		if normalizePageURL(q) != normalizePageURL(project.URL) {
			pages = append(pages, q)
		}
	}
	probe := &models.RenderingProbe{}
	for _, page := range pages {
		plain, rendered, err := p.readBothWays(ctx, page, pacer)
		if err != nil {
			p.logger.Info("site crawl: rendering probe skipped a page", zap.String("page", page), zap.Error(err))
			continue
		}
		probe.Pages = append(probe.Pages, page)
		probe.Samples = append(probe.Samples, models.RenderingSample{
			URL: page, Title: rendered.Title,
			HTMLWords: plain.TotalWordCount, JSWords: rendered.TotalWordCount,
			HTMLLinks: len(plain.Links), JSLinks: len(rendered.Links),
		})
		probe.HTMLWords += plain.TotalWordCount
		probe.JSWords += rendered.TotalWordCount
		probe.HTMLLinks += len(plain.Links)
		probe.JSLinks += len(rendered.Links)
	}
	if len(probe.Pages) == 0 {
		return nil
	}
	probe.NeedsJS = needsJS(probe)
	probe.Mode = crawlMode(project.UseJS, probe.NeedsJS)
	return probe
}

// crawlMode is how a site is crawled: the probe can only add JavaScript to a
// project that does not ask for it, never take it away.
func crawlMode(projectUsesJS, needsJS bool) string {
	if projectUsesJS || needsJS {
		return renderModeJS
	}
	return renderModeHTML
}

// needsJS reports whether the plain HTML clearly lacks the rendered content.
func needsJS(r *models.RenderingProbe) bool {
	short := func(plain, rendered int, share float64) bool {
		return rendered > 0 && float64(plain) < share*float64(rendered)
	}
	return short(r.HTMLWords, r.JSWords, renderingWordsShare) || short(r.HTMLLinks, r.JSLinks, renderingLinksShare)
}

// readBothWays fetches a page without and with JavaScript, without storing.
func (p *Processor) readBothWays(ctx context.Context, page string, pacer *renderPacer) (plain, rendered *models.PageSnapshot, err error) {
	for _, js := range []bool{false, true} {
		if err = pacer.wait(ctx); err != nil {
			return nil, nil, err
		}
		got, fetchErr := p.fetchPage(ctx, page, js)
		if fetchErr != nil {
			return nil, nil, fetchErr
		}
		if got.Status >= 400 {
			return nil, nil, fmt.Errorf("HTTP %d", got.Status)
		}
		snap, extractErr := extractSnapshot(page, got.Status, got.HTML)
		if extractErr != nil {
			return nil, nil, extractErr
		}
		if js {
			rendered = snap
		} else {
			plain = snap
		}
	}
	return plain, rendered, nil
}

// StartSiteCrawlScheduler starts the daily site crawl of active projects.
func StartSiteCrawlScheduler(db *mongo.Database, asynqClient *asynq.Client, logger *zap.Logger) {
	ticker := time.NewTicker(1 * time.Hour)
	go func() {
		for range ticker.C {
			enqueueSiteCrawls(db, asynqClient, logger)
		}
	}()
	logger.Info("site crawl scheduler started")
}

func enqueueSiteCrawls(db *mongo.Database, asynqClient *asynq.Client, logger *zap.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cursor, err := db.Collection("projects").Find(ctx, bson.M{"active": true}, options.Find().SetProjection(bson.M{"_id": 1}))
	if err != nil {
		logger.Error("site crawl scheduler: failed to list projects", zap.Error(err))
		return
	}
	var projects []struct {
		ID bson.ObjectID `bson:"_id"`
	}
	if err = cursor.All(ctx, &projects); err != nil {
		logger.Error("site crawl scheduler: failed to decode projects", zap.Error(err))
		return
	}
	for _, pr := range projects {
		recent, countErr := db.Collection("site_audits").CountDocuments(ctx, bson.M{
			"project_id": pr.ID, "started_at": bson.M{"$gte": time.Now().UTC().Add(-siteCrawlEvery)},
		})
		if countErr != nil || recent > 0 {
			continue
		}
		task, taskErr := NewSiteCrawlTask(pr.ID.Hex())
		if taskErr != nil {
			continue
		}
		if _, err = asynqClient.EnqueueContext(ctx, task); err != nil && !errors.Is(err, asynq.ErrDuplicateTask) {
			logger.Error("site crawl scheduler: failed to enqueue", zap.String("project_id", pr.ID.Hex()), zap.Error(err))
		}
	}
}
