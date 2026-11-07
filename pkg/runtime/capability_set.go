package runtime

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/axiom-studio/openseal/pkg/capability"
)

// Deferred capability loading (section 07 §7). A large Agent does not see
// every authorized capability as a tool. Each step starts from the pinned set
// (the chat's host tools and the openseal.builtin Skill) plus every capability
// loaded earlier on the rendered session path. The model loads more with the
// load_capabilities kernel tool and calls them in its next step. The set is a
// pure function of the claim's authorized catalog and the session path; it
// never adds authority, it only intersects the catalog.

const (
	LoadCapabilitiesTool   = "load_capabilities"
	SearchCapabilitiesTool = "search_capabilities"

	// pinnedCapabilitySkillID is the platform Skill whose instructions and
	// actions are always visible, like a coding agent's built-in tools. It
	// equals builtin.SkillID.
	pinnedCapabilitySkillID = "openseal.builtin"

	defaultPinnedCapabilityBytes  = 64 << 10
	defaultCapabilityCatalogMax   = 64
	defaultCapabilityCatalogBytes = 16 << 10
	defaultCapabilitySummaryBytes = 64 << 10

	// capabilityHandleMinLength is the prompt ID length past which discovery
	// shows a short handle instead.
	capabilityHandleMinLength = 64
	// capabilityDescriptionRunes bounds a discovery description.
	capabilityDescriptionRunes = 180
)

// Capability ID prefixes.
const (
	capabilityActionPrefix    = "action:"
	capabilityPromptPrefix    = "prompt:"
	capabilityWorkspacePrefix = "workspace:"
	capabilityRunbookPrefix   = "runbook:"
)

// CapabilityCatalog is the authorized catalog of one claim (from the resolved binding).
type CapabilityCatalog struct {
	Scope             Scope
	AgentID           string
	Actions           []capability.ModelAction
	SkillPrompts      []HostedSkillPrompt
	HostTools         []HostToolSpec
	RunbookOperations []HostedRunbookOperation
}

// CapabilitySetConfig bounds the pinned set and decides search mode. Zero or
// negative fields take their defaults.
type CapabilitySetConfig struct {
	PinnedBytes  int // default 64 KiB: pinned definitions, skipped (never truncated) past it
	CatalogMax   int // default 64 entries before search mode
	CatalogBytes int // default 16 KiB of listing before search mode
	SummaryBytes int // default 64 KiB: directory bound in search mode
}

func (c CapabilitySetConfig) withDefaults() CapabilitySetConfig {
	if c.PinnedBytes <= 0 {
		c.PinnedBytes = defaultPinnedCapabilityBytes
	}
	if c.CatalogMax <= 0 {
		c.CatalogMax = defaultCapabilityCatalogMax
	}
	if c.CatalogBytes <= 0 {
		c.CatalogBytes = defaultCapabilityCatalogBytes
	}
	if c.SummaryBytes <= 0 {
		c.SummaryBytes = defaultCapabilitySummaryBytes
	}
	return c
}

// capabilityEntry is one catalog line: the canonical ID, its kind
// (instructions | action | workspace | runbook) and discovery text.
type capabilityEntry struct {
	ID          string
	Kind        string
	Name        string
	Description string
}

// CapabilitySet is the visible capability set of one step.
type CapabilitySet struct {
	catalog   CapabilityCatalog
	config    CapabilitySetConfig
	deferred  bool
	searching bool

	entries   []capabilityEntry
	prompts   map[string]HostedSkillPrompt
	actions   map[string]capability.ModelAction
	hostTools map[string]HostToolSpec
	runbooks  map[string]HostedRunbookOperation

	// Discovery handles are presentation only; loaded IDs stay canonical.
	discoveryIDs map[string]string
	canonicalIDs map[string]string

	pinned map[string]bool
	loaded map[string]bool
	// order is the visible order: pinned first, then loads in first-load
	// order. Appending keeps the tool list a prefix of the next step's.
	order  []string
	prefix string
}

