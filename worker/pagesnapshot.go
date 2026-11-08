package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"

	"github.com/creever/crawler-api/models"
)

const (
	snapshotTTL = 24 * time.Hour
	// snapshotVersion invalidates cached snapshots when extraction changes.
	snapshotVersion = 5

	// navLinkDensity marks a block as navigation: nearly all of its text is
	// link text. Measured on cipomarket.hu: menu 0.92, product list 0.23,
	// category description 0.00.
	navLinkDensity = 0.6
	navMinChars    = 60
	// proseLinkDensity is the highest link density of a prose block.
	proseLinkDensity = 0.25
	proseMinChars    = 150
	// sidebarLinkDensity marks an <aside> as a sidebar rather than main content.
	sidebarLinkDensity = 0.4

	listingMinCards = 4
	// minHeadlineChars is the shortest heading that marks a card of a list.
	minHeadlineChars = 20

	maxSnapshotHeadings = 40
	maxSnapshotLinks    = 40
	maxGraphLinks       = 300
	maxSampleItems      = 20
	maxListingBrands    = 20
	maxProseChars       = 20000

	// minContentWords is the least visible content a page needs for its
	// snapshot to be trusted.
	minContentWords = 30
)

const (
	priceAmount   = `(\d{1,3}(?:[ .,\x{a0}]\d{3})+(?:[.,]\d{1,2})?|\d+(?:[.,]\d{1,2})?)`
	priceCurrency = `(Ft|HUF|€|EUR|\$|USD|£|GBP|zł|PLN|Kč|CZK|lei|RON|kr|SEK|NOK|DKK|CHF)`
)

var (
	reFilterRegion = regexp.MustCompile(`(?i)filter|facet|sidebar|szuro|szűrő`)
	reCategoryText = regexp.MustCompile(`(?i)categor|kategor`)
	reTextRegion   = regexp.MustCompile(`(?i)description|leiras|leírás|seo-text`)
	// rePrice finds an amount with its currency on either side. The currency
	// must not run on into a word, so "12 kronor" or "5 kraj" is not "12 kr".
	rePrice  = regexp.MustCompile(`(?i)(?:` + priceAmount + `\s*` + priceCurrency + `|` + priceCurrency + `\s*` + priceAmount + `)(?:[^\p{L}\p{N}]|$)`)
	reSpaces = regexp.MustCompile(`\s+`)
)

// blockTags end a run of inline text.
var blockTags = map[atom.Atom]bool{
	atom.Address: true, atom.Article: true, atom.Aside: true, atom.Blockquote: true,
	atom.Br: true, atom.Dd: true, atom.Div: true, atom.Dl: true, atom.Dt: true,
	atom.Figcaption: true, atom.Figure: true, atom.Footer: true, atom.Form: true,
	atom.H1: true, atom.H2: true, atom.H3: true, atom.H4: true, atom.H5: true,
	atom.H6: true, atom.Header: true, atom.Hr: true, atom.Li: true, atom.Main: true,
	atom.Nav: true, atom.Ol: true, atom.P: true, atom.Pre: true, atom.Section: true,
	atom.Table: true, atom.Td: true, atom.Th: true, atom.Tr: true, atom.Ul: true,
}

// htmlBlock is the text statistics of one DOM element.
type htmlBlock struct {
	node     *html.Node
	children []*htmlBlock
	text     int // visible characters, including descendants
	link     int // characters inside links, including descendants
}

func (b *htmlBlock) linkDensity() float64 {
	if b.text == 0 {
		return 0
	}
	return float64(b.link) / float64(b.text)
}

// isInvisible reports whether a node's content never reaches a reader.
func isInvisible(n *html.Node) bool {
	switch n.DataAtom {
	case atom.Script, atom.Style, atom.Noscript, atom.Template, atom.Svg, atom.Iframe,
		atom.Head, atom.Select, atom.Option, atom.Textarea:
		return true
	}
	if _, ok := attr(n, "hidden"); ok {
		return true
	}
	if v, _ := attr(n, "aria-hidden"); v == "true" {
		return true
	}
	style, _ := attr(n, "style")
	style = strings.ToLower(strings.ReplaceAll(style, " ", ""))
	return strings.Contains(style, "display:none") || strings.Contains(style, "visibility:hidden")
}

func attr(n *html.Node, key string) (string, bool) {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return a.Val, true
		}
	}
	return "", false
}

func classAndID(n *html.Node) string {
	c, _ := attr(n, "class")
	id, _ := attr(n, "id")
	return c + " " + id
}

