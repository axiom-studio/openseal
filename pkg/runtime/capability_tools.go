package runtime

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

// load_capabilities and search_capabilities are kernel tools (section 07 §7.4,
// §7.5). Both are pure functions of the step's CapabilitySet: they never run
// or authorize an action, they only reveal authorized instructions and
// schemas. A load's newly visible IDs become the tool entry's
// Kernel.LoadedCapabilities, which LoadedCapabilityIDs reads back from the
// session path at the next step start.

const (
	// maxCapabilityLoadIDs bounds one load_capabilities call.
	maxCapabilityLoadIDs = 8

	capabilityLoadToolDescription = "Load full instructions and argument schemas for one to eight capabilityCatalog IDs. Copy IDs verbatim, including their prefix; an action name alone is not an ID. Action IDs include their bound Skill instructions. Loaded tools become callable in your next step. Loading an already-loaded ID is a no-op."

	capabilitySearchLoadToolDescription = "Load full instructions and argument schemas for one to eight capability IDs from search_capabilities results. Copy IDs verbatim, including their prefix; an action name alone is not an ID. Action IDs include their bound Skill instructions. Loaded tools become callable in your next step. Loading an already-loaded ID is a no-op."

	// capabilityLookupUnavailableText is Atlas's hostedCapabilityLookupRejection
	// text for an ID absent from the catalog.
	capabilityLookupUnavailableText = "capability lookup requested an ID absent from the current authorized catalog"

	capabilityLoadedNext        = "Use the loaded tools in your next step; do not load these IDs again."
	capabilityAlreadyLoadedNext = "These capabilities are already loaded. Use their tools directly; do not load these IDs again."
	capabilityNotLoadedNext     = " Load each ID under notLoaded on its own in a later step."
	capabilityNotLoadedReason   = "too large to load together; load it on its own"
)

// LoadCapabilitiesSpec is the load_capabilities tool definition. It is
// byte-identical across steps: it carries no per-step enum, and every ID is
// validated against the step's authorized catalog when it runs.
func LoadCapabilitiesSpec(searching bool) ModelStepTool {
	description := capabilityLoadToolDescription
	if searching {
		description = capabilitySearchLoadToolDescription
	}
	return ModelStepTool{
		Name:        LoadCapabilitiesTool,
		Description: description,
		Group:       ModelStepToolHarness,
		InputSchema: map[string]interface{}{
			"type": "object", "additionalProperties": false, "required": []string{"ids"},
			"properties": map[string]interface{}{"ids": map[string]interface{}{
				"type": "array", "minItems": 1, "maxItems": maxCapabilityLoadIDs, "uniqueItems": true,
				"items": map[string]interface{}{"type": "string", "description": "An exact capability ID, including its prefix."},
			}},
		},
	}
}

// capabilityLoadedItem is one newly loaded capability in a load result.
type capabilityLoadedItem struct {
	ID                    string `json:"id"`
	Name                  string `json:"name,omitempty"`
	Instructions          string `json:"instructions,omitempty"`
	InstructionsTruncated bool   `json:"instructionsTruncated,omitempty"`
	Tool                  string `json:"tool,omitempty"`
	Summary               string `json:"summary,omitempty"`
}

// capabilityAlreadyLoadedItem points the model at the tools it already has.
type capabilityAlreadyLoadedItem struct {
	ID      string   `json:"id"`
	Tools   []string `json:"tools,omitempty"`
	Summary string   `json:"summary,omitempty"`
}

type capabilityNotLoadedItem struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

type capabilityLoadContent struct {
	Status        string                        `json:"status"`
	Error         string                        `json:"error,omitempty"`
	IDs           []string                      `json:"ids,omitempty"`
	Loaded        []capabilityLoadedItem        `json:"loaded,omitempty"`
	AlreadyLoaded []capabilityAlreadyLoadedItem `json:"alreadyLoaded,omitempty"`
	NotLoaded     []capabilityNotLoadedItem     `json:"notLoaded,omitempty"`
	Next          string                        `json:"next,omitempty"`
}

