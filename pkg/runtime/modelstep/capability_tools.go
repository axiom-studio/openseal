package modelstep

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/axiom-studio/openseal/pkg/models"
)

// load_capabilities and search_capabilities are kernel tools (section 07 §7.4,
// §7.5). Both are pure functions of the step's CapabilitySet: they never run
// or authorize an action, they only reveal authorized instructions and
// schemas. A load freezes the definitions of the tools it makes callable on
// its tool entry (ToolEntry.SurfacedTools, capability_surface.go) and records
// the instructions it carried (ToolEntry.LoadedInstructions); the next step
// reads both back from the session path.

const (
	// maxCapabilityLoadIDs bounds one load_capabilities call.
	maxCapabilityLoadIDs = 8

	capabilityLoadToolDescription = "Load full instructions and argument schemas for one to eight exact capability IDs from the capabilities note or search_capabilities results. Copy IDs verbatim, including their prefix; an action name alone is not an ID. Loading an action also loads its bound Skill instructions and the other actions of the same Skill connection that fit. Loaded tools become callable in your next step. Loading an already-loaded ID is a no-op."

	// capabilityLookupUnavailableText is Atlas's hostedCapabilityLookupRejection
	// text for an ID absent from the catalog.
	capabilityLookupUnavailableText = "capability lookup requested an ID absent from the current authorized catalog"

	capabilityLoadedNext        = "Use the loaded tools in your next step; do not load these IDs again."
	capabilityAlreadyLoadedNext = "These capabilities are already loaded. Use their tools directly; do not load these IDs again."
	capabilityNotLoadedNext     = " Load each ID under notLoaded on its own in a later step."
	capabilityNotLoadedReason   = "too large to load together; load it on its own"
)

// capabilityLoadingText is the system section of a deferred capability set.
// It names no capability and does not depend on the catalog's size, so it is
// the same for every deferred claim; what can be loaded is in the
// capabilities note.
const CapabilityLoadingText = `Capability loading. Core capabilities are already loaded: their tools are callable now and their instructions are already in your context. The capabilities note in the conversation lists what else you can load: every capability by exact ID, or, when there are more than fit, a directory with one line per Skill or connected server. A later capabilities note lists only what changed since. Answer directly, or use a loaded tool, whenever that is enough. Only when the task needs a capability that is not loaded, call load_capabilities with one to eight exact IDs as a normal step; when the note shows a directory, or does not list the ID you need, first call search_capabilities with a few keywords (the service, the object and the verb, e.g. "notion search pages") to get exact IDs. Loaded tools become callable in your next step. Searching and loading reveal authorized instructions and schemas only; they never execute or authorize an action. Do not search or load for simple replies, and never invent IDs. Existing approval, tenant, evidence and action validation rules remain in force. Copy IDs verbatim, including their prefix; an action name alone is not a lookup ID. If the listed ID is action:service.fetch, pass {"ids":["action:service.fetch"]}; service.fetch alone is the action name. Loading an action ID automatically loads its bound Skill instructions, its argument schema and the other actions of the same Skill connection that fit; do not first load its instruction module in a separate discovery round. Batch independently needed capabilities in one lookup. Instruction-only capabilities remain available when their guidance is needed without an action. If a search finds nothing useful, try other keywords once, then answer with what you have.` + capabilitySkillDiscoveryInstructions

