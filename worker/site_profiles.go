package worker

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/creever/crawler-api/models"
)

// profileVersion is the current profile format; see PageProfile.Version.
const profileVersion = 1

const (
	// sampleCandidates is how many of a site's top pages can be sampled.
	sampleCandidates = 300
	// profileFreshFor keeps a profiled page out of the sample for a while.
	profileFreshFor = 14 * 24 * time.Hour
	profileTTL      = 60 * 24 * time.Hour
	profileTextChar = 400
)

var (
	reIDSuffix = regexp.MustCompile(`[-_]?\d{3,}$`)
	// reAssetPath matches files a crawl may have picked up that are no pages.
	reAssetPath = regexp.MustCompile(`(?i)\.(css|js|mjs|map|json|xml|txt|ico|png|jpe?g|gif|webp|avif|svg|pdf|zip|gz|mp4|webm|mp3|woff2?|ttf|eot)$`)
)

// pageKind reads what a page is from the page itself.
func pageKind(pageURL string, snap *models.PageSnapshot) string {
	if pagePath(pageURL) == "/" {
		return models.PageKindHome
	}
	for _, t := range snap.SchemaTypes {
		switch t {
		case "Article", "NewsArticle", "BlogPosting", "Report":
			return models.PageKindArticle
		}
	}
	product := false
	for _, t := range snap.SchemaTypes {
		product = product || t == "Product"
	}
	if isListingPage(snap) || (!product && isSmallListing(snap)) {
		return models.PageKindListing
	}
	if product {
		return models.PageKindProduct
	}
	return models.PageKindOther
}

// isSmallListing recognizes a listing with few items and no text of its own:
// its item cards still outweigh everything else on the page. A product page's
// related-items grid does not, and its Product markup rules it out first.
func isSmallListing(snap *models.PageSnapshot) bool {
	return snap.Listing != nil && snap.Listing.ItemCount >= listingMinCards &&
		snap.Coverage.ListingChars >= 2*snap.Coverage.ProseChars
}

// pageGroup returns the page type findings merge on. Home, listing, product and
// article pages are recognized by content, because their URLs may share no
// shape. Other pages group by their first path segment if they sit below one;
// a page directly under the root stands alone, since /about and /faq have
// nothing in common but their depth.
func pageGroup(pageURL, kind string) string {
	if kind != models.PageKindOther {
		return kind
	}
	segments := strings.Split(strings.Trim(pagePath(pageURL), "/"), "/")
	if len(segments) < 2 {
		return "page:" + pagePath(pageURL)
	}
	return "other:/" + segments[0]
}

func snapshotGroup(pageURL string, snap *models.PageSnapshot) string {
	if snap == nil {
		return "page:" + pagePath(pageURL)
	}
	return pageGroup(pageURL, pageKind(pageURL, snap))
}

// groupLabel names a page type in a task title.
func groupLabel(group string) string {
	switch group {
	case models.PageKindHome:
		return "the home page"
	case models.PageKindListing:
		return "listing pages"
	case models.PageKindProduct:
		return "product pages"
	case models.PageKindArticle:
		return "article pages"
	case "":
		return "the whole site"
	}
	if rest, ok := strings.CutPrefix(group, "other:"); ok {
		return "pages under " + rest
	}
	return strings.TrimPrefix(group, "page:")
}

func contentH1(snap *models.PageSnapshot) string {
	for _, h := range snap.Headings {
		if h.Level == 1 && h.Role == models.HeadingRoleContent {
			return h.Text
		}
	}
	return ""
}

func isNoindex(snap *models.PageSnapshot) bool {
	return strings.Contains(strings.ToLower(snap.Robots+" "+snap.XRobotsTag), "noindex")
}

func profileFromSnapshot(snap *models.PageSnapshot, now time.Time) models.PageProfile {
	text, _ := clip(snap.CategoryText, profileTextChar)
	kind := pageKind(snap.SourceURL, snap)
	h1s := 0
	for _, h := range snap.Headings {
		if h.Level == 1 && h.Role == models.HeadingRoleContent {
			h1s++
		}
	}
	return models.PageProfile{
		ProjectID: snap.ProjectID, URL: snap.URL, SourceURL: snap.SourceURL, StatusCode: snap.StatusCode,
		Title: snap.Title, MetaDescription: snap.MetaDescription, H1: contentH1(snap), CategoryText: text,
		Canonical: snap.Canonical, Robots: snap.Robots, Kind: kind, Group: pageGroup(snap.SourceURL, kind),
		Unavailable: kind == models.PageKindProduct && isUnavailablePage(snap),
		FinalURL:    snap.FinalURL, Redirects: snap.Redirects, Noindex: isNoindex(snap), SchemaTypes: snap.SchemaTypes,
		H1Count: h1s, ContentWords: snap.ContentWordCount, RenderMs: snap.RenderMs, Links: snap.Links,
		Depth: -1, Version: profileVersion,
		FetchedAt: snap.FetchedAt, ExpiresAt: now.Add(profileTTL),
	}
}