// ExecuteLoadCapabilities validates and applies one load against the step's
// set. toolName maps a newly visible action, host-tool or runbook ID to its
// provider tool name (supplied by the tool-set builder so names match
// ExecuteModelStep exactly); nil or "" omits the name.
//
// Rules (Atlas loadNativeCall): handles resolve and duplicates collapse; any
// unknown ID loads nothing; already-visible IDs are a successful no-op; new IDs
// close over their bound prompts. The set is updated in place, so a later
// load in the same step sees these IDs as already loaded. loaded is the
// closure in markLoaded order and becomes the entry's Kernel.LoadedCapabilities.
func ExecuteLoadCapabilities(set *CapabilitySet, arguments map[string]interface{}, toolName func(id string) string) (content json.RawMessage, isError bool, loaded []string) {
	ids, err := capabilityLoadArguments(arguments)
	if err == nil && set == nil {
		err = fmt.Errorf("capability loading is not offered for this step")
	}
	if err != nil {
		return encodeCapabilityContent(capabilityLoadContent{Status: "invalid_arguments", Error: err.Error()}), true, nil
	}
	nameOf := func(id string) string {
		if toolName == nil {
			return ""
		}
		return toolName(id)
	}

	var requested, already, unknown []string
	seen := map[string]bool{}
	for _, raw := range ids {
		canonical, ok := set.Resolve(raw)
		if !ok {
			if !seen["\x00"+raw] {
				seen["\x00"+raw] = true
				unknown = append(unknown, raw)
			}
			continue
		}
		if seen[canonical] {
			continue
		}
		seen[canonical] = true
		if set.loaded[canonical] {
			already = append(already, canonical)
		} else {
			requested = append(requested, canonical)
		}
	}
	if len(unknown) > 0 {
		return encodeCapabilityContent(capabilityLoadContent{Status: "not_available", Error: capabilityLookupUnavailableText, IDs: unknown}), true, nil
	}

	result := capabilityLoadContent{}
	for _, id := range already {
		item := capabilityAlreadyLoadedItem{ID: set.discoveryID(id), Summary: set.entrySummary(id)}
		if name := nameOf(id); name != "" {
			item.Tools = []string{name}
		}
		result.AlreadyLoaded = append(result.AlreadyLoaded, item)
	}

	// Load in request order while the encoded result fits; the rest is
	// reported, not loaded. A first capability that cannot fit on its own has
	// its instructions cut to fit.
	pending := map[string]bool{}
	for index, id := range requested {
		closure := set.closure(id)
		var items []capabilityLoadedItem
		for _, candidate := range closure {
			if pending[candidate] {
				continue
			}
			items = append(items, set.loadedItem(candidate, nameOf(candidate)))
		}
		trial := result
		trial.Loaded = append(append([]capabilityLoadedItem(nil), result.Loaded...), items...)
		trial.NotLoaded = capabilityNotLoaded(set, requested[index+1:])
		if !capabilityContentFits(finishCapabilityLoad(trial)) {
			if len(result.Loaded) > 0 {
				result.NotLoaded = capabilityNotLoaded(set, requested[index:])
				break
			}
			fitted, ok := fitCapabilityInstructions(trial, len(result.Loaded))
			if !ok {
				result.NotLoaded = capabilityNotLoaded(set, requested[index:])
				break
			}
			trial = fitted
		}
		trial.NotLoaded = nil
		result = trial
		for _, candidate := range closure {
			if !pending[candidate] {
				pending[candidate] = true
				loaded = append(loaded, candidate)
			}
		}
	}
	for _, id := range requested {
		if pending[id] {
			set.markLoaded(id)
		}
	}
	result = finishCapabilityLoad(result)
	if result.Status == "not_loaded" {
		return encodeCapabilityContent(result), true, nil
	}
	return encodeCapabilityContent(result), false, loaded
}

func finishCapabilityLoad(c capabilityLoadContent) capabilityLoadContent {
	switch {
	case len(c.Loaded) > 0:
		c.Status = "loaded"
		c.Next = capabilityLoadedNext
	case len(c.AlreadyLoaded) > 0 && len(c.NotLoaded) == 0:
		c.Status = "already_loaded"
		c.Next = capabilityAlreadyLoadedNext
	default:
		c.Status = "not_loaded"
		c.Next = ""
	}
	if len(c.NotLoaded) > 0 {
		c.Next = strings.TrimSpace(c.Next + capabilityNotLoadedNext)
	}
	return c
}