// measure builds the block tree under n. insideLink is true within an <a>.
func measure(n *html.Node, insideLink bool) *htmlBlock {
	b := &htmlBlock{node: n}
	insideLink = insideLink || n.DataAtom == atom.A
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		switch c.Type {
		case html.TextNode:
			l := utf8.RuneCountInString(strings.TrimSpace(c.Data))
			b.text += l
			if insideLink {
				b.link += l
			}
		case html.ElementNode:
			if isInvisible(c) {
				continue
			}
			child := measure(c, insideLink)
			b.children = append(b.children, child)
			b.text += child.text
			b.link += child.link
		}
	}
	return b
}

// visibleText returns n's visible text with block boundaries kept as spaces.
func visibleText(n *html.Node) string {
	return textOutside(n, nil)
}

// textOutside is visibleText without the subtrees rooted at skip nodes.
func textOutside(n *html.Node, skip map[*html.Node]bool) string {
	var sb strings.Builder
	var walk func(*html.Node)
	walk = func(cur *html.Node) {
		for c := cur.FirstChild; c != nil; c = c.NextSibling {
			switch c.Type {
			case html.TextNode:
				sb.WriteString(c.Data)
			case html.ElementNode:
				if isInvisible(c) || skip[c] {
					continue
				}
				block := blockTags[c.DataAtom]
				if block {
					sb.WriteByte(' ')
				}
				walk(c)
				if block {
					sb.WriteByte(' ')
				}
			}
		}
	}
	walk(n)
	return cleanText(sb.String())
}

func cleanText(s string) string {
	return strings.TrimSpace(reSpaces.ReplaceAllString(strings.ReplaceAll(s, " ", " "), " "))
}

// isChromeBlock reports whether b is site chrome: navigation or a filter panel.
func isChromeBlock(b *htmlBlock) bool {
	n := b.node
	switch n.DataAtom {
	case atom.Nav, atom.Footer:
		return true
	case atom.Header:
		if !containsH1(n) {
			return true
		}
	case atom.Aside:
		// Templates also use <aside> for the main column: cipomarket.hu's product
		// page keeps its price, description and availability in one, with the
		// H1 sometimes outside it. A sidebar is recognized by its links.
		if !containsH1(n) && b.linkDensity() >= sidebarLinkDensity {
			return true
		}
	}
	if role, _ := attr(n, "role"); role == "navigation" || role == "banner" || role == "contentinfo" {
		return true
	}
	if n.DataAtom != atom.Body && reFilterRegion.MatchString(classAndID(n)) {
		return true
	}
	// On a short page the menu outweighs the content, so a wrapper up to <body>
	// can look like navigation. A block holding the H1 is never navigation, and
	// neither is <body>: its parts are judged one by one.
	return n.DataAtom != atom.Body && b.text >= navMinChars && b.linkDensity() >= navLinkDensity &&
		!containsH1(n) && !containsListing(b)
}

func containsH1(n *html.Node) bool {
	return findFirst(n, func(x *html.Node) bool { return x.DataAtom == atom.H1 }) != nil
}

// listingCards returns the repeated item cards directly under b, if any. A
// card holds a link, and most cards of a list show a price or, on a list of
// articles or jobs, their own heading.
func listingCards(b *htmlBlock) []*htmlBlock {
	groups := make(map[string][]*htmlBlock)
	for _, c := range b.children {
		class, _ := attr(c.node, "class")
		if fields := strings.Fields(class); len(fields) > 0 {
			groups[c.node.Data+"."+fields[0]] = append(groups[c.node.Data+"."+fields[0]], c)
		} else if c.node.DataAtom == atom.Li || c.node.DataAtom == atom.Article {
			groups[c.node.Data] = append(groups[c.node.Data], c)
		}
	}
	var best []*htmlBlock
	for _, cards := range groups {
		if len(cards) < listingMinCards || len(cards) <= len(best) {
			continue
		}
		items := 0
		for _, c := range cards {
			if firstLink(c.node) != nil && (rePrice.MatchString(visibleText(c.node)) || hasCardHeading(c.node)) {
				items++
			}
		}
		if items*2 >= len(cards) {
			best = cards
		}
	}
	return best
}

// hasCardHeading reports a heading long enough to be a headline. A menu's
// column headings, such as "Women", are too short.
func hasCardHeading(n *html.Node) bool {
	return findFirst(n, func(x *html.Node) bool {
		switch x.DataAtom {
		case atom.H2, atom.H3, atom.H4:
			return utf8.RuneCountInString(visibleText(x)) >= minHeadlineChars
		}
		return false
	}) != nil
}

