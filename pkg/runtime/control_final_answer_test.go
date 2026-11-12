package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep/modelsteptest"

	"github.com/axiom-studio/openseal/pkg/presentation"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// finalFixture is a control fixture that runs the real turn_final_answer
// handler.
func newFinalFixture(t *testing.T, calls ...agentrun.LoopToolCall) *controlFixture {
	t.Helper()
	f := newControlFixture(t, calls)
	registry := controlRegistry{}
	registry.register(ControlToolFinalAnswer, finalAnswerHandler{})
	f.exec.handlers = registry
	return f
}

func finalCall(id, args string) agentrun.LoopToolCall {
	return agentrun.LoopToolCall{ID: id, Name: string(ControlToolFinalAnswer), Arguments: json.RawMessage(args)}
}

func (f *controlFixture) settleFinal(callID string, authority agentrun.ControlAuthority) ControlResult {
	f.t.Helper()
	result, err := f.exec.Settle(f.ctx, f.context(callID, authority))
	if err != nil {
		f.t.Fatalf("settle %s: %v", callID, err)
	}
	return result
}

// plainContext is the context of a plain-text final on its own (running) step.
func (f *controlFixture) plainContext(stepID string, sequence int64, authority agentrun.ControlAuthority) agentrun.ControlContext {
	return agentrun.ControlContext{
		Run: f.run, WorkerID: controlTestWorker, Session: f.session, Authority: authority,
		Step:      &AgentTurn{ID: stepID, RunID: f.run.ID, Sequence: sequence, Status: AgentTurnStatusRunning},
		Assistant: &SessionEntry{Type: SessionEntryAssistant, Assistant: &AssistantEntry{StopReason: ModelStopEnd}},
		Binding:   &agentrun.TurnRunnerBinding{Authority: authority},
	}
}

func (f *controlFixture) plain(stepID string, sequence int64, authority agentrun.ControlAuthority, text string) ControlResult {
	f.t.Helper()
	result, err := f.exec.PlainTextFinal(f.ctx, f.plainContext(stepID, sequence, authority), text)
	if err != nil {
		f.t.Fatalf("plain final %s: %v", stepID, err)
	}
	return result
}

func requireFinal(t *testing.T, result ControlResult) *agentrun.FinalAnswer {
	t.Helper()
	if result.Kind != ControlFinal || result.Final == nil {
		t.Fatalf("result = %#v, want a final answer", result)
	}
	if err := result.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	return result.Final
}

func requireRejected(t *testing.T, f *controlFixture, result ControlResult, callID, code string) map[string]interface{} {
	t.Helper()
	if result.Kind != ControlContinue || result.Entry == nil || result.Entry.State != ToolEntryError {
		t.Fatalf("result = %#v, want a rejection", result)
	}
	stored := f.stored(callID)
	value := requireControlError(t, stored.Tool.Result, ToolResultFailed, code)
	if code != controlArgumentsInvalidCode {
		meta, err := DecodeControlEntryMeta(stored)
		if err != nil || meta == nil || meta.ErrorCode != code || meta.Kind != ControlToolFinalAnswer {
			t.Fatalf("meta = %#v, %v", meta, err)
		}
	}
	return value
}

func requireRejectionNote(t *testing.T, result ControlResult, stepID, code string) *SessionEntry {
	t.Helper()
	note := result.Entry
	if result.Kind != ControlContinue && result.Kind != ControlFailed || note == nil || note.Type != SessionEntryNote ||
		note.Note == nil || note.Note.Kind != NoteFinalRejection || note.SourceKey != FinalRejectionEntryKey(stepID) {
		t.Fatalf("result = %#v, want a final_rejection note", result)
	}
	var data finalRejectionNoteData
	if err := json.Unmarshal(note.Note.Data, &data); err != nil || data.ErrorCode != code {
		t.Fatalf("note data = %s, %v", note.Note.Data, err)
	}
	return note
}

func schemaProperties(t *testing.T, schema map[string]interface{}) (map[string]interface{}, []string) {
	t.Helper()
	properties, _ := schema["properties"].(map[string]interface{})
	required, _ := schema["required"].([]string)
	return properties, required
}

