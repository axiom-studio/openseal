package modelstep

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/axiom-studio/openseal/pkg/models"
)

// Search-based discovery (section 07 §7.5, ported from Atlas
// hosted_capability_search.go). Past the catalog budget the capabilities note
// carries one line per Skill or connected server instead of every ID, and the
// model finds exact IDs with search_capabilities before loading them with
// load_capabilities. Search is offered whenever the catalog is, so the
// tool list does not change when a catalog crosses the budget. Search ranks only the claim's authorized catalog; it
// reveals IDs and short descriptions, never schemas, and never authorizes or
// loads anything.

const (
	capabilitySearchDefaultLimit  = 8
	capabilitySearchMaxLimit      = 20
	capabilitySearchMaxQueryRunes = 200
	// MaxCapabilitySearchesPerRun bounds searching; past it a search returns
	// no results and tells the model to load from what it has or answer.
	MaxCapabilitySearchesPerRun = 8

	capabilitySearchToolDescription = "Find authorized capabilities that are not loaded yet. Pass a few keywords (service, object, verb); returns up to limit exact capability IDs with short descriptions, best match first. Load the ones you need with load_capabilities. Searching never executes anything."
)

// SearchCapabilitiesSpec is the search_capabilities tool definition. It is
// offered whenever the Run offers capabilities.
func SearchCapabilitiesSpec() models.ModelStepTool {
	return models.ModelStepTool{
		Name:        SearchCapabilitiesTool,
		Description: capabilitySearchToolDescription,
		Group:       models.ModelStepToolHarness,
		InputSchema: map[string]interface{}{
			"type": "object", "additionalProperties": false, "required": []string{"query"},
			"properties": map[string]interface{}{
				"query": map[string]interface{}{"type": "string", "minLength": 1, "maxLength": capabilitySearchMaxQueryRunes,
					"description": "A few keywords: the service, the object and the verb."},
				"limit": map[string]interface{}{"type": "integer", "minimum": 1, "maximum": capabilitySearchMaxLimit,
					"description": "Maximum results; default 8."},
			},
		},
	}
}

type capabilitySearchResult struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Description string `json:"description,omitempty"`
	Loaded      bool   `json:"loaded"`
}

type capabilitySearchContent struct {
	Status  string                   `json:"status"`
	Error   string                   `json:"error,omitempty"`
	Results []capabilitySearchResult `json:"results,omitempty"`
	Next    string                   `json:"next,omitempty"`
}

// noMatch keeps an empty results array in the no_match shape.
type capabilitySearchNoMatch struct {
	Status  string                   `json:"status"`
	Results []capabilitySearchResult `json:"results"`
	Next    string                   `json:"next"`
}

// ExecuteSearchCapabilities ranks the catalog for query. searchesThisRun
// counts the completed search_capabilities entries of this Run in the path
// before this call (CapabilitySearchesThisRun); past
// MaxCapabilitySearchesPerRun the call returns search_limit_reached.
func ExecuteSearchCapabilities(set *CapabilitySet, arguments map[string]interface{}, searchesThisRun int) (content json.RawMessage, isError bool) {
	if set == nil {
		return encodeCapabilityContent(capabilitySearchContent{Status: "not_available", Error: "capability search is not offered in this Run"}), true
	}
	query, limit, err := capabilitySearchArguments(arguments)
	if err != nil {
		return encodeCapabilityContent(capabilitySearchContent{Status: "invalid_arguments", Error: err.Error()}), true
	}
	if searchesThisRun+1 > MaxCapabilitySearchesPerRun {
		return encodeCapabilityContent(capabilitySearchContent{Status: "search_limit_reached",
			Next: "Capability search is closed for the rest of this Run. Load IDs from earlier results with load_capabilities, use loaded tools, or answer."}), false
	}
	results := set.search(query, limit)
	if len(results) == 0 {
		return encodeCapabilityContent(capabilitySearchNoMatch{Status: "no_match", Results: []capabilitySearchResult{},
			Next: "Nothing matched. Try different keywords (the service name, the object, the verb) once, or answer without a new capability."}), false
	}
	return encodeCapabilityContent(capabilitySearchContent{Status: "search_results", Results: results,
		Next: "Load the IDs you need with load_capabilities, then use their tools in your next step. Entries marked loaded are already callable."}), false
}

// CapabilitySearchesThisRun counts the completed search_capabilities tool
// entries of runID on the rendered path.
func CapabilitySearchesThisRun(path []*models.SessionEntry, runID string) int {
	count := 0
	for _, entry := range path {
		if entry != nil && entry.Type == models.SessionEntryTool && entry.RunID == runID && entry.State == models.ToolEntryCompleted &&
			entry.Tool != nil && entry.Tool.Name == SearchCapabilitiesTool {
			count++
		}
	}
	return count
}

