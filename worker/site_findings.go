package worker

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/creever/crawler-api/models"
)

const (
	// minPatternPages is the least number of pages that makes a pattern.
	minPatternPages = 3
	// minDuplicatePages is lower: two pages with one title is already a defect.
	minDuplicatePages = 2
	// minTypePages is the least number of affected pages of one page type that
	// earns a task of its own. Below it, a page type is a detail of the site and
	// its pages join one site-level task, or a small site would get several
	// tasks for what is one problem.
	minTypePages = 5
	// sharedPhraseWords is the shortest run of words counted as shared wording.
	sharedPhraseWords = 5
	// sharedPhraseShare is the part of the pages a phrase has to appear on.
	sharedPhraseShare  = 0.3
	maxSharedPhrases   = 3
	findingExamples    = 5
	phraseOverlapShare = 0.8
)

// siteFinding is a pattern found in code across page profiles. Nothing in it
// depends on the site: it compares what pages have in common.
type siteFinding struct {
	Issue, KeyPart, Scope, Group string
	Title, Problem, Goal         string
	Confidence                   string
	Pages                        []models.PageProfile
	Examples                     []models.TaskExample
	Avoid                        []string
	Checks                       []models.TaskCheck
}

// taskKey is stable across runs, so a finding updates its task.
func taskKey(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:12])
}

var reToken = regexp.MustCompile(`[\p{L}\p{N}]+`)

func tokens(s string) []string {
	return reToken.FindAllString(strings.ToLower(s), -1)
}

// normalizedText compares text by its words only, so punctuation or an emoji
// moving around doesn't count as a change.
func normalizedText(s string) string {
	return " " + strings.Join(tokens(s), " ") + " "
}

// indexablePages keeps the pages whose texts compete in search. A page
// canonicalized elsewhere may repeat its target on purpose.
func indexablePages(profiles []models.PageProfile) []models.PageProfile {
	var out []models.PageProfile
	for _, pr := range profiles {
		if pr.StatusCode >= 400 || strings.Contains(strings.ToLower(pr.Robots), "noindex") {
			continue
		}
		if pr.Canonical != "" && normalizePageURL(resolveAgainst(pr.SourceURL, pr.Canonical)) != pr.URL {
			continue
		}
		out = append(out, pr)
	}
	return out
}

// byPageType splits pages by group. Large groups get a finding each, because
// one page type usually has one source. The rest counts together as one
// site-level finding.
func byPageType(pages []models.PageProfile) (map[string][]models.PageProfile, []models.PageProfile) {
	all := make(map[string][]models.PageProfile)
	for _, pr := range pages {
		all[pr.Group] = append(all[pr.Group], pr)
	}
	groups := make(map[string][]models.PageProfile)
	var rest []models.PageProfile
	for _, g := range sortedKeys(all) {
		if len(all[g]) >= minTypePages {
			groups[g] = all[g]
		} else {
			rest = append(rest, all[g]...)
		}
	}
	return groups, rest
}

// perPageType emits one finding per page type, then one for the remaining pages.
func perPageType(pages []models.PageProfile, min int, build func(group string, pages []models.PageProfile) siteFinding) []siteFinding {
	groups, rest := byPageType(pages)
	var out []siteFinding
	for _, g := range sortedKeys(groups) {
		f := build(g, groups[g])
		f.Scope, f.Group, f.KeyPart = models.TaskScopeGroup, g, g
		out = append(out, f)
	}
	if len(rest) >= min {
		f := build("", rest)
		f.Scope, f.KeyPart = models.TaskScopeSite, "site"
		out = append(out, f)
	}
	return out
}

func where(pages []models.PageProfile, keep func(models.PageProfile) bool) []models.PageProfile {
	var out []models.PageProfile
	for _, pr := range pages {
		if keep(pr) {
			out = append(out, pr)
		}
	}
	return out
}

func fieldExamples(pages []models.PageProfile, field string, value func(models.PageProfile) string) []models.TaskExample {
	var out []models.TaskExample
	for i, pr := range pages {
		if i == findingExamples {
			break
		}
		out = append(out, models.TaskExample{Page: pr.SourceURL, Field: field, Current: value(pr)})
	}
	return out
}

func profileTitle(pr models.PageProfile) string { return pr.Title }
func profileMeta(pr models.PageProfile) string  { return pr.MetaDescription }
func profileH1(pr models.PageProfile) string    { return pr.H1 }
func profileText(pr models.PageProfile) string  { return pr.CategoryText }

