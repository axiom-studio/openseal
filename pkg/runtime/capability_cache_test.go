package runtime

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
)

// Cache-stable capability loading: a load, a new Run and a Skill connected
// between Runs only append to what the model already saw.

// fixtureNoteAppender appends capabilities notes to a render fixture.
type fixtureNoteAppender struct{ f *renderFixture }

func (a fixtureNoteAppender) AppendAgentSessionEntries(_ context.Context, req AppendAgentSessionEntriesRequest) (*AppendAgentSessionEntriesResult, error) {
	var out []*SessionEntry
	for _, entry := range req.Entries {
		out = append(out, a.f.add(entry))
	}
	return &AppendAgentSessionEntriesResult{Entries: out}, nil
}

// ensureNote runs the producer's capabilities-note check for one step.
func (f *prepareFixture) ensureNote(step string) bool {
	f.t.Helper()
	path := f.render.path()
	in := StepInput{Run: f.run, Step: &AgentTurn{ID: step, RunID: f.run.ID}, Session: path.Session, Path: path, Binding: f.binding, Mode: StepModeNormal}
	appended, err := ensureCapabilityNote(context.Background(), fixtureNoteAppender{f.render}, in, path, f.preparer.CapabilityListing(in), f.render.entries[0].CreatedAt)
	if err != nil {
		f.t.Fatal(err)
	}
	return appended
}

func (f *prepareFixture) capabilityNotes() []*SessionEntry {
	var notes []*SessionEntry
	for _, entry := range f.render.entries {
		if entry.Note != nil && entry.Note.Kind == NoteCapabilities {
			notes = append(notes, entry)
		}
	}
	return notes
}

func mustJSON(t testing.TB, value interface{}) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestPrepareStepPrefixStableAcrossLoad(t *testing.T) {
	f := newPrepareFixture(t, fullPrepareAuthority())
	f.ensureNote("step-1")
	before := f.prepare("step-1")
	load := f.addLoad("step-1", "c1", "action:gmail.read")
	after := f.prepare("step-2")

	if mustJSON(t, after.System) != mustJSON(t, before.System) {
		t.Fatal("system changed after a load")
	}
	if mustJSON(t, after.Tools[:len(before.Tools)]) != mustJSON(t, before.Tools) {
		t.Fatalf("harness and core changed after a load:\n%v\n%v", toolNames(before.Tools), toolNames(after.Tools))
	}
	added := after.Tools[len(before.Tools):]
	if len(added) != 1 || added[0].Name != "action_gmail_read" || added[0].Group != ModelStepToolLoaded || added[0].Surface != load.ID {
		t.Fatalf("surfaced = %+v", added)
	}
	if mustJSON(t, after.Entries[:len(before.Entries)]) != mustJSON(t, before.Entries) {
		t.Fatal("earlier entries changed after a load")
	}
	var surfaces []string
	for _, entry := range after.Entries {
		if entry.ID == load.ID {
			surfaces = entry.Surfaces
		} else if len(entry.Surfaces) > 0 {
			t.Fatalf("entry %s surfaces %v", entry.ID, entry.Surfaces)
		}
	}
	if !reflect.DeepEqual(surfaces, []string{"action_gmail_read"}) {
		t.Fatalf("load entry surfaces = %v", surfaces)
	}
	// A load adds no note: the catalog did not change.
	if f.ensureNote("step-2") || len(f.capabilityNotes()) != 1 {
		t.Fatalf("a load appended a capabilities note: %d", len(f.capabilityNotes()))
	}
}

func TestPrepareStepStableAcrossRunsWhenSkillConnected(t *testing.T) {
	t.Run("catalog", func(t *testing.T) { testSkillConnectStable(t, false) })
	// A small catalog (three actions, two prompts) is deferred as well, so
	// connecting a Skill never moves its tools into the core group.
	t.Run("small catalog", func(t *testing.T) { testSkillConnectStable(t, true) })
}