// crawlOwnedFields are kept when a page is read alone: the site crawl sets
// them from the whole site.
var crawlOwnedFields = []string{"in_sitemap", "inbound", "depth", "broken_links", "redirect_links", "impressions", "clicks", "issues"}

func (p *Processor) saveProfile(ctx context.Context, snap *models.PageSnapshot, now time.Time) error {
	profile := profileFromSnapshot(snap, now)
	raw, err := bson.Marshal(profile)
	if err != nil {
		return err
	}
	var set bson.M
	if err = bson.Unmarshal(raw, &set); err != nil {
		return err
	}
	delete(set, "_id")
	insert := bson.M{}
	for _, f := range crawlOwnedFields {
		insert[f] = set[f]
		delete(set, f)
	}
	_, err = p.db.Collection("page_profiles").UpdateOne(ctx,
		bson.M{"project_id": profile.ProjectID, "url": profile.URL},
		bson.M{"$set": set, "$setOnInsert": insert}, options.UpdateOne().SetUpsert(true))
	return err
}

func (p *Processor) loadProfiles(ctx context.Context, projectOID bson.ObjectID) ([]models.PageProfile, error) {
	cursor, err := p.db.Collection("page_profiles").Find(ctx, bson.M{"project_id": projectOID})
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx) //nolint:errcheck
	var profiles []models.PageProfile
	if err = cursor.All(ctx, &profiles); err != nil {
		return nil, err
	}
	return profiles, nil
}

// urlShape is a page's URL with its content replaced: /cipo-nike-44123 gives
// "/{slug-id}" and /news/2024/hello gives "/{seg}/{seg}/{seg}". It only spreads
// the sample over a site's sections, so it may be rough.
func urlShape(raw string) string {
	path := strings.Trim(pagePath(raw), "/")
	if path == "" {
		return "/"
	}
	segments := strings.Split(path, "/")
	var b strings.Builder
	for i, s := range segments {
		switch {
		case i == len(segments)-1 && reIDSuffix.MatchString(s):
			b.WriteString("/{slug-id}")
		default:
			b.WriteString("/{seg}")
		}
	}
	return b.String()
}

// pickSample takes up to n URLs in turns from each URL shape, so that a few
// thousand product URLs cannot crowd out the site's other sections. Candidates
// arrive strongest first, and each shape keeps that order.
func pickSample(candidates []string, fresh map[string]bool, n int) []string {
	buckets := make(map[string][]string)
	var shapes []string
	seen := make(map[string]bool)
	for _, c := range candidates {
		key := normalizePageURL(c)
		if seen[key] || fresh[key] {
			continue
		}
		seen[key] = true
		shape := urlShape(c)
		if _, ok := buckets[shape]; !ok {
			shapes = append(shapes, shape)
		}
		buckets[shape] = append(buckets[shape], c)
	}
	var out []string
	for len(out) < n {
		progressed := false
		for _, shape := range shapes {
			if len(buckets[shape]) == 0 || len(out) == n {
				continue
			}
			out = append(out, buckets[shape][0])
			buckets[shape] = buckets[shape][1:]
			progressed = true
		}
		if !progressed {
			break
		}
	}
	return out
}

// sampleableURL drops parameter URLs, assets and other sites' pages. Filter and
// sort variants would only repeat their parent page.
func sampleableURL(raw, siteURL string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.RawQuery != "" || strings.Contains(u.Path, ":") || reAssetPath.MatchString(u.Path) {
		return false
	}
	return sameSite(u.Host, hostOf(siteURL))
}

