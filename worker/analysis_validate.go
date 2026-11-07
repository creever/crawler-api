package worker

import (
	"fmt"
	"regexp"
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
	Kind         string   `json:"kind"`
	Priority     int      `json:"priority"`
	Effort       string   `json:"effort"`
	Target       string   `json:"target"`
	Proposed     string   `json:"proposed"`
	Rationale    string   `json:"rationale"`
	EvidenceRefs []string `json:"evidence_refs"`
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
		out.Evidence = append(out.Evidence, models.AnalysisEvidence{Ref: ref, Claim: strings.TrimSpace(e.Claim), Fact: fact})
	}
	if len(out.Evidence) == 0 {
		out.Problems = append(out.Problems, "diagnosis has no valid evidence")
	}

	seenKinds := make(map[string]bool)
	for _, a := range resp.Actions {
		a.Kind = strings.TrimSpace(a.Kind)
		a.Proposed = strings.TrimSpace(a.Proposed)
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

func validateAction(a *analysisAction, pack *contextPack, snap *models.PageSnapshot, corpus string) string {
	if !models.IsSuggestionKind(a.Kind) {
		return "unknown kind"
	}
	if a.Proposed == "" {
		return "proposed is empty"
	}
	refs := make([]string, 0, len(a.EvidenceRefs))
	for _, r := range a.EvidenceRefs {
		ref := normalizeRef(r)
		if _, ok := pack.Facts[ref]; !ok {
			return fmt.Sprintf("cites unknown fact %q", r)
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
	}
	return ""
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
// the model brought in from outside. Hungarian suffixes are tolerated by
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
