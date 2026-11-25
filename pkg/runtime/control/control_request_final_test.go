package control

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/collaboration"
	"github.com/axiom-studio/openseal/pkg/runtime/collaboration/collaborationtest"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

// newRequestFinalFixture claims run (a queued decision Run in store) and gives
// it a session whose one finished step made call. The executor runs the real
// decision handler.
func newRequestFinalFixture(t *testing.T, store *sqlite.Store, run *models.AgentRun, call agentrun.LoopToolCall) *controlFixture {
	t.Helper()
	ctx := context.Background()
	// After the reconciler created the Run (claims ignore Runs from the future).
	now := time.Now().UTC().Add(time.Second).Truncate(time.Millisecond)
	claimed, err := store.ClaimNextAgentRun(ctx, models.AgentRunClaim{
		Scope: run.Scope, Kind: run.Kind, AssignedAgentID: run.AssignedAgentID, WorkerID: controlTestWorker,
		Now: now, LeaseDuration: time.Hour, AgingInterval: time.Minute,
	})
	if err != nil || claimed == nil || claimed.ID != run.ID {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	lease := models.AgentRunLeaseGuard{WorkerID: controlTestWorker, Now: now}
	opened, err := store.CreateRunAgentSession(ctx, models.CreateRunAgentSessionRequest{
		Scope: run.Scope, OwnerRunID: run.ID, AgentID: run.AssignedAgentID, Lease: lease, Now: now,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	stepID := "step-1"
	assistant := &models.SessionEntry{
		ID: models.SessionEntryID(opened.Session.ID, models.AssistantEntryKey(stepID)), RunID: run.ID, StepID: stepID, Step: 1,
		Type: models.SessionEntryAssistant, Assistant: &models.AssistantEntry{
			Provider: models.ModelStepProviderAnthropic, Model: "claude-x", StopReason: models.ModelStopToolCalls,
			ToolCalls: []models.ModelToolCall{{ID: call.ID, Name: call.Name, Arguments: call.Arguments}},
		},
	}
	tool := &models.SessionEntry{
		ID: models.SessionEntryID(opened.Session.ID, models.ToolEntryKey(stepID, call.ID)), RunID: run.ID, StepID: stepID, Step: 1,
		Type: models.SessionEntryTool, State: models.ToolEntryPending, ToolCallID: call.ID,
		Tool: &models.ToolEntry{Name: call.Name, Arguments: call.Arguments, SideEffect: models.ToolSideEffectWrite},
	}
	appended, err := store.AppendAgentSessionEntries(ctx, models.AppendAgentSessionEntriesRequest{
		Scope: run.Scope, SessionID: opened.Session.ID, RunID: run.ID, Lease: &lease,
		Entries: []*models.SessionEntry{assistant, tool}, Now: now,
	})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	current, err := store.GetAgentRun(ctx, run.Scope, run.ID)
	if err != nil {
		t.Fatalf("reload run: %v", err)
	}
	current.LastAppliedTurn = 1
	f := &controlFixture{
		t: t, ctx: ctx, store: store, run: current, session: opened.Session, now: now,
		step:    &models.AgentTurn{ID: stepID, RunID: run.ID, Sequence: 1, Status: models.AgentTurnStatusCompleted},
		entries: map[string]*models.SessionEntry{},
	}
	for _, entry := range appended.Entries {
		if entry.Type == models.SessionEntryTool {
			f.entries[entry.ToolCallID] = entry
		}
	}
	registry := Registry{}
	registry.register(models.ControlToolRequestDecision, requestDecisionHandler{})
	f.exec = &Executor{Portfolio: store, Sessions: store, Now: func() time.Time { return now }, handlers: registry}
	return f
}

func requestDecisionCall(id, args string) agentrun.LoopToolCall {
	return agentrun.LoopToolCall{ID: id, Name: string(models.ControlToolRequestDecision), Arguments: json.RawMessage(args)}
}

// completeWithFinal ends the claimed Run as the loop's final chain does for a
// non-conversation Run: completed with FinalAnswer.Output.
func (f *controlFixture) completeWithFinal(final *agentrun.FinalAnswer) *models.AgentRun {
	f.t.Helper()
	completed, _, err := activity.NewRunActivityService(f.store, f.store).TransitionRun(f.ctx, f.run.Scope, f.run.ID, activity.RunTransitionRequest{
		ExpectedRevision: f.run.Revision, Status: models.AgentRunStatusCompleted, Output: final.Output,
		Summary: "Run completed with its final answer", LeaseOwner: controlTestWorker,
	})
	if err != nil {
		f.t.Fatalf("complete run: %v", err)
	}
	return completed
}

func onlyRunWithSource(t *testing.T, store *sqlite.Store, scope models.Scope, parentID string, source models.RunSource) *models.AgentRun {
	t.Helper()
	runs, err := store.ListAgentRuns(t.Context(), models.AgentRunFilter{Scope: scope, ParentRunID: parentID, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	var found *models.AgentRun
	for _, run := range runs {
		if run.Source != source {
			continue
		}
		if found != nil {
			t.Fatalf("two %s runs: %s, %s", source, found.ID, run.ID)
		}
		found = run
	}
	if found == nil {
		t.Fatalf("no %s run under %s: %#v", source, parentID, runs)
	}
	return found
}

func reconcileDecisionRun(t *testing.T) (*collaborationtest.TeamStore, *collaboration.AgentRequestInboxReconciler, *models.AgentRequest, *models.AgentRun) {
	t.Helper()
	store := &collaborationtest.TeamStore{Store: kerneltest.NewStore(t)}
	source, request := collaborationtest.CreateInboxRequest(t, store, models.AgentRequestAcceptanceRecipientReview, models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "recipient"})
	reconciler, err := collaboration.NewAgentRequestInboxReconciler(store)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := reconciler.Reconcile(t.Context(), request.Scope, "recipient"); err != nil || result.DecisionRunsCreated != 1 {
		t.Fatalf("reconcile = %#v, %v", result, err)
	}
	return store, reconciler, request, onlyRunWithSource(t, store.Store, request.Scope, source.ID, models.RunSourceRequestDecision)
}

func TestDecisionToolWritesOutputConsumedByResolveDecisionRun(t *testing.T) {
	store, reconciler, request, run := reconcileDecisionRun(t)
	f := newRequestFinalFixture(t, store.Store, run,
		requestDecisionCall("decide", `{"decision":"reject","message":" Outside my role. "}`))
	authority := agentrun.ControlAuthority{AgentID: "recipient", FinalMode: agentrun.FinalModeDecision}
	result, err := f.exec.Settle(f.ctx, f.context("decide", authority))
	if err != nil {
		t.Fatal(err)
	}
	final := requireFinal(t, result)
	want := map[string]interface{}{collaboration.AgentRequestDecisionOutputKey: map[string]interface{}{"decision": "reject", "message": "Outside my role."}}
	if !reflect.DeepEqual(final.Output, want) || final.Answer != "Outside my role." || final.PlainText ||
		final.ToolEntryID != f.entries["decide"].ID || final.ToolCallID != "decide" || final.StepID != f.step.ID {
		t.Fatalf("final = %#v", final)
	}
	// The entry stays open for the loop to mark delivered.
	if stored := f.stored("decide"); stored.State != models.ToolEntryPending || stored.Tool.Result != nil {
		t.Fatalf("entry = %#v", stored)
	}
	completed := f.completeWithFinal(final)
	resolved, err := reconciler.ResolveDecisionRun(t.Context(), completed)
	if err != nil || !resolved {
		t.Fatalf("resolve = %v, %v", resolved, err)
	}
	current, err := collaboration.NewService(store).GetAgentRequest(t.Context(), request.Scope, request.ID)
	if err != nil || current.Status != models.AgentRequestStatusRejected || current.Response != "Outside my role." {
		t.Fatalf("request = %#v, %v", current, err)
	}
}

func TestClarificationDecisionRequiresQuestion(t *testing.T) {
	authority := agentrun.ControlAuthority{AgentID: "recipient", FinalMode: agentrun.FinalModeDecision}
	for name, args := range map[string]string{
		"empty question":   `{"decision":"request_clarification","message":"  "}`,
		"unknown decision": `{"decision":"maybe","message":"unsure"}`,
		"empty reason":     `{"decision":"accept","message":""}`,
	} {
		t.Run(name, func(t *testing.T) {
			store, _, _, run := reconcileDecisionRun(t)
			f := newRequestFinalFixture(t, store.Store, run, requestDecisionCall("decide", args))
			result, err := f.exec.Settle(f.ctx, f.context("decide", authority))
			if err != nil {
				t.Fatal(err)
			}
			if result.Kind != Continue || result.Entry == nil || result.Entry.State != models.ToolEntryError {
				t.Fatalf("result = %#v", result)
			}
			stored := f.stored("decide")
			requireControlError(t, stored.Tool.Result, models.ToolResultFailed, requestDecisionInvalidCode)
			meta, err := DecodeControlEntryMeta(stored)
			if err != nil || meta == nil || meta.Kind != models.ControlToolRequestDecision || meta.ErrorCode != requestDecisionInvalidCode {
				t.Fatalf("meta = %#v, %v", meta, err)
			}
		})
	}
	// Unknown fields are refused by the strict decoder.
	store, _, _, run := reconcileDecisionRun(t)
	f := newRequestFinalFixture(t, store.Store, run,
		requestDecisionCall("decide", `{"decision":"accept","message":"ok","budget":3}`))
	if _, err := f.exec.Settle(f.ctx, f.context("decide", authority)); err != nil {
		t.Fatal(err)
	}
	requireControlError(t, f.stored("decide").Tool.Result, models.ToolResultFailed, ArgumentsInvalidCode)
	// One concrete question is accepted.
	store, _, _, run = reconcileDecisionRun(t)
	g := newRequestFinalFixture(t, store.Store, run,
		requestDecisionCall("decide", `{"decision":"request_clarification","message":"Which launch region?"}`))
	result, err := g.exec.Settle(g.ctx, g.context("decide", authority))
	if err != nil {
		t.Fatal(err)
	}
	decision, message, err := collaboration.ParseAgentRequestDecisionOutput(requireFinal(t, result).Output)
	if err != nil || decision != collaboration.AgentRequestDecisionRequestClarification || message != "Which launch region?" {
		t.Fatalf("decision = %q %q %v", decision, message, err)
	}
}

func TestDecisionBudgetIsTwelveSteps(t *testing.T) {
	_, _, _, run := reconcileDecisionRun(t)
	want := &models.BudgetPolicy{
		MaxAttempts: 5, MaxTurns: 12, MaxInputTokens: 192000, MaxOutputTokens: 4096,
		MaxTotalTokens: 208384, MaxDurationMS: 180000,
	}
	if !reflect.DeepEqual(run.Budget, want) {
		t.Fatalf("decision budget = %#v", run.Budget)
	}
}

func TestRequestFinalToolSchemas(t *testing.T) {
	decision := builtinControlSpecs(agentrun.ControlAuthority{FinalMode: agentrun.FinalModeDecision})
	if len(decision) != 1 || decision[0].Name != models.ControlToolRequestDecision || decision[0].Phase != PhaseSettle {
		t.Fatalf("decision specs = %#v", decision)
	}
	properties, required := schemaProperties(t, decision[0].InputSchema)
	if !reflect.DeepEqual(required, []string{"decision", "message"}) || decision[0].InputSchema["additionalProperties"] != false {
		t.Fatalf("decision schema = %#v", decision[0].InputSchema)
	}
	enum := properties["decision"].(map[string]interface{})["enum"]
	if !reflect.DeepEqual(enum, []string{"accept", "reject", "request_clarification"}) {
		t.Fatalf("decision enum = %#v", enum)
	}
	// The plain-text note names the tool's required fields.
	if got := NewRegistry(BuiltinTools()...).finalToolRequiredFields(models.ControlToolRequestDecision, agentrun.ControlAuthority{FinalMode: agentrun.FinalModeDecision}); got != "decision, message" {
		t.Fatalf("required fields = %q", got)
	}
}

func schemaProperties(t *testing.T, schema map[string]interface{}) (map[string]interface{}, []string) {
	t.Helper()
	properties, _ := schema["properties"].(map[string]interface{})
	required, _ := schema["required"].([]string)
	return properties, required
}