func testSkillConnectStable(t *testing.T, small bool) {
	f := newPrepareFixture(t, fullPrepareAuthority())
	if small {
		f.binding.ModelActions = f.binding.ModelActions[:3]
	}
	if !f.ensureNote("step-1") {
		t.Fatal("the first step has no capabilities note")
	}
	f.render.assistant("run-1", "step-1")
	first := f.prepare("step-2")

	// Between Runs the user connects Slack: two actions and a prompt.
	f.run = &AgentRun{ID: "run-2", Scope: testSessionScope, AssignedAgentID: "agent-1"}
	f.render.user("run-2", "post the launch notes to slack")
	f.preparer.config.SkillPrompts = append(f.preparer.config.SkillPrompts,
		HostedSkillPrompt{SkillID: "skill-slack", Version: "1", Reference: "skill-slack@1", Name: "Slack", Instructions: "Slack rules."})
	for _, name := range []string{"slack.post", "slack.read"} {
		f.binding.ModelActions = append(f.binding.ModelActions, capability.ModelAction{
			Name: name, Description: "Slack " + name, SkillID: "skill-slack", Version: "1", Action: name,
			Risk: capability.RiskLevelWrite, SideEffect: capability.SideEffectWrite, InputSchema: map[string]interface{}{"type": "object"},
		})
	}
	if !f.ensureNote("run-2-step-1") {
		t.Fatal("connecting a Skill appended no note")
	}
	second := f.prepare("run-2-step-1")

	if mustJSON(t, second.System) != mustJSON(t, first.System) || mustJSON(t, second.Tools) != mustJSON(t, first.Tools) {
		t.Fatal("connecting a Skill changed the system or the tools")
	}
	if mustJSON(t, second.Entries[:len(first.Entries)]) != mustJSON(t, first.Entries) {
		t.Fatal("connecting a Skill changed earlier entries")
	}
	notes := f.capabilityNotes()
	if len(notes) != 2 {
		t.Fatalf("capabilities notes = %d, want 2", len(notes))
	}
	delta := notes[1].Note.Text
	if !strings.HasPrefix(delta, "<capabilities_update>") || !strings.Contains(delta, "- action:slack.post — Slack slack.post") ||
		!strings.Contains(delta, "- prompt:skill-slack@1 — Slack: ") || strings.Contains(delta, "action:notion.search") {
		t.Fatalf("delta note = %q", delta)
	}
	if last := second.Entries[len(second.Entries)-1]; last.Note == nil || last.Note.Kind != string(NoteCapabilities) {
		t.Fatalf("the delta is not the last entry: %+v", last)
	}
	// Nothing changed since: no third note.
	if f.ensureNote("run-2-step-2") {
		t.Fatal("an unchanged catalog appended a note")
	}
}

func TestCapabilityNoteOnceAndAfterCompaction(t *testing.T) {
	f := newPrepareFixture(t, fullPrepareAuthority())
	if !f.ensureNote("step-1") || f.ensureNote("step-2") {
		t.Fatal("the note must be appended exactly once")
	}
	full := f.capabilityNotes()[0].Note.Text
	for _, want := range []string{"<capabilities>", "Capabilities you can load", "- action:notion.search — Read notion.search",
		"- prompt:skill-notion@1 — Notion", "Tools that change something carry a summary", "A Skill's tools are only the operations authorized"} {
		if !strings.Contains(full, want) {
			t.Fatalf("full note lacks %q:\n%s", want, full)
		}
	}
	if strings.Contains(full, "capabilities_update") {
		t.Fatal("the first note is a delta")
	}

	// A compaction that summarizes the note: the next step lists everything again.
	f.render.assistant("run-1", "step-2")
	kept := f.render.user("run-1", "and the budget?")
	f.render.compaction("run-1", kept.ID)
	if !f.ensureNote("step-3") {
		t.Fatal("no note after the compaction summarized it")
	}
	notes := f.capabilityNotes()
	if len(notes) != 2 || notes[1].Note.Text != full {
		t.Fatalf("after compaction: %d notes, full again = %v", len(notes), len(notes) == 2 && notes[1].Note.Text == full)
	}
	if f.ensureNote("step-4") {
		t.Fatal("a note was appended twice after the compaction")
	}
	req := f.prepare("step-4")
	if req.Entries[len(req.Entries)-1].Note == nil {
		t.Fatal("the new note is not rendered")
	}
}