func TestFinalAnswerToolSchemaFollowsAuthority(t *testing.T) {
	keys := func(m map[string]interface{}) []string {
		var out []string
		for _, field := range []string{"answer", "appliedSkillRefs", "visual", "broadcastToChannel", "report", "silent", "evidenceClaims", "files"} {
			if _, ok := m[field]; ok {
				out = append(out, field)
			}
		}
		return out
	}
	cases := []struct {
		name          string
		authority     agentrun.ControlAuthority
		wantFields    []string
		wantRequired  []string
		answerSpoken  bool
		wantSkillEnum []string
	}{
		{"default", agentrun.ControlAuthority{}, []string{"answer", "report", "files"}, []string{"answer"}, false, nil},
		{"chat with skills", agentrun.ControlAuthority{SkillPromptRefs: []string{"s1", "s2"}, VisualAllowed: true, BroadcastAllowed: true},
			[]string{"answer", "appliedSkillRefs", "visual", "broadcastToChannel", "report", "files"}, []string{"answer", "appliedSkillRefs"}, false, []string{"s1", "s2"}},
		{"spoken", agentrun.ControlAuthority{FinalMode: conversationrun.FinalModeSpoken}, []string{"answer", "silent"}, []string{"answer", "silent"}, true, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			specs := builtinControlSpecs(tc.authority)
			var spec *ControlToolSpec
			for i := range specs {
				if specs[i].Name == ControlToolFinalAnswer {
					spec = &specs[i]
				}
			}
			if spec == nil || spec.Phase != ControlPhaseSettle {
				t.Fatalf("final tool spec = %#v", spec)
			}
			properties, required := schemaProperties(t, spec.InputSchema)
			if got := keys(properties); !reflect.DeepEqual(got, tc.wantFields) || len(properties) != len(tc.wantFields) {
				t.Fatalf("properties = %v, want %v", got, tc.wantFields)
			}
			if !reflect.DeepEqual(required, tc.wantRequired) {
				t.Fatalf("required = %v, want %v", required, tc.wantRequired)
			}
			if spec.InputSchema["additionalProperties"] != false {
				t.Fatal("schema must be closed")
			}
			answer := properties["answer"].(map[string]interface{})
			if spoken := strings.HasPrefix(answer["description"].(string), "Only the words to speak"); spoken != tc.answerSpoken {
				t.Fatalf("answer description = %v", answer["description"])
			}
			if tc.wantSkillEnum != nil {
				// The refs are validated when the answer is checked; the
				// schema carries no enum, so connecting a Skill never
				// changes the tool.
				items := properties["appliedSkillRefs"].(map[string]interface{})["items"].(map[string]interface{})
				if _, ok := items["enum"]; ok || items["type"] != "string" {
					t.Fatalf("skill items = %v", items)
				}
			}
			// Constant per claim (prompt cache).
			again := builtinControlSpecs(tc.authority)
			if !reflect.DeepEqual(specs, again) {
				t.Fatal("specs differ between two listings")
			}
		})
	}
	// Decision and review Runs never get turn_final_answer.
	for _, mode := range []agentrun.FinalAnswerMode{agentrun.FinalModeDecision, agentrun.FinalModeReview} {
		for _, spec := range builtinControlSpecs(agentrun.ControlAuthority{FinalMode: mode}) {
			if spec.Name == ControlToolFinalAnswer {
				t.Fatalf("%s offers turn_final_answer", mode)
			}
		}
	}
}

func TestFinalAnswerToolAcceptedLeavesEntryForDelivery(t *testing.T) {
	f := newFinalFixture(t, finalCall("final", `{"answer":"  Here it is.  "}`))
	final := requireFinal(t, f.settleFinal("final", agentrun.ControlAuthority{}))
	entry := f.entries["final"]
	if final.PlainText || final.Answer != "Here it is." || final.StepID != entry.StepID ||
		final.ToolEntryID != entry.ID || final.ToolCallID != "final" || final.Park != nil {
		t.Fatalf("final = %#v", final)
	}
	if !reflect.DeepEqual(final.Output, map[string]interface{}{"summary": "Here it is."}) {
		t.Fatalf("output = %#v", final.Output)
	}
	// The loop marks the entry delivered after posting.
	if stored := f.stored("final"); stored.State != ToolEntryPending || stored.Tool.Result != nil {
		t.Fatalf("entry = %s %#v", stored.State, stored.Tool.Result)
	}
}

func TestPlainTextIsFinalAnswer(t *testing.T) {
	f := newFinalFixture(t)
	final := requireFinal(t, f.plain("step-2", 2, agentrun.ControlAuthority{}, "  The deploy finished.\n"))
	if !final.PlainText || final.Answer != "The deploy finished." || final.StepID != "step-2" ||
		final.ToolEntryID != "" || final.ToolCallID != "" {
		t.Fatalf("final = %#v", final)
	}
	if final.Output["summary"] != "The deploy finished." {
		t.Fatalf("output = %#v", final.Output)
	}
	// A plain final on an entry with tool calls is a loop bug.
	cc := f.plainContext("step-3", 3, agentrun.ControlAuthority{})
	cc.Assistant.Assistant.ToolCalls = []ModelToolCall{{ID: "x", Name: "react"}}
	if _, err := f.exec.PlainTextFinal(f.ctx, cc, "text"); err == nil {
		t.Fatal("plain final accepted with tool calls")
	}
	// Lease rule.
	cc = f.plainContext("step-3", 3, agentrun.ControlAuthority{})
	cc.WorkerID = "other"
	if _, err := f.exec.PlainTextFinal(f.ctx, cc, "text"); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("foreign worker = %v", err)
	}
}

func TestPlainTextJSONFinalIsRepaired(t *testing.T) {
	f := newFinalFixture(t)
	authority := agentrun.ControlAuthority{BroadcastAllowed: true}
	final := requireFinal(t, f.plain("step-2", 2, authority, ` {"answer":"Posted.","broadcastToChannel":true} `))
	if !final.PlainText || final.Answer != "Posted." || final.Output["broadcastToChannel"] != true || final.Output["summary"] != "Posted." {
		t.Fatalf("final = %#v", final)
	}
	// JSON that is not the tool's arguments stays text.
	for _, text := range []string{`{"result":"ok"}`, `{"answer":"x","report":1}`, `{"answer":"x","silent":true}`} {
		final := requireFinal(t, f.plain("step-3", 3, authority, text))
		if final.Answer != text {
			t.Fatalf("%s answer = %q", text, final.Answer)
		}
	}
}

