package modelstep

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
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

	// PinnedCapabilitySkillID is the platform Skill whose instructions and
	// actions are always visible, like a coding agent's built-in tools. It
	// equals builtin.SkillID.
	PinnedCapabilitySkillID = "openseal.builtin"

	defaultPinnedCapabilityBytes  = 64 << 10
	defaultCapabilityCatalogMax   = 64
	defaultCapabilityCatalogBytes = 16 << 10
	defaultCapabilitySummaryBytes = 64 << 10
	defaultSkillLoadBudgetBytes   = 12 << 10

	// capabilityHandleMinLength is the prompt ID length past which discovery
	// shows a short handle instead.
	capabilityHandleMinLength = 64
	// capabilityDescriptionRunes bounds a discovery description.
	capabilityDescriptionRunes = 180
)

// Capability ID prefixes.
const (
	CapabilityPromptPrefix    = "prompt:"
	CapabilityWorkspacePrefix = "workspace:"
	capabilityRunbookPrefix   = "runbook:"
)

// CapabilityCatalog is the authorized catalog of one claim (from the resolved binding).
type CapabilityCatalog struct {
	Scope             models.Scope
	AgentID           string
	Actions           []capability.ModelAction
	SkillPrompts      []models.HostedSkillPrompt
	HostTools         []HostToolSpec
	RunbookOperations []agentrun.HostedRunbookOperation
}