// fitCapabilityInstructions cuts the instructions of the loaded items from
// index first onward, in order, so the result fits. ok is false when even
// empty instructions do not fit.
func fitCapabilityInstructions(c capabilityLoadContent, first int) (capabilityLoadContent, bool) {
	loaded := append([]capabilityLoadedItem(nil), c.Loaded...)
	c.Loaded = loaded
	full := make([]string, len(loaded))
	for i := first; i < len(loaded); i++ {
		full[i] = loaded[i].Instructions
		loaded[i].Instructions = ""
	}
	if !capabilityContentFits(finishCapabilityLoad(c)) {
		return c, false
	}
	for i := first; i < len(loaded); i++ {
		if full[i] == "" {
			continue
		}
		loaded[i].Instructions = full[i]
		if capabilityContentFits(finishCapabilityLoad(c)) {
			continue
		}
		// Largest byte cut that fits.
		low, high := 0, len(full[i])
		for low < high {
			mid := (low + high + 1) / 2
			loaded[i].Instructions = cutUTF8(full[i], mid)
			loaded[i].InstructionsTruncated = true
			if capabilityContentFits(finishCapabilityLoad(c)) {
				low = mid
			} else {
				high = mid - 1
			}
		}
		loaded[i].Instructions = cutUTF8(full[i], low)
		loaded[i].InstructionsTruncated = true
		// Nothing after a cut prompt has room for instructions.
		for j := i + 1; j < len(loaded); j++ {
			if full[j] != "" {
				loaded[j].InstructionsTruncated = true
			}
		}
		break
	}
	return c, true
}

func capabilityNotLoaded(set *CapabilitySet, ids []string) []capabilityNotLoadedItem {
	if len(ids) == 0 {
		return nil
	}
	items := make([]capabilityNotLoadedItem, 0, len(ids))
	for _, id := range ids {
		items = append(items, capabilityNotLoadedItem{ID: set.discoveryID(id), Reason: capabilityNotLoadedReason})
	}
	return items
}

// loadedItem is the result line of one newly visible capability: a prompt
// carries its full instructions; a tool carries its provider name and
// catalog description.
func (s *CapabilitySet) loadedItem(id, tool string) capabilityLoadedItem {
	item := capabilityLoadedItem{ID: s.discoveryID(id)}
	if p, ok := s.prompts[id]; ok {
		item.Name = strings.Join(strings.Fields(p.Name), " ")
		item.Instructions = p.Instructions
		return item
	}
	item.Tool = tool
	item.Summary = s.entrySummary(id)
	return item
}

// entrySummary is the catalog description of an ID.
func (s *CapabilitySet) entrySummary(id string) string {
	for _, entry := range s.entries {
		if entry.ID == id {
			return entry.Description
		}
	}
	return ""
}

func capabilityLoadArguments(arguments map[string]interface{}) ([]string, error) {
	for _, key := range capabilitySortedKeys(arguments) {
		if key != "ids" {
			return nil, fmt.Errorf("unknown argument %q; pass only ids", key)
		}
	}
	raw, ok := arguments["ids"]
	if !ok {
		return nil, fmt.Errorf("ids is required: one to eight exact capability IDs")
	}
	var ids []string
	switch values := raw.(type) {
	case []string:
		ids = append(ids, values...)
	case []interface{}:
		for _, value := range values {
			text, ok := value.(string)
			if !ok {
				return nil, fmt.Errorf("ids must be strings")
			}
			ids = append(ids, text)
		}
	default:
		return nil, fmt.Errorf("ids must be an array of exact capability IDs")
	}
	if len(ids) == 0 || len(ids) > maxCapabilityLoadIDs {
		return nil, fmt.Errorf("ids must hold one to eight exact capability IDs")
	}
	for i, id := range ids {
		ids[i] = strings.TrimSpace(id)
		if ids[i] == "" {
			return nil, fmt.Errorf("capability IDs must be nonempty strings")
		}
	}
	return ids, nil
}

func capabilityContentFits(content interface{}) bool {
	encoded, err := json.Marshal(content)
	return err == nil && len(encoded) <= MaxToolResultContentBytes
}

func encodeCapabilityContent(content interface{}) json.RawMessage {
	encoded, err := json.Marshal(content)
	if err != nil {
		encoded, _ = json.Marshal(map[string]string{"status": "invalid_arguments", "error": err.Error()})
	}
	return encoded
}

// capabilityIntegerArgument reads an optional integer tool argument decoded
// from JSON (float64 or json.Number) or built in Go (int).
func capabilityIntegerArgument(value interface{}) (int, bool) {
	switch v := value.(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		if v != math.Trunc(v) || math.IsInf(v, 0) || math.Abs(v) > 1<<31 {
			return 0, false
		}
		return int(v), true
	case json.Number:
		n, err := v.Int64()
		if err != nil || n > 1<<31 || n < -(1<<31) {
			return 0, false
		}
		return int(n), true
	}
	return 0, false
}

// capabilitySortedKeys gives deterministic error text over unknown arguments.
func capabilitySortedKeys(values map[string]interface{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
