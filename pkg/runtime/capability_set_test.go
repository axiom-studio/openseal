package runtime

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/builtin"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/workspace"
)

// capabilityCatalogFixture is a deferred catalog: eight Notion-like actions
// bound to one prompt each, four research prompts, and the built-in Skill.
func capabilityCatalogFixture() CapabilityCatalog {
	catalog := CapabilityCatalog{Scope: Scope{Kind: "tenant", ID: "tenant-1"}, AgentID: "agent-1"}
	for i := 0; i < 4; i++ {
		catalog.SkillPrompts = append(catalog.SkillPrompts, HostedSkillPrompt{
			SkillID: fmt.Sprintf("research.%d", i), Version: "1.0.0", Reference: fmt.Sprintf("skill-research-%d@1", i),
			Name: fmt.Sprintf("Research %d", i), Description: "Research guidance", Instructions: fmt.Sprintf("Research instructions %d.", i),
		})
	}
	catalog.SkillPrompts = append(catalog.SkillPrompts, HostedSkillPrompt{
		SkillID: "notion", Version: "3", Reference: "skill-notion@3", Name: "Notion", Description: "Notion workspace", Instructions: "Use Notion carefully.",
	})
	for i := 0; i < 8; i++ {
		catalog.Actions = append(catalog.Actions, capability.ModelAction{
			Name: fmt.Sprintf("notion.op%d", i), SkillID: "notion", Version: "3", Action: fmt.Sprintf("op%d", i),
			Description: fmt.Sprintf("Notion operation %d", i), InputSchema: map[string]interface{}{"type": "object"},
		})
	}
	catalog.SkillPrompts = append(catalog.SkillPrompts, HostedSkillPrompt{
		SkillID: builtin.SkillID, Version: "1", Reference: "builtin-z@1", Name: "Built-in", Instructions: "Built-in instructions.",
	}, HostedSkillPrompt{
		SkillID: builtin.SkillID, Version: "1", Reference: "builtin-a@1", Name: "Built-in A", Instructions: "Built-in A instructions.",
	})
	catalog.Actions = append(catalog.Actions, capability.ModelAction{
		Name: "openseal.builtin.web_search", SkillID: builtin.SkillID, Version: "1", Action: "web_search", Description: "Search the web",
	}, capability.ModelAction{
		Name: "openseal.builtin.generate_image", SkillID: builtin.SkillID, Version: "1", Action: "generate_image", Description: "Make an image",
	})
	catalog.HostTools = HostToolSpecs(workspace.ChatAuthority("chat-1"))
	return catalog
}

func TestPinnedCapabilitySkillIDIsBuiltin(t *testing.T) {
	if pinnedCapabilitySkillID != builtin.SkillID {
		t.Fatalf("pinned skill %q != builtin %q", pinnedCapabilitySkillID, builtin.SkillID)
	}
}

func TestCapabilitySetSmallCatalogIsDeferred(t *testing.T) {
	catalog := CapabilityCatalog{
		Actions:      []capability.ModelAction{{Name: "notion.search", SkillID: "notion", Version: "1"}},
		SkillPrompts: []HostedSkillPrompt{{SkillID: "notion", Version: "1", Reference: "skill-notion@1"}},
		HostTools:    HostToolSpecs(workspace.ChatAuthority("chat-1")),
	}
	s := NewCapabilitySet(catalog, nil, CapabilitySetConfig{})
	if s.Searching() {
		t.Fatal("a small catalog is listed, not searched")
	}
	// Only the pinned host tools are visible; the Skill loads like any other.
	if !s.Loaded("workspace:" + workspace.OperationReadFile) {
		t.Fatal("pinned host tool not visible")
	}
	for _, id := range []string{"action:notion.search", "prompt:skill-notion@1"} {
		if s.Loaded(id) {
			t.Fatalf("%s visible without a load", id)
		}
	}
}

