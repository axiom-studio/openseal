package modelstep_test

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime/modelstep/modelsteptest"

	"github.com/axiom-studio/openseal/pkg/builtin"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
	"github.com/axiom-studio/openseal/pkg/workspace"
)

func TestPinnedCapabilitySkillIDIsBuiltin(t *testing.T) {
	if modelstep.PinnedCapabilitySkillID != builtin.SkillID {
		t.Fatalf("pinned skill %q != builtin %q", modelstep.PinnedCapabilitySkillID, builtin.SkillID)
	}
}

func TestCapabilitySetSmallCatalogIsDeferred(t *testing.T) {
	catalog := modelstep.CapabilityCatalog{
		Actions:      []capability.ModelAction{{Name: "notion.search", SkillID: "notion", Version: "1"}},
		SkillPrompts: []models.HostedSkillPrompt{{SkillID: "notion", Version: "1", Reference: "skill-notion@1"}},
		HostTools:    modelstep.HostToolSpecs(workspace.ChatAuthority("chat-1")),
	}
	s := modelstep.NewCapabilitySet(catalog, nil, modelstep.CapabilitySetConfig{})
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
	catalog := modelsteptest.CapabilityCatalogFixture()
	s := modelstep.NewCapabilitySet(catalog, nil, modelstep.CapabilitySetConfig{})
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
	big := modelsteptest.CapabilityCatalogFixture()
	big.Actions[len(big.Actions)-2].Description = strings.Repeat("x", 4096)
	budget := 0
	probe := modelstep.NewCapabilitySet(big, nil, modelstep.CapabilitySetConfig{})
	for _, id := range probe.IDs() {
		if id != "action:openseal.builtin.web_search" {
			budget += probe.DefinitionCost(id)
		}
	}
	small := modelstep.NewCapabilitySet(big, nil, modelstep.CapabilitySetConfig{PinnedBytes: budget})
	if small.Loaded("action:openseal.builtin.web_search") || !small.Loaded("action:openseal.builtin.generate_image") {
		t.Fatalf("budget skip: %v", small.IDs())
	}
	if !small.Loaded("workspace:" + workspace.OperationRunCommand) {
		t.Fatal("run_command must stay pinned ahead of the builtin Skill")
	}
	reloaded := modelstep.NewCapabilitySet(big, []string{"action:openseal.builtin.web_search"}, modelstep.CapabilitySetConfig{PinnedBytes: budget})
	if !reloaded.Loaded("action:openseal.builtin.web_search") || reloaded.Pinned("action:openseal.builtin.web_search") {
		t.Fatal("skipped pinned capability must stay loadable")
	}
	for _, action := range func() []capability.ModelAction { a, _, _, _ := reloaded.Visible(); return a }() {
		if action.Name == "openseal.builtin.web_search" && len(action.Description) != 4096 {
			t.Fatal("definition truncated")
		}
	}
}

func longPromptCatalogFixture() modelstep.CapabilityCatalog {
	catalog := modelsteptest.CapabilityCatalogFixture()
	for i := 0; i < 4; i++ {
		catalog.SkillPrompts[i].Reference = fmt.Sprintf("skill:example.research.module.%d@1.2.3#binding:workforce:deployment-1234567890:example.research.module.%d@17", i, i)
	}
	return catalog
}

