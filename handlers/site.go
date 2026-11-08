package handlers

import (
	"context"
	"errors"
	"net/http"
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
	siteHistoryRuns = 30
	sitePageSizeMax = 100
)

// SiteHandler serves the whole-site view: audit, pages and crawl runs.
type SiteHandler struct {
	db          *mongo.Database
	asynqClient *asynq.Client
	inspector   *asynq.Inspector
}

// NewSiteHandler creates a new SiteHandler.
func NewSiteHandler(db *mongo.Database, asynqClient *asynq.Client, inspector *asynq.Inspector) *SiteHandler {
	return &SiteHandler{db: db, asynqClient: asynqClient, inspector: inspector}
}

type siteIssue struct {
	Code        string    `json:"code"`
	Severity    string    `json:"severity"`
	Pages       int       `json:"pages"`
	Impressions float64   `json:"impressions"`
	Tasks       []taskRef `json:"tasks"`
}

type siteHistoryPoint struct {
	StartedAt    time.Time `json:"started_at"`
	Score        float64   `json:"score"`
	Critical     int       `json:"critical"`
	Warning      int       `json:"warning"`
	PagesFetched int       `json:"pages_fetched"`
}

type siteGroup struct {
	models.SiteAuditGroup
	OpenTasks int `json:"open_tasks"`
}

type siteOverview struct {
	Audit   *models.SiteAudit  `json:"audit"`
	Running bool               `json:"running"`
	History []siteHistoryPoint `json:"history"`
	Issues  []siteIssue        `json:"issues"`
	Groups  []siteGroup        `json:"groups"`
	Tasks   map[string]int     `json:"tasks"`  // by status
	Impact  map[string]int     `json:"impact"` // by verdict
}

// Overview godoc
// @Summary      The whole-site picture: score, coverage, issues, page types and work
// @Tags         projects
// @Produce      json
// @Param        id  path  string  true  "Project ID"
// @Router       /api/v1/projects/{id}/site/overview [get]
func (h *SiteHandler) Overview(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	id, ok := projectFromRequest(ctx, c, h.db)
	if !ok {
		return
	}
	out := siteOverview{History: []siteHistoryPoint{}, Issues: []siteIssue{}, Groups: []siteGroup{}, Tasks: map[string]int{}, Impact: map[string]int{}}
	var err error
	if out.Audit, err = worker.LatestAudit(ctx, h.db, id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch audit"})
		return
	}
	out.Running = h.crawlRunning(ctx, id)

	cursor, err := h.db.Collection("site_audits").Find(ctx, bson.M{"project_id": id, "status": "done"},
		options.Find().SetSort(bson.D{{Key: "started_at", Value: -1}}).SetLimit(siteHistoryRuns))
	if err == nil {
		var audits []models.SiteAudit
		if err = cursor.All(ctx, &audits); err == nil {
			for i := len(audits) - 1; i >= 0; i-- {
				a := audits[i]
				out.History = append(out.History, siteHistoryPoint{
					StartedAt: a.StartedAt, Score: a.Score, PagesFetched: a.PagesFetched,
					Critical: a.IssuesBySeverity[models.SeverityCritical], Warning: a.IssuesBySeverity[models.SeverityWarning],
				})
			}
		}
	}

	var tasks []models.SEOTask
	tc, err := h.db.Collection("seo_tasks").Find(ctx, bson.M{"project_id": id},
		options.Find().SetProjection(bson.M{"title": 1, "status": 1, "scope": 1, "issue": 1, "page_group": 1, "metrics": 1, "measurement.verdict": 1}))
	if err == nil {
		_ = tc.All(ctx, &tasks)
	}
	openByGroup := map[string]int{}
	tasksByIssue := map[string][]taskRef{}
	for _, t := range tasks {
		out.Tasks[string(t.Status)]++
		if t.Measurement != nil && t.Measurement.Verdict != "" {
			out.Impact[t.Measurement.Verdict]++
		}
		if t.IsActive() {
			openByGroup[t.PageGroup]++
		}
		if t.Status != models.TaskStatusResolved && t.Status != models.TaskStatusDismissed && len(tasksByIssue[t.Issue]) < 3 {
			tasksByIssue[t.Issue] = append(tasksByIssue[t.Issue], taskRef{ID: t.ID, Title: t.Title, Status: t.Status, Scope: t.Scope, Pages: t.Metrics.Pages})
		}
	}

	ic, err := h.db.Collection("page_profiles").Aggregate(ctx, bson.A{
		bson.M{"$match": bson.M{"project_id": id}},
		bson.M{"$unwind": "$issues"},
		bson.M{"$group": bson.M{"_id": bson.M{"code": "$issues.code", "severity": "$issues.severity"},
			"pages": bson.M{"$sum": 1}, "impressions": bson.M{"$sum": "$impressions"}}},
	})
	if err == nil {
		var rows []struct {
			ID struct {
				Code     string `bson:"code"`
				Severity string `bson:"severity"`
			} `bson:"_id"`
			Pages       int     `bson:"pages"`
			Impressions float64 `bson:"impressions"`
		}
		if err = ic.All(ctx, &rows); err == nil {
			for _, r := range rows {
				refs := tasksByIssue[worker.AuditIssueTask[r.ID.Code]]
				if refs == nil {
					refs = []taskRef{}
				}
				out.Issues = append(out.Issues, siteIssue{Code: r.ID.Code, Severity: r.ID.Severity, Pages: r.Pages, Impressions: r.Impressions, Tasks: refs})
			}
		}
	}
	rank := map[string]int{models.SeverityCritical: 0, models.SeverityWarning: 1, models.SeverityNotice: 2}
	sortIssues(out.Issues, rank)

	if out.Audit != nil {
		for _, g := range out.Audit.Groups {
			out.Groups = append(out.Groups, siteGroup{SiteAuditGroup: g, OpenTasks: openByGroup[g.Group]})
		}
	}
	c.JSON(http.StatusOK, out)
}