// containsListing reports item cards at or below b. A list of linked articles
// is nearly all links, but it is the page's content.
func containsListing(b *htmlBlock) bool {
	if len(listingCards(b)) > 0 {
		return true
	}
	for _, c := range b.children {
		if containsListing(c) {
			return true
		}
	}
	return false
}

func firstLink(n *html.Node) *html.Node {
	if n.DataAtom == atom.A {
		if _, ok := attr(n, "href"); ok {
			return n
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode {
			if l := firstLink(c); l != nil {
				return l
			}
		}
	}
	return nil
}

func findFirst(n *html.Node, match func(*html.Node) bool) *html.Node {
	if n.Type == html.ElementNode && match(n) {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if f := findFirst(c, match); f != nil {
			return f
		}
	}
	return nil
}

// segmenter sorts a page's blocks into chrome, listing and prose regions.
type segmenter struct {
	chrome  map[*html.Node]bool
	listing []*htmlBlock
	prose   []*htmlBlock
}

// walk classifies b. A block is taken whole only when it is uniform; a mixed
// block is split into its children.
func (s *segmenter) walk(b *htmlBlock) {
	if b.text == 0 {
		return
	}
	if isChromeBlock(b) {
		s.chrome[b.node] = true
		return
	}
	if cards := listingCards(b); len(cards) > 0 {
		s.listing = append(s.listing, cards...)
		for _, c := range b.children {
			if !containsBlock(cards, c) {
				s.walk(c)
			}
		}
		return
	}
	if !hasMixedContent(b) && b.text >= proseMinChars && b.linkDensity() < proseLinkDensity {
		s.prose = append(s.prose, b)
		return
	}
	for _, c := range b.children {
		s.walk(c)
	}
}

func containsBlock(list []*htmlBlock, b *htmlBlock) bool {
	for _, x := range list {
		if x == b {
			return true
		}
	}
	return false
}

// hasMixedContent reports whether a descendant of b is chrome or a listing, so
// b cannot be read as one piece of text.
func hasMixedContent(b *htmlBlock) bool {
	for _, c := range b.children {
		if isChromeBlock(c) || len(listingCards(c)) > 0 || hasMixedContent(c) {
			return true
		}
	}
	return false
}

// extractSnapshot turns rendered HTML into a structured page snapshot.
func extractSnapshot(pageURL string, statusCode int, rendered string) (*models.PageSnapshot, error) {
	doc, err := html.Parse(strings.NewReader(rendered))
	if err != nil {
		return nil, fmt.Errorf("parse html: %w", err)
	}
	snap := &models.PageSnapshot{URL: normalizePageURL(pageURL), SourceURL: pageURL, StatusCode: statusCode}
	extractHead(doc, snap)

	body := findFirst(doc, func(n *html.Node) bool { return n.DataAtom == atom.Body })
	if body == nil {
		snap.LowConfidence = true
		return snap, nil
	}
	root := measure(body, false)
	seg := &segmenter{chrome: make(map[*html.Node]bool)}
	seg.walk(root)

	extractProse(seg, snap)
	extractListing(seg, body, snap)
	extractHeadings(body, seg, snap)
	extractLinksAndImages(body, seg, pageURL, snap)

	snap.TotalWordCount = len(strings.Fields(visibleText(body)))
	// Text too short to be a prose block still counts: a thin page must show up
	// as thin, not as empty.
	outside := listingNodes(seg)
	for n := range seg.chrome {
		outside[n] = true
	}
	rest := textOutside(body, outside)
	if len(snap.Prose) == 0 && rest != "" {
		snap.Prose = []string{rest}
	}
	snap.ContentWordCount = len(strings.Fields(rest))
	for _, c := range seg.listing {
		snap.ContentWordCount += len(strings.Fields(visibleText(c.node)))
	}
	snap.Coverage.TotalChars = root.text
	for _, c := range seg.listing {
		snap.Coverage.ListingChars += c.text
	}
	for _, p := range seg.prose {
		snap.Coverage.ProseChars += p.text
	}
	sumChrome(root, seg, &snap.Coverage.ChromeChars)

	// Only a page that lost its main content is untrusted. A page with an H1 and
	// little text is readable, and being thin is a finding about it.
	snap.LowConfidence = !hasContentH1(snap) && (snap.ContentWordCount < minContentWords || len(snap.Prose) == 0)
	snap.Fingerprint = snapshotFingerprint(snap)
	return snap, nil
}

func hasContentH1(s *models.PageSnapshot) bool {
	for _, h := range s.Headings {
		if h.Level == 1 && h.Role == models.HeadingRoleContent {
			return true
		}
	}
	return false
}

func sumChrome(b *htmlBlock, seg *segmenter, total *int) {
	if seg.chrome[b.node] {
		*total += b.text
		return
	}
	for _, c := range b.children {
		sumChrome(c, seg, total)
	}
}

func extractHead(doc *html.Node, snap *models.PageSnapshot) {
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.DataAtom {
			case atom.Html:
				if lang, ok := attr(n, "lang"); ok && snap.Lang == "" {
					snap.Lang = strings.TrimSpace(lang)
				}
			case atom.Title:
				if snap.Title == "" {
					snap.Title = cleanText(rawText(n))
				}
			case atom.Meta:
				name, _ := attr(n, "name")
				prop, _ := attr(n, "property")
				content, _ := attr(n, "content")
				content = cleanText(content)
				switch strings.ToLower(name) {
				case "description":
					snap.MetaDescription = content
				case "robots":
					snap.Robots = content
				}
				switch strings.ToLower(prop) {
				case "og:title":
					snap.OGTitle = content
				case "og:description":
					snap.OGDescription = content
				}
			case atom.Link:
				rel, _ := attr(n, "rel")
				href, _ := attr(n, "href")
				switch strings.ToLower(rel) {
				case "canonical":
					snap.Canonical = href
				case "alternate":
					if lang, ok := attr(n, "hreflang"); ok {
						snap.Hreflang = append(snap.Hreflang, lang)
					}
				}
			case atom.Script:
				if t, _ := attr(n, "type"); strings.EqualFold(t, "application/ld+json") {
					raw := rawText(n)
					snap.SchemaTypes = appendUnique(snap.SchemaTypes, jsonLDTypes(raw)...)
					mergeAvailability(snap, jsonLDAvailability(raw))
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
}

func rawText(n *html.Node) string {
	var sb strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.TextNode {
			sb.WriteString(c.Data)
		}
	}
	return sb.String()
}

// jsonLDTypes collects every @type of a JSON-LD block.
func jsonLDTypes(raw string) []string {
	var v interface{}
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return nil
	}
	var types []string
	var walk func(interface{})
	walk = func(x interface{}) {
		switch t := x.(type) {
		case []interface{}:
			for _, e := range t {
				walk(e)
			}
		case map[string]interface{}:
			switch at := t["@type"].(type) {
			case string:
				types = append(types, at)
			case []interface{}:
				for _, e := range at {
					if s, ok := e.(string); ok {
						types = append(types, s)
					}
				}
			}
			for _, e := range t {
				walk(e)
			}
		}
	}
	walk(v)
	return types
}

// jsonLDAvailability collects every schema.org availability value of a JSON-LD
// block, without the https://schema.org/ prefix.
func jsonLDAvailability(raw string) []string {
	var v interface{}
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return nil
	}
	var values []string
	var walk func(interface{})
	walk = func(x interface{}) {
		switch t := x.(type) {
		case []interface{}:
			for _, e := range t {
				walk(e)
			}
		case map[string]interface{}:
			if s, ok := t["availability"].(string); ok {
				values = append(values, s[strings.LastIndexByte(s, '/')+1:])
			}
			for _, e := range t {
				walk(e)
			}
		}
	}
	walk(v)
	return values
}