// NewCapabilitySet builds the visible set for one step: pinned first, then
// loadedIDs (from LoadedCapabilityIDs) in order, each closed over its bound
// prompts, intersected with the catalog. A catalog with duplicate IDs keeps
// the first definition of each.
func NewCapabilitySet(catalog CapabilityCatalog, loadedIDs []string, config CapabilitySetConfig) *CapabilitySet {
	s := &CapabilitySet{
		catalog:   catalog,
		config:    config.withDefaults(),
		prompts:   map[string]HostedSkillPrompt{},
		actions:   map[string]capability.ModelAction{},
		hostTools: map[string]HostToolSpec{},
		runbooks:  map[string]HostedRunbookOperation{},
		pinned:    map[string]bool{},
		loaded:    map[string]bool{},
	}
	s.indexCatalog()
	s.initDiscoveryIDs()
	// Small catalogs fit in one exchange: everything is visible and no
	// load or search tool is offered.
	s.deferred = !(len(catalog.Actions) < 8 && len(catalog.SkillPrompts) < 4 && len(catalog.RunbookOperations) < 4)
	if !s.deferred {
		for _, entry := range s.visibleOrderAll() {
			s.add(entry)
		}
		return s
	}
	s.searching = s.exceedsCatalogBudget()
	for _, id := range s.pinnedIDs() {
		s.markLoaded(id)
	}
	// A bound instruction module loaded with a pinned action is pinned too.
	for id := range s.loaded {
		s.pinned[id] = true
	}
	for _, id := range loadedIDs {
		if canonical, ok := s.Resolve(id); ok {
			s.markLoaded(canonical)
		}
	}
	s.prefix = s.buildPromptPrefix()
	return s
}

func (s *CapabilitySet) indexCatalog() {
	seen := map[string]bool{}
	addEntry := func(entry capabilityEntry) bool {
		if seen[entry.ID] {
			return false
		}
		seen[entry.ID] = true
		s.entries = append(s.entries, entry)
		return true
	}
	for _, p := range s.catalog.SkillPrompts {
		id := capabilityPromptPrefix + p.Reference
		if addEntry(capabilityEntry{id, "instructions", p.Name, capabilityDiscoveryDescription(p.Description)}) {
			s.prompts[id] = p
		}
	}
	for _, a := range s.catalog.Actions {
		id := capabilityActionPrefix + a.Name
		if addEntry(capabilityEntry{id, "action", "", capabilityDiscoveryDescription(a.Description)}) {
			s.actions[id] = a
		}
	}
	for _, o := range s.catalog.HostTools {
		id := capabilityWorkspacePrefix + o.Operation
		if addEntry(capabilityEntry{id, "workspace", o.Operation, capabilityDiscoveryDescription(o.Description)}) {
			s.hostTools[id] = o
		}
	}
	for _, o := range s.catalog.RunbookOperations {
		id := capabilityRunbookPrefix + o.Entrypoint
		if addEntry(capabilityEntry{id, "runbook", o.Name, capabilityDiscoveryDescription(o.Description)}) {
			s.runbooks[id] = o
		}
	}
}

// visibleOrderAll is the order of a non-deferred set: host tools in kernel
// order, then prompts, actions and runbooks in catalog order.
func (s *CapabilitySet) visibleOrderAll() []string {
	var ids []string
	for _, kind := range []string{"workspace", "instructions", "action", "runbook"} {
		for _, entry := range s.entries {
			if entry.Kind == kind {
				ids = append(ids, entry.ID)
			}
		}
	}
	return ids
}

// capabilityDiscoveryDescription bounds discovery text. Loading the exact ID
// supplies the complete instructions and schema.
func capabilityDiscoveryDescription(value string) string {
	clean := strings.Join(strings.Fields(value), " ")
	runes := []rune(clean)
	if len(runes) <= capabilityDescriptionRunes {
		return clean
	}
	return strings.TrimSpace(string(runes[:capabilityDescriptionRunes])) + "…"
}

