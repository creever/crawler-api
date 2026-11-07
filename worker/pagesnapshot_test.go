package worker

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/creever/crawler-api/models"
)

func cipomarketSnapshot(t *testing.T) *models.PageSnapshot {
	t.Helper()
	raw, err := os.ReadFile("testdata/cipomarket_category.html")
	if err != nil {
		t.Fatal(err)
	}
	snap, err := extractSnapshot("https://cipomarket.hu/noi/bokacsizma", 200, string(raw))
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func TestSnapshotCategoryPageHead(t *testing.T) {
	s := cipomarketSnapshot(t)
	if s.Title != "Női bokacsizma webáruház | cipomarket.hu" {
		t.Errorf("title = %q", s.Title)
	}
	if !strings.HasPrefix(s.MetaDescription, "Nézd meg Te is webáruházunk") {
		t.Errorf("meta = %q", s.MetaDescription)
	}
	if s.Canonical != "https://cipomarket.hu/noi/bokacsizma" {
		t.Errorf("canonical = %q", s.Canonical)
	}
}

func TestSnapshotFindsExistingCategoryText(t *testing.T) {
	s := cipomarketSnapshot(t)
	if !strings.HasPrefix(s.CategoryText, "A télről, hidegről kinek ne jutna") {
		t.Fatalf("category text = %.80q", s.CategoryText)
	}
	if strings.Contains(s.CategoryText, "Elérhetőségek") {
		t.Errorf("footer leaked into category text: %q", s.CategoryText)
	}
}

func TestSnapshotExcludesSiteChrome(t *testing.T) {
	s := cipomarketSnapshot(t)
	joined := strings.Join(s.Prose, " ")
	for _, menu := range []string{"Népszerű márkák", "Bejelentkezés", "Mérettáblázat", "Adatvédelmi nyilatkozat"} {
		if strings.Contains(joined, menu) {
			t.Errorf("site chrome %q leaked into prose", menu)
		}
	}
	if s.Coverage.ChromeChars == 0 {
		t.Error("expected chrome to be counted")
	}
	if s.ContentWordCount >= s.TotalWordCount {
		t.Errorf("content words %d should be below total %d", s.ContentWordCount, s.TotalWordCount)
	}
}

func TestSnapshotSummarizesListing(t *testing.T) {
	s := cipomarketSnapshot(t)
	l := s.Listing
	if l == nil {
		t.Fatal("no listing found")
	}
	if l.ItemCount != 60 {
		t.Errorf("item count = %d, want 60 unique cards", l.ItemCount)
	}
	if l.PageCount != 8 {
		t.Errorf("page count = %d, want 8", l.PageCount)
	}
	if l.Currency != "FT" || l.PriceMin <= 0 || l.PriceMax < l.PriceMin {
		t.Errorf("prices = %v-%v %s", l.PriceMin, l.PriceMax, l.Currency)
	}
	found := false
	for _, b := range l.Brands {
		found = found || b == "Skechers"
	}
	if !found {
		t.Errorf("brands = %v", l.Brands)
	}
	if len(l.SampleItems) == 0 || !strings.Contains(l.SampleItems[0], "női bokacsizma") {
		t.Errorf("samples = %v", l.SampleItems)
	}
}

func TestSnapshotMarksFilterHeadings(t *testing.T) {
	s := cipomarketSnapshot(t)
	var h1, skechers *models.PageHeading
	for i := range s.Headings {
		h := &s.Headings[i]
		if h.Level == 1 {
			h1 = h
		}
		if h.Text == "Skechers" {
			skechers = h
		}
	}
	if h1 == nil || h1.Text != "Női bokacsizma" || h1.Role != models.HeadingRoleContent {
		t.Errorf("h1 = %+v", h1)
	}
	if skechers == nil || skechers.Role == models.HeadingRoleContent {
		t.Errorf("brand filter heading = %+v", skechers)
	}
}

func TestSnapshotArticlePageKeepsProse(t *testing.T) {
	page := `<html><head><title>Cikk</title></head><body>
	<ul class="menu">` + strings.Repeat(`<li><a href="/x">Menüpont neve</a></li>`, 10) + `</ul>
	<main><h1>Hogyan válassz cipőt</h1>
	<p>` + strings.Repeat("A megfelelő cipő kiválasztása fontos döntés mindenkinek. ", 12) + `</p></main>
	<div style="display:none">rejtett szöveg ami nem látszik</div>
	<script>var x = "szkript szöveg";</script></body></html>`
	s, err := extractSnapshot("https://example.com/cikk/", 200, page)
	if err != nil {
		t.Fatal(err)
	}
	if s.Listing != nil {
		t.Errorf("unexpected listing %+v", s.Listing)
	}
	if s.CategoryText != "" {
		t.Errorf("article must not get category text: %q", s.CategoryText)
	}
	joined := strings.Join(s.Prose, " ")
	if !strings.Contains(joined, "megfelelő cipő") || strings.Contains(joined, "Menüpont") ||
		strings.Contains(joined, "rejtett") || strings.Contains(joined, "szkript") {
		t.Errorf("prose = %q", joined)
	}
	if s.LowConfidence {
		t.Error("article should be trusted")
	}
}

func fixtureSnapshot(t *testing.T, file, pageURL string) *models.PageSnapshot {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + file)
	if err != nil {
		t.Fatal(err)
	}
	s, err := extractSnapshot(pageURL, 200, string(raw))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSnapshotProductPage(t *testing.T) {
	s := fixtureSnapshot(t, "cipomarket_product.html", "https://cipomarket.hu/bokacsizma-skechers-168216-choc-barna-96790")
	if s.LowConfidence {
		t.Fatal("the product page must be trusted")
	}
	if !hasContentH1(s) {
		t.Errorf("product H1 not recognized as content: %+v", s.Headings)
	}
	if !strings.Contains(strings.Join(s.Prose, " "), "Warm Tech Memory Foam") {
		t.Errorf("product description lost, prose = %.300q", s.Prose)
	}
	if s.CategoryText != "" {
		t.Errorf("a product description is not category text: %.100q", s.CategoryText)
	}
}

func TestSnapshotThinProductPageIsReadable(t *testing.T) {
	s := fixtureSnapshot(t, "cipomarket_product_thin.html", "https://cipomarket.hu/cipo-nike-ferfi-utcai-cipo-feher-44123")
	if s.LowConfidence {
		t.Fatalf("a thin page is a finding, not a blocked page: %+v", s.Coverage)
	}
	joined := strings.Join(s.Prose, " ")
	for _, want := range []string{"A termék jelenleg nem elérhető", "Nike Férfi fehér Utcai cipö"} {
		if !strings.Contains(joined, want) {
			t.Errorf("main column lost %q, prose = %.300q", want, joined)
		}
	}
	if strings.Contains(joined, "Népszerű márkák") {
		t.Error("site chrome leaked into prose")
	}
	if s.ContentWordCount == 0 {
		t.Error("content words must be counted")
	}
}

func TestSnapshotHeadingWithShortTextStaysReadable(t *testing.T) {
	s, err := extractSnapshot("https://example.com/p", 200, `<html><body><h1>Nike cipő</h1><p>Rövid leírás.</p></body></html>`)
	if err != nil {
		t.Fatal(err)
	}
	if s.LowConfidence || len(s.Prose) != 1 || s.ContentWordCount == 0 {
		t.Errorf("snapshot = %+v", s)
	}
}

func TestSnapshotLinkHeavyAsideIsChrome(t *testing.T) {
	links := strings.Repeat(`<li><a href="/x">Kapcsolódó oldal neve</a></li>`, 8)
	s, err := extractSnapshot("https://example.com/p", 200, `<html><body><h1>Cím</h1><aside><ul>`+links+`</ul></aside><p>`+strings.Repeat("Tartalmi szöveg az oldalról. ", 12)+`</p></body></html>`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(s.Prose, " "), "Kapcsolódó") {
		t.Errorf("sidebar leaked into prose: %q", s.Prose)
	}
}

func TestSnapshotHomePage(t *testing.T) {
	s := fixtureSnapshot(t, "cipomarket_home.html", "https://cipomarket.hu/")
	if s.LowConfidence {
		t.Fatal("the home page must be trusted")
	}
	if !strings.Contains(strings.Join(s.Prose, " "), "Miért érdemes") {
		t.Errorf("prose = %.300q", s.Prose)
	}
	if strings.Contains(strings.Join(s.Prose, " "), "Bejelentkezés") {
		t.Error("site chrome leaked into prose")
	}
}

func TestSnapshotUnrecognizedMainColumnIsLowConfidence(t *testing.T) {
	cards := strings.Repeat(`<div class="card"><h3><a href="/t">Termék neve hosszabban leírva itt</a></h3><span>12.990 Ft</span><p>Rövid leírás a termékről néhány szóban elmondva neked most</p></div>`, 6)
	page := `<html><body><nav class="x"><h1>Cím</h1></nav><div class="list">` + strings.ReplaceAll(cards, `/t"`, `/t?a"`) + `</div></body></html>`
	s, err := extractSnapshot("https://example.com/p", 200, page)
	if err != nil {
		t.Fatal(err)
	}
	if !s.LowConfidence {
		t.Errorf("no heading and no text must not be trusted: %+v", s)
	}
}

func TestSnapshotEmptyShellIsLowConfidence(t *testing.T) {
	s, err := extractSnapshot("https://example.com/", 200, `<html><body><div id="app"></div></body></html>`)
	if err != nil {
		t.Fatal(err)
	}
	if !s.LowConfidence {
		t.Error("an empty SPA shell must not be trusted")
	}
}

func TestFingerprintIgnoresListingChurn(t *testing.T) {
	a := &models.PageSnapshot{Title: "T", CategoryText: "szöveg"}
	b := &models.PageSnapshot{Title: "T", CategoryText: "szöveg",
		Listing: &models.ListingSummary{ItemCount: 5, PriceMin: 100}}
	c := &models.PageSnapshot{Title: "T2", CategoryText: "szöveg"}
	if snapshotFingerprint(a) != snapshotFingerprint(b) {
		t.Error("listing churn must not change the fingerprint")
	}
	if snapshotFingerprint(a) == snapshotFingerprint(c) {
		t.Error("title change must change the fingerprint")
	}
}

func TestNormalizePageURL(t *testing.T) {
	cases := map[string]string{
		"https://www.Cipomarket.hu/noi/cipo/": "https://cipomarket.hu/noi/cipo",
		"http://cipomarket.hu/noi/cipo#top":   "https://cipomarket.hu/noi/cipo",
		"https://cipomarket.hu":               "https://cipomarket.hu/",
		"https://cipomarket.hu/":              "https://cipomarket.hu/",
		"https://cipomarket.hu/noi/cipo/?p=2": "https://cipomarket.hu/noi/cipo?p=2",
	}
	for in, want := range cases {
		if got := normalizePageURL(in); got != want {
			t.Errorf("normalizePageURL(%q) = %q, want %q", in, got, want)
		}
	}
	if got := pagePath("https://cipomarket.hu/noi/cipo/?p=2"); got != "/noi/cipo" {
		t.Errorf("pagePath = %q", got)
	}
	if got := pagePath("https://cipomarket.hu"); got != "/" {
		t.Errorf("pagePath root = %q", got)
	}
}

func TestParseCrawlDelay(t *testing.T) {
	cases := []struct {
		robots string
		want   time.Duration
	}{
		{"User-agent: *\nCrawl-delay: 4\n\nDisallow: /admin/\n", 4 * time.Second},
		{"User-agent: Googlebot\nCrawl-delay: 9\n", defaultCrawlDelay},
		{"User-agent: *\nCrawl-delay: 900\n", maxCrawlDelay},
		{"", defaultCrawlDelay},
	}
	for _, c := range cases {
		if got := parseCrawlDelay(c.robots); got != c.want {
			t.Errorf("parseCrawlDelay(%q) = %v, want %v", c.robots, got, c.want)
		}
	}
}

// A news section: nearly all of its text is links, and it has no H1 or prices.
func TestLinkHeavyPageWithoutH1(t *testing.T) {
	var cards strings.Builder
	for i := 0; i < 14; i++ {
		fmt.Fprintf(&cards, `<article class="teaser"><a href="/news/%d"><h3>Headline number %d about something that happened today</h3></a></article>`, i, i)
	}
	page := `<html lang="hu"><body><nav><a href="/a">Section one</a><a href="/b">Section two</a></nav><main><div class="list">` + cards.String() + `</div></main></body></html>`
	snap, err := extractSnapshot("https://news.example/section", 200, page)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Listing == nil || snap.Listing.ItemCount != 14 {
		t.Fatalf("listing: %+v", snap.Listing)
	}
	if !isListingPage(snap) || snap.Coverage.ChromeChars >= snap.Coverage.TotalChars/2 {
		t.Fatalf("the list was read as navigation: %+v", snap.Coverage)
	}
}