func TestCapabilitySetPinsHostToolsThenBuiltinWithinBudget(t *testing.T) {
	catalog := capabilityCatalogFixture()
	s := NewCapabilitySet(catalog, nil, CapabilitySetConfig{})
	var want []string
	for _, spec := range catalog.HostTools {
		want = append(want, "workspace:"+spec.Operation)
	}
	want = append(want, "prompt:builtin-a@1", "prompt:builtin-z@1",
		"action:openseal.builtin.generate_image", "action:openseal.builtin.web_search")
	if got := s.IDs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("pinned order\n got %v\nwant %v", got, want)
	}
	for _, id := range want {
		if !s.Pinned(id) {
			t.Fatalf("%s not pinned", id)
		}
	}
	if s.Loaded("action:notion.op0") || s.Loaded("prompt:skill-notion@3") {
		t.Fatal("non-pinned capability visible without a load")
	}
	actions, prompts, hostTools, _ := s.Visible()
	if len(hostTools) != len(catalog.HostTools) || hostTools[0].Operation != catalog.HostTools[0].Operation {
		t.Fatalf("host tools: %+v", hostTools)
	}
	if len(prompts) != 2 || len(actions) != 2 || actions[0].Name != "openseal.builtin.generate_image" {
		t.Fatalf("builtin visible: %v %v", prompts, actions)
	}

	// An oversized pinned definition is skipped, never truncated, and stays loadable.
	big := capabilityCatalogFixture()
	big.Actions[len(big.Actions)-2].Description = strings.Repeat("x", 4096)
	budget := 0
	probe := NewCapabilitySet(big, nil, CapabilitySetConfig{})
	for _, id := range probe.IDs() {
		if id != "action:openseal.builtin.web_search" {
			budget += probe.definitionCost(id)
		}
	}
	small := NewCapabilitySet(big, nil, CapabilitySetConfig{PinnedBytes: budget})
	if small.Loaded("action:openseal.builtin.web_search") || !small.Loaded("action:openseal.builtin.generate_image") {
		t.Fatalf("budget skip: %v", small.IDs())
	}
	if !small.Loaded("workspace:" + workspace.OperationRunCommand) {
		t.Fatal("run_command must stay pinned ahead of the builtin Skill")
	}
	reloaded := NewCapabilitySet(big, []string{"action:openseal.builtin.web_search"}, CapabilitySetConfig{PinnedBytes: budget})
	if !reloaded.Loaded("action:openseal.builtin.web_search") || reloaded.Pinned("action:openseal.builtin.web_search") {
		t.Fatal("skipped pinned capability must stay loadable")
	}
	for _, action := range func() []capability.ModelAction { a, _, _, _ := reloaded.Visible(); return a }() {
		if action.Name == "openseal.builtin.web_search" && len(action.Description) != 4096 {
			t.Fatal("definition truncated")
		}
	}
}

func longPromptCatalogFixture() CapabilityCatalog {
	catalog := capabilityCatalogFixture()
	for i := 0; i < 4; i++ {
		catalog.SkillPrompts[i].Reference = fmt.Sprintf("skill:example.research.module.%d@1.2.3#binding:workforce:deployment-1234567890:example.research.module.%d@17", i, i)
	}
	return catalog
}