// initDiscoveryIDs compacts only long instruction references. A handle binds
// the exact reference, scope and Agent, never a list index, so removing a
// capability cannot repoint its old handle. Any collision, including a
// canonical ID that looks like a generated handle, falls back to the full ID.
func (s *CapabilitySet) initDiscoveryIDs() {
	occupied := make(map[string]int, len(s.entries))
	candidates := map[string]string{}
	for _, entry := range s.entries {
		occupied[entry.ID]++
		if entry.Kind == "instructions" && len(entry.ID) > capabilityHandleMinLength {
			sum := sha256.Sum256([]byte(fmt.Sprintf("%q/%q/%q/%q", s.catalog.Scope.Kind, s.catalog.Scope.ID, s.catalog.AgentID, entry.ID)))
			handle := fmt.Sprintf("%s%x", capabilityPromptPrefix, sum[:8])
			candidates[entry.ID] = handle
			occupied[handle]++
		}
	}
	s.discoveryIDs = map[string]string{}
	s.canonicalIDs = map[string]string{}
	for id, handle := range candidates {
		if occupied[handle] == 1 && occupied[id] == 1 {
			s.discoveryIDs[id] = handle
			s.canonicalIDs[handle] = id
		}
	}
}

// discoveryID is the ID the model sees for a canonical ID.
func (s *CapabilitySet) discoveryID(id string) string {
	if handle, ok := s.discoveryIDs[id]; ok {
		return handle
	}
	return id
}

func (s *CapabilitySet) known(id string) bool {
	_, prompt := s.prompts[id]
	_, action := s.actions[id]
	_, host := s.hostTools[id]
	_, runbook := s.runbooks[id]
	return prompt || action || host || runbook
}

func (s *CapabilitySet) add(id string) bool {
	if s.loaded[id] {
		return false
	}
	s.loaded[id] = true
	s.order = append(s.order, id)
	return true
}

// boundPrompts are an action's bound Skill instructions: prerequisites the
// model cannot bypass by selecting only the action's schema.
func (s *CapabilitySet) boundPrompts(id string) []string {
	action, ok := s.actions[id]
	if !ok {
		return nil
	}
	var ids []string
	for _, p := range s.catalog.SkillPrompts {
		promptID := capabilityPromptPrefix + p.Reference
		if p.SkillID == action.SkillID && p.Version == action.Version && s.known(promptID) {
			ids = append(ids, promptID)
		}
	}
	return ids
}

// closure is what loading a canonical ID adds, in markLoaded order: its bound
// prompts first, then the ID, without anything already visible.
func (s *CapabilitySet) closure(id string) []string {
	var ids []string
	for _, candidate := range append(s.boundPrompts(id), id) {
		if !s.loaded[candidate] && !slices.Contains(ids, candidate) {
			ids = append(ids, candidate)
		}
	}
	return ids
}

// markLoaded loads one canonical catalog ID with its bound prompts and
// returns the newly visible IDs.
func (s *CapabilitySet) markLoaded(id string) []string {
	if !s.known(id) {
		return nil
	}
	added := s.closure(id)
	for _, candidate := range added {
		s.add(candidate)
	}
	return added
}

// definitionCost is the encoded size of one capability's full definition.
func (s *CapabilitySet) definitionCost(id string) int {
	var value interface{}
	if p, ok := s.prompts[id]; ok {
		value = p
	} else if a, ok := s.actions[id]; ok {
		value = a
	} else if o, ok := s.hostTools[id]; ok {
		value = struct {
			Name        string                 `json:"name"`
			Description string                 `json:"description,omitempty"`
			InputSchema map[string]interface{} `json:"inputSchema,omitempty"`
		}{o.Operation, o.Description, o.InputSchema}
	} else if o, ok := s.runbooks[id]; ok {
		value = o
	}
	encoded, _ := json.Marshal(value)
	return len(encoded)
}