func TestSurfacedToolDefinitionFrozen(t *testing.T) {
	f := newPrepareFixture(t, fullPrepareAuthority())
	f.ensureNote("step-1")
	f.addLoad("step-1", "c1", "action:notion.fetch", "action:gmail.send")
	before := f.prepare("step-2")

	// The Skill is revised: notion.fetch gets a new description and schema.
	for i := range f.binding.ModelActions {
		if f.binding.ModelActions[i].Name == "notion.fetch" {
			f.binding.ModelActions[i].Description = "Fetch a page, revised"
			f.binding.ModelActions[i].InputSchema = map[string]interface{}{"type": "object", "properties": map[string]interface{}{"pageId": map[string]interface{}{"type": "string"}}}
		}
	}
	revised := f.prepare("step-3")
	if mustJSON(t, revised.Tools) != mustJSON(t, before.Tools) {
		t.Fatal("a revised Skill rewrote a surfaced tool")
	}

	// gmail.send is revoked: its tool stays declared and the note says so.
	var kept []capability.ModelAction
	for _, action := range f.binding.ModelActions {
		if action.Name != "gmail.send" {
			kept = append(kept, action)
		}
	}
	f.binding.ModelActions = kept
	revoked := f.prepare("step-4")
	if mustJSON(t, revoked.Tools) != mustJSON(t, before.Tools) {
		t.Fatal("a revoked surfaced tool was withdrawn")
	}
	if !f.ensureNote("step-4") {
		t.Fatal("revoking a surfaced tool appended no note")
	}
	notes := f.capabilityNotes()
	delta := notes[len(notes)-1].Note.Text
	if !strings.Contains(delta, "Tools no longer available (a call returns an error):\n- action_gmail_send") ||
		!strings.Contains(delta, "No longer available:\n- action:gmail.send") {
		t.Fatalf("revocation delta = %q", delta)
	}

	// The executor answers a call of the revoked tool with is_error.
	x := newToolExecFixture(t, ControlAuthority{})
	step := x.step("step-1", AgentTurnStatusRunning, toolCall("call-revoked", "action_gmail_send", `{}`))
	if _, err := x.binding.Tools.ExecuteStepTools(x.ctx, x.input(step)); err != nil {
		t.Fatal(err)
	}
	stored := x.entry(step.entries[0])
	if stored.State != ToolEntryError || !stored.Tool.Result.IsError || resultValue(t, stored)["error"] != loopToolNotAvailableCode {
		t.Fatalf("revoked call = %s %v", stored.State, stored.Tool.Result)
	}
}