func TestCapabilitySetDiscoveryHandlesForLongPromptRefs(t *testing.T) {
	catalog := longPromptCatalogFixture()
	s := NewCapabilitySet(catalog, nil, CapabilitySetConfig{})
	id := "prompt:" + catalog.SkillPrompts[3].Reference
	handle := s.discoveryID(id)
	if handle == id || !strings.HasPrefix(handle, "prompt:") || len(handle) != len("prompt:")+16 {
		t.Fatalf("long reference not compacted: %q", handle)
	}
	if short := s.discoveryID("prompt:skill-notion@3"); short != "prompt:skill-notion@3" {
		t.Fatalf("short reference compacted: %q", short)
	}
	listing := capabilityNoteLines(s.catalogLines())
	if !strings.Contains(listing, "\n- "+handle+" — ") || strings.Contains(listing, id) {
		t.Fatal("the capabilities listing must show the handle, not the long ID")
	}
	if got, ok := s.Resolve(handle); !ok || got != id {
		t.Fatalf("Resolve(handle) = %q, %v", got, ok)
	}
	if got, ok := s.Resolve(id); !ok || got != id {
		t.Fatalf("Resolve(canonical) = %q, %v", got, ok)
	}
	// Loads keep canonical identity: a handle passed as loaded ID resolves.
	loaded := NewCapabilitySet(catalog, []string{handle}, CapabilitySetConfig{})
	if !loaded.Loaded(id) || !loaded.Loaded(handle) {
		t.Fatal("handle load not resolved")
	}
	// A handle never rebinds across tenant, agent, version or revocation.
	for _, change := range []string{"tenant", "agent", "version", "revoked", "reordered"} {
		next := longPromptCatalogFixture()
		switch change {
		case "tenant":
			next.Scope.ID = "other"
		case "agent":
			next.AgentID = "other"
		case "version":
			next.SkillPrompts[3].Reference += "changed"
		case "revoked":
			next.SkillPrompts = append(next.SkillPrompts[:3], next.SkillPrompts[4:]...)
		case "reordered":
			next.SkillPrompts[3], next.SkillPrompts[4] = next.SkillPrompts[4], next.SkillPrompts[3]
		}
		got, ok := NewCapabilitySet(next, nil, CapabilitySetConfig{}).Resolve(handle)
		if change == "reordered" {
			if !ok || got != id {
				t.Fatalf("reordering changed identity: %q %v", got, ok)
			}
		} else if ok {
			t.Fatalf("%s: stale or foreign handle resolved to %q", change, got)
		}
	}
}

func TestCapabilitySetHandleCollisionFallsBack(t *testing.T) {
	catalog := longPromptCatalogFixture()
	id := "prompt:" + catalog.SkillPrompts[0].Reference
	handle := NewCapabilitySet(catalog, nil, CapabilitySetConfig{}).discoveryID(id)
	catalog.SkillPrompts[1].Reference = strings.TrimPrefix(handle, "prompt:")
	next := NewCapabilitySet(catalog, nil, CapabilitySetConfig{})
	if next.discoveryID(id) != id {
		t.Fatal("ambiguous handle retained")
	}
	if got, ok := next.Resolve(handle); !ok || got != handle {
		t.Fatalf("canonical ID misresolved: %q %v", got, ok)
	}
}

func TestCapabilitySetLoadedOrderAppendsAndClosesOverBoundPrompts(t *testing.T) {
	catalog := capabilityCatalogFixture()
	base := NewCapabilitySet(catalog, nil, CapabilitySetConfig{})
	pinned := base.IDs()
	s := NewCapabilitySet(catalog, []string{"action:notion.op5", "prompt:skill-research-2@1", "action:notion.op1", "action:notion.op5"}, CapabilitySetConfig{})
	want := append(append([]string(nil), pinned...),
		"prompt:skill-notion@3", "action:notion.op5", "prompt:skill-research-2@1", "action:notion.op1")
	if got := s.IDs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("load order\n got %v\nwant %v", got, want)
	}
	if !reflect.DeepEqual(s.IDs()[:len(pinned)], pinned) {
		t.Fatal("loads must append after the pinned prefix")
	}
	actions, prompts, _, _ := s.Visible()
	if actions[len(actions)-2].Name != "notion.op5" || actions[len(actions)-1].Name != "notion.op1" {
		t.Fatalf("visible actions out of load order: %v", actions)
	}
	if prompts[len(prompts)-2].Reference != "skill-notion@3" || prompts[len(prompts)-1].Instructions != "Research instructions 2." {
		t.Fatalf("visible prompts: %v", prompts)
	}
	if got := s.closure("action:notion.op2"); !reflect.DeepEqual(got, []string{"action:notion.op2"}) {
		t.Fatalf("closure of an action whose prompt is loaded: %v", got)
	}
	if got := base.closure("action:notion.op2"); !reflect.DeepEqual(got, []string{"prompt:skill-notion@3", "action:notion.op2"}) {
		t.Fatalf("closure: %v", got)
	}
}

