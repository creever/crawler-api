package worker

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/creever/crawler-api/models"
)

const (
	contextTopQueries     = 30
	contextCompetingBasis = 10 // top queries checked for competing pages
	contextCompetingPages = 5
	maxRelatedPages       = 3

	contextCategoryChars = 3000
	contextProseChars    = 6000 // category text included
	contextLinks         = 30
	// contextSignalsPerType keeps a page with dozens of query-level signals from
	// crowding out everything else; each type's strongest ones are shown.
	contextSignalsPerType = 5
	contextDetailChars    = 300
	relatedExcerptChars   = 500
)

// pageQueryStat is one query of a page, current window vs previous window.
type pageQueryStat struct {
	Query           string
	Clicks          float64
	Impressions     float64
	Position        float64
	PrevClicks      float64
	PrevImpressions float64
	PrevPosition    float64
}

// pageDeviceStat is a page's GSC performance on one device type.
type pageDeviceStat struct {
	Device      string
	Clicks      float64
	Impressions float64
	Position    float64
}

// ga4PageStat is a page's GA4 landing-page performance.
type ga4PageStat struct {
	Sessions    float64
	Conversions float64
	Revenue     float64
}

// competingPage is another page of the site shown for the target's queries.
type competingPage struct {
	Page        string
	Clicks      float64
	Impressions float64
	Queries     int
}

// relatedPage is a page read alongside the target.
type relatedPage struct {
	URL      string
	Reason   string
	Snapshot *models.PageSnapshot
}

// contextInput is everything known about a page before the prompt is built.
type contextInput struct {
	Brand         string
	Language      string
	Currency      string
	Page          string
	Opportunities []models.Opportunity
	Snapshot      *models.PageSnapshot
	Queries       []pageQueryStat
	Devices       []pageDeviceStat
	GA4           *ga4PageStat
	Related       []relatedPage
	// RelatedFailed lists related pages that could not be read.
	RelatedFailed []string
}

// contextPack is the rendered prompt context. Every fact has an ID the model
// must cite, so citations can be verified in code.
type contextPack struct {
	Text     string
	Language string
	Facts    map[string]string
	Coverage models.AnalysisCoverage

	sb       strings.Builder
	counters map[string]int
}

func (c *contextPack) section(title string) {
	fmt.Fprintf(&c.sb, "\n## %s\n", title)
	c.Coverage.Sections = append(c.Coverage.Sections, title)
}

func (c *contextPack) fact(prefix, format string, args ...interface{}) {
	c.counters[prefix]++
	id := fmt.Sprintf("%s%d", prefix, c.counters[prefix])
	text := fmt.Sprintf(format, args...)
	c.Facts[id] = text
	fmt.Fprintf(&c.sb, "[%s] %s\n", id, text)
}

func clip(s string, n int) (string, bool) {
	r := []rune(s)
	if len(r) <= n {
		return s, false
	}
	return string(r[:n]) + " …", true
}

// buildContextPack renders the input in a fixed order within fixed budgets.
// Whatever a budget cuts off is recorded in Coverage.Truncated.
func buildContextPack(in contextInput) *contextPack {
	c := &contextPack{Language: in.Language, Facts: make(map[string]string), counters: make(map[string]int)}
	snap := in.Snapshot
	c.Coverage.ContentWords = snap.ContentWordCount
	c.Coverage.SnapshotAt = snap.FetchedAt
	c.Coverage.PagesRead = []string{in.Page}
	c.Coverage.GSCQueries = len(in.Queries)
	c.Coverage.HasGA4 = in.GA4 != nil

	fmt.Fprintf(&c.sb, "Site brand: %s\nPage under analysis: %s\n", in.Brand, in.Page)

	writeSignals(c, in.Opportunities)

	writePageFacts(c, in)
	writeContent(c, snap)
	writeSearchData(c, in)
	writeRelated(c, in)

	c.section("Not available")
	c.sb.WriteString("Inbound internal links and competitor/SERP data were not collected. Make no claims about them.\n")

	c.Text = c.sb.String()
	c.Coverage.CharsSent = utf8.RuneCountInString(c.Text)
	return c
}