// CapabilitySetConfig bounds the pinned set and decides search mode. Zero or
// negative fields take their defaults.
type CapabilitySetConfig struct {
	PinnedBytes  int // default 64 KiB: pinned definitions, skipped (never truncated) past it
	CatalogMax   int // default 64 entries before search mode
	CatalogBytes int // default 16 KiB of listing before search mode
	SummaryBytes int // default 64 KiB: directory bound in search mode
	// SkillLoadBudget bounds the definitions a whole-Skill load surfaces
	// beyond the requested action (default 12 KiB).
	SkillLoadBudget int
	// PinnedSkillIDs are Skills pinned like the built-in one: their
	// instructions and actions are visible from the first step, within
	// PinnedBytes. Pinning is part of the binding, so it never changes
	// what earlier steps declared.
	PinnedSkillIDs []string
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
	if c.SkillLoadBudget <= 0 {
		c.SkillLoadBudget = defaultSkillLoadBudgetBytes
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
	searching bool

	entries   []capabilityEntry
	prompts   map[string]models.HostedSkillPrompt
	actions   map[string]capability.ModelAction
	hostTools map[string]HostToolSpec
	runbooks  map[string]agentrun.HostedRunbookOperation

	// Discovery handles are presentation only; loaded IDs stay canonical.
	discoveryIDs map[string]string
	canonicalIDs map[string]string

	pinned map[string]bool
	loaded map[string]bool
	// order is the visible order: pinned first, then loads in first-load
	// order. Appending keeps the tool list a prefix of the next step's.
	order []string
}

// NewCapabilitySet builds the visible set for one step: pinned first, then
// loadedIDs (from PathLoadedCapabilityIDs) in order, each closed over its bound
// prompts, intersected with the catalog. A catalog with duplicate IDs keeps
// the first definition of each.
func NewCapabilitySet(catalog CapabilityCatalog, loadedIDs []string, config CapabilitySetConfig) *CapabilitySet {
	return newCapabilitySet(catalog, nil, loadedIDs, config)
}

// NewPathCapabilitySet is the visible set of a step over path: the tools
// carried over its latest compaction, then what the path's loads loaded.
func NewPathCapabilitySet(catalog CapabilityCatalog, path *models.AgentSessionPath, config CapabilitySetConfig) *CapabilitySet {
	return newCapabilitySet(catalog, PathCarriedCapabilityIDs(path), PathLoadedCapabilityIDs(path), config)
}

// newCapabilitySet takes carriedIDs as tools only: a carried tool's bound
// instructions were in a load result the compaction summarized, so they are
// not loaded, and loading them (or the tool again) returns their text.
func newCapabilitySet(catalog CapabilityCatalog, carriedIDs, loadedIDs []string, config CapabilitySetConfig) *CapabilitySet {
	s := &CapabilitySet{
		catalog:   catalog,
		config:    config.withDefaults(),
		prompts:   map[string]models.HostedSkillPrompt{},
		actions:   map[string]capability.ModelAction{},
		hostTools: map[string]HostToolSpec{},
		runbooks:  map[string]agentrun.HostedRunbookOperation{},
		pinned:    map[string]bool{},
		loaded:    map[string]bool{},
	}
	s.indexCatalog()
	s.initDiscoveryIDs()
	// Loading is always deferred, even for a small catalog: connecting a
	// Skill then only adds catalog lines to the capabilities note and never
	// rewrites the tools or system prompt the model already saw.
	s.searching = s.exceedsCatalogBudget()
	for _, id := range s.pinnedIDs() {
		s.markLoaded(id)
	}
	// A bound instruction module loaded with a pinned action is pinned too.
	for id := range s.loaded {
		s.pinned[id] = true
	}
	for _, id := range carriedIDs {
		if canonical, ok := s.Resolve(id); ok && s.Known(canonical) {
			s.add(canonical)
		}
	}
	for _, id := range loadedIDs {
		if canonical, ok := s.Resolve(id); ok {
			s.markLoaded(canonical)
		}
	}
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
		id := CapabilityPromptPrefix + p.Reference
		if addEntry(capabilityEntry{id, "instructions", p.Name, capabilityDiscoveryDescription(p.Description)}) {
			s.prompts[id] = p
		}
	}
	for _, a := range s.catalog.Actions {
		id := models.CapabilityActionPrefix + a.Name
		if addEntry(capabilityEntry{id, "action", "", capabilityDiscoveryDescription(a.Description)}) {
			s.actions[id] = a
		}
	}
	for _, o := range s.catalog.HostTools {
		id := CapabilityWorkspacePrefix + o.Operation
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
			handle := fmt.Sprintf("%s%x", CapabilityPromptPrefix, sum[:8])
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

// Known reports whether id is a capability in the authorized catalog.
func (s *CapabilitySet) Known(id string) bool {
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
		promptID := CapabilityPromptPrefix + p.Reference
		if p.SkillID == action.SkillID && p.Version == action.Version && s.Known(promptID) {
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
	if !s.Known(id) {
		return nil
	}
	added := s.closure(id)
	for _, candidate := range added {
		s.add(candidate)
	}
	return added
}

// toolCost is the encoded size of the tool definition an action or host-tool
// ID surfaces: name, description and input schema.
func (s *CapabilitySet) toolCost(id string) int {
	var name, description string
	var schema map[string]interface{}
	if a, ok := s.actions[id]; ok {
		name, description, schema = a.Name, a.Description, GovernedToolInputSchema(a)
	} else if o, ok := s.hostTools[id]; ok {
		name, description, schema = o.ToolName, o.Description, o.InputSchema
	} else {
		return s.definitionCost(id)
	}
	encoded, _ := json.Marshal(schema)
	return len(name) + len(description) + len(encoded)
}

// skillSiblings are the actions a load of action id brings along (whole-Skill
// load): the not yet visible actions of the same Skill connection (Skill,
// version and binding), in catalog order, while their tool definitions fit
// SkillLoadBudget after id's own. Instructions and runbooks have none.
func (s *CapabilitySet) skillSiblings(id string, pending map[string]bool) []string {
	action, ok := s.actions[id]
	if !ok {
		return nil
	}
	used := s.toolCost(id)
	var siblings []string
	for _, candidate := range s.catalog.Actions {
		sibling := models.CapabilityActionPrefix + candidate.Name
		if sibling == id || s.loaded[sibling] || pending[sibling] || slices.Contains(siblings, sibling) {
			continue
		}
		if candidate.SkillID != action.SkillID || candidate.Version != action.Version || candidate.BindingID != action.BindingID {
			continue
		}
		if s.actions[sibling].Name != candidate.Name {
			continue // a duplicate catalog name keeps its first definition
		}
		cost := s.toolCost(sibling)
		if used+cost > s.config.SkillLoadBudget {
			continue
		}
		used += cost
		siblings = append(siblings, sibling)
	}
	return siblings
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
		hostIDs = append(hostIDs, CapabilityWorkspacePrefix+o.Operation)
	}
	for _, p := range s.catalog.SkillPrompts {
		if s.pinnedSkill(p.SkillID) {
			prompts = append(prompts, CapabilityPromptPrefix+p.Reference)
		}
	}
	for _, a := range s.catalog.Actions {
		if s.pinnedSkill(a.SkillID) {
			actions = append(actions, models.CapabilityActionPrefix+a.Name)
		}
	}
	sort.Strings(prompts)
	sort.Strings(actions)
	var accepted []string
	counted := map[string]bool{}
	seen := map[string]bool{}
	used := 0
	for _, id := range append(append(hostIDs, prompts...), actions...) {
		if seen[id] || !s.Known(id) {
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

func (s *CapabilitySet) pinnedSkill(skillID string) bool {
	return skillID == PinnedCapabilitySkillID || slices.Contains(s.config.PinnedSkillIDs, skillID)
}

// Offered reports whether the step offers capability search and loading: the
// catalog has any capability at all. Every chat catalog has the built-in Skill
// and the workspace tools, so this only leaves out a bare catalog (an Agent
// trial with no Skills).
func (s *CapabilitySet) Offered() bool { return s != nil && len(s.entries) > 0 }

// Searching reports search mode: the full catalog listing is past the
// catalog budget, so the capabilities note carries a directory and the model searches.
func (s *CapabilitySet) Searching() bool { return s != nil && s.searching }

// Visible returns the visible catalog subset in load order (pinned, then loads).
func (s *CapabilitySet) Visible() (actions []capability.ModelAction, prompts []models.HostedSkillPrompt, hostTools []HostToolSpec, runbooks []agentrun.HostedRunbookOperation) {
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
	if s.Known(id) {
		return id, true
	}
	return "", false
}

// catalogListingBytes is the size of the full catalog listing.
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

// capabilityGroup is one directory line.
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

// DirectoryLines are the directory lines bounded by SummaryBytes, keyed by
// group; the remainder is counted and found by search.
func (s *CapabilitySet) DirectoryLines() []CapabilityNoteItem {
	groups := s.capabilityGroups()
	var items []CapabilityNoteItem
	written := 0
	for index, g := range groups {
		line := "- " + g.key
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
			items = append(items, CapabilityNoteItem{Key: "group:…", Section: capabilityNoteCatalog,
				Text: "- … " + strconv.Itoa(len(groups)-index) + " more; search to find them"})
			return items
		}
		items = append(items, CapabilityNoteItem{Key: "group:" + g.key, Section: capabilityNoteCatalog, Text: line})
		written += len(line)
	}
	return items
}

// CatalogLines are the full listing, one line per catalog ID.
func (s *CapabilitySet) CatalogLines() []CapabilityNoteItem {
	items := make([]CapabilityNoteItem, 0, len(s.entries))
	for _, entry := range s.entries {
		text := entry.Description
		if entry.Name != "" {
			text = entry.Name + ": " + text
		}
		id := s.discoveryID(entry.ID)
		items = append(items, CapabilityNoteItem{Key: id, Section: capabilityNoteCatalog, Text: "- " + id + " — " + text})
	}
	return items
}

// CatalogAction is the authorized governed action with capability id id.
func (s *CapabilitySet) CatalogAction(id string) (capability.ModelAction, bool) {
	action, ok := s.actions[id]
	return action, ok
}

// CatalogHostTool is the authorized host tool with capability id id.
func (s *CapabilitySet) CatalogHostTool(id string) (HostToolSpec, bool) {
	spec, ok := s.hostTools[id]
	return spec, ok
}
