package worker

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/creever/crawler-api/models"
)

const (
	maxTitleChars = 60
	maxMetaChars  = 160
)

// analysisResponse is the JSON the model returns.
type analysisResponse struct {
	Diagnosis struct {
		PrimaryIssue string `json:"primary_issue"`
		RootCause    string `json:"root_cause"`
		Confidence   string `json:"confidence"`
		Evidence     []struct {
			Ref   string `json:"ref"`
			Claim string `json:"claim"`
		} `json:"evidence"`
		RuledOut []string `json:"ruled_out"`
	} `json:"diagnosis"`
	Actions        []analysisAction `json:"actions"`
	NoActionReason string           `json:"no_action_reason"`
	MissingData    []string         `json:"missing_data"`
}

type analysisAction struct {
	Issue        string   `json:"issue"`
	Scope        string   `json:"scope"`
	Kind         string   `json:"kind"`
	Priority     int      `json:"priority"`
	Effort       string   `json:"effort"`
	Target       string   `json:"target"`
	Proposed     string   `json:"proposed"`
	Rationale    string   `json:"rationale"`
	EvidenceRefs []string `json:"evidence_refs"`
	// Structured is set when the model sent proposed as an object or a list.
	// That still reads fine as a manual task, but it is not publishable text.
	Structured bool `json:"-"`
}

// UnmarshalJSON accepts objects and lists in the text fields. One action in the
// wrong shape must not lose the whole analysis.
func (a *analysisAction) UnmarshalJSON(data []byte) error {
	type plain analysisAction
	aux := struct {
		*plain
		Target    json.RawMessage `json:"target"`
		Proposed  json.RawMessage `json:"proposed"`
		Rationale json.RawMessage `json:"rationale"`
	}{plain: (*plain)(a)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	var err error
	if a.Target, _, err = flattenJSONText(aux.Target); err != nil {
		return err
	}
	if a.Proposed, a.Structured, err = flattenJSONText(aux.Proposed); err != nil {
		return err
	}
	a.Rationale, _, err = flattenJSONText(aux.Rationale)
	return err
}

// flattenJSONText turns a JSON value into text and reports if it wasn't a string.
func flattenJSONText(raw json.RawMessage) (string, bool, error) {
	if len(raw) == 0 {
		return "", false, nil
	}
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", false, err
	}
	if s, ok := v.(string); ok {
		return s, false, nil
	}
	if v == nil {
		return "", false, nil
	}
	return flattenValue(v), true, nil
}

func flattenValue(v interface{}) string {
	switch t := v.(type) {
	case map[string]interface{}:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+": "+flattenValue(t[k]))
		}
		return strings.Join(parts, "; ")
	case []interface{}:
		parts := make([]string, 0, len(t))
		for _, e := range t {
			parts = append(parts, flattenValue(e))
		}
		return strings.Join(parts, "; ")
	default:
		return fmt.Sprint(t)
	}
}

// validatedAnalysis is what survives validation.
type validatedAnalysis struct {
	Evidence []models.AnalysisEvidence
	Actions  []analysisAction
	Problems []string
}

func normalizeRef(ref string) string {
	return strings.ToUpper(strings.Trim(strings.TrimSpace(ref), "[]"))
}

// validateAnalysis checks a response against the context pack. Evidence and
// actions failing a check are dropped and reported as problems.
func validateAnalysis(resp *analysisResponse, pack *contextPack, snap *models.PageSnapshot) validatedAnalysis {
	var out validatedAnalysis
	corpus := strings.ToLower(pack.Text)

	if strings.TrimSpace(resp.Diagnosis.PrimaryIssue) == "" {
		out.Problems = append(out.Problems, "diagnosis.primary_issue is empty")
	}
	for _, e := range resp.Diagnosis.Evidence {
		ref := normalizeRef(e.Ref)
		fact, ok := pack.Facts[ref]
		if !ok {
			out.Problems = append(out.Problems, fmt.Sprintf("evidence cites unknown fact %q", e.Ref))
			continue
		}
		if contradictsLength(e.Claim, fact) {
			out.Problems = append(out.Problems, fmt.Sprintf("evidence %s: the claim contradicts the fact: %s", ref, fact))
			continue
		}
		out.Evidence = append(out.Evidence, models.AnalysisEvidence{Ref: ref, Claim: strings.TrimSpace(e.Claim), Fact: fact})
	}
	if len(out.Evidence) == 0 {
		out.Problems = append(out.Problems, "diagnosis has no valid evidence")
	}

	seenKinds := make(map[string]bool)
	for _, a := range resp.Actions {
		a.Kind = strings.TrimSpace(a.Kind)
		a.Proposed = strings.TrimSpace(a.Proposed)
		a.Issue = normalizeIssue(a.Issue)
		a.Scope = normalizeScope(a.Scope)
		if problem := validateAction(&a, pack, snap, corpus); problem != "" {
			out.Problems = append(out.Problems, fmt.Sprintf("action %s: %s", a.Kind, problem))
			continue
		}
		// One page has one title, meta and category text.
		if models.IsApplyableKind(a.Kind) {
			if seenKinds[a.Kind] {
				out.Problems = append(out.Problems, fmt.Sprintf("action %s: proposed more than once", a.Kind))
				continue
			}
			seenKinds[a.Kind] = true
		}
		out.Actions = append(out.Actions, a)
	}
	return out
}