// pinnedIDs is the fixed always-visible set: the chat's host tools in kernel
// order, then the built-in Skill's prompts by reference and its actions by
// name. An ID whose definition (plus bound prompts not yet counted) does not
// fit PinnedBytes is skipped, never truncated, and stays loadable.
func (s *CapabilitySet) pinnedIDs() []string {
	var hostIDs, prompts, actions []string
	for _, o := range s.catalog.HostTools {
		hostIDs = append(hostIDs, capabilityWorkspacePrefix+o.Operation)
	}
	for _, p := range s.catalog.SkillPrompts {
		if p.SkillID == pinnedCapabilitySkillID {
			prompts = append(prompts, capabilityPromptPrefix+p.Reference)
		}
	}
	for _, a := range s.catalog.Actions {
		if a.SkillID == pinnedCapabilitySkillID {
			actions = append(actions, capabilityActionPrefix+a.Name)
		}
	}
	sort.Strings(prompts)
	sort.Strings(actions)
	var accepted []string
	counted := map[string]bool{}
	seen := map[string]bool{}
	used := 0
	for _, id := range append(append(hostIDs, prompts...), actions...) {
		if seen[id] || !s.known(id) {
			continue
		}
		seen[id] = true
		cost := 0
		var adds []string
		for _, candidate := range append(s.boundPrompts(id), id) {
			if !counted[candidate] && !slices.Contains(adds, candidate) {
				adds = append(adds, candidate)
				cost += s.definitionCost(candidate)
			}
		}
		if used+cost > s.config.PinnedBytes {
			continue
		}
		for _, candidate := range adds {
			counted[candidate] = true
		}
		used += cost
		accepted = append(accepted, id)
	}
	return accepted
}

// Deferred reports whether loading is in effect. It is false for small
// catalogs: then everything is visible and no load/search tool is offered.
func (s *CapabilitySet) Deferred() bool { return s != nil && s.deferred }

// Searching reports search mode: the full catalog listing is past the
// catalog budget, so the prompt carries a directory and the model searches.
func (s *CapabilitySet) Searching() bool { return s != nil && s.searching }

// Visible returns the visible catalog subset in load order (pinned, then loads).
func (s *CapabilitySet) Visible() (actions []capability.ModelAction, prompts []HostedSkillPrompt, hostTools []HostToolSpec, runbooks []HostedRunbookOperation) {
	if s == nil {
		return nil, nil, nil, nil
	}
	for _, id := range s.order {
		if p, ok := s.prompts[id]; ok {
			prompts = append(prompts, p)
		}
		if a, ok := s.actions[id]; ok {
			actions = append(actions, a)
		}
		if o, ok := s.hostTools[id]; ok {
			hostTools = append(hostTools, o)
		}
		if o, ok := s.runbooks[id]; ok {
			runbooks = append(runbooks, o)
		}
	}
	return actions, prompts, hostTools, runbooks
}

// IDs returns the visible canonical IDs in order.
func (s *CapabilitySet) IDs() []string {
	if s == nil {
		return nil
	}
	return append([]string(nil), s.order...)
}

// Loaded reports whether an ID (canonical or discovery handle) is visible.
func (s *CapabilitySet) Loaded(id string) bool {
	if s == nil {
		return false
	}
	canonical, ok := s.Resolve(id)
	return ok && s.loaded[canonical]
}

// Pinned reports whether a canonical ID is part of the pinned set.
func (s *CapabilitySet) Pinned(id string) bool { return s != nil && s.pinned[id] }

// Resolve maps a discovery handle or canonical ID to the canonical ID; ok=false if absent.
func (s *CapabilitySet) Resolve(id string) (string, bool) {
	if s == nil {
		return "", false
	}
	id = strings.TrimSpace(id)
	if canonical, ok := s.canonicalIDs[id]; ok {
		return canonical, true
	}
	if s.known(id) {
		return id, true
	}
	return "", false
}

// PromptPrefix is the stable system-prompt block: loading instructions plus
// the full catalog listing (or the directory in search mode). It depends only
// on the catalog, never on what is loaded. It is "" when loading is not in
// effect.
func (s *CapabilitySet) PromptPrefix() string {
	if s == nil {
		return ""
	}
	return s.prefix
}