func sortIssues(list []siteIssue, rank map[string]int) {
	for i := 1; i < len(list); i++ {
		for j := i; j > 0; j-- {
			a, b := list[j-1], list[j]
			if rank[a.Severity] < rank[b.Severity] ||
				(rank[a.Severity] == rank[b.Severity] && (a.Impressions > b.Impressions || (a.Impressions == b.Impressions && a.Pages >= b.Pages))) {
				break
			}
			list[j-1], list[j] = b, a
		}
	}
}

func (h *SiteHandler) crawlRunning(ctx context.Context, id bson.ObjectID) bool {
	n, err := h.db.Collection("site_audits").CountDocuments(ctx, bson.M{
		"project_id": id, "status": "running", "started_at": bson.M{"$gte": time.Now().UTC().Add(-4 * time.Hour)},
	})
	if err == nil && n > 0 {
		return true
	}
	if h.inspector == nil {
		return false
	}
	if pending, pErr := h.inspector.ListPendingTasks("default"); pErr == nil {
		for _, t := range pending {
			if t.Type == worker.TypeSiteCrawl && payloadHasProject(t.Payload, id.Hex()) {
				return true
			}
		}
	}
	return false
}

type sitePageRow struct {
	URL         string             `json:"url"`
	SourceURL   string             `json:"source_url"`
	Kind        string             `json:"kind"`
	Group       string             `json:"group"`
	StatusCode  int                `json:"status_code"`
	Title       string             `json:"title"`
	Noindex     bool               `json:"noindex"`
	InSitemap   bool               `json:"in_sitemap"`
	Inbound     int                `json:"inbound"`
	Depth       int                `json:"depth"`
	Impressions float64            `json:"impressions"`
	Clicks      float64            `json:"clicks"`
	Issues      []models.PageIssue `json:"issues"`
	FetchedAt   time.Time          `json:"fetched_at"`
}