// missingField reports pages lacking a field.
func missingField(pages []models.PageProfile, issue, field, label string, value func(models.PageProfile) string) []siteFinding {
	missing := where(pages, func(pr models.PageProfile) bool { return strings.TrimSpace(value(pr)) == "" })
	return perPageType(missing, minPatternPages, func(group string, ps []models.PageProfile) siteFinding {
		return siteFinding{
			Issue: issue, Confidence: "high", Pages: ps,
			Title:    fmt.Sprintf("Missing %s on %s", label, groupLabel(group)),
			Problem:  fmt.Sprintf("%d pages have no %s in their rendered HTML.", len(ps), label),
			Goal:     fmt.Sprintf("Every page of this type outputs a %s that describes that page.", label),
			Examples: fieldExamples(ps, field, value),
			Checks:   []models.TaskCheck{{Type: models.CheckNotEmpty, Field: field}},
		}
	})
}

// tooLong reports fields over their display limit.
func tooLong(pages []models.PageProfile, issue, field, label string, limit int, value func(models.PageProfile) string) []siteFinding {
	long := where(pages, func(pr models.PageProfile) bool { return utf8.RuneCountInString(value(pr)) > limit })
	return perPageType(long, minPatternPages, func(group string, ps []models.PageProfile) siteFinding {
		return siteFinding{
			Issue: issue, Confidence: "high", Pages: ps,
			Title:    fmt.Sprintf("%s over %d characters on %s", capitalize(label), limit, groupLabel(group)),
			Problem:  fmt.Sprintf("%d pages have a %s longer than %d characters, so search results can cut it off.", len(ps), label, limit),
			Goal:     fmt.Sprintf("The %s fits in %d characters on these pages, with the page-specific words first.", label, limit),
			Examples: fieldExamples(ps, field, value),
			Checks:   []models.TaskCheck{{Type: models.CheckMaxLength, Field: field, Limit: limit}},
		}
	})
}

// duplicateField reports pages sharing an identical value. All clusters of a
// page type form one finding: thirty pairs of equal titles are one defect.
func duplicateField(pages []models.PageProfile, issue, field, label string, value func(models.PageProfile) string) []siteFinding {
	clusters := make(map[string][]models.PageProfile)
	for _, pr := range pages {
		if key := normalizedText(value(pr)); strings.TrimSpace(key) != "" {
			clusters[key] = append(clusters[key], pr)
		}
	}
	var dup []models.PageProfile
	for _, key := range sortedKeys(clusters) {
		if len(clusters[key]) >= minDuplicatePages {
			dup = append(dup, clusters[key]...)
		}
	}
	findings := perPageType(dup, minDuplicatePages, func(group string, ps []models.PageProfile) siteFinding {
		distinct := make(map[string]bool)
		for _, pr := range ps {
			distinct[normalizedText(value(pr))] = true
		}
		return siteFinding{
			Issue: issue, Confidence: "high", Pages: ps,
			Title:    fmt.Sprintf("Duplicate %s on %s", label, groupLabel(group)),
			Problem:  fmt.Sprintf("%d pages share %d identical %s value(s), so search results cannot tell them apart.", len(ps), len(distinct), label),
			Goal:     fmt.Sprintf("Each page has its own %s, describing what that page offers.", label),
			Examples: fieldExamples(ps, field, value),
			Checks:   []models.TaskCheck{{Type: models.CheckUnique, Field: field}},
		}
	})
	return withStrayDuplicates(findings, dup, value)
}

// withStrayDuplicates adds pages whose page type was too small for a finding of
// its own to the finding holding their twin. A home page sharing its title with
// two inner pages is part of that defect.
func withStrayDuplicates(findings []siteFinding, dup []models.PageProfile, value func(models.PageProfile) string) []siteFinding {
	placed := make(map[string]int)
	for i, f := range findings {
		for _, pr := range f.Pages {
			placed[normalizedText(value(pr))] = i
		}
	}
	covered := make(map[string]bool)
	for _, f := range findings {
		for _, pr := range f.Pages {
			covered[pr.URL] = true
		}
	}
	for _, pr := range dup {
		if i, ok := placed[normalizedText(value(pr))]; ok && !covered[pr.URL] {
			findings[i].Pages = append(findings[i].Pages, pr)
			covered[pr.URL] = true
		}
	}
	return findings
}

