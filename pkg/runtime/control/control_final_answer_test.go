package control

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep/modelsteptest"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// newFinalFixture is a control fixture for plain replies: no tool handlers.
func newFinalFixture(t *testing.T, calls ...agentrun.LoopToolCall) *controlFixture {
	t.Helper()
	f := newControlFixture(t, calls)
	f.exec.handlers = NewRegistry(BuiltinTools()...)
	return f
}

// plainContext is the context of a plain-text final on its own (running) step.
func (f *controlFixture) plainContext(stepID string, sequence int64, authority agentrun.ControlAuthority) agentrun.ControlContext {
	return agentrun.ControlContext{
		Run: f.run, WorkerID: controlTestWorker, Session: f.session, Authority: authority,
		Step:      &models.AgentTurn{ID: stepID, RunID: f.run.ID, Sequence: sequence, Status: models.AgentTurnStatusRunning},
		Assistant: &models.SessionEntry{Type: models.SessionEntryAssistant, Assistant: &models.AssistantEntry{StopReason: models.ModelStopEnd}},
		Binding:   &agentrun.TurnRunnerBinding{Authority: authority},
	}
}

func (f *controlFixture) plain(stepID string, sequence int64, authority agentrun.ControlAuthority, text string) Result {
	f.t.Helper()
	result, err := f.exec.PlainTextFinal(f.ctx, f.plainContext(stepID, sequence, authority), text)
	if err != nil {
		f.t.Fatalf("plain final %s: %v", stepID, err)
	}
	return result
}

func requireFinal(t *testing.T, result Result) *agentrun.FinalAnswer {
	t.Helper()
	if result.Kind != Final || result.Final == nil {
		t.Fatalf("result = %#v, want a final answer", result)
	}
	if err := result.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	return result.Final
}

func requireRejectionNote(t *testing.T, result Result, stepID, code string) *models.SessionEntry {
	t.Helper()
	note := result.Entry
	if result.Kind != Continue && result.Kind != Failed || note == nil || note.Type != models.SessionEntryNote ||
		note.Note == nil || note.Note.Kind != models.NoteFinalRejection || note.SourceKey != models.FinalRejectionEntryKey(stepID) {
		t.Fatalf("result = %#v, want a final_rejection note", result)
	}
	var data finalRejectionNoteData
	if err := json.Unmarshal(note.Note.Data, &data); err != nil || data.ErrorCode != code {
		t.Fatalf("note data = %s, %v", note.Note.Data, err)
	}
	return note
}