// mergeAvailability sets the page's availability: one purchasable offer is
// enough for in_stock.
func mergeAvailability(snap *models.PageSnapshot, values []string) {
	for _, v := range values {
		switch strings.ToLower(v) {
		case "instock", "instoreonly", "onlineonly", "limitedavailability", "preorder", "presale", "backorder":
			snap.Availability = "in_stock"
		case "outofstock", "soldout", "discontinued":
			if snap.Availability == "" {
				snap.Availability = "out_of_stock"
			}
		}
	}
}

func appendUnique(list []string, items ...string) []string {
	for _, it := range items {
		found := false
		for _, x := range list {
			if x == it {
				found = true
				break
			}
		}
		if !found {
			list = append(list, it)
		}
	}
	return list
}

// extractProse keeps each distinct prose block and identifies a listing page's
// descriptive text. A product page's description is prose, not category text,
// so generic hints only count where the listing dominates the page.
func extractProse(seg *segmenter, snap *models.PageSnapshot) {
	listingChars, proseChars := 0, 0
	for _, c := range seg.listing {
		listingChars += c.text
	}
	for _, b := range seg.prose {
		proseChars += b.text
	}
	// Twice the prose, so a product page's related-items grid does not count:
	// measured 12:1 on a category page and about 1:1 on a product page.
	listingPage := listingChars > 0 && listingChars >= 2*proseChars

	seen := make(map[string]bool)
	total := 0
	longest := ""
	for _, b := range seg.prose {
		b = dominantChild(b)
		text := visibleText(b.node)
		if seen[text] {
			continue
		}
		seen[text] = true
		if total+utf8.RuneCountInString(text) > maxProseChars {
			continue
		}
		total += utf8.RuneCountInString(text)
		snap.Prose = append(snap.Prose, text)
		if utf8.RuneCountInString(text) > utf8.RuneCountInString(longest) {
			longest = text
		}
		if snap.CategoryText == "" && (inRegion(b.node, reCategoryText) || (listingPage && inRegion(b.node, reTextRegion))) {
			snap.CategoryText = text
		}
	}
	if snap.CategoryText == "" && listingPage {
		snap.CategoryText = longest
	}
}