func TestCapabilitySetFiltersLoadsAbsentFromCatalog(t *testing.T) {
	catalog := capabilityCatalogFixture()
	pinned := NewCapabilitySet(catalog, nil, CapabilitySetConfig{}).IDs()
	// action:notion.op7 was revoked (binding revision); the other unknowns never existed.
	catalog.Actions = append(catalog.Actions[:7], catalog.Actions[8:]...)
	s := NewCapabilitySet(catalog, []string{"action:notion.op7", "action:ghost", "prompt:skill-gone@1", "", "action:notion.op0"}, CapabilitySetConfig{})
	want := append(append([]string(nil), pinned...), "prompt:skill-notion@3", "action:notion.op0")
	if got := s.IDs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
	if s.Loaded("action:notion.op7") {
		t.Fatal("revoked capability stayed visible")
	}
	if _, ok := s.Resolve("action:notion.op7"); ok {
		t.Fatal("revoked capability resolved")
	}
}

// capabilityNoteLines joins listing items as the note renders them.
func capabilityNoteLines(items []capabilityNoteItem) string {
	var b strings.Builder
	for _, item := range items {
		b.WriteString("\n" + item.Text)
	}
	return b.String()
}

func TestCapabilityListingIndependentOfLoads(t *testing.T) {
	catalog := capabilityCatalogFixture()
	empty := NewCapabilitySet(catalog, nil, CapabilitySetConfig{})
	loaded := NewCapabilitySet(catalog, []string{"action:notion.op3", "prompt:skill-research-0@1"}, CapabilitySetConfig{})
	if empty.Searching() {
		t.Fatal("fixture must list the full catalog")
	}
	listing := capabilityNoteLines(empty.catalogLines())
	if listing == "" || listing != capabilityNoteLines(loaded.catalogLines()) {
		t.Fatal("listing depends on loads")
	}
	for _, want := range []string{"\n- action:notion.op3 — Notion operation 3", "\n- prompt:skill-notion@3 — Notion: Notion workspace",
		"\n- workspace:" + workspace.OperationReadFile + " — " + workspace.OperationReadFile + ": "} {
		if !strings.Contains(listing, want) {
			t.Fatalf("listing missing %q", want)
		}
	}
	for _, banned := range []string{"Use Notion carefully.", "Research instructions"} {
		if strings.Contains(listing, banned) {
			t.Fatalf("listing contains %q", banned)
		}
	}
	for _, want := range []string{"load_capabilities", "search_capabilities", "Loaded tools become callable in your next step", "capabilities note"} {
		if !strings.Contains(capabilityLoadingText, want) {
			t.Fatalf("loading instructions missing %q", want)
		}
	}
}