func TestSilentOnlySpoken(t *testing.T) {
	f := newFinalFixture(t,
		finalCall("default", `{"answer":"","silent":true}`),
		finalCall("spoken-text", `{"answer":"Hello","silent":true}`),
		finalCall("spoken", `{"answer":"","silent":true}`),
		finalCall("spoken-missing", `{"answer":"Hi"}`),
	)
	// silent is not offered outside spoken Runs.
	value := requireRejected(t, f, f.settleFinal("default", agentrun.ControlAuthority{}), "default", controlArgumentsInvalidCode)
	if !strings.Contains(value["message"].(string), `"silent"`) {
		t.Fatalf("message = %v", value["message"])
	}
	spoken := agentrun.ControlAuthority{FinalMode: conversationrun.FinalModeSpoken}
	requireRejected(t, f, f.settleFinal("spoken-text", spoken), "spoken-text", finalSilentInvalidCode)
	value = requireRejected(t, f, f.settleFinal("spoken-missing", spoken), "spoken-missing", controlArgumentsInvalidCode)
	if value["message"] != `missing required field "silent"` {
		t.Fatalf("message = %v", value["message"])
	}
	final := requireFinal(t, f.settleFinal("spoken", spoken))
	if final.Answer != "" || !final.Silent() || !reflect.DeepEqual(final.Output, map[string]interface{}{"silent": true}) {
		t.Fatalf("final = %#v", final)
	}
}

func TestSpokenEmptyTextIsSilent(t *testing.T) {
	f := newFinalFixture(t)
	final := requireFinal(t, f.plain("step-2", 2, agentrun.ControlAuthority{FinalMode: conversationrun.FinalModeSpoken}, "  \n"))
	if !final.PlainText || !final.Silent() || final.Answer != "" {
		t.Fatalf("final = %#v", final)
	}
}