// dominantChild descends to the child holding nearly all of b's text, which
// sheds wrapper labels such as a widget's header line.
func dominantChild(b *htmlBlock) *htmlBlock {
	for {
		var next *htmlBlock
		for _, c := range b.children {
			if float64(c.text) >= 0.95*float64(b.text) {
				next = c
				break
			}
		}
		if next == nil {
			return b
		}
		b = next
	}
}

// inRegion reports whether n or a near ancestor has a matching class or id,
// e.g. <div class="category-description"><div class="widgetbody">.
func inRegion(n *html.Node, re *regexp.Regexp) bool {
	for i := 0; n != nil && i < 3; i++ {
		if n.Type == html.ElementNode && re.MatchString(classAndID(n)) {
			return true
		}
		n = n.Parent
	}
	return false
}

// extractListing summarizes the listing cards, counting each linked item once.
func extractListing(seg *segmenter, body *html.Node, snap *models.PageSnapshot) {
	if len(seg.listing) == 0 {
		return
	}
	listing := &models.ListingSummary{PageCount: paginationPages(body)}
	seen := make(map[string]bool)
	brandCount := make(map[string]int)
	for _, c := range seg.listing {
		link := firstLink(c.node)
		if link == nil {
			continue
		}
		href, _ := attr(link, "href")
		if seen[href] {
			continue
		}
		seen[href] = true
		listing.ItemCount++

		name := cardName(c.node)
		if name != "" {
			if b := brandFromName(name); b != "" {
				brandCount[b]++
			}
			if len(listing.SampleItems) < maxSampleItems {
				listing.SampleItems = appendUnique(listing.SampleItems, name)
			}
		}
		if price, currency, ok := cardPrice(visibleText(c.node)); ok {
			listing.Currency = currency
			if listing.PriceMin == 0 || price < listing.PriceMin {
				listing.PriceMin = price
			}
			if price > listing.PriceMax {
				listing.PriceMax = price
			}
		}
	}
	listing.Brands = topKeys(brandCount, maxListingBrands)
	snap.Listing = listing
}

// cardName prefers a card's heading, then its first non-empty link text.
func cardName(n *html.Node) string {
	if h := findFirst(n, func(x *html.Node) bool {
		switch x.DataAtom {
		case atom.H2, atom.H3, atom.H4:
			return true
		}
		return false
	}); h != nil {
		if t := visibleText(h); t != "" {
			return t
		}
	}
	if a := findFirst(n, func(x *html.Node) bool { return x.DataAtom == atom.A && visibleText(x) != "" }); a != nil {
		return visibleText(a)
	}
	return ""
}

// brandFromName returns the leading tokens that carry an uppercase letter:
// "Marco Tozzi női csizma" gives "Marco Tozzi", "s.Oliver női" gives "s.Oliver".
func brandFromName(name string) string {
	var brand []string
	for _, tok := range strings.Fields(name) {
		hasUpper := false
		for _, r := range tok {
			if unicode.IsUpper(r) {
				hasUpper = true
				break
			}
		}
		if !hasUpper {
			break
		}
		brand = append(brand, tok)
	}
	return strings.Join(brand, " ")
}

// cardPrice returns the lowest price of a card: the current one when a card
// also shows the original price.
func cardPrice(text string) (float64, string, bool) {
	lowest, currency := 0.0, ""
	for _, m := range rePrice.FindAllStringSubmatch(text, -1) {
		amount, cur := m[1], m[2]
		if amount == "" {
			cur, amount = m[3], m[4]
		}
		v := parseAmount(amount)
		if v > 0 && (lowest == 0 || v < lowest) {
			lowest, currency = v, strings.ToUpper(cur)
		}
	}
	return lowest, currency, lowest > 0
}