// capabilitySkillDiscoveryInstructions is Atlas's
// hostedSkillDiscoveryInstructions, reading exact IDs from the capabilities
// note or search results.
const capabilitySkillDiscoveryInstructions = ` Discovery finds Skills, which are installed and configured through request_setup. An app from find_apps (type mcp-server, a remote MCP server) is connected through request_connection, never through request_setup. For Skill discovery, use the requested service or Skill name, or concise task keywords, as the query. Omit requiredActions on the first lookup; it is an exact-name filter, not a task description. Never put natural-language task descriptions there. After inspecting a discovery result, copy exact actions[].name values if an operation filter is needed. Discovery action names are permission identifiers, not executable capability IDs: load operations only by copying their exact id from the capabilities note or search_capabilities results, never by constructing action: plus a discovered name. If the operation is absent from the capabilities note and search_capabilities results, inspect the saved binding access and request the missing access instead of loading an invented ID. A filter mismatch is not evidence of a missing account. Use exact observed operations and an accurate first query; do not automatically repeat a failed lookup or silently broaden its scope. If the account is saved but the needed operation is not enabled, identify all exact discovered operations necessary to complete the user's task, including reading details after listing identifiers, and request them together through request_setup on the existing binding; do not request one permission at a time for a task that already requires several operations; explain that permission is needed instead of asking the user to reconnect the account. Discovery reads metadata; omit maximumRisk unless the user requested a catalog risk filter. A Skill may offer both read and destructive actions; its highest declared risk does not make discovering it dangerous. Execution and approval enforce authority separately. An HTTP 403 does not by itself mean an account needs reconnection: distinguish provider API configuration, organization policy, consent scopes, and authorization using the returned evidence. Do not open reauthorization or retry a changed query for an unchanged configuration error. Inspect returned readiness and setup requirements; a search result is not a working connection. Missing credentials block execution, but an installed Skill with only credential or binding-configuration incompatibilities can use request_setup to collect configuration; do not label it absent or require installation. An empty successful discovery result establishes only that the supplied query had no match. Explain that limitation without claiming the service cannot exist; do not repeat the lookup automatically. For connection requests, if configuration, credentials, or authorization are needed, create request_setup directly with the exact discovered identity instead of attempting upsert_binding first. Omit bindingId unless an existing binding has been verified through authorized binding metadata; never invent binding IDs or credential references. For user-facing progress, describe the service and the next useful step in the agent's persona. Keep it brief. Do not narrate schema validation, identity checks, lookup filters, credential references, or internal retries. After saying you are opening setup, emit another update only when there is a real result, a changed plan, or information the user must provide. A successful request_setup result already confirms the pending request: tell the user to complete it and finish the turn without polling list_setup_requests. Do not replace it with repeated generic searches or an unsupported claim that an administrator must install it.`

// LoadCapabilitiesSpec is the load_capabilities tool definition. It is
// byte-identical across steps and catalogs: it carries no per-step enum and
// no search-mode wording, and every ID is validated against the step's
// authorized catalog when it runs.
func LoadCapabilitiesSpec() models.ModelStepTool {
	return models.ModelStepTool{
		Name:        LoadCapabilitiesTool,
		Description: capabilityLoadToolDescription,
		Group:       models.ModelStepToolHarness,
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
// close over their bound prompts, and a requested action brings the other
// actions of its Skill connection within the SkillLoadBudget. The set is
// updated in place, so a later load in the same step sees these IDs as
// already loaded. loaded is every newly visible ID in load order; the
// executor freezes its tools and instructions on the tool entry.
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
	// its instructions cut to fit. Each requested action brings the other
	// actions of its Skill connection (skillSiblings); a sibling that does
	// not fit is left out silently, since it was not asked for.
	pending := map[string]bool{}
	accept := func(closure []string) {
		for _, candidate := range closure {
			if !pending[candidate] {
				pending[candidate] = true
				loaded = append(loaded, candidate)
			}
		}
	}
	itemsOf := func(closure []string) []capabilityLoadedItem {
		var items []capabilityLoadedItem
		for _, candidate := range closure {
			if !pending[candidate] {
				items = append(items, set.loadedItem(candidate, nameOf(candidate)))
			}
		}
		return items
	}
	for index, id := range requested {
		if pending[id] {
			continue
		}
		closure := set.closure(id)
		trial := result
		trial.Loaded = append(append([]capabilityLoadedItem(nil), result.Loaded...), itemsOf(closure)...)
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
		accept(closure)
		for _, sibling := range set.skillSiblings(id, pending) {
			closure := set.closure(sibling)
			trial := result
			trial.Loaded = append(append([]capabilityLoadedItem(nil), result.Loaded...), itemsOf(closure)...)
			trial.NotLoaded = capabilityNotLoaded(set, requested[index+1:])
			if !capabilityContentFits(finishCapabilityLoad(trial)) {
				continue
			}
			trial.NotLoaded = nil
			result = trial
			accept(closure)
		}
	}
	for _, id := range loaded {
		set.markLoaded(id)
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
			loaded[i].Instructions = models.TruncateUTF8(full[i], mid)
			loaded[i].InstructionsTruncated = true
			if capabilityContentFits(finishCapabilityLoad(c)) {
				low = mid
			} else {
				high = mid - 1
			}
		}
		loaded[i].Instructions = models.TruncateUTF8(full[i], low)
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
	return err == nil && len(encoded) <= models.MaxToolResultContentBytes
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