// LoadedCapabilityIDs walks the rendered session path and returns, in
// first-load order, the canonical IDs from every completed load_capabilities
// tool entry (ToolEntry.LoadedCapabilities). Pinned IDs are not included;
// NewCapabilitySet adds them. Duplicates keep their first position.
//
// AgentSessionPath.Entries already starts at the latest compaction's kept
// tail. If the slice still holds a compaction entry, loads before its
// FirstKeptEntryID are dropped, as the rendered path drops them.
func LoadedCapabilityIDs(path []*SessionEntry) []string {
	type load struct {
		entryID string
		ids     []string
	}
	var loads []load
	var seenIDs []string
	for _, entry := range path {
		if entry == nil {
			continue
		}
		seenIDs = append(seenIDs, entry.ID)
		switch entry.Type {
		case SessionEntryCompaction:
			kept := ""
			if entry.Compaction != nil {
				kept = entry.Compaction.FirstKeptEntryID
			}
			start := len(seenIDs) - 1 // nothing kept
			for index, id := range seenIDs[:len(seenIDs)-1] {
				if kept != "" && id == kept {
					start = index
					break
				}
			}
			keptIDs := map[string]bool{}
			for _, id := range seenIDs[start:] {
				keptIDs[id] = true
			}
			filtered := loads[:0]
			for _, l := range loads {
				if keptIDs[l.entryID] {
					filtered = append(filtered, l)
				}
			}
			loads = filtered
		case SessionEntryTool:
			if entry.Tool != nil && entry.Tool.Name == LoadCapabilitiesTool && entry.State == ToolEntryCompleted && len(entry.Tool.LoadedCapabilities) > 0 {
				loads = append(loads, load{entryID: entry.ID, ids: entry.Tool.LoadedCapabilities})
			}
		}
	}
	var result []string
	seen := map[string]bool{}
	for _, l := range loads {
		for _, id := range l.ids {
			id = strings.TrimSpace(id)
			if id != "" && !seen[id] {
				seen[id] = true
				result = append(result, id)
			}
		}
	}
	return result
}

// catalogListingBytes is the size of the full capabilityCatalog listing.
func (s *CapabilitySet) catalogListingBytes() int {
	total := 0
	for _, entry := range s.entries {
		total += len("\n- ") + len(s.discoveryID(entry.ID)) + len(" — ") + len(entry.Name) + len(": ") + len(entry.Description)
	}
	return total
}

func (s *CapabilitySet) exceedsCatalogBudget() bool {
	if len(s.entries) > s.config.CatalogMax {
		return true
	}
	return s.catalogListingBytes() > s.config.CatalogBytes
}

// capabilityGroup is one capabilityDirectory line.
type capabilityGroup struct {
	key          string
	name         string
	tools        int
	instructions bool
}

// capabilityActionGroup is the directory group of an action: a binding's
// tools (mcp.<binding>.<tool>) group under mcp.<binding>, others under their
// Skill.
func capabilityActionGroup(a capability.ModelAction) (string, bool) {
	if a.BindingTool != nil {
		if dot := strings.LastIndex(a.Name, "."); dot > 0 {
			return a.Name[:dot], true
		}
	}
	return a.SkillID, false
}

// capabilityGroups summarizes the catalog per Skill or connected server,
// with host tools under workspace and runbooks under runbooks, ordered by key.
func (s *CapabilitySet) capabilityGroups() []capabilityGroup {
	groups := map[string]*capabilityGroup{}
	group := func(key string) *capabilityGroup {
		if groups[key] == nil {
			groups[key] = &capabilityGroup{key: key}
		}
		return groups[key]
	}
	bindingGroups := map[string]string{}
	for _, a := range s.catalog.Actions {
		key, bound := capabilityActionGroup(a)
		if bound {
			bindingGroups[a.SkillID+"\x00"+a.BindingID] = key
		}
		group(key).tools++
	}
	for _, p := range s.catalog.SkillPrompts {
		key := p.SkillID
		if bound, ok := bindingGroups[p.SkillID+"\x00"+p.BindingID]; ok {
			key = bound
		}
		g := group(key)
		g.instructions = true
		if g.name == "" {
			g.name = strings.Join(strings.Fields(p.Name), " ")
		}
	}
	for range s.catalog.HostTools {
		group("workspace").tools++
	}
	for range s.catalog.RunbookOperations {
		group("runbooks").tools++
	}
	result := make([]capabilityGroup, 0, len(groups))
	for _, g := range groups {
		result = append(result, *g)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].key < result[j].key })
	return result
}