// writeSignals renders the strongest signals of each type. Opportunities
// arrive ranked, so the first ones of a type are its strongest.
func writeSignals(c *contextPack, opps []models.Opportunity) {
	c.section("Detected signals")
	shown := make(map[models.OpportunityType]int)
	total := make(map[models.OpportunityType]int)
	var order []models.OpportunityType
	for _, o := range opps {
		if total[o.Type] == 0 {
			order = append(order, o.Type)
		}
		total[o.Type]++
		if shown[o.Type] < contextSignalsPerType {
			shown[o.Type]++
			c.fact("S", "%s", describeSignal(o))
		}
	}
	for _, t := range order {
		if total[t] > shown[t] {
			fmt.Fprintf(&c.sb, "(%s: %d signals in total, the strongest %d are shown.)\n", t, total[t], shown[t])
			c.Coverage.Truncated = append(c.Coverage.Truncated, "signals: "+string(t))
		}
	}
}

func describeSignal(o models.Opportunity) string {
	parts := []string{string(o.Type)}
	if o.Query != "" {
		parts = append(parts, fmt.Sprintf("query %q", o.Query))
	}
	keys := make([]string, 0, len(o.Metrics))
	for k := range o.Metrics {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := o.Metrics[k]
		if k == "ctr" {
			parts = append(parts, fmt.Sprintf("ctr=%.2f%%", v*100))
		} else {
			parts = append(parts, fmt.Sprintf("%s=%.1f", k, v))
		}
	}
	if o.Details != "" {
		details, _ := clip(o.Details, contextDetailChars)
		parts = append(parts, details)
	}
	return strings.Join(parts, ", ")
}

func writePageFacts(c *contextPack, in contextInput) {
	snap := in.Snapshot
	c.section("Page head and technical facts")
	c.fact("P", "HTTP status: %d", snap.StatusCode)
	c.fact("P", "%s", lengthFact("Title", snap.Title, maxTitleChars))
	c.fact("P", "%s", lengthFact("Meta description", snap.MetaDescription, maxMetaChars))
	switch {
	case snap.Canonical == "":
		c.fact("P", "Canonical: missing")
	case normalizePageURL(snap.Canonical) == normalizePageURL(in.Page):
		c.fact("P", "Canonical: self-referencing")
	default:
		c.fact("P", "Canonical points to another URL: %s", snap.Canonical)
	}
	c.fact("P", "Meta robots: %q", snap.Robots)
	c.fact("P", "Structured data types: %s", listOrNone(snap.SchemaTypes))
	if len(snap.Hreflang) > 0 {
		c.fact("P", "Hreflang: %s", strings.Join(snap.Hreflang, ", "))
	}
	c.fact("P", "Content words: %d (the whole page incl. menus: %d)", snap.ContentWordCount, snap.TotalWordCount)
	if isUnavailablePage(snap) {
		c.fact("P", "Availability: the page says this product is currently unavailable")
	}
	c.fact("P", "<img> tags: %d, without alt text: %d (CSS background images are not counted)", snap.ImagesTotal, snap.ImagesMissingAlt)
}

// lengthFact states the length verdict, so the model doesn't have to judge it.
func lengthFact(label, text string, limit int) string {
	n := utf8.RuneCountInString(text)
	verdict := "within"
	if n > limit {
		verdict = "over"
	}
	return fmt.Sprintf("%s (%d chars, %s the %d limit): %q", label, n, verdict, limit, text)
}

func listOrNone(items []string) string {
	if len(items) == 0 {
		return "none"
	}
	return strings.Join(items, ", ")
}