func TestCapabilitySetDiscoveryHandlesForLongPromptRefs(t *testing.T) {
	catalog := longPromptCatalogFixture()
	s := modelstep.NewCapabilitySet(catalog, nil, modelstep.CapabilitySetConfig{})
	id := "prompt:" + catalog.SkillPrompts[3].Reference
	handle := s.DiscoveryID(id)
	if handle == id || !strings.HasPrefix(handle, "prompt:") || len(handle) != len("prompt:")+16 {
		t.Fatalf("long reference not compacted: %q", handle)
	}
	if short := s.DiscoveryID("prompt:skill-notion@3"); short != "prompt:skill-notion@3" {
		t.Fatalf("short reference compacted: %q", short)
	}
	listing := modelsteptest.CapabilityNoteLines(s.CatalogLines())
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
	loaded := modelstep.NewCapabilitySet(catalog, []string{handle}, modelstep.CapabilitySetConfig{})
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
		got, ok := modelstep.NewCapabilitySet(next, nil, modelstep.CapabilitySetConfig{}).Resolve(handle)
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
	handle := modelstep.NewCapabilitySet(catalog, nil, modelstep.CapabilitySetConfig{}).DiscoveryID(id)
	catalog.SkillPrompts[1].Reference = strings.TrimPrefix(handle, "prompt:")
	next := modelstep.NewCapabilitySet(catalog, nil, modelstep.CapabilitySetConfig{})
	if next.DiscoveryID(id) != id {
		t.Fatal("ambiguous handle retained")
	}
	if got, ok := next.Resolve(handle); !ok || got != handle {
		t.Fatalf("canonical ID misresolved: %q %v", got, ok)
	}
}

func TestCapabilitySetLoadedOrderAppendsAndClosesOverBoundPrompts(t *testing.T) {
	catalog := modelsteptest.CapabilityCatalogFixture()
	base := modelstep.NewCapabilitySet(catalog, nil, modelstep.CapabilitySetConfig{})
	pinned := base.IDs()
	s := modelstep.NewCapabilitySet(catalog, []string{"action:notion.op5", "prompt:skill-research-2@1", "action:notion.op1", "action:notion.op5"}, modelstep.CapabilitySetConfig{})
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
	if got := s.Closure("action:notion.op2"); !reflect.DeepEqual(got, []string{"action:notion.op2"}) {
		t.Fatalf("closure of an action whose prompt is loaded: %v", got)
	}
	if got := base.Closure("action:notion.op2"); !reflect.DeepEqual(got, []string{"prompt:skill-notion@3", "action:notion.op2"}) {
		t.Fatalf("closure: %v", got)
	}
}