// writeCapabilityDirectory writes the directory bounded by SummaryBytes; the
// remainder is counted and found by search.
func (s *CapabilitySet) writeCapabilityDirectory(b *strings.Builder) {
	groups := s.capabilityGroups()
	b.WriteString("\n\ncapabilityDirectory (one Skill or connected server per line; search to find its capabilities):")
	written := 0
	for index, g := range groups {
		line := "\n- " + g.key
		if g.name != "" && g.name != g.key {
			line += " — " + capabilityDiscoveryDescription(g.name)
		}
		switch {
		case g.tools == 1:
			line += " (1 tool"
		case g.tools > 1:
			line += " (" + strconv.Itoa(g.tools) + " tools"
		default:
			line += " (instructions"
		}
		if g.tools > 0 && g.instructions {
			line += ", instructions"
		}
		line += ")"
		if written+len(line) > s.config.SummaryBytes {
			b.WriteString("\n- … " + strconv.Itoa(len(groups)-index) + " more; search to find them")
			return
		}
		b.WriteString(line)
		written += len(line)
	}
}

func (s *CapabilitySet) buildPromptPrefix() string {
	var b strings.Builder
	if s.searching {
		b.WriteString(capabilitySearchSelectionInstructions)
		b.WriteString(capabilitySearchActionLoadingInstructions)
		b.WriteString(capabilitySkillDiscoverySearchInstructions)
		s.writeCapabilityDirectory(&b)
		return b.String()
	}
	b.WriteString(capabilitySelectionInstructions)
	b.WriteString(capabilityActionLoadingInstructions)
	b.WriteString(capabilitySkillDiscoveryInstructions)
	b.WriteString("\n\ncapabilityCatalog (one key per line, then its description):")
	for _, entry := range s.entries {
		text := entry.Description
		if entry.Name != "" {
			text = entry.Name + ": " + text
		}
		b.WriteString("\n- " + s.discoveryID(entry.ID) + " — " + text)
	}
	return b.String()
}

const capabilitySelectionInstructions = `Capability loading. Core capabilities are already loaded: their tools are callable now and their instructions are already in your context. Every authorized capability is listed by ID in capabilityCatalog below, including the ones already loaded. Answer directly, or use a loaded tool, whenever that is enough. Only when the task needs a catalog capability that is not loaded, call load_capabilities with one to eight exact catalog IDs as a normal step; its instructions arrive with the result. Loaded tools become callable in your next step. Loading reveals authorized instructions and schemas only; it never executes or authorizes an action. Do not load irrelevant capabilities for simple replies or invent arguments for unloaded ones. Existing approval, tenant, evidence and action validation rules remain in force.`

const capabilityActionLoadingInstructions = ` Copy capabilityCatalog keys verbatim, including their prefix; an action name alone is not a lookup ID. If the catalog key is action:service.fetch, pass {"ids":["action:service.fetch"]}; service.fetch alone is the action name. Loading an action ID automatically loads its bound Skill instructions and argument schema together. When the needed action is identifiable in capabilityCatalog, load that action directly; do not first load its instruction module in a separate discovery round. Batch independently needed capabilities in one lookup. Instruction-only capabilities remain available when their guidance is needed without an action.`