func TestEmptyPlainTextRejectedThenFails(t *testing.T) {
	f := newFinalFixture(t)
	for i, step := range []string{"step-2", "step-3"} {
		result := f.plain(step, int64(i+2), agentrun.ControlAuthority{}, "")
		if result.Kind != ControlContinue {
			t.Fatalf("attempt %d = %#v", i+1, result)
		}
		note := requireRejectionNote(t, result, step, finalEmptyReplyCode)
		if !strings.HasPrefix(note.Note.Text, "An empty reply is not an answer.") {
			t.Fatalf("note = %q", note.Note.Text)
		}
	}
	// Replaying the same step's rejection keeps one note.
	again := f.plain("step-3", 3, agentrun.ControlAuthority{}, "")
	if again.Kind != ControlContinue {
		t.Fatalf("replay = %#v", again)
	}
	// The third distinct step fails the Run.
	result := f.plain("step-4", 4, agentrun.ControlAuthority{}, "")
	if result.Kind != ControlFailed || result.Failure == nil || result.Failure.Code != finalEmptyReplyCode {
		t.Fatalf("third empty reply = %#v", result)
	}
	if err := result.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

func (f *controlFixture) completeEntry(callID string) {
	f.t.Helper()
	entry := f.entries[callID]
	_, err := f.store.TransitionToolEntry(f.ctx, ToolEntryTransitionRequest{
		Scope: f.run.Scope, EntryID: entry.ID, RunID: f.run.ID,
		Lease: AgentRunLeaseGuard{WorkerID: controlTestWorker, Now: f.now},
		From:  entry.State, To: ToolEntryCompleted,
		Result: controlJSONResult(ToolResultSucceeded, nil), Now: f.now,
	})
	if err != nil {
		f.t.Fatalf("complete %s: %v", callID, err)
	}
}

func TestEmptyAnswerAfterReactionIsReactionsOnly(t *testing.T) {
	f := newFinalFixture(t,
		agentrun.LoopToolCall{ID: "react", Name: string(ControlToolReact), Arguments: json.RawMessage(`{}`)},
		finalCall("empty", `{"answer":" "}`),
		finalCall("after", `{"answer":""}`),
	)
	requireRejected(t, f, f.settleFinal("empty", agentrun.ControlAuthority{}), "empty", finalAnswerEmptyCode)
	if result := f.plain("step-2", 2, agentrun.ControlAuthority{}, ""); result.Kind != ControlContinue {
		t.Fatalf("empty plain text before a reaction = %#v", result)
	}
	f.completeEntry("react")
	final := requireFinal(t, f.settleFinal("after", agentrun.ControlAuthority{}))
	if !final.ReactionsOnly() || final.Answer != "" {
		t.Fatalf("final = %#v", final)
	}
	final = requireFinal(t, f.plain("step-3", 3, agentrun.ControlAuthority{}, ""))
	if !final.ReactionsOnly() || !final.PlainText {
		t.Fatalf("plain final = %#v", final)
	}
}

func TestInvalidVisualOnceThenDropped(t *testing.T) {
	invalid := `{"answer":"Here.","visual":{"title":"Prices","components":[]}}`
	f := newFinalFixture(t, finalCall("first", invalid), finalCall("second", invalid))
	authority := agentrun.ControlAuthority{VisualAllowed: true}
	value := requireRejected(t, f, f.settleFinal("first", authority), "first", finalVisualInvalidCode)
	if !strings.Contains(value["message"].(string), "at least one component") {
		t.Fatalf("message = %v", value["message"])
	}
	final := requireFinal(t, f.settleFinal("second", authority))
	if final.Visual() != nil || final.Answer != "Here." {
		t.Fatalf("final = %#v", final)
	}
	if rejection, _ := final.Output["visualRejection"].(string); !strings.Contains(rejection, "at least one component") {
		t.Fatalf("visualRejection = %v", final.Output["visualRejection"])
	}
	// visual is not offered without VisualAllowed.
	g := newFinalFixture(t, finalCall("final", invalid))
	requireRejected(t, g, g.settleFinal("final", agentrun.ControlAuthority{}), "final", controlArgumentsInvalidCode)
}

func TestValidVisualRidesOnOutput(t *testing.T) {
	visual := presentation.Visual{Title: "Status", Components: []presentation.Component{}}
	if err := json.Unmarshal([]byte(`[{"id":"note","type":"text","text":"All good"}]`), &visual.Components); err != nil {
		t.Fatalf("components: %v", err)
	}
	if _, err := presentation.Validate(context.Background(), visual.Surface(presentation.StateReady), presentation.ValidateOptions{SkipHostChecks: true}); err != nil {
		t.Skipf("fixture visual is not valid in this catalogue: %v", err)
	}
	args, _ := json.Marshal(map[string]interface{}{"answer": "Done.", "visual": visual})
	f := newFinalFixture(t, finalCall("final", string(args)))
	final := requireFinal(t, f.settleFinal("final", agentrun.ControlAuthority{VisualAllowed: true}))
	if got := final.Visual(); got == nil || got.Title != "Status" || len(got.Components) != 1 {
		t.Fatalf("visual = %#v", got)
	}
	if _, rejected := final.Output["visualRejection"]; rejected {
		t.Fatal("valid visual rejected")
	}
}

func TestReportTakesPrecedenceOverAnswer(t *testing.T) {
	f := newFinalFixture(t, finalCall("final", `{"answer":"Short version.","report":"  The full report.  "}`))
	final := requireFinal(t, f.settleFinal("final", agentrun.ControlAuthority{}))
	if final.Answer != "The full report." || final.Output["summary"] != "Short version." || final.Output["report"] != "The full report." {
		t.Fatalf("final = %#v", final)
	}
	// report is not offered on spoken Runs.
	g := newFinalFixture(t, finalCall("final", `{"answer":"Hi","silent":false,"report":"x"}`))
	requireRejected(t, g, g.settleFinal("final", agentrun.ControlAuthority{FinalMode: conversationrun.FinalModeSpoken}), "final", controlArgumentsInvalidCode)
}

func TestBroadcastToChannelInOutput(t *testing.T) {
	f := newFinalFixture(t, finalCall("on", `{"answer":"News.","broadcastToChannel":true}`), finalCall("off", `{"answer":"Reply.","broadcastToChannel":false}`))
	authority := agentrun.ControlAuthority{BroadcastAllowed: true}
	final := requireFinal(t, f.settleFinal("on", authority))
	if !final.BroadcastToChannel() || final.Output["broadcastToChannel"] != true {
		t.Fatalf("final = %#v", final)
	}
	final = requireFinal(t, f.settleFinal("off", authority))
	if final.BroadcastToChannel() {
		t.Fatalf("broadcast false = %#v", final.Output)
	}
	if _, ok := final.Output["broadcastToChannel"]; ok {
		t.Fatal("false broadcast written to output")
	}
	g := newFinalFixture(t, finalCall("final", `{"answer":"x","broadcastToChannel":true}`))
	requireRejected(t, g, g.settleFinal("final", agentrun.ControlAuthority{}), "final", controlArgumentsInvalidCode)
}

func TestFinalAnswerSkillSelections(t *testing.T) {
	f := newFinalFixture(t,
		finalCall("missing", `{"answer":"x"}`),
		finalCall("dup", `{"answer":"x","appliedSkillRefs":["s1","s1"]}`),
		finalCall("unknown", `{"answer":"x","appliedSkillRefs":["s9"]}`),
		finalCall("ok", `{"answer":"x","appliedSkillRefs":["s2"]}`),
	)
	authority := agentrun.ControlAuthority{SkillPromptRefs: []string{"s1", "s2"}}
	requireRejected(t, f, f.settleFinal("missing", authority), "missing", controlArgumentsInvalidCode)
	// A ref cited twice counts once; a ref no loaded Skill has is left out.
	for _, id := range []string{"dup", "unknown"} {
		got := requireFinal(t, f.settleFinal(id, authority))
		applied := 0
		for _, selection := range got.Output["skillSelections"].([]HostedSkillSelection) {
			if selection.Disposition == HostedSkillApplied {
				applied++
			}
		}
		if want := map[string]int{"dup": 1, "unknown": 0}[id]; applied != want {
			t.Fatalf("%s: applied = %d, want %d", id, applied, want)
		}
	}
	final := requireFinal(t, f.settleFinal("ok", authority))
	want := []HostedSkillSelection{
		{SkillRef: "s1", Disposition: HostedSkillNotApplied, Summary: "Not used in this answer."},
		{SkillRef: "s2", Disposition: HostedSkillApplied, Summary: "Used in this answer."},
	}
	if !reflect.DeepEqual(final.Output["skillSelections"], want) {
		t.Fatalf("selections = %#v", final.Output["skillSelections"])
	}
	// Plain text cites nothing: every offered Skill is not applied.
	plain := requireFinal(t, f.plain("step-2", 2, authority, "Answer."))
	if selections := plain.Output["skillSelections"].([]HostedSkillSelection); len(selections) != 2 ||
		selections[0].Disposition != HostedSkillNotApplied || selections[1].Disposition != HostedSkillNotApplied {
		t.Fatalf("plain selections = %#v", selections)
	}
}

func TestDecisionRunPlainTextGetsNoteThenFails(t *testing.T) {
	f := newFinalFixture(t)
	decision := agentrun.ControlAuthority{FinalMode: agentrun.FinalModeDecision}
	for i, step := range []string{"step-2", "step-3"} {
		result := f.plain(step, int64(i+2), decision, "I accept the request.")
		if result.Kind != ControlContinue {
			t.Fatalf("attempt %d = %#v", i+1, result)
		}
		note := requireRejectionNote(t, result, step, finalToolRequiredCode)
		if !strings.HasPrefix(note.Note.Text, "Finish with agent_request_decision (") {
			t.Fatalf("note = %q", note.Note.Text)
		}
	}
	// The JSON repair is only for turn_final_answer, which decision Runs lack.
	result := f.plain("step-4", 4, decision, `{"answer":"accept"}`)
	if result.Kind != ControlFailed || result.Failure == nil || result.Failure.Code != finalToolRequiredCode ||
		result.Failure.Kind != conversation.RunFailureKindExecution {
		t.Fatalf("third plain reply = %#v", result)
	}
	requireRejectionNote(t, result, "step-4", finalToolRequiredCode)
	// The tool itself is not available in a decision Run.
	g := newFinalFixture(t, finalCall("final", `{"answer":"x"}`))
	result = g.settleFinal("final", decision)
	requireControlError(t, g.stored("final").Tool.Result, ToolResultRefused, "tool_not_available")
	// Review Runs name their own tool.
	h := newFinalFixture(t)
	note := requireRejectionNote(t, h.plain("step-2", 2, agentrun.ControlAuthority{FinalMode: agentrun.FinalModeReview}, "ok"), "step-2", finalToolRequiredCode)
	if !strings.HasPrefix(note.Note.Text, "Finish with agent_request_review (") {
		t.Fatalf("review note = %q", note.Note.Text)
	}
}

// governedEntry turns the fixture's pending call into a finished governed
// loop call of the Run.
func (f *controlFixture) governedEntry(callID, skillID, action string, output map[string]interface{}) {
	f.t.Helper()
	actionID := "action-" + callID
	f.putAction(&ActionCall{
		ID: actionID, Scope: f.run.Scope, RunID: f.run.ID, SkillID: skillID, Action: action,
		Status: ActionCallStatusSucceeded, Origin: ActionOriginLoop, Output: output, CreatedAt: f.now, UpdatedAt: f.now,
	})
	entry := f.entries[callID]
	_, err := f.store.TransitionToolEntry(f.ctx, ToolEntryTransitionRequest{
		Scope: f.run.Scope, EntryID: entry.ID, RunID: f.run.ID,
		Lease: AgentRunLeaseGuard{WorkerID: controlTestWorker, Now: f.now},
		From:  ToolEntryPending, To: ToolEntryCompleted, ActionCallID: actionID,
		Result: controlJSONResult(ToolResultSucceeded, output), Now: f.now,
	})
	if err != nil {
		f.t.Fatalf("finish governed %s: %v", callID, err)
	}
}

func TestFinalAnswerParksForOTPKeepsRunAlive(t *testing.T) {
	f := newFinalFixture(t,
		agentrun.LoopToolCall{ID: "browser", Name: "live_browser_submit", Arguments: json.RawMessage(`{}`)},
		finalCall("final", `{"answer":"  What is the code the bank just sent you? "}`),
		finalCall("cannot-ask", `{"answer":"What is the code?"}`),
	)
	f.governedEntry("browser", liveBrowserSkillID, "live-browser-submit", map[string]interface{}{"status": "otp_required"})
	final := requireFinal(t, f.settleFinal("final", agentrun.ControlAuthority{CanAskUser: true}))
	park := final.Park
	if park == nil || !park.Deliver || park.Park.Status != AgentRunStatusWaitingForEvent || park.Park.Wake == nil ||
		park.Park.Wake.Type != "user_message" || park.Park.Summary != "What is the code the bank just sent you?" {
		t.Fatalf("park = %#v", park)
	}
	if err := park.Park.Validate(); err != nil {
		t.Fatalf("park validate: %v", err)
	}
	// The answer is delivered; the Run itself is untouched (still running).
	run, err := f.store.GetAgentRun(f.ctx, f.run.Scope, f.run.ID)
	if err != nil || run.Status != AgentRunStatusRunning || run.LeaseOwner != controlTestWorker {
		t.Fatalf("run = %#v, %v", run, err)
	}
	// A Run that cannot ask its conversation completes.
	final = requireFinal(t, f.settleFinal("cannot-ask", agentrun.ControlAuthority{}))
	if final.Park != nil {
		t.Fatalf("park without ask = %#v", final.Park)
	}
	// Plain text gets the same park.
	plain := requireFinal(t, f.plain("step-2", 2, agentrun.ControlAuthority{CanAskUser: true}, "Which code did you get?"))
	if plain.Park == nil || !plain.Park.Deliver {
		t.Fatalf("plain park = %#v", plain.Park)
	}
}

func TestTaskFinalParksOnPendingSetup(t *testing.T) {
	f := newFinalFixture(t,
		agentrun.LoopToolCall{ID: "setup", Name: "openseal_skills_request_setup", Arguments: json.RawMessage(`{}`)},
		finalCall("final", `{"answer":"I asked you to connect the account."}`),
	)
	f.run.Kind = RunKindAgentWork
	f.run.Context = map[string]interface{}{ConversationTaskContextKey: "task-1"}
	f.governedEntry("setup", SkillManagementSkillID, agentrun.SkillActionRequestSetup, map[string]interface{}{
		"setupRequest": map[string]interface{}{"id": "setup-1", "status": "pending", "runId": f.run.ID, "actionCallId": "action-setup"},
	})
	final := requireFinal(t, f.settleFinal("final", agentrun.ControlAuthority{TaskWork: true, CanAskUser: true}))
	park := final.Park
	if park == nil || park.Deliver || park.Park.Status != AgentRunStatusWaitingForEvent || park.Park.Wake == nil ||
		park.Park.Wake.Type != conversationrun.ConversationTaskSkillSetupWakeType || park.Park.Wake.Reference != "setup-1" ||
		park.Park.Summary != skillSetupWaitSummary {
		t.Fatalf("park = %#v", park)
	}
	// A kernel resolution receipt clears it.
	f.run.PendingInterventions = []AgentRunIntervention{{ID: "i1", SkillSetupResolution: &SkillSetupResolutionReceipt{
		Scope: f.run.Scope, RunID: f.run.ID, RequestID: "setup-1", ActionCallID: "action-setup", Status: "resolved",
	}}}
	final = requireFinal(t, f.plain("step-2", 2, agentrun.ControlAuthority{TaskWork: true}, "Connected; here is the report."))
	if final.Park != nil {
		t.Fatalf("park after resolution = %#v", final.Park)
	}
}

type recordingPublisher struct {
	properties map[string]interface{}
	validated  []map[string]json.RawMessage
	reject     error
}

func (p *recordingPublisher) FinalAnswerProperties(run *AgentRun) map[string]interface{} {
	return p.properties
}

func (p *recordingPublisher) ValidateFinalAnswer(run *AgentRun, fields map[string]json.RawMessage) error {
	p.validated = append(p.validated, fields)
	return p.reject
}

func (p *recordingPublisher) PublishFinalOutput(ctx context.Context, publication agentrun.FinalOutputPublication) (map[string]interface{}, error) {
	return nil, errors.New("not called by the control section")
}

func TestFinalOutputPublisherValidatesExtensionFields(t *testing.T) {
	files := `[{"name":"a.md","mediaType":"text/markdown","text":"# A"}]`
	f := newFinalFixture(t,
		finalCall("unknown", `{"answer":"x","attachments":[]}`),
		finalCall("rejected", `{"answer":"x","generatedFiles":`+files+`}`),
		finalCall("ok", `{"answer":"Done.","generatedFiles":`+files+`}`),
		finalCall("none", `{"answer":"Done."}`),
	)
	publisher := &recordingPublisher{properties: map[string]interface{}{
		"generatedFiles": map[string]interface{}{"type": "array"},
		"answer":         map[string]interface{}{"type": "number"}, // kernel names are never the host's
	}}
	context := func(callID string) agentrun.ControlContext {
		cc := f.context(callID, agentrun.ControlAuthority{})
		cc.Binding.FinalOutput = publisher
		return cc
	}
	settle := func(callID string) ControlResult {
		result, err := f.exec.Settle(f.ctx, context(callID))
		if err != nil {
			t.Fatalf("settle %s: %v", callID, err)
		}
		return result
	}
	// A field nobody offers is dropped, not refused: the answer stands.
	requireFinal(t, settle("unknown"))
	if len(publisher.validated) != 0 {
		t.Fatalf("a dropped field reached the publisher: %#v", publisher.validated)
	}
	publisher.reject = errors.New("generatedFiles[0].name must end in .txt")
	value := requireRejected(t, f, settle("rejected"), "rejected", finalOutputInvalidCode)
	if value["message"] != "generatedFiles[0].name must end in .txt" {
		t.Fatalf("message = %v", value["message"])
	}
	publisher.reject = nil
	final := requireFinal(t, settle("ok"))
	if _, leaked := final.Output["generatedFiles"]; leaked || final.Output["summary"] != "Done." {
		t.Fatalf("output = %#v", final.Output)
	}
	last := publisher.validated[len(publisher.validated)-1]
	if len(last) != 1 || string(last["generatedFiles"]) != files {
		t.Fatalf("validated fields = %#v", last)
	}
	publication, err := FinalOutputPublicationFor(f.run, *final, f.stored("ok"), publisher)
	if err != nil || publication == nil || publication.StepID != f.step.ID || publication.ToolCallID != "ok" ||
		string(publication.Fields["generatedFiles"]) != files || len(publication.Fields) != 1 {
		t.Fatalf("publication = %#v, %v", publication, err)
	}
	final = requireFinal(t, settle("none"))
	if publication, err := FinalOutputPublicationFor(f.run, *final, f.stored("none"), publisher); err != nil || publication != nil {
		t.Fatalf("publication without fields = %#v, %v", publication, err)
	}
	// The tool offered to the model gains the host properties, never required.
	tools := WithFinalOutputProperties(builtinControlModelStepTools(agentrun.ControlAuthority{}), f.run, publisher)
	properties, required := schemaProperties(t, tools[0].InputSchema)
	if tools[0].Name != string(ControlToolFinalAnswer) || properties["generatedFiles"] == nil ||
		properties["answer"].(map[string]interface{})["type"] != "string" || !reflect.DeepEqual(required, []string{"answer"}) {
		t.Fatalf("tool = %#v", tools[0])
	}
	if base := builtinControlModelStepTools(agentrun.ControlAuthority{}); base[0].InputSchema["properties"].(map[string]interface{})["generatedFiles"] != nil {
		t.Fatal("WithFinalOutputProperties changed the base tool")
	}
	// Extension fields without a publisher are dropped as unknown.
	g := newFinalFixture(t, finalCall("final", `{"answer":"x","generatedFiles":[]}`))
	if dropped := requireFinal(t, g.settleFinal("final", agentrun.ControlAuthority{})); dropped.Output["generatedFiles"] != nil {
		t.Fatalf("an unknown field reached the output: %#v", dropped.Output)
	}
}

func TestBuiltinFinalAnswerChecksRunCompletionBeforeTaskReview(t *testing.T) {
	var names []string
	for _, check := range builtinFinalAnswerChecks() {
		names = append(names, check.Name)
	}
	if !reflect.DeepEqual(names, []string{"completion", "task_completion_review"}) {
		t.Fatalf("checks = %v", names)
	}
}

func TestFinalAnswerChecksRunInOrderAndMayFail(t *testing.T) {
	var order []string
	completion := finalAnswerCheck{Name: "completion", Check: func(ctx context.Context, x *ControlExecutor, cc agentrun.ControlContext, d *finalAnswerDraft) (*finalAnswerRejection, error) {
		order = append(order, "completion")
		d.Output["checked"] = true
		return nil, nil
	}}
	review := finalAnswerCheck{Name: "review", Check: func(ctx context.Context, x *ControlExecutor, cc agentrun.ControlContext, d *finalAnswerDraft) (*finalAnswerRejection, error) {
		order = append(order, "review")
		prior, err := x.priorFinalErrorCodes(ctx, cc)
		if err != nil {
			return nil, err
		}
		if prior["task_completion_revise"] > 0 {
			return &finalAnswerRejection{Code: "task_completion_rejected", Message: "Still not done.",
				Failure: &agentrun.LoopFailure{Kind: conversation.RunFailureKindTaskCompletion, Code: "task_completion_rejected", Message: "The task result was rejected."}}, nil
		}
		return &finalAnswerRejection{Code: "task_completion_revise", Message: "The task result needs more work: add the totals."}, nil
	}}
	f := newFinalFixture(t, finalCall("first", `{"answer":"x"}`), finalCall("second", `{"answer":"x"}`))
	f.exec.finalChecks = []finalAnswerCheck{completion, review}
	requireRejected(t, f, f.settleFinal("first", agentrun.ControlAuthority{}), "first", "task_completion_revise")
	result := f.settleFinal("second", agentrun.ControlAuthority{})
	if result.Kind != ControlFailed || result.Failure.Code != "task_completion_rejected" || result.Entry.State != ToolEntryError {
		t.Fatalf("second = %#v", result)
	}
	if !reflect.DeepEqual(order, []string{"completion", "review", "completion", "review"}) {
		t.Fatalf("order = %v", order)
	}
}

func TestFinalAnswerTakesOneSkillRefSentAsAString(t *testing.T) {
	call, err := decodeFinalAnswerCall(json.RawMessage(`{"answer":"Done.","appliedSkillRefs":"skill:meal"}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(call.Args.AppliedSkillRefs) != 1 || call.Args.AppliedSkillRefs[0] != "skill:meal" {
		t.Fatalf("appliedSkillRefs = %#v", call.Args.AppliedSkillRefs)
	}
	call, err = decodeFinalAnswerCall(json.RawMessage(`{"answer":"Done.","appliedSkillRefs":""}`))
	if err != nil || len(call.Args.AppliedSkillRefs) != 0 {
		t.Fatalf("empty string: %#v %v", call, err)
	}
	if _, err := decodeFinalAnswerCall(json.RawMessage(`{"answer":"Done.","appliedSkillRefs":7}`)); err == nil {
		t.Fatal("a number must still be refused")
	}
}

// addRunActionCall stores a finished action call of the fixture's Run.
func (f *controlFixture) addRunActionCall(id string, status ActionCallStatus, sideEffect skill.SideEffect) {
	f.t.Helper()
	f.addActionCallFor(f.run.ID, id, status, sideEffect)
}

// addActionCallFor stores a finished action call of the given Run.
func (f *controlFixture) addActionCallFor(runID, id string, status ActionCallStatus, sideEffect skill.SideEffect) {
	f.t.Helper()
	f.putAction(&ActionCall{
		ID: id, Scope: f.run.Scope, RunID: runID, SkillID: "mail", Action: "send", Status: status,
		SideEffect: sideEffect, Origin: ActionOriginLoop, CreatedAt: f.now, UpdatedAt: f.now,
	})
}

func TestFinalAnswerRoutineOccurrenceSilent(t *testing.T) {
	f := newFinalFixture(t,
		finalCall("text", `{"answer":"Nothing new","silent":true}`),
		finalCall("report", `{"answer":"","report":"x","silent":true}`),
		finalCall("files", `{"answer":"","files":["out.txt"],"silent":true}`),
		finalCall("quiet", `{"answer":"","silent":true}`),
	)
	routine := agentrun.ControlAuthority{RoutineOccurrence: true}
	properties, required := schemaProperties(t, finalAnswerSchema(routine))
	silent, ok := properties["silent"].(map[string]interface{})
	if !ok || !strings.Contains(silent["description"].(string), "nothing is posted") {
		t.Fatalf("silent property = %#v", properties["silent"])
	}
	if !reflect.DeepEqual(required, []string{"answer"}) {
		t.Fatalf("required = %v, silent must stay optional for routines", required)
	}
	requireRejected(t, f, f.settleFinal("text", routine), "text", finalSilentInvalidCode)
	requireRejected(t, f, f.settleFinal("report", routine), "report", finalSilentInvalidCode)
	requireRejected(t, f, f.settleFinal("files", routine), "files", finalSilentInvalidCode)
	// Reads and failed writes do not count as a change.
	f.addRunActionCall("read", ActionCallStatusSucceeded, skill.SideEffectRead)
	f.addRunActionCall("none", ActionCallStatusSucceeded, skill.SideEffectNone)
	f.addRunActionCall("failed-write", ActionCallStatusFailed, skill.SideEffectWrite)
	final := requireFinal(t, f.settleFinal("quiet", routine))
	if final.Answer != "" || !final.Silent() || !reflect.DeepEqual(final.Output, map[string]interface{}{"silent": true}) {
		t.Fatalf("final = %#v", final)
	}
}

func TestRoutineOccurrenceSilentRefusedAfterWrite(t *testing.T) {
	for _, status := range []ActionCallStatus{ActionCallStatusSucceeded, ActionCallStatusCompensated} {
		t.Run(string(status), func(t *testing.T) {
			f := newFinalFixture(t, finalCall("quiet", `{"answer":"","silent":true}`))
			f.addRunActionCall("sent", status, skill.SideEffectExternal)
			value := requireRejected(t, f, f.settleFinal("quiet", agentrun.ControlAuthority{RoutineOccurrence: true}), "quiet", finalSilentAfterChangeCode)
			if value["message"] != "This run changed something, so report what it did." {
				t.Fatalf("message = %v", value["message"])
			}
		})
	}
}

func TestSilentStillRejectedForChatRuns(t *testing.T) {
	f := newFinalFixture(t, finalCall("chat", `{"answer":"","silent":true}`))
	// A chat reply in a routine's conversation is a conversation Run: it is
	// not a routine occurrence and is not offered silent.
	run := cloneAgentRun(f.run)
	run.Kind = RunKindConversation
	run.Context = map[string]interface{}{scheduledTaskContextKey: true}
	authority, err := ResolveControlAuthority(f.ctx, ControlAuthorityInput{Run: run, DeploymentID: "agent-1", Deterministic: true})
	if err != nil || authority.RoutineOccurrence {
		t.Fatalf("authority = %#v, %v", authority, err)
	}
	if finalFieldOffered("silent", authority) {
		t.Fatal("silent offered to a chat Run")
	}
	value := requireRejected(t, f, f.settleFinal("chat", authority), "chat", controlArgumentsInvalidCode)
	if !strings.Contains(value["message"].(string), `"silent"`) {
		t.Fatalf("message = %v", value["message"])
	}
	occurrence := cloneAgentRun(f.run)
	occurrence.Context = map[string]interface{}{scheduledTaskContextKey: true}
	authority, err = ResolveControlAuthority(f.ctx, ControlAuthorityInput{Run: occurrence, DeploymentID: "agent-1", Deterministic: true})
	if err != nil || !authority.RoutineOccurrence || !finalFieldOffered("silent", authority) {
		t.Fatalf("occurrence authority = %#v, %v", authority, err)
	}
}

// A write by a delegated or forked child under the occurrence counts too.
func TestRoutineOccurrenceSilentRefusedAfterChildWrite(t *testing.T) {
	f := newFinalFixture(t, finalCall("quiet", `{"answer":"","silent":true}`))
	rootID := f.run.RootRunID
	if rootID == "" {
		rootID = f.run.ID
	}
	child := cloneAgentRun(f.run)
	child.ID, child.ParentRunID, child.RootRunID = "occurrence-child", f.run.ID, rootID
	if err := f.store.CreateAgentRun(f.ctx, child); err != nil {
		t.Fatalf("create child run: %v", err)
	}
	f.addActionCallFor(child.ID, "child-sent", ActionCallStatusSucceeded, skill.SideEffectExternal)
	requireRejected(t, f, f.settleFinal("quiet", agentrun.ControlAuthority{RoutineOccurrence: true}), "quiet", finalSilentAfterChangeCode)
}

func TestFinalAnswerSchemaStableAcrossSkillRefs(t *testing.T) {
	one := agentrun.ControlAuthority{AgentID: "agent-1", SkillPromptRefs: []string{"skill-notion@1"}}
	two := agentrun.ControlAuthority{AgentID: "agent-1", SkillPromptRefs: []string{"skill-notion@1", "skill-slack@1"}}
	if mustJSON(t, finalAnswerSchema(one)) != mustJSON(t, finalAnswerSchema(two)) || finalAnswerDescription(one) != finalAnswerDescription(two) {
		t.Fatal("the final answer tool changes with the Skill refs")
	}
	if selections, rejection := finalSkillSelections([]string{"skill-gone@1"}, two.SkillPromptRefs); rejection != nil || len(selections) != 2 ||
		selections[0].Disposition != HostedSkillNotApplied || selections[1].Disposition != HostedSkillNotApplied {
		t.Fatalf("an unknown ref must be left out: %#v %v", selections, rejection)
	}
	// Delegate, fork and operation tools carry no target list either.
	targets := agentrun.ControlAuthority{AgentID: "agent-1", ForkAllowed: true,
		DelegateTargets: []agentrun.HostedAgentTarget{{ID: "agent-2", DisplayName: "Ada"}}, ForkTargets: []agentrun.HostedAgentTarget{{ID: "agent-2"}},
		Operations: []agentrun.HostedRunbookOperation{{Entrypoint: "report", Name: "Report"}}}
	more := targets
	more.DelegateTargets = append(more.DelegateTargets, agentrun.HostedAgentTarget{ID: "agent-3", DisplayName: "Bo"})
	more.ForkTargets = append(more.ForkTargets, agentrun.HostedAgentTarget{ID: "agent-3"})
	more.Operations = append(more.Operations, agentrun.HostedRunbookOperation{Entrypoint: "audit", Name: "Audit"})
	for _, name := range []ControlToolName{ControlToolDelegate, ControlToolFork, ControlToolRunOperation} {
		handler := newControlRegistry(builtinControlTools()...)[name]
		if mustJSON(t, handler.Spec(targets)) != mustJSON(t, handler.Spec(more)) {
			t.Fatalf("%s changes with its targets", name)
		}
	}
	items := modelsteptest.CapabilityNoteLines(modelstep.ControlTargetItems(more))
	for _, want := range []string{"- agent-2: Ada", "- agent-3: Bo", "- audit — Audit. Input schema:"} {
		if !strings.Contains(items, want) {
			t.Fatalf("targets missing %q from the note: %s", want, items)
		}
	}
}
