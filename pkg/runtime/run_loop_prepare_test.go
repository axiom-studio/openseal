package runtime

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/workspace"
)

func prepareTestActions() []capability.ModelAction {
	read := func(skill, name string) capability.ModelAction {
		return capability.ModelAction{
			Name: name, Description: "Read " + name, SkillID: skill, Version: "1", Action: name,
			Risk: capability.RiskLevelRead, SideEffect: capability.SideEffectRead,
			InputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"q": map[string]interface{}{"type": "string"}}},
		}
	}
	actions := []capability.ModelAction{
		read(pinnedCapabilitySkillID, "openseal.list_setup_requests"),
		read(pinnedCapabilitySkillID, "openseal.discover_skills"),
	}
	for _, name := range []string{"notion.search", "notion.fetch", "gmail.list", "gmail.read", "drive.list", "drive.read", "calendar.list"} {
		actions = append(actions, read("skill-"+strings.SplitN(name, ".", 2)[0], name))
	}
	send := read("skill-gmail", "gmail.send")
	send.Risk, send.SideEffect = capability.RiskLevelExternal, capability.SideEffectExternal
	return append(actions, send)
}

type prepareFixture struct {
	t        *testing.T
	preparer *catalogModelStepPreparer
	binding  *TurnRunnerBinding
	run      *AgentRun
	render   *renderFixture
}

func newPrepareFixture(t *testing.T, authority ControlAuthority) *prepareFixture {
	registry, _ := fakeControlRegistry()
	preparer := newCatalogModelStepPreparer(catalogModelStepPreparerConfig{
		AgentInstructions: []string{"You are Finn.", "Purpose: help the team."},
		SkillPrompts: []HostedSkillPrompt{
			{SkillID: pinnedCapabilitySkillID, Version: "1", Reference: "openseal.builtin@1", Name: "Built-in", Instructions: "Use the built-in tools."},
			{SkillID: "skill-notion", Version: "1", Reference: "skill-notion@1", Name: "Notion", Instructions: "Notion rules."},
		},
		HostTools: HostToolSpecs(workspace.ChatAuthority("chat-1")),
	})
	preparer.controls = registry
	f := &prepareFixture{
		t: t, preparer: preparer,
		binding: &TurnRunnerBinding{
			DeploymentID: "agent-1", DefinitionID: "def-1", DefinitionVersion: "3", Model: "claude-test",
			ModelActions: prepareTestActions(), Authority: authority,
			Prompt: PromptContribution{
				AgentInstructions: []string{"Reply in the chat's language."},
				Sections:          []ModelStepSystemSection{{ID: "channel_rules", Title: "Channel", Text: "Keep replies short."}},
			},
		},
		run:    &AgentRun{ID: "run-1", Scope: testSessionScope, AssignedAgentID: "agent-1"},
		render: newRenderFixture(t, "chat"),
	}
	f.render.user("run-1", "find the launch notes")
	return f
}

func (f *prepareFixture) prepare(step string) ModelStepRequest {
	f.t.Helper()
	path := f.render.path()
	req, err := f.preparer.PrepareModelStep(context.Background(), StepInput{
		Run: f.run, Step: &AgentTurn{ID: step, RunID: f.run.ID}, Session: path.Session, Path: path,
		Binding: f.binding, Mode: StepModeNormal,
	})
	if err != nil {
		f.t.Fatalf("PrepareModelStep: %v", err)
	}
	if err := req.Validate(); err != nil {
		f.t.Fatalf("prepared request does not validate: %v", err)
	}
	return req
}

func toolNames(tools []ModelStepTool) []string {
	names := make([]string, len(tools))
	for i, tool := range tools {
		names[i] = tool.Name
	}
	return names
}

func fullPrepareAuthority() ControlAuthority {
	return ControlAuthority{
		AgentID: "agent-1", CanReportProgress: true, CanWait: true, CanAskUser: true, CanReact: true,
		TaskStart:   &HostedConversationTaskContext{ConversationID: "conv-1", CanStart: true},
		ForkAllowed: true,
	}
}

func TestPrepareToolOrderStableAcrossSteps(t *testing.T) {
	f := newPrepareFixture(t, fullPrepareAuthority())
	first := f.prepare("step-1")

	want := []string{
		"turn_final_answer", "turn_report_progress", "wait", "ask_user", "react",
		"conversation_start_task", "control_fork", "load_capabilities",
	}
	for _, spec := range HostToolSpecs(workspace.ChatAuthority("chat-1")) {
		want = append(want, spec.ToolName)
	}
	want = append(want, "action_openseal_discover_skills", "action_openseal_list_setup_requests")
	if got := toolNames(first.Tools); !reflect.DeepEqual(got, want) {
		t.Fatalf("tools = %v\nwant    %v", got, want)
	}
	for _, tool := range first.Tools {
		group := ModelStepToolHarness
		if strings.HasPrefix(tool.Name, governedToolPrefix) {
			group = ModelStepToolCore
		}
		if tool.Group != group {
			t.Errorf("tool %s group = %s, want %s", tool.Name, tool.Group, group)
		}
	}
	if first.Stream.FinalAnswerTool != "turn_final_answer" {
		t.Errorf("stream final tool = %q", first.Stream.FinalAnswerTool)
	}

	// A later step over a longer path (a read call and its result) sends the
	// same system and tools byte for byte.
	f.render.assistant("run-1", "step-1", "c1")
	f.render.tool("run-1", "step-1", "c1", ToolEntryCompleted)
	second := f.prepare("step-2")
	a, _ := json.Marshal([]interface{}{first.System, first.Tools})
	b, _ := json.Marshal([]interface{}{second.System, second.Tools})
	if string(a) != string(b) {
		t.Fatalf("system/tools changed between steps:\n%s\n%s", a, b)
	}
	if len(second.Entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(second.Entries))
	}
}