// Pages godoc
// @Summary      Pages read on the site, filtered and sorted
// @Tags         projects
// @Produce      json
// @Param        id        path   string  true   "Project ID"
// @Param        group     query  string  false  "Page type"
// @Param        issue     query  string  false  "Audit issue code"
// @Param        severity  query  string  false  "critical, warning or notice"
// @Param        state     query  string  false  "ok, error, redirect or noindex"
// @Param        q         query  string  false  "Text in the URL or title"
// @Param        sort      query  string  false  "impressions (default), clicks, depth, inbound or url"
// @Param        page      query  int     false  "1-based page"
// @Param        size      query  int     false  "Rows per page, at most 100"
// @Router       /api/v1/projects/{id}/site/pages [get]
func (h *SiteHandler) Pages(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	id, ok := projectFromRequest(ctx, c, h.db)
	if !ok {
		return
	}
	// Pages read before the current profile format show once read again.
	filter := bson.M{"project_id": id, "version": bson.M{"$gte": 1}}
	if g := c.Query("group"); g != "" {
		filter["group"] = g
	}
	if code := c.Query("issue"); code != "" {
		filter["issues.code"] = code
	}
	if sev := c.Query("severity"); sev != "" {
		filter["issues.severity"] = sev
	}
	switch c.Query("state") {
	case "ok":
		filter["status_code"] = bson.M{"$lt": 400}
		filter["noindex"] = bson.M{"$ne": true}
	case "error":
		filter["status_code"] = bson.M{"$gte": 400}
	case "noindex":
		filter["noindex"] = true
	case "redirect":
		filter["redirects"] = bson.M{"$gt": 0}
	}
	if q := strings.TrimSpace(c.Query("q")); q != "" {
		re := bson.M{"$regex": regexpQuote(q), "$options": "i"}
		filter["$or"] = bson.A{bson.M{"url": re}, bson.M{"title": re}}
	}
	sortKey := bson.D{{Key: "impressions", Value: -1}, {Key: "url", Value: 1}}
	switch c.Query("sort") {
	case "clicks":
		sortKey = bson.D{{Key: "clicks", Value: -1}, {Key: "url", Value: 1}}
	case "depth":
		sortKey = bson.D{{Key: "depth", Value: -1}, {Key: "url", Value: 1}}
	case "inbound":
		sortKey = bson.D{{Key: "inbound", Value: 1}, {Key: "url", Value: 1}}
	case "url":
		sortKey = bson.D{{Key: "url", Value: 1}}
	}
	size, _ := strconv.Atoi(c.DefaultQuery("size", "50"))
	if size <= 0 || size > sitePageSizeMax {
		size = 50
	}
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	if page < 1 {
		page = 1
	}
	col := h.db.Collection("page_profiles")
	total, err := col.CountDocuments(ctx, filter)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to count pages"})
		return
	}
	cursor, err := col.Find(ctx, filter, options.Find().SetSort(sortKey).SetSkip(int64((page-1)*size)).SetLimit(int64(size)).
		SetProjection(bson.M{"links": 0, "category_text": 0}))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch pages"})
		return
	}
	var profiles []models.PageProfile
	if err = cursor.All(ctx, &profiles); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to decode pages"})
		return
	}
	rows := make([]sitePageRow, 0, len(profiles))
	for _, pr := range profiles {
		issues := pr.Issues
		if issues == nil {
			issues = []models.PageIssue{}
		}
		rows = append(rows, sitePageRow{
			URL: pr.URL, SourceURL: pr.SourceURL, Kind: pr.Kind, Group: pr.Group, StatusCode: pr.StatusCode, Title: pr.Title,
			Noindex: pr.Noindex, InSitemap: pr.InSitemap, Inbound: pr.Inbound, Depth: pr.Depth,
			Impressions: pr.Impressions, Clicks: pr.Clicks, Issues: issues, FetchedAt: pr.FetchedAt,
		})
	}
	c.JSON(http.StatusOK, gin.H{"total": total, "page": page, "size": size, "items": rows})
}