// sharedPhrase is wording found across pages. Seed is a run of exactly
// sharedPhraseWords words that every page has; Text is the seed extended as far
// as all of those pages agree.
type sharedPhrase struct {
	Seed, Text string
	Pages      []models.PageProfile
}

func containsWords(words []string, seed []string) int {
	for i := 0; i+len(seed) <= len(words); i++ {
		match := true
		for j := range seed {
			match = match && words[i+j] == seed[j]
		}
		if match {
			return i
		}
	}
	return -1
}

// extendPhrase grows seed in both directions while all pages agree.
func extendPhrase(seed []string, texts [][]string) []string {
	starts := make([]int, len(texts))
	for i, words := range texts {
		starts[i] = containsWords(words, seed)
	}
	before, after := 0, 0
	for grew := true; grew; {
		grew = false
		agree := func(offset func(start int) int) bool {
			word := ""
			for i, words := range texts {
				at := offset(starts[i])
				if at < 0 || at >= len(words) || (word != "" && words[at] != word) {
					return false
				}
				word = words[at]
			}
			return true
		}
		if agree(func(s int) int { return s - before - 1 }) {
			before++
			grew = true
		}
		if agree(func(s int) int { return s + len(seed) + after }) {
			after++
			grew = true
		}
	}
	first := texts[0]
	return first[starts[0]-before : starts[0]+len(seed)+after]
}

// findSharedPhrases returns the wording repeated across a share of the pages,
// strongest first. Phrases covering about the same pages are one template and
// count once.
func findSharedPhrases(pages []models.PageProfile, value func(models.PageProfile) string) []sharedPhrase {
	with := where(pages, func(pr models.PageProfile) bool { return value(pr) != "" })
	threshold := int(math.Ceil(float64(len(with)) * sharedPhraseShare))
	if threshold < minPatternPages {
		threshold = minPatternPages
	}
	occurrences := make(map[string]map[int]bool)
	words := make([][]string, len(with))
	for i, pr := range with {
		words[i] = tokens(value(pr))
		for j := 0; j+sharedPhraseWords <= len(words[i]); j++ {
			seed := strings.Join(words[i][j:j+sharedPhraseWords], " ")
			if occurrences[seed] == nil {
				occurrences[seed] = make(map[int]bool)
			}
			occurrences[seed][i] = true
		}
	}
	seeds := make([]string, 0, len(occurrences))
	for seed, on := range occurrences {
		if len(on) >= threshold {
			seeds = append(seeds, seed)
		}
	}
	sort.Slice(seeds, func(i, j int) bool {
		if len(occurrences[seeds[i]]) != len(occurrences[seeds[j]]) {
			return len(occurrences[seeds[i]]) > len(occurrences[seeds[j]])
		}
		return seeds[i] < seeds[j]
	})

	var out []sharedPhrase
	var taken []map[int]bool
	for _, seed := range seeds {
		on := occurrences[seed]
		overlaps := false
		for _, t := range taken {
			common := 0
			for i := range on {
				if t[i] {
					common++
				}
			}
			overlaps = overlaps || float64(common) >= phraseOverlapShare*float64(len(on))
		}
		if overlaps {
			continue
		}
		taken = append(taken, on)
		var ps []models.PageProfile
		var texts [][]string
		for i := range with {
			if on[i] {
				ps = append(ps, with[i])
				texts = append(texts, words[i])
			}
		}
		out = append(out, sharedPhrase{Seed: seed, Text: strings.Join(extendPhrase(strings.Fields(seed), texts), " "), Pages: ps})
		if len(out) == maxSharedPhrases {
			break
		}
	}
	return out
}