func TestPrepareLoadAppendsTools(t *testing.T) {
	f := newPrepareFixture(t, fullPrepareAuthority())
	before := f.prepare("step-1")

	f.render.add(&SessionEntry{RunID: "run-1", StepID: "step-1", Type: SessionEntryAssistant, Assistant: &AssistantEntry{
		Provider: ModelStepProviderAnthropic, Model: "claude-test", StopReason: ModelStopToolCalls,
		ToolCalls: []ModelToolCall{{ID: "c1", Name: LoadCapabilitiesTool, Arguments: json.RawMessage(`{"ids":["action:notion.search","action:gmail.send"]}`)}},
	}})
	f.render.add(&SessionEntry{RunID: "run-1", StepID: "step-1", Type: SessionEntryTool, State: ToolEntryCompleted, ToolCallID: "c1", Tool: &ToolEntry{
		Name: LoadCapabilitiesTool, Arguments: json.RawMessage(`{"ids":["action:notion.search","action:gmail.send"]}`), SideEffect: ToolSideEffectNone,
		LoadedCapabilities: []string{"prompt:skill-notion@1", "action:notion.search", "action:gmail.send"},
		Result:             &ToolResult{Status: ToolResultSucceeded, Content: []ContentPart{{Type: ContentPartText, Text: `{"status":"loaded"}`}}},
	}})
	after := f.prepare("step-2")

	if len(after.Tools) != len(before.Tools)+2 {
		t.Fatalf("tools after load = %v", toolNames(after.Tools))
	}
	if !reflect.DeepEqual(after.Tools[:len(before.Tools)], before.Tools) {
		t.Fatalf("a load changed the earlier tools:\n%v\n%v", toolNames(before.Tools), toolNames(after.Tools))
	}
	added := after.Tools[len(before.Tools):]
	if added[0].Name != "action_notion_search" || added[1].Name != "action_gmail_send" {
		t.Fatalf("appended tools = %v, want load order", toolNames(added))
	}
	for _, tool := range added {
		if tool.Group != ModelStepToolLoaded {
			t.Errorf("tool %s group = %s, want loaded", tool.Name, tool.Group)
		}
	}
	if added[1].Capability != "gmail.send" {
		t.Errorf("capability = %q", added[1].Capability)
	}
	if _, reviewed := added[1].InputSchema["properties"].(map[string]interface{})["reviewContext"]; !reviewed {
		t.Errorf("reviewed action schema has no reviewContext: %v", added[1].InputSchema)
	}
	// Loaded instructions travel in the tool result, never the system.
	if !reflect.DeepEqual(after.System, before.System) {
		t.Fatalf("system changed after a load")
	}
	if action, ok := LoopGovernedAction(f.binding.ModelActions, "action_gmail_send"); !ok || action.Name != "gmail.send" {
		t.Fatalf("LoopGovernedAction = %v %v", action.Name, ok)
	}
	if name := loopCapabilityToolName(f.binding.ModelActions)("action:notion.search"); name != "action_notion_search" {
		t.Fatalf("load tool name = %q", name)
	}
}

func sectionIDs(sections []ModelStepSystemSection) []string {
	ids := make([]string, len(sections))
	for i, section := range sections {
		ids[i] = section.ID
	}
	return ids
}