func regexpQuote(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(`\.+*?()|[]{}^$`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Page godoc
// @Summary      Everything known about one page: profile, search queries, issues, suggestions and tasks
// @Tags         projects
// @Produce      json
// @Param        id   path   string  true  "Project ID"
// @Param        url  query  string  true  "Page URL"
// @Router       /api/v1/projects/{id}/site/page [get]
func (h *SiteHandler) Page(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	id, ok := projectFromRequest(ctx, c, h.db)
	if !ok {
		return
	}
	var profile models.PageProfile
	err := h.db.Collection("page_profiles").FindOne(ctx, bson.M{"project_id": id, "url": worker.NormalizePageURL(c.Query("url"))}).Decode(&profile)
	if errors.Is(err, mongo.ErrNoDocuments) {
		c.JSON(http.StatusNotFound, gin.H{"error": "this page has not been read yet"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch page"})
		return
	}
	spellings := bson.A{profile.SourceURL, profile.URL, profile.URL + "/"}

	queries, err := worker.PageQueries(ctx, h.db, id, profile.SourceURL)
	if err != nil {
		queries = nil
	}
	var suggestions []models.Suggestion
	if sc, sErr := h.db.Collection("suggestions").Find(ctx, bson.M{"project_id": id, "page": bson.M{"$in": spellings}},
		options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}).SetLimit(20).SetProjection(bson.M{"prompt": 0, "raw_response": 0})); sErr == nil {
		_ = sc.All(ctx, &suggestions)
	}
	linked, err := withTaskRefs(ctx, h.db, suggestions)
	if err != nil {
		linked = []suggestionView{}
	}
	var tasks []models.SEOTask
	if tc, tErr := h.db.Collection("seo_tasks").Find(ctx, bson.M{"project_id": id, "pages": bson.M{"$in": spellings}},
		options.Find().SetProjection(bson.M{"title": 1, "status": 1, "scope": 1, "metrics": 1, "issue": 1})); tErr == nil {
		_ = tc.All(ctx, &tasks)
	}
	refs := make([]taskRef, 0, len(tasks))
	for _, t := range tasks {
		refs = append(refs, taskRef{ID: t.ID, Title: t.Title, Status: t.Status, Scope: t.Scope, Pages: t.Metrics.Pages})
	}
	opps := []models.Opportunity{}
	if oc, oErr := h.db.Collection("opportunities").Find(ctx, bson.M{"project_id": id, "page": bson.M{"$in": spellings}},
		options.Find().SetSort(bson.D{{Key: "score", Value: -1}}).SetLimit(20)); oErr == nil {
		_ = oc.All(ctx, &opps)
	}
	if profile.Issues == nil {
		profile.Issues = []models.PageIssue{}
	}
	if queries == nil {
		queries = []worker.PageQueryStat{}
	}
	c.JSON(http.StatusOK, gin.H{"profile": profile, "queries": queries, "suggestions": linked, "tasks": refs, "opportunities": opps, "links_out": len(profile.Links)})
}

// Crawl godoc
// @Summary      Start a site crawl now
// @Tags         projects
// @Produce      json
// @Param        id  path  string  true  "Project ID"
// @Router       /api/v1/projects/{id}/site/crawl [post]
func (h *SiteHandler) Crawl(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id, ok := projectFromRequest(ctx, c, h.db)
	if !ok {
		return
	}
	if h.crawlRunning(ctx, id) {
		c.JSON(http.StatusConflict, gin.H{"error": "a site crawl is already running for this project"})
		return
	}
	task, err := worker.NewSiteCrawlTask(id.Hex())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to build task"})
		return
	}
	if _, err = h.asynqClient.EnqueueContext(ctx, task); err != nil {
		if errors.Is(err, asynq.ErrDuplicateTask) {
			c.JSON(http.StatusConflict, gin.H{"error": "a site crawl is already queued for this project"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to enqueue task"})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"status": "enqueued"})
}

// Vitals godoc
// @Summary      Core Web Vitals of the project's page types, latest run
// @Tags         projects
// @Produce      json
// @Param        id  path  string  true  "Project ID"
// @Router       /api/v1/projects/{id}/site/vitals [get]
func (h *SiteHandler) Vitals(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id, ok := projectFromRequest(ctx, c, h.db)
	if !ok {
		return
	}
	rows, err := worker.LatestVitals(ctx, h.db, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch vitals"})
		return
	}
	c.JSON(http.StatusOK, rows)
}

// RunVitals godoc
// @Summary      Measure the project's page types now
// @Tags         projects
// @Produce      json
// @Param        id  path  string  true  "Project ID"
// @Router       /api/v1/projects/{id}/site/vitals [post]
func (h *SiteHandler) RunVitals(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id, ok := projectFromRequest(ctx, c, h.db)
	if !ok {
		return
	}
	task, err := worker.NewSiteVitalsTask(id.Hex())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to build task"})
		return
	}
	if _, err = h.asynqClient.EnqueueContext(ctx, task); err != nil {
		if errors.Is(err, asynq.ErrDuplicateTask) {
			c.JSON(http.StatusConflict, gin.H{"error": "a measurement started within the last hour"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to enqueue task"})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"status": "enqueued"})
}