func capabilitySearchArguments(arguments map[string]interface{}) (string, int, error) {
	for _, key := range capabilitySortedKeys(arguments) {
		if key != "query" && key != "limit" {
			return "", 0, fmt.Errorf("unknown argument %q; pass query and optionally limit", key)
		}
	}
	text, _ := arguments["query"].(string)
	query := strings.TrimSpace(text)
	if query == "" || utf8.RuneCountInString(query) > capabilitySearchMaxQueryRunes {
		return "", 0, fmt.Errorf("capability search needs a query of 1 to %d characters", capabilitySearchMaxQueryRunes)
	}
	limit := 0
	if raw, ok := arguments["limit"]; ok && raw != nil {
		value, ok := capabilityIntegerArgument(raw)
		if !ok || value < 1 || value > capabilitySearchMaxLimit {
			return "", 0, fmt.Errorf("capability search limit must be 1 to %d", capabilitySearchMaxLimit)
		}
		limit = value
	}
	return query, limit, nil
}

// capabilitySearchDoc is one catalog entry's search text, tokenized once.
type capabilitySearchDoc struct {
	entry    capabilityEntry
	position int
	idTokens []string
	text     []string
}

func (s *CapabilitySet) searchIndex() []capabilitySearchDoc {
	groups := map[string]string{}
	for _, a := range s.catalog.Actions {
		if key, bound := capabilityActionGroup(a); bound {
			groups[models.CapabilityActionPrefix+a.Name] = key
		}
	}
	index := make([]capabilitySearchDoc, 0, len(s.entries))
	for position, entry := range s.entries {
		index = append(index, capabilitySearchDoc{
			entry: entry, position: position,
			idTokens: capabilitySearchTokens(entry.ID + " " + entry.Name + " " + groups[entry.ID]),
			text:     capabilitySearchTokens(entry.Description),
		})
	}
	return index
}

// search ranks the authorized catalog by simple token scoring: an ID or name
// token matching a query token scores highest, a prefix match less, and the
// description least, each weighted by how rare the query token is across the
// catalog (so a server's name outweighs a verb every server has); matching
// every query token adds a bonus. Ties keep catalog order.
func (s *CapabilitySet) search(query string, limit int) []capabilitySearchResult {
	if limit <= 0 {
		limit = capabilitySearchDefaultLimit
	}
	if limit > capabilitySearchMaxLimit {
		limit = capabilitySearchMaxLimit
	}
	terms := capabilitySearchTokens(query)
	if len(terms) == 0 {
		return nil
	}
	type match struct {
		doc  *capabilitySearchDoc
		best []int
	}
	var matches []match
	frequency := make([]int, len(terms))
	index := s.searchIndex()
	for i := range index {
		doc := &index[i]
		var best []int
		for t, term := range terms {
			score := 0
			for _, token := range doc.idTokens {
				switch {
				case token == term:
					score = max(score, 6)
				case len(term) >= 3 && strings.HasPrefix(token, term):
					score = max(score, 3)
				}
			}
			for _, token := range doc.text {
				switch {
				case token == term:
					score = max(score, 2)
				case len(term) >= 3 && strings.HasPrefix(token, term):
					score = max(score, 1)
				}
			}
			if score > 0 {
				if best == nil {
					best = make([]int, len(terms))
				}
				best[t] = score
				frequency[t]++
			}
		}
		if best != nil {
			matches = append(matches, match{doc, best})
		}
	}
	type scored struct {
		doc   *capabilitySearchDoc
		score float64
	}
	weights := make([]float64, len(terms))
	for t := range terms {
		weights[t] = 1 + math.Log(float64(len(index)+1)/float64(frequency[t]+1))
	}
	ranked := make([]scored, 0, len(matches))
	for _, m := range matches {
		score, matched := 0.0, 0
		for t, best := range m.best {
			if best > 0 {
				matched++
				score += float64(best) * weights[t]
			}
		}
		if matched == len(terms) && len(terms) > 1 {
			score *= 1.5
		}
		ranked = append(ranked, scored{m.doc, score})
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		return ranked[i].doc.position < ranked[j].doc.position
	})
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}
	results := make([]capabilitySearchResult, 0, len(ranked))
	for _, item := range ranked {
		description := item.doc.entry.Description
		if item.doc.entry.Name != "" {
			description = item.doc.entry.Name + ": " + description
		}
		results = append(results, capabilitySearchResult{
			ID: s.discoveryID(item.doc.entry.ID), Kind: item.doc.entry.Kind,
			Description: description, Loaded: s.loaded[item.doc.entry.ID],
		})
	}
	return results
}

// capabilitySearchStopwords are too common in IDs and descriptions to rank by.
var capabilitySearchStopwords = map[string]bool{
	"a": true, "an": true, "and": true, "the": true, "of": true, "to": true, "for": true, "in": true, "on": true,
	"or": true, "with": true, "my": true, "me": true, "is": true, "it": true, "action": true, "prompt": true,
}

// capabilitySearchTokens lowercases text and splits it at every
// non-alphanumeric rune and camelCase boundary, dropping stopwords and
// duplicates.
func capabilitySearchTokens(text string) []string {
	var tokens []string
	seen := map[string]bool{}
	var current []rune
	flush := func() {
		if len(current) == 0 {
			return
		}
		token := strings.ToLower(string(current))
		current = current[:0]
		if !seen[token] && !capabilitySearchStopwords[token] {
			seen[token] = true
			tokens = append(tokens, token)
		}
	}
	var previous rune
	for _, r := range text {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if unicode.IsUpper(r) && unicode.IsLower(previous) {
				flush()
			}
			current = append(current, r)
		default:
			flush()
		}
		previous = r
	}
	flush()
	return tokens
}