// siteCandidates lists a project's known URLs besides its sitemap, strongest
// first: its search pages, then whatever earlier crawls saw.
func (p *Processor) siteCandidates(ctx context.Context, project *models.Project) ([]string, error) {
	curStart, end, _, _ := analysisWindows()
	rows, err := p.gscPageTotals(ctx, project.ID, curStart, end, sampleCandidates)
	if err != nil {
		return nil, fmt.Errorf("search pages: %w", err)
	}
	candidates := []string{project.URL}
	for _, r := range rows {
		candidates = append(candidates, r.Page)
	}
	cursor, err := p.db.Collection("seo_data").Find(ctx, bson.M{"project_id": project.ID},
		options.Find().SetProjection(bson.M{"url": 1}).SetLimit(sampleCandidates))
	if err != nil {
		return nil, fmt.Errorf("crawled pages: %w", err)
	}
	defer cursor.Close(ctx) //nolint:errcheck
	var crawled []struct {
		URL string `bson:"url"`
	}
	if err = cursor.All(ctx, &crawled); err != nil {
		return nil, fmt.Errorf("crawled pages: %w", err)
	}
	for _, c := range crawled {
		candidates = append(candidates, c.URL)
	}
	out := candidates[:0]
	for _, c := range candidates {
		if sampleableURL(c, project.URL) {
			out = append(out, c)
		}
	}
	return out, nil
}

// pageTotal is one page's search totals.
type pageTotal struct {
	Page        string  `bson:"_id"`
	Clicks      float64 `bson:"clicks"`
	Impressions float64 `bson:"impressions"`
}

// gscPageTotals sums Search Console rows per page, strongest first.
func (p *Processor) gscPageTotals(ctx context.Context, projectOID bson.ObjectID, start, end time.Time, limit int64) ([]pageTotal, error) {
	pipeline := bson.A{
		bson.D{{Key: "$match", Value: bson.D{{Key: "project_id", Value: projectOID}, {Key: "date", Value: dateRange(start, end)}}}},
		bson.D{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$page"},
			{Key: "clicks", Value: bson.D{{Key: "$sum", Value: "$clicks"}}},
			{Key: "impressions", Value: bson.D{{Key: "$sum", Value: "$impressions"}}},
		}}},
		bson.D{{Key: "$sort", Value: bson.D{{Key: "impressions", Value: -1}, {Key: "_id", Value: 1}}}},
	}
	if limit > 0 {
		pipeline = append(pipeline, bson.D{{Key: "$limit", Value: limit}})
	}
	cursor, err := p.db.Collection("gsc_rows").Aggregate(ctx, pipeline)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx) //nolint:errcheck
	var rows []pageTotal
	if err = cursor.All(ctx, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// pageStat is what a task sums for one page.
type pageStat struct {
	Clicks, Impressions, Sessions float64
}

// sitePageStats returns 28-day search and GA4 totals keyed by normalized URL.
// GA4 keeps paths, so its rows are matched through the given pages.
func (p *Processor) sitePageStats(ctx context.Context, projectOID bson.ObjectID, pages []models.PageProfile) (map[string]pageStat, error) {
	curStart, end, _, _ := analysisWindows()
	rows, err := p.gscPageTotals(ctx, projectOID, curStart, end, 0)
	if err != nil {
		return nil, err
	}
	stats := make(map[string]pageStat, len(rows))
	for _, r := range rows {
		key := normalizePageURL(r.Page)
		s := stats[key]
		s.Clicks += r.Clicks
		s.Impressions += r.Impressions
		stats[key] = s
	}

	cursor, err := p.db.Collection("ga4_rows").Aggregate(ctx, bson.A{
		bson.D{{Key: "$match", Value: bson.D{
			{Key: "project_id", Value: projectOID},
			{Key: "date", Value: bson.D{
				{Key: "$gte", Value: curStart.Format(GA4DateLayout)},
				{Key: "$lte", Value: end.Format(GA4DateLayout)},
			}},
		}}},
		bson.D{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$page"},
			{Key: "sessions", Value: bson.D{{Key: "$sum", Value: "$sessions"}}},
		}}},
	})
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx) //nolint:errcheck
	var ga4 []struct {
		Path     string  `bson:"_id"`
		Sessions float64 `bson:"sessions"`
	}
	if err = cursor.All(ctx, &ga4); err != nil {
		return nil, err
	}
	sessions := make(map[string]float64, len(ga4))
	for _, r := range ga4 {
		sessions[pagePath(r.Path)] += r.Sessions
	}
	for _, page := range pages {
		s := stats[page.URL]
		s.Sessions = sessions[pagePath(page.URL)]
		stats[page.URL] = s
	}
	return stats, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