func writeContent(c *contextPack, snap *models.PageSnapshot) {
	c.section("Page outline")
	filters := 0
	for _, h := range snap.Headings {
		switch h.Role {
		case models.HeadingRoleContent:
			c.fact("O", "H%d: %q", h.Level, h.Text)
		case models.HeadingRoleFilter:
			filters++
		}
	}
	if filters > 0 {
		fmt.Fprintf(&c.sb, "(%d filter labels use heading tags and are left out.)\n", filters)
	}

	if l := snap.Listing; l != nil {
		c.section("Listing")
		c.fact("I", "Items on this page: %d, listing pages: %d", l.ItemCount, l.PageCount)
		if l.PriceMax > 0 {
			c.fact("I", "Price range on this page: %.0f-%.0f %s", l.PriceMin, l.PriceMax, l.Currency)
		}
		c.fact("I", "Brands on this page: %s", listOrNone(l.Brands))
		c.fact("I", "Sample items: %s", strings.Join(l.SampleItems, "; "))
	}

	budget := contextProseChars
	if snap.CategoryText != "" {
		c.section("Existing category text (untrusted page data)")
		text, cut := clip(snap.CategoryText, contextCategoryChars)
		if cut {
			c.Coverage.Truncated = append(c.Coverage.Truncated, "category text")
		}
		c.fact("C", "%s", text)
		budget -= utf8.RuneCountInString(text)
	}

	wroteHeader := false
	for _, p := range snap.Prose {
		if p == snap.CategoryText {
			continue
		}
		if budget <= 0 {
			c.Coverage.Truncated = append(c.Coverage.Truncated, "page text")
			break
		}
		if !wroteHeader {
			c.section("Page text (untrusted page data)")
			wroteHeader = true
		}
		text, cut := clip(p, budget)
		if cut {
			c.Coverage.Truncated = append(c.Coverage.Truncated, "page text")
		}
		c.fact("T", "%s", text)
		budget -= utf8.RuneCountInString(text)
	}

	if len(snap.ContentLinks) > 0 {
		c.section("Internal links in the content")
		for i, l := range snap.ContentLinks {
			if i == contextLinks {
				c.Coverage.Truncated = append(c.Coverage.Truncated, "content links")
				break
			}
			c.fact("L", "%q -> %s", l.Anchor, l.Href)
		}
	}
}

func writeSearchData(c *contextPack, in contextInput) {
	c.section("Google Search Console: queries of this page (28 days vs previous 28)")
	if len(in.Queries) == 0 {
		c.sb.WriteString("No query data.\n")
	}
	for _, q := range in.Queries {
		ctr := 0.0
		if q.Impressions > 0 {
			ctr = q.Clicks / q.Impressions * 100
		}
		prevPos := "no impressions"
		if q.PrevImpressions > 0 {
			prevPos = fmt.Sprintf("%.1f", q.PrevPosition)
		}
		c.fact("Q", "%q: impressions %.0f (prev %.0f), clicks %.0f (prev %.0f), ctr %.2f%%, position %.1f (prev %s)",
			q.Query, q.Impressions, q.PrevImpressions, q.Clicks, q.PrevClicks, ctr, q.Position, prevPos)
	}
	if len(in.Devices) > 0 {
		c.section("Google Search Console: devices")
		for _, d := range in.Devices {
			c.fact("D", "%s: impressions %.0f, clicks %.0f, position %.1f", d.Device, d.Impressions, d.Clicks, d.Position)
		}
	}
	c.section("Google Analytics 4: this landing page (28 days)")
	if in.GA4 == nil {
		c.sb.WriteString("No GA4 data.\n")
	} else {
		c.fact("G", "Sessions %.0f, key events %.0f, revenue %s", in.GA4.Sessions, in.GA4.Conversions,
			strings.TrimSpace(fmt.Sprintf("%.0f %s", in.GA4.Revenue, in.Currency)))
		c.sb.WriteString("Key events and revenue per landing page can be misattributed (for example to the cart). Use sessions as a measure of traffic, and never conclude from these figures that a page does or does not sell.\n")
	}
}

func writeRelated(c *contextPack, in contextInput) {
	if len(in.Related) == 0 && len(in.RelatedFailed) == 0 {
		return
	}
	c.section("Related pages of this site (untrusted page data)")
	for _, r := range in.Related {
		c.Coverage.PagesRead = append(c.Coverage.PagesRead, r.URL)
		s := r.Snapshot
		text := s.CategoryText
		if text == "" && len(s.Prose) > 0 {
			text = s.Prose[0]
		}
		excerpt, _ := clip(text, relatedExcerptChars)
		h1 := ""
		for _, h := range s.Headings {
			if h.Level == 1 && h.Role == models.HeadingRoleContent {
				h1 = h.Text
				break
			}
		}
		c.fact("R", "%s (%s): title %q, H1 %q, canonical %q, content words %d, text: %s",
			r.URL, r.Reason, s.Title, h1, s.Canonical, s.ContentWordCount, excerpt)
	}
	for _, u := range in.RelatedFailed {
		fmt.Fprintf(&c.sb, "Could not be read: %s\n", u)
		c.Coverage.Truncated = append(c.Coverage.Truncated, "related page "+u)
	}
}