// normalizeIssue keeps a model's answer to the known list. An unknown code only
// costs the action its merging, so it isn't worth a repair call.
func normalizeIssue(issue string) string {
	issue = strings.ToLower(strings.TrimSpace(issue))
	if models.IsModelIssue(issue) {
		return issue
	}
	return models.IssueOther
}

func normalizeScope(scope string) string {
	if strings.EqualFold(strings.TrimSpace(scope), "page_type") {
		return "page_type"
	}
	return "page"
}

func validateAction(a *analysisAction, pack *contextPack, snap *models.PageSnapshot, corpus string) string {
	if !models.IsSuggestionKind(a.Kind) {
		return "unknown kind"
	}
	if a.Proposed == "" {
		return "proposed is empty"
	}
	if a.Structured && models.IsApplyableKind(a.Kind) {
		return "proposed must be the plain text to publish"
	}
	if problem := harmfulAction(a, snap); problem != "" {
		return problem
	}
	refs := make([]string, 0, len(a.EvidenceRefs))
	for _, r := range a.EvidenceRefs {
		ref := normalizeRef(r)
		if _, ok := pack.Facts[ref]; !ok {
			return fmt.Sprintf("cites unknown fact %q", r)
		}
		if contradictsLength(a.Rationale, pack.Facts[ref]) {
			return fmt.Sprintf("the rationale contradicts fact %s: %s", ref, pack.Facts[ref])
		}
		refs = append(refs, ref)
	}
	if len(refs) == 0 {
		return "has no evidence_refs"
	}
	a.EvidenceRefs = refs

	switch a.Kind {
	case models.SuggestionKindTitle:
		if n := utf8.RuneCountInString(a.Proposed); n > maxTitleChars {
			return fmt.Sprintf("is %d characters, the limit is %d", n, maxTitleChars)
		}
		if strings.EqualFold(a.Proposed, snap.Title) {
			return "is identical to the current title"
		}
	case models.SuggestionKindMeta:
		if n := utf8.RuneCountInString(a.Proposed); n > maxMetaChars {
			return fmt.Sprintf("is %d characters, the limit is %d", n, maxMetaChars)
		}
		if strings.EqualFold(a.Proposed, snap.MetaDescription) {
			return "is identical to the current meta description"
		}
	case models.SuggestionKindCategoryText:
		if snap.CategoryText != "" && !containsString(refs, "C1") {
			return "ignores the existing category text, cite C1 and build on it"
		}
	}

	switch a.Kind {
	case models.SuggestionKindTitle, models.SuggestionKindMeta, models.SuggestionKindCategoryText, models.SuggestionKindH1:
		if names := unsupportedNames(a.Proposed, corpus); len(names) > 0 {
			return fmt.Sprintf("uses names not found in the context: %s", strings.Join(names, ", "))
		}
		if nums := unsupportedNumbers(a.Proposed, corpus); len(nums) > 0 {
			return fmt.Sprintf("uses numbers not found in the context: %s", strings.Join(nums, ", "))
		}
		// Whether shipping is free depends on the order value, so a page text
		// must not promise it.
		if reFreeShipping.MatchString(a.Proposed) {
			return "promises free shipping, leave shipping terms out of the text"
		}
	}
	return ""
}

// Unavailable-product wording for sites without structured availability data.
var reUnavailable = regexp.MustCompile(`(?i)jelenleg nem elérhető|nincs készleten|elfogyott|` +
	`out of stock|currently unavailable|sold out|no longer available|` +
	`ausverkauft|nicht verfügbar|nicht auf lager|rupture de stock|actuellement indisponible|` +
	`agotado|sin existencias|esaurito|non disponibile|niedostępny|wyprzedany|vyprodáno|nedostupné|stoc epuizat`)

// isUnavailablePage reports whether the page says the product can't be bought,
// in its structured data or in its own text. Listing cards are not part of
// Prose, so a sold-out item on a category page doesn't count.
func isUnavailablePage(snap *models.PageSnapshot) bool {
	if snap.Availability == "out_of_stock" && !isListingPage(snap) {
		return true
	}
	return reUnavailable.MatchString(strings.Join(snap.Prose, " "))
}

// isListingPage reports whether the page's main job is to list items. A product
// page's related-items grid is too small to qualify.
func isListingPage(snap *models.PageSnapshot) bool {
	return snap.CategoryText != "" || (snap.Listing != nil && snap.Listing.ItemCount >= minListingItems)
}