func TestCapabilitySetFiltersLoadsAbsentFromCatalog(t *testing.T) {
	catalog := modelsteptest.CapabilityCatalogFixture()
	pinned := modelstep.NewCapabilitySet(catalog, nil, modelstep.CapabilitySetConfig{}).IDs()
	// action:notion.op7 was revoked (binding revision); the other unknowns never existed.
	catalog.Actions = append(catalog.Actions[:7], catalog.Actions[8:]...)
	s := modelstep.NewCapabilitySet(catalog, []string{"action:notion.op7", "action:ghost", "prompt:skill-gone@1", "", "action:notion.op0"}, modelstep.CapabilitySetConfig{})
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

func TestCapabilityListingIndependentOfLoads(t *testing.T) {
	catalog := modelsteptest.CapabilityCatalogFixture()
	empty := modelstep.NewCapabilitySet(catalog, nil, modelstep.CapabilitySetConfig{})
	loaded := modelstep.NewCapabilitySet(catalog, []string{"action:notion.op3", "prompt:skill-research-0@1"}, modelstep.CapabilitySetConfig{})
	if empty.Searching() {
		t.Fatal("fixture must list the full catalog")
	}
	listing := modelsteptest.CapabilityNoteLines(empty.CatalogLines())
	if listing == "" || listing != modelsteptest.CapabilityNoteLines(loaded.CatalogLines()) {
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
		if !strings.Contains(modelstep.CapabilityLoadingText, want) {
			t.Fatalf("loading instructions missing %q", want)
		}
	}
}

func TestCapabilityDirectorySearchModeBounded(t *testing.T) {
	catalog := modelsteptest.CapabilityCatalogFixture()
	for server := 0; server < 30; server++ {
		bindingID := fmt.Sprintf("binding-%02d", server)
		catalog.SkillPrompts = append(catalog.SkillPrompts, models.HostedSkillPrompt{SkillID: "mcp", Version: "1", BindingID: bindingID,
			Reference: fmt.Sprintf("mcp-%02d@1", server), Name: fmt.Sprintf("Server %02d", server), Instructions: "Use the server."})
		for tool := 0; tool < 4; tool++ {
			catalog.Actions = append(catalog.Actions, capability.ModelAction{
				Name: fmt.Sprintf("mcp.server%02d.tool%d", server, tool), SkillID: "mcp", Version: "1", BindingID: bindingID,
				Description: "A connected server tool", BindingTool: &capability.ModelActionBindingTool{Name: fmt.Sprintf("tool%d", tool)},
			})
		}
	}
	s := modelstep.NewCapabilitySet(catalog, nil, modelstep.CapabilitySetConfig{})
	if !s.Searching() {
		t.Fatal("more than 64 entries must switch to search mode")
	}
	directory := modelsteptest.CapabilityNoteLines(s.DirectoryLines())
	for _, want := range []string{"\n- mcp.server07 — Server 07 (4 tools, instructions)", "\n- notion — Notion (8 tools, instructions)",
		"\n- workspace (" + fmt.Sprint(len(catalog.HostTools)) + " tools)"} {
		if !strings.Contains(directory, want) {
			t.Fatalf("directory missing %q", want)
		}
	}
	if strings.Contains(directory, "action:mcp.server07.tool1") {
		t.Fatal("search mode must not list every capability")
	}
	if directory != modelsteptest.CapabilityNoteLines(modelstep.NewCapabilitySet(catalog, []string{"action:mcp.server03.tool2"}, modelstep.CapabilitySetConfig{}).DirectoryLines()) {
		t.Fatal("directory depends on loads")
	}

	bounded := modelsteptest.CapabilityNoteLines(modelstep.NewCapabilitySet(catalog, nil, modelstep.CapabilitySetConfig{SummaryBytes: 300}).DirectoryLines())
	if !strings.Contains(bounded, " more; search to find them") {
		t.Fatalf("directory not bounded: %s", bounded)
	}
	if lines := strings.Count(bounded, "\n- "); lines >= s.CapabilityGroupCount() {
		t.Fatalf("directory lists %d of %d groups", lines, s.CapabilityGroupCount())
	}

	byBytes := modelsteptest.CapabilityCatalogFixture()
	if !modelstep.NewCapabilitySet(byBytes, nil, modelstep.CapabilitySetConfig{CatalogBytes: 200}).Searching() {
		t.Fatal("a listing over the byte budget must switch to search mode")
	}
}

// capabilityLoadEntry is a load_capabilities tool entry: action and
// workspace IDs become frozen tools, other IDs loaded instructions.
func capabilityLoadEntry(id, runID string, state models.ToolEntryState, loaded ...string) *models.SessionEntry {
	tool := &models.ToolEntry{Name: modelstep.LoadCapabilitiesTool}
	for _, capabilityID := range loaded {
		switch {
		case strings.HasPrefix(capabilityID, models.CapabilityActionPrefix):
			name := "action_" + strings.ReplaceAll(strings.TrimPrefix(capabilityID, models.CapabilityActionPrefix), ".", "_")
			tool.SurfacedTools = append(tool.SurfacedTools, models.SurfacedTool{Capability: capabilityID, Name: name,
				Description: "Frozen " + name, InputSchema: map[string]interface{}{"type": "object"}})
		case strings.HasPrefix(capabilityID, modelstep.CapabilityWorkspacePrefix):
			name := strings.ReplaceAll(strings.TrimPrefix(capabilityID, modelstep.CapabilityWorkspacePrefix), ".", "_")
			tool.SurfacedTools = append(tool.SurfacedTools, models.SurfacedTool{Capability: capabilityID, Name: name,
				InputSchema: map[string]interface{}{"type": "object"}})
		default:
			tool.LoadedInstructions = append(tool.LoadedInstructions, capabilityID)
		}
	}
	return &models.SessionEntry{ID: id, RunID: runID, Type: models.SessionEntryTool, State: state, Tool: tool}
}

func TestPathLoadedCapabilityIDsIgnoresErroredAndPreCompactionLoads(t *testing.T) {
	entries := []*models.SessionEntry{
		capabilityLoadEntry("e1", "run-1", models.ToolEntryCompleted, "prompt:skill-notion@3", "action:notion.op0"),
		capabilityLoadEntry("e2", "run-1", models.ToolEntryCompleted, "action:notion.op1"),
		{ID: "e3", RunID: "run-1", Type: models.SessionEntryUser, User: &models.UserEntry{}},
		capabilityLoadEntry("e4", "run-1", models.ToolEntryError, "action:notion.op2"),
		capabilityLoadEntry("e5", "run-1", models.ToolEntryPending, "action:notion.op3"),
		{ID: "e6", Type: models.SessionEntryTool, State: models.ToolEntryCompleted, Tool: &models.ToolEntry{Name: "search_capabilities",
			SurfacedTools: []models.SurfacedTool{{Capability: "action:notion.op4", Name: "action_notion_op4"}}}},
		capabilityLoadEntry("e7", "run-1", models.ToolEntryCompleted, "action:notion.op5", "action:notion.op1"),
	}
	if got, want := modelstep.PathLoadedCapabilityIDs(&models.AgentSessionPath{Entries: entries}), []string{"prompt:skill-notion@3", "action:notion.op0", "action:notion.op1", "action:notion.op5"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}

	// The store path builder drops pre-compaction entries; the compaction
	// carries the tools of the summarized loads (not their instructions).
	session := &models.AgentSession{ID: "s1", NextSequence: 6}
	segment := []*models.SessionEntry{
		withSessionSequence(capabilityLoadEntry("p1", "run-1", models.ToolEntryCompleted, "prompt:skill-notion@3", "action:notion.op0"), "s1", 1),
		withSessionSequence(capabilityLoadEntry("p2", "run-1", models.ToolEntryCompleted, "action:notion.op1"), "s1", 2),
		withSessionSequence(&models.SessionEntry{ID: "p3", Type: models.SessionEntryCompaction, Compaction: &models.CompactionEntry{Summary: "s", FirstKeptEntryID: "p2",
			CarriedTools: modelstep.CarriedToolsAfter(nil, segmentPrefix(1))}}, "s1", 3),
		withSessionSequence(capabilityLoadEntry("p4", "run-1", models.ToolEntryCompleted, "action:notion.op2"), "s1", 4),
	}
	rendered, err := models.BuildAgentSessionPath([]*models.AgentSession{session}, [][]*models.SessionEntry{segment}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := modelstep.PathCarriedCapabilityIDs(rendered), []string{"action:notion.op0"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("rendered path carried %v want %v", got, want)
	}
	if got, want := modelstep.PathLoadedCapabilityIDs(rendered), []string{"action:notion.op1", "action:notion.op2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("rendered path got %v want %v", got, want)
	}
}

// segmentPrefix is the summarized part of the compaction fixture above.
func segmentPrefix(n int) []*models.SessionEntry {
	entries := []*models.SessionEntry{capabilityLoadEntry("p1", "run-1", models.ToolEntryCompleted, "prompt:skill-notion@3", "action:notion.op0")}
	return entries[:n]
}

func withSessionSequence(entry *models.SessionEntry, sessionID string, sequence int64) *models.SessionEntry {
	entry.SessionID = sessionID
	entry.Sequence = sequence
	return entry
}

func TestPathLoadedCapabilityIDsCarryAcrossRunsInSession(t *testing.T) {
	catalog := modelsteptest.CapabilityCatalogFixture()
	session := &models.AgentSession{ID: "conv", NextSequence: 5}
	segment := []*models.SessionEntry{
		withSessionSequence(capabilityLoadEntry("r1-load", "run-1", models.ToolEntryCompleted, "prompt:skill-notion@3", "action:notion.op4"), "conv", 1),
		withSessionSequence(&models.SessionEntry{ID: "r2-user", RunID: "run-2", Type: models.SessionEntryUser, User: &models.UserEntry{}}, "conv", 2),
		withSessionSequence(capabilityLoadEntry("r2-load", "run-2", models.ToolEntryCompleted, "action:notion.op6"), "conv", 3),
		withSessionSequence(capabilityLoadEntry("r2-again", "run-2", models.ToolEntryCompleted), "conv", 4),
	}
	path, err := models.BuildAgentSessionPath([]*models.AgentSession{session}, [][]*models.SessionEntry{segment}, nil)
	if err != nil {
		t.Fatal(err)
	}
	loaded := modelstep.PathLoadedCapabilityIDs(path)
	if want := []string{"prompt:skill-notion@3", "action:notion.op4", "action:notion.op6"}; !reflect.DeepEqual(loaded, want) {
		t.Fatalf("got %v want %v", loaded, want)
	}
	s := modelstep.NewCapabilitySet(catalog, loaded, modelstep.CapabilitySetConfig{})
	if !s.Loaded("action:notion.op4") || !s.Loaded("action:notion.op6") {
		t.Fatal("a later Run in the session must see earlier loads")
	}
}

func TestPathLoadedCapabilityIDsChildSessionInheritsParentUpToFork(t *testing.T) {
	parent := &models.AgentSession{ID: "parent", NextSequence: 4}
	child := &models.AgentSession{ID: "child", ParentSessionID: "parent", ForkEntryID: "p-load-1", ForkSequence: 1, NextSequence: 2}
	parentEntries := []*models.SessionEntry{
		withSessionSequence(capabilityLoadEntry("p-load-1", "run-parent", models.ToolEntryCompleted, "action:notion.op0"), "parent", 1),
		withSessionSequence(capabilityLoadEntry("p-load-2", "run-parent", models.ToolEntryCompleted, "action:notion.op1"), "parent", 2),
		withSessionSequence(capabilityLoadEntry("p-load-3", "run-parent", models.ToolEntryCompleted, "action:notion.op2"), "parent", 3),
	}
	childEntries := []*models.SessionEntry{
		withSessionSequence(capabilityLoadEntry("c-load-1", "run-child", models.ToolEntryCompleted, "action:notion.op3"), "child", 1),
	}
	path, err := models.BuildAgentSessionPath([]*models.AgentSession{parent, child}, [][]*models.SessionEntry{parentEntries, childEntries}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := modelstep.PathLoadedCapabilityIDs(path), []string{"action:notion.op0", "action:notion.op3"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestLoadSurfacesWholeSkillWithinBudget(t *testing.T) {
	catalog := modelsteptest.CapabilityCatalogFixture()
	s := modelstep.NewCapabilitySet(catalog, []string{"action:notion.op3"}, modelstep.CapabilitySetConfig{})
	_, isError, loaded := modelstep.ExecuteLoadCapabilities(s, loadArgs("action:notion.op5"), capabilityToolName)
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
	cost := s.ToolCost("action:notion.op0")
	bounded := modelstep.NewCapabilitySet(catalog, nil, modelstep.CapabilitySetConfig{SkillLoadBudget: 3*cost + cost/2})
	_, _, loaded = modelstep.ExecuteLoadCapabilities(bounded, loadArgs("action:notion.op5"), capabilityToolName)
	if want := []string{"prompt:skill-notion@3", "action:notion.op5", "action:notion.op0", "action:notion.op1"}; !reflect.DeepEqual(loaded, want) {
		t.Fatalf("bounded loaded = %v\nwant %v", loaded, want)
	}
	// Instructions load one at a time.
	_, _, loaded = modelstep.ExecuteLoadCapabilities(modelstep.NewCapabilitySet(catalog, nil, modelstep.CapabilitySetConfig{}), loadArgs("prompt:skill-research-1@1"), capabilityToolName)
	if !reflect.DeepEqual(loaded, []string{"prompt:skill-research-1@1"}) {
		t.Fatalf("prompt load = %v", loaded)
	}
	// Another Skill connection (binding) is not a sibling.
	other := modelsteptest.CapabilityCatalogFixture()
	other.Actions[1].BindingID = "binding-2"
	_, _, loaded = modelstep.ExecuteLoadCapabilities(modelstep.NewCapabilitySet(other, nil, modelstep.CapabilitySetConfig{}), loadArgs("action:notion.op0"), capabilityToolName)
	for _, id := range loaded {
		if id == "action:notion.op1" {
			t.Fatal("an action of another binding was loaded")
		}
	}
}