func TestFinalAnswerSchemaStableAcrossSkillRefs(t *testing.T) {
	one := ControlAuthority{AgentID: "agent-1", SkillPromptRefs: []string{"skill-notion@1"}}
	two := ControlAuthority{AgentID: "agent-1", SkillPromptRefs: []string{"skill-notion@1", "skill-slack@1"}}
	if mustJSON(t, finalAnswerSchema(one)) != mustJSON(t, finalAnswerSchema(two)) || finalAnswerDescription(one) != finalAnswerDescription(two) {
		t.Fatal("the final answer tool changes with the Skill refs")
	}
	if selections, rejection := finalSkillSelections([]string{"skill-gone@1"}, two.SkillPromptRefs); rejection != nil || len(selections) != 2 ||
		selections[0].Disposition != HostedSkillNotApplied || selections[1].Disposition != HostedSkillNotApplied {
		t.Fatalf("an unknown ref must be left out: %#v %v", selections, rejection)
	}
	// Delegate, fork and operation tools carry no target list either.
	targets := ControlAuthority{AgentID: "agent-1", ForkAllowed: true,
		DelegateTargets: []HostedAgentTarget{{ID: "agent-2", DisplayName: "Ada"}}, ForkTargets: []HostedAgentTarget{{ID: "agent-2"}},
		Operations: []HostedRunbookOperation{{Entrypoint: "report", Name: "Report"}}}
	more := targets
	more.DelegateTargets = append(more.DelegateTargets, HostedAgentTarget{ID: "agent-3", DisplayName: "Bo"})
	more.ForkTargets = append(more.ForkTargets, HostedAgentTarget{ID: "agent-3"})
	more.Operations = append(more.Operations, HostedRunbookOperation{Entrypoint: "audit", Name: "Audit"})
	for _, name := range []ControlToolName{ControlToolDelegate, ControlToolFork, ControlToolRunOperation} {
		handler := newControlRegistry(builtinControlTools()...)[name]
		if mustJSON(t, handler.Spec(targets)) != mustJSON(t, handler.Spec(more)) {
			t.Fatalf("%s changes with its targets", name)
		}
	}
	items := capabilityNoteLines(controlTargetItems(more))
	for _, want := range []string{"- agent-2: Ada", "- agent-3: Bo", "- audit — Audit. Input schema:"} {
		if !strings.Contains(items, want) {
			t.Fatalf("targets missing %q from the note: %s", want, items)
		}
	}
}