const minListingItems = 12

var (
	reURLOrPath = regexp.MustCompile(`https?://[^\s,;)"'„”\]]+|(?:^|[\s(„"'])/[\p{L}\p{N}][^\s,;)"'”\]]*`)
)

// harmfulAction rejects advice that can remove traffic and that a prompt alone
// did not stop. Both rules only cover moving this page's value elsewhere.
func harmfulAction(a *analysisAction, snap *models.PageSnapshot) string {
	if a.Kind != models.SuggestionKindCanonical && a.Kind != models.SuggestionKindRedirect {
		return ""
	}
	if isUnavailablePage(snap) {
		return "this product is unavailable and may return, keep the page and link to alternatives"
	}
	if isListingPage(snap) {
		if other := otherPageMentioned(a.Target+" "+a.Proposed, snap.SourceURL); other != "" {
			return fmt.Sprintf("a listing page must keep its own URL, do not consolidate it into %s", other)
		}
	}
	return ""
}

// otherPageMentioned returns the first URL or path in text that is neither page
// nor one of its filtered or paginated variants. Pointing filter URLs back at
// the clean listing page is correct and stays allowed.
func otherPageMentioned(text, page string) string {
	own := pagePath(page)
	for _, m := range reURLOrPath.FindAllString(text, -1) {
		m = strings.TrimRight(strings.TrimLeft(m, " \t\n(„\"'"), ".")
		u, err := url.Parse(m)
		if err != nil || (u.Host != "" && !sameSite(u.Host, hostOf(page))) {
			continue
		}
		path := pagePath(m)
		if path == own || (strings.HasPrefix(path, own+"/") && strings.Contains(path[len(own):], ":")) {
			continue
		}
		return m
	}
	return ""
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}

var (
	reTooLong      = regexp.MustCompile(`(?i)túl hossz|too long|zu lang|trop long|demasiado larg|troppo lung|za długi|příliš dlouh`)
	reFreeShipping = regexp.MustCompile(`(?i)ingyenes\s+(ki)?szállítás|free\s+(shipping|delivery)|kostenlose[rn]?\s+versand|livraison\s+gratuite|envío\s+gratis|spedizione\s+gratuita|darmowa\s+dostawa|doprava\s+zdarma|transport\s+gratuit`)
	reNumber       = regexp.MustCompile(`\d+(?:[ .\x{a0}]\d{3})*`)
)

func normalizeNumber(s string) string {
	return strings.NewReplacer(" ", "", ".", "", "\u00a0", "").Replace(s)
}

// unsupportedNumbers returns numbers of text that the context does not contain.
// An invented price, discount or threshold in a title or meta would be published
// as a statement of the shop.
func unsupportedNumbers(text, corpus string) []string {
	known := make(map[string]bool)
	for _, n := range reNumber.FindAllString(corpus, -1) {
		known[normalizeNumber(n)] = true
	}
	var missing []string
	for _, n := range reNumber.FindAllString(text, -1) {
		if !known[normalizeNumber(n)] {
			missing = appendUnique(missing, n)
		}
	}
	return missing
}

// contradictsLength reports a "too long" claim citing a text within its limit.
// Length is the one claim about a fact that can be checked in code; other
// misreadings of a fact still pass and are left to the reviewer.
func contradictsLength(claim, fact string) bool {
	return reTooLong.MatchString(claim) && strings.Contains(fact, ", within the ")
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

var (
	reWordToken     = regexp.MustCompile(`[\p{L}\p{N}][\p{L}\p{N}.'’]*`)
	reSentenceBreak = regexp.MustCompile(`[.!?|:–—]\s*$|\s-\s*$`)
)

// unsupportedNames returns capitalized words of text, other than sentence
// openers, that have no match in the context. Those are brand or product names
// the model brought in from outside. Inflected forms are tolerated by
// matching on the stem: "Skecherstől" matches "Skechers".
func unsupportedNames(text, corpus string) []string {
	var names []string
	last := 0
	for _, loc := range reWordToken.FindAllStringIndex(text, -1) {
		before := text[last:loc[0]]
		sentenceStart := last == 0 || reSentenceBreak.MatchString(before)
		// A dot closing the previous token also ends a sentence.
		if !sentenceStart && last > 0 && strings.HasSuffix(text[:last], ".") {
			sentenceStart = true
		}
		token := strings.TrimRight(text[loc[0]:loc[1]], ".'’")
		last = loc[1]

		runes := []rune(token)
		if sentenceStart || len(runes) < 3 || !unicode.IsUpper(runes[0]) {
			continue
		}
		stemLen := len(runes) - 3
		if stemLen < 4 {
			stemLen = len(runes)
		}
		if !strings.Contains(corpus, strings.ToLower(string(runes[:stemLen]))) {
			names = appendUnique(names, token)
		}
	}
	return names
}