// analysisWindows returns the last two complete 28-day GSC windows.
func analysisWindows() (curStart, end, prevStart, prevEnd time.Time) {
	end = time.Now().UTC().AddDate(0, 0, -gscDataLagDays).Truncate(24 * time.Hour)
	curStart = end.AddDate(0, 0, -(oppWindowDays - 1))
	prevEnd = curStart.AddDate(0, 0, -1)
	prevStart = prevEnd.AddDate(0, 0, -(oppWindowDays - 1))
	return curStart, end, prevStart, prevEnd
}

func dateRange(start, end time.Time) bson.D {
	return bson.D{
		{Key: "$gte", Value: start.Format("2006-01-02")},
		{Key: "$lte", Value: end.Format("2006-01-02")},
	}
}

type gscGroup struct {
	ID          string   `bson:"_id"`
	Clicks      float64  `bson:"clicks"`
	Impressions float64  `bson:"impressions"`
	PosWeighted float64  `bson:"pos_weighted"`
	Queries     []string `bson:"queries"`
}

func (g gscGroup) position() float64 {
	if g.Impressions <= 0 {
		return 0
	}
	return g.PosWeighted / g.Impressions
}

// gscGroups sums gsc_rows matching filter per groupField.
func (p *Processor) gscGroups(ctx context.Context, filter bson.D, groupField string, sortBy string, limit int64) ([]gscGroup, error) {
	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: filter}},
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$" + groupField},
			{Key: "clicks", Value: bson.D{{Key: "$sum", Value: "$clicks"}}},
			{Key: "impressions", Value: bson.D{{Key: "$sum", Value: "$impressions"}}},
			{Key: "pos_weighted", Value: bson.D{{Key: "$sum", Value: bson.D{{Key: "$multiply", Value: bson.A{"$position", "$impressions"}}}}}},
			{Key: "queries", Value: bson.D{{Key: "$addToSet", Value: "$query"}}},
		}}},
		{{Key: "$sort", Value: bson.D{{Key: sortBy, Value: -1}, {Key: "_id", Value: 1}}}},
	}
	if limit > 0 {
		pipeline = append(pipeline, bson.D{{Key: "$limit", Value: limit}})
	}
	cursor, err := p.db.Collection("gsc_rows").Aggregate(ctx, pipeline)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx) //nolint:errcheck
	var rows []gscGroup
	if err = cursor.All(ctx, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// pageSearchData loads a page's queries, device split and competing pages.
func (p *Processor) pageSearchData(ctx context.Context, projectOID bson.ObjectID, page string) ([]pageQueryStat, []pageDeviceStat, []competingPage, error) {
	curStart, end, prevStart, prevEnd := analysisWindows()

	cur, err := p.gscGroups(ctx, bson.D{
		{Key: "project_id", Value: projectOID}, {Key: "page", Value: page},
		{Key: "date", Value: dateRange(curStart, end)},
	}, "query", "impressions", contextTopQueries)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("page queries: %w", err)
	}
	queries := make([]string, 0, len(cur))
	for _, r := range cur {
		queries = append(queries, r.ID)
	}

	prev, err := p.gscGroups(ctx, bson.D{
		{Key: "project_id", Value: projectOID}, {Key: "page", Value: page},
		{Key: "query", Value: bson.M{"$in": queries}},
		{Key: "date", Value: dateRange(prevStart, prevEnd)},
	}, "query", "impressions", 0)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("page queries (previous window): %w", err)
	}
	prevByQuery := make(map[string]gscGroup, len(prev))
	for _, r := range prev {
		prevByQuery[r.ID] = r
	}

	stats := make([]pageQueryStat, 0, len(cur))
	for _, r := range cur {
		pr := prevByQuery[r.ID]
		stats = append(stats, pageQueryStat{
			Query: r.ID, Clicks: r.Clicks, Impressions: r.Impressions, Position: r.position(),
			PrevClicks: pr.Clicks, PrevImpressions: pr.Impressions, PrevPosition: pr.position(),
		})
	}

	devRows, err := p.gscGroups(ctx, bson.D{
		{Key: "project_id", Value: projectOID}, {Key: "page", Value: page},
		{Key: "date", Value: dateRange(curStart, end)},
	}, "device", "impressions", 0)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("page devices: %w", err)
	}
	devices := make([]pageDeviceStat, 0, len(devRows))
	for _, r := range devRows {
		devices = append(devices, pageDeviceStat{Device: r.ID, Clicks: r.Clicks, Impressions: r.Impressions, Position: r.position()})
	}

	basis := queries
	if len(basis) > contextCompetingBasis {
		basis = basis[:contextCompetingBasis]
	}
	var competing []competingPage
	if len(basis) > 0 {
		rows, cErr := p.gscGroups(ctx, bson.D{
			{Key: "project_id", Value: projectOID},
			{Key: "page", Value: bson.M{"$ne": page}},
			{Key: "query", Value: bson.M{"$in": basis}},
			{Key: "date", Value: dateRange(curStart, end)},
		}, "page", "impressions", contextCompetingPages)
		if cErr != nil {
			return nil, nil, nil, fmt.Errorf("competing pages: %w", cErr)
		}
		for _, r := range rows {
			competing = append(competing, competingPage{Page: r.ID, Clicks: r.Clicks, Impressions: r.Impressions, Queries: len(r.Queries)})
		}
	}
	return stats, devices, competing, nil
}