// templatedMeta reports meta descriptions that repeat one sentence. Titles are
// left out: a shared suffix with the site's name is normal there.
func templatedMeta(pages []models.PageProfile, duplicates []siteFinding) []siteFinding {
	alreadyDuplicate := make(map[string]bool)
	for _, f := range duplicates {
		for _, pr := range f.Pages {
			alreadyDuplicate[pr.URL] = true
		}
	}
	var out []siteFinding
	for _, phrase := range findSharedPhrases(pages, profileMeta) {
		covered := 0
		for _, pr := range phrase.Pages {
			if alreadyDuplicate[pr.URL] {
				covered++
			}
		}
		if float64(covered) >= phraseOverlapShare*float64(len(phrase.Pages)) {
			continue
		}
		out = append(out, siteFinding{
			Issue: models.IssueTemplatedMeta, KeyPart: phrase.Seed, Scope: models.TaskScopeSite,
			Confidence: "medium", Pages: phrase.Pages,
			Title:    fmt.Sprintf("Repeated wording in the meta description of %d pages", len(phrase.Pages)),
			Problem:  fmt.Sprintf("%d of %d pages read repeat the same wording in their meta description: %q. It says nothing about the individual page, so it gives searchers no reason to choose it.", len(phrase.Pages), len(pages), phrase.Text),
			Goal:     "Meta descriptions are built from what each page actually offers, with the generic sentence gone or reduced, and still fit in 160 characters.",
			Examples: fieldExamples(phrase.Pages, "meta_description", profileMeta),
			Checks: []models.TaskCheck{
				{Type: models.CheckNotContains, Field: "meta_description", Value: phrase.Seed},
				{Type: models.CheckMaxLength, Field: "meta_description", Limit: maxMetaChars},
			},
		})
	}
	return out
}

func unavailablePages(profiles []models.PageProfile) []siteFinding {
	gone := where(profiles, func(pr models.PageProfile) bool { return pr.Unavailable })
	if len(gone) < minPatternPages {
		return nil
	}
	return []siteFinding{{
		Issue: models.IssueUnavailable, KeyPart: models.PageKindProduct, Scope: models.TaskScopeGroup, Group: models.PageKindProduct,
		Confidence: "medium", Pages: gone,
		Title:   "Unavailable product pages need a way forward",
		Problem: fmt.Sprintf("%d product pages read say they cannot be bought. Searchers landing there reach a dead end, while the pages keep their URLs and search history.", len(gone)),
		Goal:    "Each unavailable product page keeps its URL, says clearly that the product is unavailable, and leads to the closest available alternatives.",
		Avoid: []string{
			"Do not redirect or remove these pages unless the product is gone for good: it may return.",
			"Do not change their canonical to a category page.",
		},
		Examples: fieldExamples(gone, "h1", profileH1),
	}}
}

func errorPages(profiles []models.PageProfile) []siteFinding {
	broken := where(profiles, func(pr models.PageProfile) bool { return pr.StatusCode >= 400 })
	return perPageType(broken, minPatternPages, func(group string, ps []models.PageProfile) siteFinding {
		return siteFinding{
			Issue: models.IssueHTTPError, Confidence: "high", Pages: ps,
			Title:   fmt.Sprintf("Error responses on %s", groupLabel(group)),
			Problem: fmt.Sprintf("%d known pages answer with HTTP 400 or above, for example HTTP %d.", len(ps), ps[0].StatusCode),
			Goal:    "These URLs work again, or redirect to the closest relevant page if they are gone for good.",
			Checks:  []models.TaskCheck{{Type: models.CheckStatusOK}},
		}
	})
}

// detectFindings looks for patterns across a project's page profiles.
func detectFindings(profiles []models.PageProfile) []siteFinding {
	pages := indexablePages(profiles)
	var out []siteFinding
	out = append(out, errorPages(profiles)...)
	out = append(out, missingField(pages, models.IssueMissingTitle, "title", "title", profileTitle)...)
	out = append(out, missingField(pages, models.IssueMissingMeta, "meta_description", "meta description", profileMeta)...)
	out = append(out, missingField(pages, models.IssueMissingH1, "h1", "H1", profileH1)...)
	out = append(out, duplicateField(pages, models.IssueDuplicateTitle, "title", "title", profileTitle)...)
	duplicates := duplicateField(pages, models.IssueDuplicateMeta, "meta_description", "meta description", profileMeta)
	out = append(out, duplicates...)
	out = append(out, duplicateField(pages, models.IssueDuplicateText, "category_text", "descriptive text", profileText)...)
	out = append(out, templatedMeta(pages, duplicates)...)
	out = append(out, tooLong(pages, models.IssueTitleTooLong, "title", "title", maxTitleChars, profileTitle)...)
	out = append(out, tooLong(pages, models.IssueMetaTooLong, "meta_description", "meta description", maxMetaChars, profileMeta)...)
	out = append(out, unavailablePages(profiles)...)
	out = append(out, auditFindings(profiles)...)
	return out
}

