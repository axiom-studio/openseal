package runtime

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep/modelsteptest"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
)

// Cache-stable capability loading: a load, a new Run and a Skill connected
// between Runs only append to what the model already saw.

// fixtureNoteAppender appends capabilities notes to a render fixture.
type fixtureNoteAppender struct{ f *modelsteptest.RenderFixture }

func (a fixtureNoteAppender) AppendAgentSessionEntries(_ context.Context, req AppendAgentSessionEntriesRequest) (*AppendAgentSessionEntriesResult, error) {
	var out []*SessionEntry
	for _, entry := range req.Entries {
		out = append(out, a.f.Add(entry))
	}
	return &AppendAgentSessionEntriesResult{Entries: out}, nil
}

// fixtureNoteSessions is a render fixture as a model step producer's
// session store.
type fixtureNoteSessions struct{ fixtureNoteAppender }

func (s fixtureNoteSessions) LoadAgentSessionPath(context.Context, Scope, string) (*AgentSessionPath, error) {
	return s.f.Path(), nil
}

func (s fixtureNoteSessions) GetActionCall(context.Context, Scope, string) (*ActionCall, error) {
	return nil, nil
}

// ensureNote produces one model step, which first appends the capabilities
// note the step's path needs, and reports whether it appended one.
func (f *prepareFixture) ensureNote(step string) bool {
	f.t.Helper()
	host := &modelsteptest.RecordingStepHost{Reply: func(req ModelStepRequest) ModelStepResult { return modelsteptest.StepResult(req, "Done.") }}
	producer, err := modelstep.NewModelStepProducer(host, f.preparer, fixtureNoteSessions{fixtureNoteAppender{f.render}})
	if err != nil {
		f.t.Fatal(err)
	}
	before := len(f.capabilityNotes())
	path := f.render.Path()
	in := agentrun.StepInput{Run: f.run, Step: &AgentTurn{ID: step, RunID: f.run.ID}, Session: path.Session, Path: path, Binding: f.binding, Mode: agentrun.StepModeNormal}
	if _, err := producer.Produce(context.Background(), in); err != nil {
		f.t.Fatal(err)
	}
	return len(f.capabilityNotes()) > before
}

func (f *prepareFixture) capabilityNotes() []*SessionEntry {
	var notes []*SessionEntry
	for _, entry := range f.render.Entries {
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
	f.render.Assistant("run-1", "step-1")
	first := f.prepare("step-2")

	// Between Runs the user connects Slack: two actions and a prompt.
	f.run = &AgentRun{ID: "run-2", Scope: testSessionScope, AssignedAgentID: "agent-1"}
	f.render.User("run-2", "post the launch notes to slack")
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
	f.render.Assistant("run-1", "step-2")
	kept := f.render.User("run-1", "and the budget?")
	f.render.Compaction("run-1", kept.ID)
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
	x := newToolExecFixture(t, agentrun.ControlAuthority{})
	step := x.step("step-1", AgentTurnStatusRunning, toolCall("call-revoked", "action_gmail_send", `{}`))
	if _, err := x.binding.Tools.ExecuteStepTools(x.ctx, x.input(step)); err != nil {
		t.Fatal(err)
	}
	stored := x.entry(step.entries[0])
	if stored.State != ToolEntryError || !stored.Tool.Result.IsError || resultValue(t, stored)["error"] != loopToolNotAvailableCode {
		t.Fatalf("revoked call = %s %v", stored.State, stored.Tool.Result)
	}
}

func TestLoadDescriptionModeIndependent(t *testing.T) {
	listed := modelstep.NewCapabilitySet(modelsteptest.CapabilityCatalogFixture(), nil, modelstep.CapabilitySetConfig{})
	searching := modelstep.NewCapabilitySet(modelsteptest.CapabilityCatalogFixture(), nil, modelstep.CapabilitySetConfig{CatalogBytes: 200})
	if listed.Searching() || !searching.Searching() {
		t.Fatal("fixtures must cover both modes")
	}
	f := newPrepareFixture(t, fullPrepareAuthority())
	listedReq := f.prepare("step-1")
	f.preparer.config.Capabilities.CatalogBytes = 100
	searchReq := f.prepare("step-1")
	if !f.preparer.CapabilitySet(agentrun.StepInput{Run: f.run, Binding: f.binding}).Searching() {
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

func TestLoadFreezesSurfacedToolsOnTheEntry(t *testing.T) {
	x := newToolExecFixture(t, agentrun.ControlAuthority{})
	x.binding.ModelActions = modelsteptest.CapabilityCatalogFixture().Actions
	step := x.step("step-1", AgentTurnStatusRunning, toolCall("call-load", modelstep.LoadCapabilitiesTool, `{"ids":["action:notion.op2"]}`))
	if _, err := x.binding.Tools.ExecuteStepTools(x.ctx, x.input(step)); err != nil {
		t.Fatal(err)
	}
	stored := x.entry(step.entries[0])
	if stored.State != ToolEntryCompleted || len(stored.Tool.SurfacedTools) != 8 {
		t.Fatalf("load entry = %s %+v", stored.State, stored.Tool.SurfacedTools)
	}
	first := stored.Tool.SurfacedTools[0]
	names := conversation.LoopGovernedToolNames(x.binding.ModelActions)
	if first.Capability != "action:notion.op2" || first.Name != names["notion.op2"] || first.Description != "Notion operation 2" ||
		first.InputSchema["type"] != "object" {
		t.Fatalf("frozen tool = %+v", first)
	}
}

func TestValidateSurfaces(t *testing.T) {
	base := func() ModelStepRequest {
		req := validModelStepRequest()
		req.Tools = req.Tools[:2]
		assistant := modelStepTestAssistant("c1")
		assistant.ToolCalls[0].Name = modelstep.LoadCapabilitiesTool
		req.Entries = []SessionEntryView{
			{ID: "u1", Sequence: 1, Type: SessionEntryUser, User: &UserEntryView{Content: []ContentPart{{Type: ContentPartText, Text: "hi"}}}},
			{ID: "a1", Sequence: 2, Type: SessionEntryAssistant, Assistant: assistant},
			{ID: "t1", Sequence: 3, Type: SessionEntryTool, Surfaces: []string{"x_one", "x_two"}, Tool: &ToolEntryView{
				CallID: "c1", Name: modelstep.LoadCapabilitiesTool, Arguments: json.RawMessage(`{}`), State: ToolEntryCompleted,
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