// The final answer is the plain reply: no control tool takes an answer.
func TestNoFinalAnswerTool(t *testing.T) {
	for _, authority := range []agentrun.ControlAuthority{{}, {VisualAllowed: true, CanAskUser: true, CanReact: true}, {FinalMode: agentrun.FinalModeSpoken}} {
		for _, spec := range NewRegistry(BuiltinTools()...).Specs(authority) {
			if spec.Name == "turn_final_answer" || spec.InputSchema["properties"].(map[string]interface{})["answer"] != nil {
				t.Fatalf("%s takes an answer", spec.Name)
			}
		}
	}
	if IsControlTool("turn_final_answer") {
		t.Fatal("turn_final_answer is still a control tool")
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
	cc.Assistant.Assistant.ToolCalls = []models.ModelToolCall{{ID: "x", Name: "react"}}
	if _, err := f.exec.PlainTextFinal(f.ctx, cc, "text"); err == nil {
		t.Fatal("plain final accepted with tool calls")
	}
	// Lease rule.
	cc = f.plainContext("step-3", 3, agentrun.ControlAuthority{})
	cc.WorkerID = "other"
	if _, err := f.exec.PlainTextFinal(f.ctx, cc, "text"); !errors.Is(err, models.ErrLeaseLost) {
		t.Fatalf("foreign worker = %v", err)
	}
}

// A reply that looks like the old tool's arguments is just text.
func TestPlainTextJSONStaysText(t *testing.T) {
	f := newFinalFixture(t)
	text := `{"answer":"Posted.","broadcastToChannel":true}`
	final := requireFinal(t, f.plain("step-2", 2, agentrun.ControlAuthority{}, text))
	if final.Answer != text || len(final.Output) != 1 || final.Output["summary"] != text {
		t.Fatalf("final = %#v", final)
	}
}

func TestSpokenEmptyTextIsSilent(t *testing.T) {
	f := newFinalFixture(t)
	final := requireFinal(t, f.plain("step-2", 2, agentrun.ControlAuthority{FinalMode: agentrun.FinalModeSpoken}, "  \n"))
	if !final.PlainText || !final.Silent() || final.Answer != "" {
		t.Fatalf("final = %#v", final)
	}
}

func TestEmptyPlainTextRejectedThenFails(t *testing.T) {
	f := newFinalFixture(t)
	for i, step := range []string{"step-2", "step-3"} {
		result := f.plain(step, int64(i+2), agentrun.ControlAuthority{}, "")
		if result.Kind != Continue {
			t.Fatalf("attempt %d = %#v", i+1, result)
		}
		note := requireRejectionNote(t, result, step, finalEmptyReplyCode)
		if !strings.HasPrefix(note.Note.Text, "An empty reply is not an answer.") {
			t.Fatalf("note = %q", note.Note.Text)
		}
	}
	// Replaying the same step's rejection keeps one note.
	again := f.plain("step-3", 3, agentrun.ControlAuthority{}, "")
	if again.Kind != Continue {
		t.Fatalf("replay = %#v", again)
	}
	// The third distinct step fails the Run.
	result := f.plain("step-4", 4, agentrun.ControlAuthority{}, "")
	if result.Kind != Failed || result.Failure == nil || result.Failure.Code != finalEmptyReplyCode {
		t.Fatalf("third empty reply = %#v", result)
	}
	if err := result.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

func (f *controlFixture) completeEntry(callID string) {
	f.t.Helper()
	entry := f.entries[callID]
	_, err := f.store.TransitionToolEntry(f.ctx, models.ToolEntryTransitionRequest{
		Scope: f.run.Scope, EntryID: entry.ID, RunID: f.run.ID,
		Lease: models.AgentRunLeaseGuard{WorkerID: controlTestWorker, Now: f.now},
		From:  entry.State, To: models.ToolEntryCompleted,
		Result: models.ControlJSONResult(models.ToolResultSucceeded, nil), Now: f.now,
	})
	if err != nil {
		f.t.Fatalf("complete %s: %v", callID, err)
	}
}

func TestEmptyAnswerAfterReactionIsReactionsOnly(t *testing.T) {
	f := newFinalFixture(t, agentrun.LoopToolCall{ID: "react", Name: string(models.ControlToolReact), Arguments: json.RawMessage(`{}`)})
	if result := f.plain("step-2", 2, agentrun.ControlAuthority{}, ""); result.Kind != Continue {
		t.Fatalf("empty plain text before a reaction = %#v", result)
	}
	f.completeEntry("react")
	final := requireFinal(t, f.plain("step-3", 3, agentrun.ControlAuthority{}, ""))
	if !final.ReactionsOnly() || !final.PlainText || final.Answer != "" {
		t.Fatalf("plain final = %#v", final)
	}
}

func TestDecisionRunPlainTextGetsNoteThenFails(t *testing.T) {
	f := newFinalFixture(t)
	decision := agentrun.ControlAuthority{FinalMode: agentrun.FinalModeDecision}
	for i, step := range []string{"step-2", "step-3"} {
		result := f.plain(step, int64(i+2), decision, "I accept the request.")
		if result.Kind != Continue {
			t.Fatalf("attempt %d = %#v", i+1, result)
		}
		note := requireRejectionNote(t, result, step, finalToolRequiredCode)
		if !strings.HasPrefix(note.Note.Text, "Finish with agent_request_decision (") {
			t.Fatalf("note = %q", note.Note.Text)
		}
	}
	result := f.plain("step-4", 4, decision, `{"answer":"accept"}`)
	if result.Kind != Failed || result.Failure == nil || result.Failure.Code != finalToolRequiredCode ||
		result.Failure.Kind != conversation.RunFailureKindExecution {
		t.Fatalf("third plain reply = %#v", result)
	}
	requireRejectionNote(t, result, "step-4", finalToolRequiredCode)
}

// governedEntry turns the fixture's pending call into a finished governed
// loop call of the Run.
func (f *controlFixture) governedEntry(callID, skillID, action string, output map[string]interface{}) {
	f.t.Helper()
	actionID := "action-" + callID
	f.putAction(&models.ActionCall{
		ID: actionID, Scope: f.run.Scope, RunID: f.run.ID, SkillID: skillID, Action: action,
		Status: models.ActionCallStatusSucceeded, Origin: models.ActionOriginLoop, Output: output, CreatedAt: f.now, UpdatedAt: f.now,
	})
	entry := f.entries[callID]
	_, err := f.store.TransitionToolEntry(f.ctx, models.ToolEntryTransitionRequest{
		Scope: f.run.Scope, EntryID: entry.ID, RunID: f.run.ID,
		Lease: models.AgentRunLeaseGuard{WorkerID: controlTestWorker, Now: f.now},
		From:  models.ToolEntryPending, To: models.ToolEntryCompleted, ActionCallID: actionID,
		Result: models.ControlJSONResult(models.ToolResultSucceeded, output), Now: f.now,
	})
	if err != nil {
		f.t.Fatalf("finish governed %s: %v", callID, err)
	}
}

func TestFinalAnswerParksForOTPKeepsRunAlive(t *testing.T) {
	f := newFinalFixture(t, agentrun.LoopToolCall{ID: "browser", Name: "live_browser_submit", Arguments: json.RawMessage(`{}`)})
	f.governedEntry("browser", liveBrowserSkillID, "live-browser-submit", map[string]interface{}{"status": "otp_required"})
	final := requireFinal(t, f.plain("step-2", 2, agentrun.ControlAuthority{CanAskUser: true}, "  What is the code the bank just sent you? "))
	park := final.Park
	if park == nil || !park.Deliver || park.Park.Status != models.AgentRunStatusWaitingForEvent || park.Park.Wake == nil ||
		park.Park.Wake.Type != "user_message" || park.Park.Summary != "What is the code the bank just sent you?" {
		t.Fatalf("park = %#v", park)
	}
	if err := park.Park.Validate(); err != nil {
		t.Fatalf("park validate: %v", err)
	}
	// The answer is delivered; the Run itself is untouched (still running).
	run, err := f.store.GetAgentRun(f.ctx, f.run.Scope, f.run.ID)
	if err != nil || run.Status != models.AgentRunStatusRunning || run.LeaseOwner != controlTestWorker {
		t.Fatalf("run = %#v, %v", run, err)
	}
	// A Run that cannot ask its conversation completes.
	final = requireFinal(t, f.plain("step-3", 3, agentrun.ControlAuthority{}, "What is the code?"))
	if final.Park != nil {
		t.Fatalf("park without ask = %#v", final.Park)
	}
}

func TestTaskFinalParksOnPendingSetup(t *testing.T) {
	f := newFinalFixture(t, agentrun.LoopToolCall{ID: "setup", Name: "openseal_skills_request_setup", Arguments: json.RawMessage(`{}`)})
	f.run.Kind = models.RunKindAgentWork
	f.run.Context = map[string]interface{}{models.ConversationTaskContextKey: "task-1"}
	f.governedEntry("setup", models.SkillManagementSkillID, agentrun.SkillActionRequestSetup, map[string]interface{}{
		"setupRequest": map[string]interface{}{"id": "setup-1", "status": "pending", "runId": f.run.ID, "actionCallId": "action-setup"},
	})
	final := requireFinal(t, f.plain("step-2", 2, agentrun.ControlAuthority{TaskWork: true, CanAskUser: true}, "I asked you to connect the account."))
	park := final.Park
	if park == nil || park.Deliver || park.Park.Status != models.AgentRunStatusWaitingForEvent || park.Park.Wake == nil ||
		park.Park.Wake.Type != conversationrun.ConversationTaskSkillSetupWakeType || park.Park.Wake.Reference != "setup-1" ||
		park.Park.Summary != skillSetupWaitSummary {
		t.Fatalf("park = %#v", park)
	}
	// A kernel resolution receipt clears it.
	f.run.PendingInterventions = []models.AgentRunIntervention{{ID: "i1", SkillSetupResolution: &models.SkillSetupResolutionReceipt{
		Scope: f.run.Scope, RunID: f.run.ID, RequestID: "setup-1", ActionCallID: "action-setup", Status: "resolved",
	}}}
	final = requireFinal(t, f.plain("step-3", 3, agentrun.ControlAuthority{TaskWork: true}, "Connected; here is the report."))
	if final.Park != nil {
		t.Fatalf("park after resolution = %#v", final.Park)
	}
}

func (f *controlFixture) addRunActionCall(id string, status models.ActionCallStatus, sideEffect skill.SideEffect) {
	f.t.Helper()
	f.addActionCallFor(f.run.ID, id, status, sideEffect)
}

// addActionCallFor stores a finished action call of the given Run.
func (f *controlFixture) addActionCallFor(runID, id string, status models.ActionCallStatus, sideEffect skill.SideEffect) {
	f.t.Helper()
	f.putAction(&models.ActionCall{
		ID: id, Scope: f.run.Scope, RunID: runID, SkillID: "mail", Action: "send", Status: status,
		SideEffect: sideEffect, Origin: models.ActionOriginLoop, CreatedAt: f.now, UpdatedAt: f.now,
	})
}

// An empty reply of a routine occurrence that changed nothing is silent:
// nothing is posted.
func TestRoutineOccurrenceEmptyReplyIsSilent(t *testing.T) {
	f := newFinalFixture(t)
	// Reads and failed writes do not count as a change.
	f.addRunActionCall("read", models.ActionCallStatusSucceeded, skill.SideEffectRead)
	f.addRunActionCall("none", models.ActionCallStatusSucceeded, skill.SideEffectNone)
	f.addRunActionCall("failed-write", models.ActionCallStatusFailed, skill.SideEffectWrite)
	final := requireFinal(t, f.plain("step-2", 2, agentrun.ControlAuthority{RoutineOccurrence: true}, " "))
	if final.Answer != "" || !final.Silent() || !reflect.DeepEqual(final.Output, map[string]interface{}{"silent": true}) {
		t.Fatalf("final = %#v", final)
	}
}

func TestRoutineOccurrenceEmptyReplyRefusedAfterWrite(t *testing.T) {
	for _, status := range []models.ActionCallStatus{models.ActionCallStatusSucceeded, models.ActionCallStatusCompensated} {
		t.Run(string(status), func(t *testing.T) {
			f := newFinalFixture(t)
			f.addRunActionCall("sent", status, skill.SideEffectExternal)
			note := requireRejectionNote(t, f.plain("step-2", 2, agentrun.ControlAuthority{RoutineOccurrence: true}, ""), "step-2", finalSilentAfterChangeCode)
			if note.Note.Text != "This run changed something, so report what it did." {
				t.Fatalf("note = %q", note.Note.Text)
			}
		})
	}
}

// A chat reply in a routine's conversation is a conversation Run, not a
// routine occurrence: an empty reply is refused there.
func TestEmptyReplyStillRejectedForChatRuns(t *testing.T) {
	f := newFinalFixture(t)
	run := models.CloneAgentRun(f.run)
	run.Kind = models.RunKindConversation
	run.Context = map[string]interface{}{models.ScheduledTaskContextKey: true}
	authority, err := ResolveControlAuthority(f.ctx, AuthorityInput{Run: run, DeploymentID: "agent-1", Deterministic: true})
	if err != nil || authority.RoutineOccurrence {
		t.Fatalf("authority = %#v, %v", authority, err)
	}
	requireRejectionNote(t, f.plain("step-2", 2, authority, ""), "step-2", finalEmptyReplyCode)
	occurrence := models.CloneAgentRun(f.run)
	occurrence.Context = map[string]interface{}{models.ScheduledTaskContextKey: true}
	authority, err = ResolveControlAuthority(f.ctx, AuthorityInput{Run: occurrence, DeploymentID: "agent-1", Deterministic: true})
	if err != nil || !authority.RoutineOccurrence {
		t.Fatalf("occurrence authority = %#v, %v", authority, err)
	}
}

// A write by a delegated or forked child under the occurrence counts too.
func TestRoutineOccurrenceEmptyReplyRefusedAfterChildWrite(t *testing.T) {
	f := newFinalFixture(t)
	rootID := f.run.RootRunID
	if rootID == "" {
		rootID = f.run.ID
	}
	child := models.CloneAgentRun(f.run)
	child.ID, child.ParentRunID, child.RootRunID = "occurrence-child", f.run.ID, rootID
	if err := f.store.CreateAgentRun(f.ctx, child); err != nil {
		t.Fatalf("create child run: %v", err)
	}
	f.addActionCallFor(child.ID, "child-sent", models.ActionCallStatusSucceeded, skill.SideEffectExternal)
	requireRejectionNote(t, f.plain("step-2", 2, agentrun.ControlAuthority{RoutineOccurrence: true}, ""), "step-2", finalSilentAfterChangeCode)
}

func TestControlToolsStableAcrossTargets(t *testing.T) {
	// Delegate, fork and operation tools carry no target list.
	targets := agentrun.ControlAuthority{AgentID: "agent-1", ForkAllowed: true,
		DelegateTargets: []agentrun.HostedAgentTarget{{ID: "agent-2", DisplayName: "Ada"}}, ForkTargets: []agentrun.HostedAgentTarget{{ID: "agent-2"}},
		Operations: []agentrun.HostedRunbookOperation{{Entrypoint: "report", Name: "Report"}}}
	more := targets
	more.DelegateTargets = append(more.DelegateTargets, agentrun.HostedAgentTarget{ID: "agent-3", DisplayName: "Bo"})
	more.ForkTargets = append(more.ForkTargets, agentrun.HostedAgentTarget{ID: "agent-3"})
	more.Operations = append(more.Operations, agentrun.HostedRunbookOperation{Entrypoint: "audit", Name: "Audit"})
	for _, name := range []models.ControlToolName{models.ControlToolDelegate, models.ControlToolStartSubagents, models.ControlToolRunOperation} {
		handler := NewRegistry(BuiltinTools()...)[name]
		if controlJSON(t, handler.Spec(targets)) != controlJSON(t, handler.Spec(more)) {
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