func TestPrepareSectionsFixedOrder(t *testing.T) {
	authority := fullPrepareAuthority()
	authority.FinalMode = FinalModeEvidence
	f := newPrepareFixture(t, authority)
	f.preparer.config.ImageInput = true
	f.preparer.config.RoutineScript = "Run the pinned command exactly."
	req := f.prepare("step-1")

	want := []string{
		"skill_access", "progress_rules", "conversation_tasks", "workspace", "media", "evidence",
		"review_summaries", "routine_script", loopSectionCapabilities, "channel_rules",
	}
	if got := sectionIDs(req.System.Sections); !reflect.DeepEqual(got, want) {
		t.Fatalf("sections = %v\nwant       %v", got, want)
	}
	if req.System.Sections[1].Text != loopProgressRulesText {
		t.Errorf("progress text is not the foreground variant")
	}
	wantInstructions := []string{"You are Finn.", "Purpose: help the team.", "Reply in the chat's language."}
	if !reflect.DeepEqual(req.System.AgentInstructions, wantInstructions) {
		t.Errorf("agent instructions = %v", req.System.AgentInstructions)
	}
	if len(req.System.SkillPrompts) != 1 || req.System.SkillPrompts[0].SkillID != pinnedCapabilitySkillID {
		t.Errorf("skill prompts = %+v, want only the pinned built-in", req.System.SkillPrompts)
	}

	// Task work: the activity-only progress rules; no task start, no
	// workspace, no images, no evidence, no routine script.
	bare := newPrepareFixture(t, ControlAuthority{AgentID: "agent-1", CanReportProgress: true, TaskWork: true})
	bare.preparer.config.HostTools = nil
	bare.binding.ModelActions = bare.binding.ModelActions[:2]
	bare.preparer.config.SkillPrompts = bare.preparer.config.SkillPrompts[:1]
	got := bare.prepare("step-1")
	if ids := sectionIDs(got.System.Sections); !reflect.DeepEqual(ids, []string{"progress_rules", "channel_rules"}) {
		t.Fatalf("bare sections = %v", ids)
	}
	if got.System.Sections[0].Text != loopProgressRulesTaskWorkText {
		t.Errorf("task work progress text not used")
	}
	if names := toolNames(got.Tools); !reflect.DeepEqual(names, []string{"turn_final_answer", "turn_report_progress", "action_openseal_discover_skills", "action_openseal_list_setup_requests"}) {
		t.Errorf("bare tools = %v", names)
	}

	// Decision Runs: only the decision tool, no capability listing.
	decision := newPrepareFixture(t, ControlAuthority{AgentID: "agent-1", FinalMode: FinalModeDecision, CanReportProgress: true})
	dreq := decision.prepare("step-1")
	if names := toolNames(dreq.Tools); !reflect.DeepEqual(names, []string{"agent_request_decision"}) {
		t.Errorf("decision tools = %v", names)
	}
	if ids := sectionIDs(dreq.System.Sections); !reflect.DeepEqual(ids, []string{"channel_rules"}) {
		t.Errorf("decision sections = %v", ids)
	}
	if dreq.Stream.FinalAnswerTool != "" {
		t.Errorf("decision stream final tool = %q", dreq.Stream.FinalAnswerTool)
	}

	// Explaining a failure sends no tools.
	path := f.render.path()
	explain, err := f.preparer.PrepareModelStep(context.Background(), StepInput{
		Run: f.run, Step: &AgentTurn{ID: "step-2", RunID: f.run.ID}, Session: path.Session, Path: path,
		Binding: f.binding, Mode: StepModeExplainFailure,
	})
	if err != nil {
		t.Fatal(err)
	}
	if explain.Purpose != ModelStepPurposeExplanation || len(explain.Tools) != 0 || explain.Validate() != nil {
		t.Errorf("explanation request purpose=%s tools=%d validate=%v", explain.Purpose, len(explain.Tools), explain.Validate())
	}
}

func TestPrepareNoFormWords(t *testing.T) {
	authority := fullPrepareAuthority()
	authority.FinalMode = FinalModeEvidence
	f := newPrepareFixture(t, authority)
	f.preparer.config.ImageInput = true
	f.binding.Prompt = PromptContribution{}
	f.preparer.config.AgentInstructions = nil
	req := f.prepare("step-1")
	forbidden := []string{"runoutput", "outputsummary", "continuationcheckpoint", "this turn", "proposal", "hosted-turn", "offeredactions", "_atlas"}
	var texts []string
	for _, section := range req.System.Sections {
		texts = append(texts, section.ID+": "+section.Title+"\n"+section.Text)
	}
	for _, tool := range req.Tools {
		texts = append(texts, tool.Name+": "+tool.Description)
	}
	for _, text := range texts {
		lower := strings.ToLower(text)
		for _, word := range forbidden {
			if strings.Contains(lower, word) {
				t.Errorf("prepared text contains form wording %q: %.120s", word, text)
			}
		}
	}
}

func TestLoopGovernedToolNamesValidAndUnique(t *testing.T) {
	actions := []capability.ModelAction{
		{Name: "gmail.send_email"}, {Name: "gmail_send.email"}, {Name: "mcp.server one.tool/x"},
		{Name: "skill." + strings.Repeat("long", 20)},
	}
	names := LoopGovernedToolNames(actions)
	seen := map[string]bool{}
	for _, action := range actions {
		name := names[action.Name]
		if !modelStepToolNamePattern.MatchString(name) || seen[name] {
			t.Fatalf("tool name %q for %q is invalid or repeated", name, action.Name)
		}
		seen[name] = true
		if back, ok := LoopGovernedAction(actions, name); !ok || back.Name != action.Name {
			t.Fatalf("%q resolves to %q", name, back.Name)
		}
	}
	if names["gmail.send_email"] != "action_gmail_send_email" {
		t.Fatalf("plain name = %q", names["gmail.send_email"])
	}
}