func TestLoadDescriptionModeIndependent(t *testing.T) {
	listed := NewCapabilitySet(capabilityCatalogFixture(), nil, CapabilitySetConfig{})
	searching := NewCapabilitySet(capabilityCatalogFixture(), nil, CapabilitySetConfig{CatalogBytes: 200})
	if listed.Searching() || !searching.Searching() {
		t.Fatal("fixtures must cover both modes")
	}
	f := newPrepareFixture(t, fullPrepareAuthority())
	listedReq := f.prepare("step-1")
	f.preparer.config.Capabilities.CatalogBytes = 100
	searchReq := f.prepare("step-1")
	if !f.preparer.CapabilitySet(StepInput{Run: f.run, Binding: f.binding}).Searching() {
		t.Fatal("the fixture did not switch to search mode")
	}
	if mustJSON(t, listedReq.Tools) != mustJSON(t, searchReq.Tools) || mustJSON(t, listedReq.System) != mustJSON(t, searchReq.System) {
		t.Fatal("search mode changed the tools or the system")
	}
	names := toolNames(listedReq.Tools)
	if !strings.Contains(strings.Join(names, ","), "search_capabilities,load_capabilities") {
		t.Fatalf("search_capabilities is not declared with load: %v", names)
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

func TestLoadFreezesSurfacedToolsOnTheEntry(t *testing.T) {
	x := newToolExecFixture(t, ControlAuthority{})
	x.binding.ModelActions = capabilityCatalogFixture().Actions
	step := x.step("step-1", AgentTurnStatusRunning, toolCall("call-load", LoadCapabilitiesTool, `{"ids":["action:notion.op2"]}`))
	if _, err := x.binding.Tools.ExecuteStepTools(x.ctx, x.input(step)); err != nil {
		t.Fatal(err)
	}
	stored := x.entry(step.entries[0])
	if stored.State != ToolEntryCompleted || len(stored.Tool.SurfacedTools) != 8 {
		t.Fatalf("load entry = %s %+v", stored.State, stored.Tool.SurfacedTools)
	}
	first := stored.Tool.SurfacedTools[0]
	names := LoopGovernedToolNames(x.binding.ModelActions)
	if first.Capability != "action:notion.op2" || first.Name != names["notion.op2"] || first.Description != "Notion operation 2" ||
		first.InputSchema["type"] != "object" {
		t.Fatalf("frozen tool = %+v", first)
	}
}

func TestCompactionCarriesSurfacedTools(t *testing.T) {
	f := newPrepareFixture(t, fullPrepareAuthority())
	early := f.addLoad("step-1", "c1", "action:notion.fetch")
	f.render.user("run-1", "and gmail")
	late := f.addLoad("step-2", "c2", "action:gmail.read")
	before := f.prepare("step-3")

	carried := carriedToolsAfter(&CompactionEntry{CarriedTools: []SurfacedTool{{Capability: "action:drive.list", Name: "action_drive_list",
		InputSchema: map[string]interface{}{"type": "object"}}}}, []*SessionEntry{early})
	if got := []string{carried[0].Name, carried[1].Name}; len(carried) != 2 || !reflect.DeepEqual(got, []string{"action_drive_list", "action_notion_fetch"}) {
		t.Fatalf("carried = %+v", carried)
	}

	// Summarize up to the second load: the first load's tool is carried.
	kept := late.ID
	f.render.add(&SessionEntry{RunID: "run-1", Type: SessionEntryCompaction, Compaction: &CompactionEntry{
		Summary: "Earlier work.", FirstKeptEntryID: f.render.entries[len(f.render.entries)-3].ID, TokensBefore: 9000,
		CarriedTools: carriedToolsAfter(nil, f.render.entries[:len(f.render.entries)-3]),
		Provider:     "anthropic", Model: "claude-test", Trigger: CompactionTriggerThreshold,
	}})
	after := f.prepare("step-3")
	loadedTools := func(req ModelStepRequest) []ModelStepTool {
		var tools []ModelStepTool
		for _, tool := range req.Tools {
			if tool.Group == ModelStepToolLoaded {
				tools = append(tools, tool)
			}
		}
		return tools
	}
	got := loadedTools(after)
	if len(got) != 2 || got[0].Name != "action_notion_fetch" || got[0].Surface != "" || got[1].Name != "action_gmail_read" || got[1].Surface != kept {
		t.Fatalf("loaded after compaction = %+v", got)
	}
	if mustJSON(t, got[0].InputSchema) != mustJSON(t, loadedTools(before)[0].InputSchema) {
		t.Fatal("the carried definition changed")
	}
	set := f.preparer.CapabilitySet(StepInput{Run: f.run, Binding: f.binding, Path: f.render.path()})
	if !set.Loaded("action:notion.fetch") {
		t.Fatal("a carried tool is not callable")
	}
	// The carried tool's bound instructions were in the summarized load
	// result: they are not loaded, and loading them returns their text.
	if set.Loaded("prompt:skill-notion@1") {
		t.Fatal("a carried tool's instructions count as loaded after the compaction")
	}
	content, isError, _ := ExecuteLoadCapabilities(set, map[string]interface{}{"ids": []interface{}{"prompt:skill-notion@1"}}, loopCapabilityToolName(f.binding.ModelActions))
	if isError || !strings.Contains(string(content), "Notion rules.") {
		t.Fatalf("reloading the carried tool's instructions = %v %s", isError, content)
	}
}

func TestCompactionSummaryRequestKeepsStepPrefix(t *testing.T) {
	f := newPrepareFixture(t, fullPrepareAuthority())
	early := f.addLoad("step-1", "c1", "action:notion.fetch")
	f.render.user("run-1", "and gmail")
	late := f.addLoad("step-2", "c2", "action:gmail.read")
	step := f.prepare("step-3")

	// The whole-path summary is the step's request plus the instruction.
	whole := compactionSummaryRequest(step)
	if !reflect.DeepEqual(whole.Tools, step.Tools) || !reflect.DeepEqual(whole.Entries, step.Entries) || !reflect.DeepEqual(whole.System, step.System) {
		t.Fatal("the whole-path summary request changed the step's tools, entries or system")
	}
	if err := whole.Validate(); err != nil {
		t.Fatal(err)
	}

	at := func(id string) int {
		for i, entry := range step.Entries {
			if entry.ID == id {
				return i
			}
		}
		t.Fatalf("entry %s not rendered", id)
		return -1
	}
	surfaceOf := func(req ModelStepRequest, name string) (string, bool) {
		for _, tool := range req.Tools {
			if tool.Name == name {
				return tool.Surface, true
			}
		}
		return "", false
	}
	// A chunk ending at the first load: its tool keeps the surface, the
	// later load's tool is not declared.
	first := compactionChunkSummaryRequest(step, step.Entries[:at(early.ID)+1])
	if surface, ok := surfaceOf(first, "action_notion_fetch"); !ok || surface != early.ID {
		t.Fatalf("first chunk notion surface = %q %v", surface, ok)
	}
	if _, ok := surfaceOf(first, "action_gmail_read"); ok {
		t.Fatal("a tool surfaced after the chunk is declared")
	}
	if err := first.Validate(); err != nil {
		t.Fatal(err)
	}
	// A chunk after the first load: its tool is callable from the start.
	second := compactionChunkSummaryRequest(step, step.Entries[at(early.ID)+1:])
	if surface, ok := surfaceOf(second, "action_notion_fetch"); !ok || surface != "" {
		t.Fatalf("second chunk notion surface = %q %v", surface, ok)
	}
	if surface, ok := surfaceOf(second, "action_gmail_read"); !ok || surface != late.ID {
		t.Fatalf("second chunk gmail surface = %q %v", surface, ok)
	}
}

func TestValidateSurfaces(t *testing.T) {
	base := func() ModelStepRequest {
		req := validModelStepRequest()
		req.Tools = req.Tools[:2]
		assistant := modelStepTestAssistant("c1")
		assistant.ToolCalls[0].Name = LoadCapabilitiesTool
		req.Entries = []SessionEntryView{
			{ID: "u1", Sequence: 1, Type: SessionEntryUser, User: &UserEntryView{Content: []ContentPart{{Type: ContentPartText, Text: "hi"}}}},
			{ID: "a1", Sequence: 2, Type: SessionEntryAssistant, Assistant: assistant},
			{ID: "t1", Sequence: 3, Type: SessionEntryTool, Surfaces: []string{"x_one", "x_two"}, Tool: &ToolEntryView{
				CallID: "c1", Name: LoadCapabilitiesTool, Arguments: json.RawMessage(`{}`), State: ToolEntryCompleted,
				Result: ToolResultView{Content: []ContentPart{{Type: ContentPartText, Text: "loaded"}}}}},
		}
		object := map[string]interface{}{"type": "object"}
		req.Tools = append(req.Tools,
			ModelStepTool{Name: "x_carried", InputSchema: object, Group: ModelStepToolLoaded},
			ModelStepTool{Name: "x_one", InputSchema: object, Group: ModelStepToolLoaded, Surface: "t1"},
			ModelStepTool{Name: "x_two", InputSchema: object, Group: ModelStepToolLoaded, Surface: "t1"})
		return req
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("valid request: %v", err)
	}
	cases := map[string]func(*ModelStepRequest){
		"orphan surface":         func(r *ModelStepRequest) { r.Tools[3].Surface = "missing" },
		"unlisted surfaced tool": func(r *ModelStepRequest) { r.Entries[2].Surfaces = []string{"x_one"} },
		"listed but not a tool":  func(r *ModelStepRequest) { r.Entries[2].Surfaces = append(r.Entries[2].Surfaces, "x_ghost") },
		"listed twice":           func(r *ModelStepRequest) { r.Entries[2].Surfaces = []string{"x_one", "x_two", "x_one"} },
		"out of order":           func(r *ModelStepRequest) { r.Entries[2].Surfaces = []string{"x_two", "x_one"} },
		"core with surface":      func(r *ModelStepRequest) { r.Tools[1].Surface = "t1" },
		"carried after surfaced": func(r *ModelStepRequest) { r.Tools[2], r.Tools[3], r.Tools[4] = r.Tools[3], r.Tools[4], r.Tools[2] },
		"surfaces on a user":     func(r *ModelStepRequest) { r.Entries[0].Surfaces = []string{"x_one"} },
	}
	for name, mutate := range cases {
		req := base()
		mutate(&req)
		if err := req.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestCapabilityNoteRelistsWhenOnlyAnUpdateSurvives(t *testing.T) {
	listing := capabilityListing{Mode: capabilityNoteModeList, Items: []capabilityNoteItem{
		{Key: "action:a", Section: capabilityNoteCatalog, Text: "- action:a"},
	}}
	fullText, fullData := capabilityNoteText(listing, nil)
	if !fullData.Full || !strings.HasPrefix(fullText, capabilityNoteOpen) {
		t.Fatalf("first note = %q full=%v", fullText, fullData.Full)
	}
	listing.Items = append(listing.Items, capabilityNoteItem{Key: "action:b", Section: capabilityNoteCatalog, Text: "- action:b"})
	updateText, updateData := capabilityNoteText(listing, &fullData)
	if updateData.Full || !strings.HasPrefix(updateText, "<capabilities_update>") {
		t.Fatalf("second note = %q full=%v", updateText, updateData.Full)
	}
	note := func(id, text string, data capabilityNoteData) *SessionEntry {
		encoded, _ := json.Marshal(data)
		return &SessionEntry{ID: id, Type: SessionEntryNote, Note: &NoteEntry{Kind: NoteCapabilities, Text: text, Data: encoded}}
	}
	both := &AgentSessionPath{Entries: []*SessionEntry{note("n1", fullText, fullData), note("n2", updateText, updateData)}}
	if latest := latestCapabilityNote(both); latest == nil || latest.Digest != updateData.Digest {
		t.Fatalf("latest with both notes = %+v", latest)
	}
	// A compaction summarized the full note but kept the update: the
	// listing is gone, so the next step lists everything again.
	onlyUpdate := &AgentSessionPath{Entries: []*SessionEntry{note("n2", updateText, updateData)}}
	if latest := latestCapabilityNote(onlyUpdate); latest != nil {
		t.Fatalf("an update without its full note counts as the listing: %+v", latest)
	}
	text, data := capabilityNoteText(listing, latestCapabilityNote(onlyUpdate))
	if !data.Full || !strings.Contains(text, "- action:a") || !strings.Contains(text, "- action:b") {
		t.Fatalf("relisting = %q", text)
	}
}

func TestCapabilityNoteModeChangeToNoCatalog(t *testing.T) {
	deferred := capabilityListing{Mode: capabilityNoteModeList, Items: []capabilityNoteItem{
		{Key: "action:a", Section: capabilityNoteCatalog, Text: "- action:a"},
		{Key: "action:b", Section: capabilityNoteCatalog, Text: "- action:b"},
	}}
	_, previous := capabilityNoteText(deferred, nil)
	text, data := capabilityNoteText(capabilityListing{Mode: capabilityNoteModeNone}, &previous)
	if !data.Full || strings.Contains(text, capabilityNoteRemovedHead) || !strings.Contains(text, capabilityNoteNoCatalog) {
		t.Fatalf("mode change note = %q", text)
	}
}

func TestCapabilityNoteRestoredTool(t *testing.T) {
	catalog := capabilityNoteItem{Key: "action:a", Section: capabilityNoteCatalog, Text: "- action:a"}
	revoked := capabilityListing{Mode: capabilityNoteModeList, Items: []capabilityNoteItem{
		catalog, {Key: "unavailable:action_gmail_send", Section: capabilityNoteUnavailable, Text: "- action_gmail_send"},
	}}
	_, previous := capabilityNoteText(revoked, nil)
	restored := capabilityListing{Mode: capabilityNoteModeList, Items: []capabilityNoteItem{
		catalog, {Key: "action:gmail.send", Section: capabilityNoteCatalog, Text: "- action:gmail.send"},
	}}
	text, _ := capabilityNoteText(restored, &previous)
	if !strings.Contains(text, capabilityNoteRestoredHead+"\n- action_gmail_send") || strings.Contains(text, capabilityNoteRemovedHead) {
		t.Fatalf("restored note = %q", text)
	}
}