// pageGA4 loads a page's GA4 totals; nil means there is no GA4 data for it.
func (p *Processor) pageGA4(ctx context.Context, projectOID bson.ObjectID, page string) (*ga4PageStat, error) {
	curStart, end, _, _ := analysisWindows()
	path := pagePath(page)
	cursor, err := p.db.Collection("ga4_rows").Aggregate(ctx, mongo.Pipeline{
		{{Key: "$match", Value: bson.D{
			{Key: "project_id", Value: projectOID},
			{Key: "page", Value: bson.M{"$in": bson.A{path, path + "/"}}},
			{Key: "date", Value: bson.D{
				{Key: "$gte", Value: curStart.Format(GA4DateLayout)},
				{Key: "$lte", Value: end.Format(GA4DateLayout)},
			}},
		}}},
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: nil},
			{Key: "sessions", Value: bson.D{{Key: "$sum", Value: "$sessions"}}},
			{Key: "conversions", Value: bson.D{{Key: "$sum", Value: "$conversions"}}},
			{Key: "revenue", Value: bson.D{{Key: "$sum", Value: "$revenue"}}},
		}}},
	})
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx) //nolint:errcheck
	var rows []struct {
		Sessions    float64 `bson:"sessions"`
		Conversions float64 `bson:"conversions"`
		Revenue     float64 `bson:"revenue"`
	}
	if err = cursor.All(ctx, &rows); err != nil {
		return nil, err
	}
	if len(rows) == 0 || rows[0].Sessions == 0 {
		return nil, nil
	}
	return &ga4PageStat{Sessions: rows[0].Sessions, Conversions: rows[0].Conversions, Revenue: rows[0].Revenue}, nil
}

// pickRelatedPages chooses the pages to read with the target: pages named by a
// cannibalization signal first, then the strongest competing pages.
func pickRelatedPages(page string, opps []models.Opportunity, competing []competingPage) []relatedPage {
	seen := map[string]bool{normalizePageURL(page): true}
	var out []relatedPage
	add := func(u, reason string) {
		key := normalizePageURL(u)
		if u == "" || seen[key] || len(out) >= maxRelatedPages {
			return
		}
		seen[key] = true
		out = append(out, relatedPage{URL: u, Reason: reason})
	}
	for _, o := range opps {
		for _, u := range o.Pages {
			add(u, fmt.Sprintf("competes for query %q", o.Query))
		}
	}
	for _, c := range competing {
		add(c.Page, fmt.Sprintf("shown for %d of this page's top queries, %.0f impressions", c.Queries, c.Impressions))
	}
	return out
}

// latestAnalysisFingerprint returns the snapshot fingerprint of the page's
// newest finished analysis since the given time, or "".
func (p *Processor) latestAnalysisFingerprint(ctx context.Context, projectOID bson.ObjectID, page string, since time.Time) (string, error) {
	var a models.PageAnalysis
	err := p.db.Collection("page_analyses").FindOne(ctx, bson.M{
		"project_id": projectOID, "page": page,
		"status":     bson.M{"$in": []models.AnalysisStatus{models.AnalysisStatusCompleted, models.AnalysisStatusNoAction}},
		"created_at": bson.M{"$gte": since},
	}, options.FindOne().SetSort(bson.D{{Key: "created_at", Value: -1}}).SetProjection(bson.M{"fingerprint": 1})).Decode(&a)
	if err == mongo.ErrNoDocuments {
		return "", nil
	}
	return a.Fingerprint, err
}