func TestCapabilityDirectorySearchModeBounded(t *testing.T) {
	catalog := capabilityCatalogFixture()
	for server := 0; server < 30; server++ {
		bindingID := fmt.Sprintf("binding-%02d", server)
		catalog.SkillPrompts = append(catalog.SkillPrompts, HostedSkillPrompt{SkillID: "mcp", Version: "1", BindingID: bindingID,
			Reference: fmt.Sprintf("mcp-%02d@1", server), Name: fmt.Sprintf("Server %02d", server), Instructions: "Use the server."})
		for tool := 0; tool < 4; tool++ {
			catalog.Actions = append(catalog.Actions, capability.ModelAction{
				Name: fmt.Sprintf("mcp.server%02d.tool%d", server, tool), SkillID: "mcp", Version: "1", BindingID: bindingID,
				Description: "A connected server tool", BindingTool: &capability.ModelActionBindingTool{Name: fmt.Sprintf("tool%d", tool)},
			})
		}
	}
	s := NewCapabilitySet(catalog, nil, CapabilitySetConfig{})
	if !s.Searching() {
		t.Fatal("more than 64 entries must switch to search mode")
	}
	directory := capabilityNoteLines(s.directoryLines())
	for _, want := range []string{"\n- mcp.server07 — Server 07 (4 tools, instructions)", "\n- notion — Notion (8 tools, instructions)",
		"\n- workspace (" + fmt.Sprint(len(catalog.HostTools)) + " tools)"} {
		if !strings.Contains(directory, want) {
			t.Fatalf("directory missing %q", want)
		}
	}
	if strings.Contains(directory, "action:mcp.server07.tool1") {
		t.Fatal("search mode must not list every capability")
	}
	if directory != capabilityNoteLines(NewCapabilitySet(catalog, []string{"action:mcp.server03.tool2"}, CapabilitySetConfig{}).directoryLines()) {
		t.Fatal("directory depends on loads")
	}

	bounded := capabilityNoteLines(NewCapabilitySet(catalog, nil, CapabilitySetConfig{SummaryBytes: 300}).directoryLines())
	if !strings.Contains(bounded, " more; search to find them") {
		t.Fatalf("directory not bounded: %s", bounded)
	}
	if lines := strings.Count(bounded, "\n- "); lines >= len(s.capabilityGroups()) {
		t.Fatalf("directory lists %d of %d groups", lines, len(s.capabilityGroups()))
	}

	byBytes := capabilityCatalogFixture()
	if !NewCapabilitySet(byBytes, nil, CapabilitySetConfig{CatalogBytes: 200}).Searching() {
		t.Fatal("a listing over the byte budget must switch to search mode")
	}
}

// capabilityLoadEntry is a load_capabilities tool entry: action and
// workspace IDs become frozen tools, other IDs loaded instructions.
func capabilityLoadEntry(id, runID string, state ToolEntryState, loaded ...string) *SessionEntry {
	tool := &ToolEntry{Name: LoadCapabilitiesTool}
	for _, capabilityID := range loaded {
		switch {
		case strings.HasPrefix(capabilityID, capabilityActionPrefix):
			name := "action_" + strings.ReplaceAll(strings.TrimPrefix(capabilityID, capabilityActionPrefix), ".", "_")
			tool.SurfacedTools = append(tool.SurfacedTools, SurfacedTool{Capability: capabilityID, Name: name,
				Description: "Frozen " + name, InputSchema: map[string]interface{}{"type": "object"}})
		case strings.HasPrefix(capabilityID, capabilityWorkspacePrefix):
			name := strings.ReplaceAll(strings.TrimPrefix(capabilityID, capabilityWorkspacePrefix), ".", "_")
			tool.SurfacedTools = append(tool.SurfacedTools, SurfacedTool{Capability: capabilityID, Name: name,
				InputSchema: map[string]interface{}{"type": "object"}})
		default:
			tool.LoadedInstructions = append(tool.LoadedInstructions, capabilityID)
		}
	}
	return &SessionEntry{ID: id, RunID: runID, Type: SessionEntryTool, State: state, Tool: tool}
}