var reAmountDecimals = regexp.MustCompile(`[.,]\d{1,2}$`)

// parseAmount reads "1 299", "1.299,50" and "1,299.50" alike. Decimals are
// dropped: prices only serve as a range for the page.
func parseAmount(s string) float64 {
	digits := strings.NewReplacer(" ", "", ".", "", ",", "", "\u00a0", "").Replace(reAmountDecimals.ReplaceAllString(s, ""))
	v, err := strconv.ParseFloat(digits, 64)
	if err != nil {
		return 0
	}
	return v
}

func topKeys(counts map[string]int, n int) []string {
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if counts[keys[i]] != counts[keys[j]] {
			return counts[keys[i]] > counts[keys[j]]
		}
		return keys[i] < keys[j]
	})
	if len(keys) > n {
		keys = keys[:n]
	}
	return keys
}

// paginationPages returns the highest page number linked from the page, read
// from page query parameters such as ?p=8 or ?page=8.
func paginationPages(body *html.Node) int {
	highest := 1
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.DataAtom == atom.A {
			if href, ok := attr(n, "href"); ok {
				if u, err := url.Parse(href); err == nil {
					for _, key := range []string{"p", "page", "pg", "paged", "oldal", "seite", "pagina", "strona"} {
						if v, convErr := strconv.Atoi(u.Query().Get(key)); convErr == nil && v > highest && v < 10000 {
							highest = v
						}
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(body)
	return highest
}

// regionRole returns the role of the region n sits in. Listing cards are
// reported as "listing" so their repeated headings stay out of the outline.
func regionRole(n *html.Node, seg *segmenter, cards map[*html.Node]bool) string {
	for cur := n; cur != nil; cur = cur.Parent {
		if cards[cur] {
			return "listing"
		}
		if seg.chrome[cur] {
			if cur.DataAtom != atom.Body && reFilterRegion.MatchString(classAndID(cur)) {
				return models.HeadingRoleFilter
			}
			return models.HeadingRoleNav
		}
	}
	return models.HeadingRoleContent
}

func listingNodes(seg *segmenter) map[*html.Node]bool {
	cards := make(map[*html.Node]bool, len(seg.listing))
	for _, c := range seg.listing {
		cards[c.node] = true
	}
	return cards
}

// extractHeadings records H1-H3 outside listing cards, each with its region.
// A heading inside a linked list of filters is not a content heading even if
// it uses a heading tag.
func extractHeadings(body *html.Node, seg *segmenter, snap *models.PageSnapshot) {
	cards := listingNodes(seg)
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && isInvisible(n) {
			return
		}
		level := 0
		switch n.DataAtom {
		case atom.H1:
			level = 1
		case atom.H2:
			level = 2
		case atom.H3:
			level = 3
		}
		if level > 0 && len(snap.Headings) < maxSnapshotHeadings {
			role := regionRole(n, seg, cards)
			text := visibleText(n)
			if text != "" && role != "listing" {
				snap.Headings = append(snap.Headings, models.PageHeading{Level: level, Text: text, Role: role})
			}
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(body)
}

// extractLinksAndImages records same-site links found outside site chrome and
// listing cards, plus image alt coverage for the whole page.
func extractLinksAndImages(body *html.Node, seg *segmenter, pageURL string, snap *models.PageSnapshot) {
	base := documentBase(body, pageURL)
	cards := listingNodes(seg)
	seen := make(map[string]bool)
	graph := make(map[string]bool)
	self := normalizePageURL(pageURL)
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.DataAtom == atom.A && len(snap.Links) < maxGraphLinks {
			href, _ := attr(n, "href")
			if resolved := internalLink(base, href); resolved != "" {
				if key := normalizePageURL(resolved); key != self && !graph[key] {
					graph[key] = true
					snap.Links = append(snap.Links, resolved)
				}
			}
		}
		if n.Type == html.ElementNode {
			switch n.DataAtom {
			case atom.Img:
				snap.ImagesTotal++
				if alt, _ := attr(n, "alt"); strings.TrimSpace(alt) == "" {
					snap.ImagesMissingAlt++
				}
			case atom.A:
				href, _ := attr(n, "href")
				if len(snap.ContentLinks) < maxSnapshotLinks && regionRole(n, seg, cards) == models.HeadingRoleContent {
					if resolved := internalLink(base, href); resolved != "" && !seen[resolved] {
						if anchor := visibleText(n); isContentAnchor(anchor) && !samePage(resolved, pageURL) {
							seen[resolved] = true
							snap.ContentLinks = append(snap.ContentLinks, models.PageLink{Href: resolved, Anchor: anchor})
						}
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(body)
}

// documentBase is what relative links resolve against: the page URL, or the
// <base href> of its head, as a browser reads it.
func documentBase(body *html.Node, pageURL string) *url.URL {
	page, err := url.Parse(pageURL)
	if err != nil {
		return nil
	}
	root := body
	for root.Parent != nil {
		root = root.Parent
	}
	if b := findFirst(root, func(n *html.Node) bool { return n.DataAtom == atom.Base }); b != nil {
		if href, ok := attr(b, "href"); ok {
			if ref, refErr := url.Parse(strings.TrimSpace(href)); refErr == nil {
				return page.ResolveReference(ref)
			}
		}
	}
	return page
}

// isContentAnchor rejects anchors that say nothing, like pagination numbers.
func isContentAnchor(anchor string) bool {
	if utf8.RuneCountInString(anchor) < 3 {
		return false
	}
	_, err := strconv.Atoi(anchor)
	return err != nil
}

// samePage reports whether two URLs differ only in their query, as pagination
// and sort links do.
func samePage(a, b string) bool {
	ua, errA := url.Parse(a)
	ub, errB := url.Parse(b)
	return errA == nil && errB == nil && sameSite(ua.Host, ub.Host) && pagePath(a) == pagePath(b)
}

func internalLink(base *url.URL, href string) string {
	href = strings.TrimSpace(href)
	if base == nil || href == "" || strings.HasPrefix(href, "#") {
		return ""
	}
	u, err := url.Parse(href)
	if err != nil {
		return ""
	}
	r := base.ResolveReference(u)
	if (r.Scheme != "http" && r.Scheme != "https") || !sameSite(r.Host, base.Host) {
		return ""
	}
	r.Fragment = ""
	return r.String()
}

func sameSite(a, b string) bool {
	return strings.TrimPrefix(strings.ToLower(a), "www.") == strings.TrimPrefix(strings.ToLower(b), "www.")
}

// snapshotFingerprint hashes what an analysis depends on. Listing prices and
// item samples change daily and are left out on purpose.
func snapshotFingerprint(s *models.PageSnapshot) string {
	h := sha256.New()
	write := func(parts ...string) {
		for _, p := range parts {
			_, _ = io.WriteString(h, p)
			_, _ = h.Write([]byte{0})
		}
	}
	write(strconv.Itoa(s.StatusCode), s.Title, s.MetaDescription, s.Canonical, s.Robots, s.CategoryText)
	write(s.Prose...)
	for _, hd := range s.Headings {
		if hd.Role == models.HeadingRoleContent {
			write(strconv.Itoa(hd.Level), hd.Text)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// normalizePageURL maps the URL spellings used by GSC, crawls and GA4 to one
// key: lowercase host without www, no fragment, no trailing slash.
func normalizePageURL(raw string) string {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	u.Scheme = "https"
	u.Host = strings.TrimPrefix(strings.ToLower(u.Host), "www.")
	u.Fragment = ""
	if p := strings.TrimRight(u.EscapedPath(), "/"); p == "" {
		u.Path, u.RawPath = "/", ""
	} else if decoded, decErr := url.PathUnescape(p); decErr == nil {
		u.Path, u.RawPath = decoded, ""
	}
	return u.String()
}

// NormalizePageURL maps the spellings of one page URL to one key.
func NormalizePageURL(raw string) string { return normalizePageURL(raw) }

// pagePath returns the path of a page URL, the key used by GA4 rows.
func pagePath(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return raw
	}
	if p := strings.TrimRight(u.Path, "/"); p != "" {
		return p
	}
	return "/"
}

// loadSnapshot returns the project's cached snapshot of a page, or nil.
func (p *Processor) loadSnapshot(ctx context.Context, projectOID bson.ObjectID, pageURL string) (*models.PageSnapshot, error) {
	var snap models.PageSnapshot
	err := p.db.Collection("page_snapshots").FindOne(ctx, bson.M{
		"project_id": projectOID,
		"url":        normalizePageURL(pageURL),
		"version":    snapshotVersion,
		"expires_at": bson.M{"$gt": time.Now().UTC()},
	}).Decode(&snap)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &snap, nil
}

// getSnapshot returns a fresh snapshot of a page, rendering it only when the
// cache has none. A render waits for the pacer so the site's crawl delay holds.
func (p *Processor) getSnapshot(ctx context.Context, projectOID bson.ObjectID, pageURL string, useJS bool, pacer *renderPacer) (*models.PageSnapshot, error) {
	cached, err := p.loadSnapshot(ctx, projectOID, pageURL)
	if err != nil {
		return nil, fmt.Errorf("load snapshot: %w", err)
	}
	if cached != nil {
		return cached, nil
	}
	return p.readPage(ctx, projectOID, pageURL, useJS, pacer)
}

// readPage renders a page now, ignoring the cache, and stores its snapshot and
// profile.
func (p *Processor) readPage(ctx context.Context, projectOID bson.ObjectID, pageURL string, useJS bool, pacer *renderPacer) (*models.PageSnapshot, error) {
	return p.readPageThen(ctx, projectOID, pageURL, useJS, pacer, nil)
}

// readPageThen is readPage with a callback that gets the render.
func (p *Processor) readPageThen(ctx context.Context, projectOID bson.ObjectID, pageURL string, useJS bool, pacer *renderPacer, onRender func(*renderedPage)) (*models.PageSnapshot, error) {
	if err := pacer.wait(ctx); err != nil {
		return nil, err
	}
	page, err := p.fetchPage(ctx, pageURL, useJS)
	if err != nil {
		return nil, err
	}
	if onRender != nil {
		onRender(page)
	}
	snap, err := extractSnapshot(pageURL, page.Status, page.HTML)
	if err != nil {
		return nil, err
	}
	// A JavaScript render is what bots get from /prerender: a page they ask
	// for is refreshed in the cache at no extra cost.
	if useJS {
		p.refreshCachedRender(ctx, projectOID, pageURL, page)
	}
	snap.FinalURL, snap.Redirects, snap.XRobotsTag, snap.RenderMs = page.FinalURL, page.Redirects, page.XRobotsTag, page.RenderMs
	now := time.Now().UTC()
	snap.ProjectID = projectOID
	snap.Version = snapshotVersion
	snap.FetchedAt = now
	snap.ExpiresAt = now.Add(snapshotTTL)
	if _, err = p.db.Collection("page_snapshots").ReplaceOne(ctx,
		bson.M{"project_id": projectOID, "url": snap.URL}, snap, options.Replace().SetUpsert(true),
	); err != nil {
		return nil, fmt.Errorf("store snapshot: %w", err)
	}
	// The 404 probe is no page of the site, so it gets no profile.
	if strings.Contains(snap.URL, notFoundPath) {
		return snap, nil
	}
	if err = p.saveProfile(ctx, snap, now); err != nil {
		return nil, fmt.Errorf("store profile: %w", err)
	}
	return snap, nil
}

// renderPacer spaces renders of one site and caps how many a run may do.
type renderPacer struct {
	delay     time.Duration
	remaining int
	last      time.Time
}

var errRenderBudget = errors.New("render budget of this run is used up")

func (r *renderPacer) wait(ctx context.Context) error {
	if r.remaining <= 0 {
		return errRenderBudget
	}
	r.remaining--
	if pause := r.delay - time.Since(r.last); !r.last.IsZero() && pause > 0 {
		select {
		case <-time.After(pause):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	r.last = time.Now()
	return nil
}

const (
	defaultCrawlDelay = 1 * time.Second
	maxCrawlDelay     = 30 * time.Second
)

// parseCrawlDelay reads the Crawl-delay of the "*" group of a robots.txt.
func parseCrawlDelay(robots string) time.Duration {
	inStarGroup := false
	for _, line := range strings.Split(robots, "\n") {
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
			inStarGroup = value == "*"
		case "crawl-delay":
			if inStarGroup {
				if secs, err := strconv.ParseFloat(value, 64); err == nil && secs > 0 {
					d := time.Duration(secs * float64(time.Second))
					if d > maxCrawlDelay {
						return maxCrawlDelay
					}
					return d
				}
			}
		}
	}
	return defaultCrawlDelay
}

// siteCrawlDelay fetches the crawl delay of a site; any failure means default.
func (p *Processor) siteCrawlDelay(ctx context.Context, siteURL string) time.Duration {
	u, err := url.Parse(siteURL)
	if err != nil || u.Host == "" {
		return defaultCrawlDelay
	}
	robotsURL := url.URL{Scheme: u.Scheme, Host: u.Host, Path: "/robots.txt"}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, robotsURL.String(), nil)
	if err != nil {
		return defaultCrawlDelay
	}
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return defaultCrawlDelay
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return defaultCrawlDelay
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return defaultCrawlDelay
	}
	return parseCrawlDelay(string(body))
}