const capabilitySearchSelectionInstructions = `Capability loading. Core capabilities are already loaded: their tools are callable now and their instructions are already in your context. This Agent has more capabilities than fit here, so capabilityDirectory below lists one line per Skill or connected server instead of every capability. Answer directly, or use a loaded tool, whenever that is enough. Only when the task needs a capability that is not loaded, call search_capabilities with a few keywords (the service, the object and the verb, e.g. "notion search pages") to get exact capability IDs, then call load_capabilities with one to eight of those IDs as the next step; their instructions arrive with the result. Loaded tools become callable in your next step. Searching and loading reveal authorized instructions and schemas only; they never execute or authorize an action. Do not search or load for simple replies, and never invent IDs: copy them from search results. Existing approval, tenant, evidence and action validation rules remain in force.`

const capabilitySearchActionLoadingInstructions = ` Copy IDs from search_capabilities results verbatim, including their prefix; an action name alone is not a lookup ID. Loading an action ID automatically loads its bound Skill instructions and argument schema together; do not first load its instruction module in a separate round. Batch independently needed capabilities in one lookup. If a search finds nothing useful, try other keywords once, then answer with what you have.`

// capabilitySkillDiscoveryInstructions is Atlas's hostedSkillDiscoveryInstructions verbatim.
const capabilitySkillDiscoveryInstructions = ` For Skill discovery, use the requested service or Skill name, or concise task keywords, as the query. Omit requiredActions on the first lookup; it is an exact-name filter, not a task description. Never put natural-language task descriptions there. After inspecting a discovery result, copy exact actions[].name values if an operation filter is needed. Discovery action names are permission identifiers, not executable capability IDs: load operations only by copying their exact id from capabilityCatalog, never by constructing action: plus a discovered name. If the operation is absent from capabilityCatalog, inspect the saved binding access and request the missing access instead of loading an invented ID. A filter mismatch is not evidence of a missing account. Use exact observed operations and an accurate first query; do not automatically repeat a failed lookup or silently broaden its scope. If the account is saved but the needed operation is not enabled, identify all exact discovered operations necessary to complete the user's task, including reading details after listing identifiers, and request them together through request_setup on the existing binding; do not request one permission at a time for a task that already requires several operations; explain that permission is needed instead of asking the user to reconnect the account. Discovery reads metadata; omit maximumRisk unless the user requested a catalog risk filter. A Skill may offer both read and destructive actions; its highest declared risk does not make discovering it dangerous. Execution and approval enforce authority separately. An HTTP 403 does not by itself mean an account needs reconnection: distinguish provider API configuration, organization policy, consent scopes, and authorization using the returned evidence. Do not open reauthorization or retry a changed query for an unchanged configuration error. Inspect returned readiness and setup requirements; a search result is not a working connection. Missing credentials block execution, but an installed Skill with only credential or binding-configuration incompatibilities can use request_setup to collect configuration; do not label it absent or require installation. An empty successful discovery result establishes only that the supplied query had no match. Explain that limitation without claiming the service cannot exist; do not repeat the lookup automatically. For connection requests, if configuration, credentials, or authorization are needed, create request_setup directly with the exact discovered identity instead of attempting upsert_binding first. Omit bindingId unless an existing binding has been verified through authorized binding metadata; never invent binding IDs or credential references. For user-facing progress, describe the service and the next useful step in the agent's persona. Keep it brief. Do not narrate schema validation, identity checks, lookup filters, credential references, or internal retries. After saying you are opening setup, emit another update only when there is a real result, a changed plan, or information the user must provide. A successful request_setup result already confirms the pending request: tell the user to complete it and finish the turn without polling list_setup_requests. Do not replace it with repeated generic searches or an unsupported claim that an administrator must install it.`

// capabilitySkillDiscoverySearchInstructions takes exact IDs from search
// results instead of a listed catalog.
var capabilitySkillDiscoverySearchInstructions = strings.NewReplacer(
	"from capabilityCatalog", "from search_capabilities results",
	"absent from capabilityCatalog", "absent from search_capabilities results",
).Replace(capabilitySkillDiscoveryInstructions)