func withIssue(profiles []models.PageProfile, codes ...string) []models.PageProfile {
	return where(profiles, func(pr models.PageProfile) bool {
		for _, i := range pr.Issues {
			for _, c := range codes {
				if i.Code == c {
					return true
				}
			}
		}
		return false
	})
}

// auditFindings turns the site audit's structural issues into patterns. They
// come from the link graph and the sitemap, which a page cannot show alone.
func auditFindings(profiles []models.PageProfile) []siteFinding {
	var out []siteFinding
	out = append(out, perPageType(withIssue(profiles, AuditNoindexInSearch), minPatternPages, func(group string, ps []models.PageProfile) siteFinding {
		return siteFinding{
			Issue: models.IssueNoindex, Confidence: "high", Pages: ps,
			Title:   fmt.Sprintf("Noindex on %s that should be in search", groupLabel(group)),
			Problem: fmt.Sprintf("%d pages are in the sitemap or get search impressions, yet carry noindex, so search engines drop them.", len(ps)),
			Goal:    "These pages are indexable, or, if they should stay out of search, they leave the sitemap and stop being linked as search pages.",
			Avoid:   []string{"Check first whether the noindex is intentional before removing it."},
			Checks:  []models.TaskCheck{{Type: models.CheckIndexable}},
		}
	})...)
	out = append(out, perPageType(withIssue(profiles, AuditOrphan), minPatternPages, func(group string, ps []models.PageProfile) siteFinding {
		return siteFinding{
			Issue: models.IssueOrphanPages, Confidence: "medium", Pages: ps,
			Title:   fmt.Sprintf("No internal link leads to %s", groupLabel(group)),
			Problem: fmt.Sprintf("%d pages are in the sitemap or in search, but no page read links to them, so visitors and crawlers can hardly reach them.", len(ps)),
			Goal:    "Each of these pages is linked from a relevant page of the site, such as its category or a related page.",
		}
	})...)
	out = append(out, perPageType(withIssue(profiles, AuditBrokenLinks), minPatternPages, func(group string, ps []models.PageProfile) siteFinding {
		return siteFinding{
			Issue: models.IssueBrokenLinks, Confidence: "high", Pages: ps,
			Title:   fmt.Sprintf("Links to broken pages from %s", groupLabel(group)),
			Problem: fmt.Sprintf("%d pages link to pages of the site that answer with an error. When many pages share the link, it usually sits in a shared menu or block.", len(ps)),
			Goal:    "Links point to working pages: fixed, or removed where the target is gone for good.",
		}
	})...)
	out = append(out, perPageType(withIssue(profiles, AuditRedirected), minPatternPages, func(group string, ps []models.PageProfile) siteFinding {
		return siteFinding{
			Issue: models.IssueSitemap, Confidence: "high", Pages: ps,
			Title:   fmt.Sprintf("Redirecting URLs in the sitemap: %s", groupLabel(group)),
			Problem: fmt.Sprintf("%d URLs in the sitemap redirect elsewhere. A sitemap should list the final, indexable URLs only.", len(ps)),
			Goal:    "The sitemap lists the redirect targets instead of these URLs.",
		}
	})...)
	for kind, types := range expectedSchema {
		missing := where(withIssue(profiles, AuditMissingSchema), func(pr models.PageProfile) bool { return pr.Kind == kind })
		if len(missing) < minPatternPages {
			continue
		}
		out = append(out, siteFinding{
			Issue: models.IssueStructuredData, KeyPart: kind, Scope: models.TaskScopeGroup, Group: kind,
			Confidence: "high", Pages: missing,
			Title:   fmt.Sprintf("No %s structured data on %s", types[0], groupLabel(kind)),
			Problem: fmt.Sprintf("%d %s declare no %s structured data, so they cannot show rich results.", len(missing), groupLabel(kind), strings.Join(types, " or ")),
			Goal:    fmt.Sprintf("Every page of this type outputs valid %s JSON-LD from the data the page shows.", types[0]),
			Avoid:   []string{"Do not mark up data the page does not show, such as ratings or prices that are not visible."},
			Checks:  []models.TaskCheck{{Type: models.CheckHasSchema, Value: strings.Join(types, ",")}},
		})
	}
	return out
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	return strings.ToUpper(string(r[0])) + string(r[1:])
}