func TestPathLoadedCapabilityIDsIgnoresErroredAndPreCompactionLoads(t *testing.T) {
	entries := []*SessionEntry{
		capabilityLoadEntry("e1", "run-1", ToolEntryCompleted, "prompt:skill-notion@3", "action:notion.op0"),
		capabilityLoadEntry("e2", "run-1", ToolEntryCompleted, "action:notion.op1"),
		{ID: "e3", RunID: "run-1", Type: SessionEntryUser, User: &UserEntry{}},
		capabilityLoadEntry("e4", "run-1", ToolEntryError, "action:notion.op2"),
		capabilityLoadEntry("e5", "run-1", ToolEntryPending, "action:notion.op3"),
		{ID: "e6", Type: SessionEntryTool, State: ToolEntryCompleted, Tool: &ToolEntry{Name: "search_capabilities",
			SurfacedTools: []SurfacedTool{{Capability: "action:notion.op4", Name: "action_notion_op4"}}}},
		capabilityLoadEntry("e7", "run-1", ToolEntryCompleted, "action:notion.op5", "action:notion.op1"),
	}
	if got, want := PathLoadedCapabilityIDs(&AgentSessionPath{Entries: entries}), []string{"prompt:skill-notion@3", "action:notion.op0", "action:notion.op1", "action:notion.op5"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}

	// The store path builder drops pre-compaction entries; the compaction
	// carries the tools of the summarized loads (not their instructions).
	session := &AgentSession{ID: "s1", NextSequence: 6}
	segment := []*SessionEntry{
		withSessionSequence(capabilityLoadEntry("p1", "run-1", ToolEntryCompleted, "prompt:skill-notion@3", "action:notion.op0"), "s1", 1),
		withSessionSequence(capabilityLoadEntry("p2", "run-1", ToolEntryCompleted, "action:notion.op1"), "s1", 2),
		withSessionSequence(&SessionEntry{ID: "p3", Type: SessionEntryCompaction, Compaction: &CompactionEntry{Summary: "s", FirstKeptEntryID: "p2",
			CarriedTools: carriedToolsAfter(nil, segmentPrefix(1))}}, "s1", 3),
		withSessionSequence(capabilityLoadEntry("p4", "run-1", ToolEntryCompleted, "action:notion.op2"), "s1", 4),
	}
	rendered, err := buildAgentSessionPath([]*AgentSession{session}, [][]*SessionEntry{segment}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := PathCarriedCapabilityIDs(rendered), []string{"action:notion.op0"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("rendered path carried %v want %v", got, want)
	}
	if got, want := PathLoadedCapabilityIDs(rendered), []string{"action:notion.op1", "action:notion.op2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("rendered path got %v want %v", got, want)
	}
}

// segmentPrefix is the summarized part of the compaction fixture above.
func segmentPrefix(n int) []*SessionEntry {
	entries := []*SessionEntry{capabilityLoadEntry("p1", "run-1", ToolEntryCompleted, "prompt:skill-notion@3", "action:notion.op0")}
	return entries[:n]
}

func withSessionSequence(entry *SessionEntry, sessionID string, sequence int64) *SessionEntry {
	entry.SessionID = sessionID
	entry.Sequence = sequence
	return entry
}

func TestPathLoadedCapabilityIDsCarryAcrossRunsInSession(t *testing.T) {
	catalog := capabilityCatalogFixture()
	session := &AgentSession{ID: "conv", NextSequence: 5}
	segment := []*SessionEntry{
		withSessionSequence(capabilityLoadEntry("r1-load", "run-1", ToolEntryCompleted, "prompt:skill-notion@3", "action:notion.op4"), "conv", 1),
		withSessionSequence(&SessionEntry{ID: "r2-user", RunID: "run-2", Type: SessionEntryUser, User: &UserEntry{}}, "conv", 2),
		withSessionSequence(capabilityLoadEntry("r2-load", "run-2", ToolEntryCompleted, "action:notion.op6"), "conv", 3),
		withSessionSequence(capabilityLoadEntry("r2-again", "run-2", ToolEntryCompleted), "conv", 4),
	}
	path, err := buildAgentSessionPath([]*AgentSession{session}, [][]*SessionEntry{segment}, nil)
	if err != nil {
		t.Fatal(err)
	}
	loaded := PathLoadedCapabilityIDs(path)
	if want := []string{"prompt:skill-notion@3", "action:notion.op4", "action:notion.op6"}; !reflect.DeepEqual(loaded, want) {
		t.Fatalf("got %v want %v", loaded, want)
	}
	s := NewCapabilitySet(catalog, loaded, CapabilitySetConfig{})
	if !s.Loaded("action:notion.op4") || !s.Loaded("action:notion.op6") {
		t.Fatal("a later Run in the session must see earlier loads")
	}
}

func TestPathLoadedCapabilityIDsChildSessionInheritsParentUpToFork(t *testing.T) {
	parent := &AgentSession{ID: "parent", NextSequence: 4}
	child := &AgentSession{ID: "child", ParentSessionID: "parent", ForkEntryID: "p-load-1", ForkSequence: 1, NextSequence: 2}
	parentEntries := []*SessionEntry{
		withSessionSequence(capabilityLoadEntry("p-load-1", "run-parent", ToolEntryCompleted, "action:notion.op0"), "parent", 1),
		withSessionSequence(capabilityLoadEntry("p-load-2", "run-parent", ToolEntryCompleted, "action:notion.op1"), "parent", 2),
		withSessionSequence(capabilityLoadEntry("p-load-3", "run-parent", ToolEntryCompleted, "action:notion.op2"), "parent", 3),
	}
	childEntries := []*SessionEntry{
		withSessionSequence(capabilityLoadEntry("c-load-1", "run-child", ToolEntryCompleted, "action:notion.op3"), "child", 1),
	}
	path, err := buildAgentSessionPath([]*AgentSession{parent, child}, [][]*SessionEntry{parentEntries, childEntries}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := PathLoadedCapabilityIDs(path), []string{"action:notion.op0", "action:notion.op3"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestLoadSurfacesWholeSkillWithinBudget(t *testing.T) {
	catalog := capabilityCatalogFixture()
	s := NewCapabilitySet(catalog, []string{"action:notion.op3"}, CapabilitySetConfig{})
	_, isError, loaded := ExecuteLoadCapabilities(s, loadArgs("action:notion.op5"), capabilityToolName)
	if isError {
		t.Fatal("load failed")
	}
	// The requested action first, then its Skill's other actions in catalog
	// order; op3 was already loaded and the bound prompt was too.
	want := []string{"action:notion.op5", "action:notion.op0", "action:notion.op1", "action:notion.op2", "action:notion.op4", "action:notion.op6", "action:notion.op7"}
	if !reflect.DeepEqual(loaded, want) {
		t.Fatalf("loaded = %v\nwant %v", loaded, want)
	}

	// A budget for two more definitions keeps the first two siblings.
	cost := s.toolCost("action:notion.op0")
	bounded := NewCapabilitySet(catalog, nil, CapabilitySetConfig{SkillLoadBudget: 3*cost + cost/2})
	_, _, loaded = ExecuteLoadCapabilities(bounded, loadArgs("action:notion.op5"), capabilityToolName)
	if want := []string{"prompt:skill-notion@3", "action:notion.op5", "action:notion.op0", "action:notion.op1"}; !reflect.DeepEqual(loaded, want) {
		t.Fatalf("bounded loaded = %v\nwant %v", loaded, want)
	}
	// Instructions load one at a time.
	_, _, loaded = ExecuteLoadCapabilities(NewCapabilitySet(catalog, nil, CapabilitySetConfig{}), loadArgs("prompt:skill-research-1@1"), capabilityToolName)
	if !reflect.DeepEqual(loaded, []string{"prompt:skill-research-1@1"}) {
		t.Fatalf("prompt load = %v", loaded)
	}
	// Another Skill connection (binding) is not a sibling.
	other := capabilityCatalogFixture()
	other.Actions[1].BindingID = "binding-2"
	_, _, loaded = ExecuteLoadCapabilities(NewCapabilitySet(other, nil, CapabilitySetConfig{}), loadArgs("action:notion.op0"), capabilityToolName)
	for _, id := range loaded {
		if id == "action:notion.op1" {
			t.Fatal("an action of another binding was loaded")
		}
	}
}
