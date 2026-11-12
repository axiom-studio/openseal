package runtime

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

// newRequestFinalFixture claims run (a queued decision or review Run in store)
// and gives it a session whose one finished step made call. The executor runs
// the real decision and review handlers.
func newRequestFinalFixture(t *testing.T, store *sqlite.Store, run *AgentRun, call agentrun.LoopToolCall) *controlFixture {
	t.Helper()
	ctx := context.Background()
	// After the reconciler created the Run (claims ignore Runs from the future).
	now := time.Now().UTC().Add(time.Second).Truncate(time.Millisecond)
	claimed, err := store.ClaimNextAgentRun(ctx, AgentRunClaim{
		Scope: run.Scope, Kind: run.Kind, AssignedAgentID: run.AssignedAgentID, WorkerID: controlTestWorker,
		Now: now, LeaseDuration: time.Hour, AgingInterval: time.Minute,
	})
	if err != nil || claimed == nil || claimed.ID != run.ID {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	lease := AgentRunLeaseGuard{WorkerID: controlTestWorker, Now: now}
	opened, err := store.CreateRunAgentSession(ctx, CreateRunAgentSessionRequest{
		Scope: run.Scope, OwnerRunID: run.ID, AgentID: run.AssignedAgentID, Lease: lease, Now: now,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	stepID := "step-1"
	assistant := &SessionEntry{
		ID: SessionEntryID(opened.Session.ID, AssistantEntryKey(stepID)), RunID: run.ID, StepID: stepID, Step: 1,
		Type: SessionEntryAssistant, Assistant: &AssistantEntry{
			Provider: ModelStepProviderAnthropic, Model: "claude-x", StopReason: ModelStopToolCalls,
			ToolCalls: []ModelToolCall{{ID: call.ID, Name: call.Name, Arguments: call.Arguments}},
		},
	}
	tool := &SessionEntry{
		ID: SessionEntryID(opened.Session.ID, ToolEntryKey(stepID, call.ID)), RunID: run.ID, StepID: stepID, Step: 1,
		Type: SessionEntryTool, State: ToolEntryPending, ToolCallID: call.ID,
		Tool: &ToolEntry{Name: call.Name, Arguments: call.Arguments, SideEffect: ToolSideEffectWrite},
	}
	appended, err := store.AppendAgentSessionEntries(ctx, AppendAgentSessionEntriesRequest{
		Scope: run.Scope, SessionID: opened.Session.ID, RunID: run.ID, Lease: &lease,
		Entries: []*SessionEntry{assistant, tool}, Now: now,
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
		step:    &AgentTurn{ID: stepID, RunID: run.ID, Sequence: 1, Status: AgentTurnStatusCompleted},
		entries: map[string]*SessionEntry{},
	}
	for _, entry := range appended.Entries {
		if entry.Type == SessionEntryTool {
			f.entries[entry.ToolCallID] = entry
		}
	}
	registry := controlRegistry{}
	registry.register(ControlToolRequestDecision, requestDecisionHandler{})
	registry.register(ControlToolCompletionReview, completionReviewHandler{})
	f.exec = &ControlExecutor{Portfolio: store, Sessions: store, Now: func() time.Time { return now }, handlers: registry}
	return f
}

func requestDecisionCall(id, args string) agentrun.LoopToolCall {
	return agentrun.LoopToolCall{ID: id, Name: string(ControlToolRequestDecision), Arguments: json.RawMessage(args)}
}

func completionReviewCall(id, args string) agentrun.LoopToolCall {
	return agentrun.LoopToolCall{ID: id, Name: string(ControlToolCompletionReview), Arguments: json.RawMessage(args)}
}

// completeWithFinal ends the claimed Run as the loop's final chain does for a
// non-conversation Run: completed with FinalAnswer.Output.
func (f *controlFixture) completeWithFinal(final *agentrun.FinalAnswer) *AgentRun {
	f.t.Helper()
	completed, _, err := NewRunActivityService(f.store, f.store).TransitionRun(f.ctx, f.run.Scope, f.run.ID, RunTransitionRequest{
		ExpectedRevision: f.run.Revision, Status: AgentRunStatusCompleted, Output: final.Output,
		Summary: "Run completed with its final answer", LeaseOwner: controlTestWorker,
	})
	if err != nil {
		f.t.Fatalf("complete run: %v", err)
	}
	return completed
}

func onlyRunWithSource(t *testing.T, store *sqlite.Store, scope Scope, parentID string, source RunSource) *AgentRun {
	t.Helper()
	runs, err := store.ListAgentRuns(t.Context(), AgentRunFilter{Scope: scope, ParentRunID: parentID, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	var found *AgentRun
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

func reconcileDecisionRun(t *testing.T) (*agentRequestInboxTeamStore, *AgentRequestInboxReconciler, *AgentRequest, *AgentRun) {
	t.Helper()
	store := &agentRequestInboxTeamStore{Store: newTestKernelStore(t)}
	source, request := createInboxRequest(t, store, AgentRequestAcceptanceRecipientReview, CollaborationParty{Type: OwnerTypeAgent, ID: "recipient"})
	reconciler, err := NewAgentRequestInboxReconciler(store)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := reconciler.Reconcile(t.Context(), request.Scope, "recipient"); err != nil || result.DecisionRunsCreated != 1 {
		t.Fatalf("reconcile = %#v, %v", result, err)
	}
	return store, reconciler, request, onlyRunWithSource(t, store.Store, request.Scope, source.ID, RunSourceRequestDecision)
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
	want := map[string]interface{}{AgentRequestDecisionOutputKey: map[string]interface{}{"decision": "reject", "message": "Outside my role."}}
	if !reflect.DeepEqual(final.Output, want) || final.Answer != "Outside my role." || final.PlainText ||
		final.ToolEntryID != f.entries["decide"].ID || final.ToolCallID != "decide" || final.StepID != f.step.ID {
		t.Fatalf("final = %#v", final)
	}
	// The entry stays open for the loop to mark delivered.
	if stored := f.stored("decide"); stored.State != ToolEntryPending || stored.Tool.Result != nil {
		t.Fatalf("entry = %#v", stored)
	}
	completed := f.completeWithFinal(final)
	resolved, err := reconciler.ResolveDecisionRun(t.Context(), completed)
	if err != nil || !resolved {
		t.Fatalf("resolve = %v, %v", resolved, err)
	}
	current, err := NewCollaborationService(store).GetAgentRequest(t.Context(), request.Scope, request.ID)
	if err != nil || current.Status != AgentRequestStatusRejected || current.Response != "Outside my role." {
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
			if result.Kind != ControlContinue || result.Entry == nil || result.Entry.State != ToolEntryError {
				t.Fatalf("result = %#v", result)
			}
			stored := f.stored("decide")
			requireControlError(t, stored.Tool.Result, ToolResultFailed, requestDecisionInvalidCode)
			meta, err := DecodeControlEntryMeta(stored)
			if err != nil || meta == nil || meta.Kind != ControlToolRequestDecision || meta.ErrorCode != requestDecisionInvalidCode {
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
	requireControlError(t, f.stored("decide").Tool.Result, ToolResultFailed, controlArgumentsInvalidCode)
	// One concrete question is accepted.
	store, _, _, run = reconcileDecisionRun(t)
	g := newRequestFinalFixture(t, store.Store, run,
		requestDecisionCall("decide", `{"decision":"request_clarification","message":"Which launch region?"}`))
	result, err := g.exec.Settle(g.ctx, g.context("decide", authority))
	if err != nil {
		t.Fatal(err)
	}
	decision, message, err := ParseAgentRequestDecisionOutput(requireFinal(t, result).Output)
	if err != nil || decision != AgentRequestDecisionRequestClarification || message != "Which launch region?" {
		t.Fatalf("decision = %q %q %v", decision, message, err)
	}
}

func TestReviewToolResolvesCompletionReview(t *testing.T) {
	scope := Scope{Kind: "tenant", ID: "inbox"}
	store := &agentRequestInboxTeamStore{
		Store: newTestKernelStore(t),
		deployment: &kernelteam.Deployment{
			ID: "release-team", Scope: capability.ScopeReference{Kind: scope.Kind, ID: scope.ID},
			DefinitionID: "release-team", ActiveVersion: "1", Status: kernelteam.DeploymentActive, Revision: 1,
			Roster: []kernelteam.RosterAssignment{
				{ID: "coordinator", AgentDeploymentID: "coordinator-agent"},
				{ID: "reviewer", AgentDeploymentID: "reviewer-agent", Approver: true},
				{ID: "specialist", AgentDeploymentID: "specialist-agent"},
			},
		},
		definition: &kernelteam.Definition{
			ID: "release-team", Version: "1",
			Delegation: kernelteam.DelegationPolicy{
				MaximumDepth: 3, MaximumConcurrent: 3, AllowPeerDelegation: true,
				RequireAcceptance: true, RequireCompletionReview: true,
			},
		},
	}
	source, err := NewPortfolioService(store).CreateAgentRun(t.Context(), CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "release-team"}, AssignedAgentID: "coordinator-agent",
		Goal: "Coordinate a reviewed release", Source: RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	service := NewCollaborationService(store)
	created, err := service.CreateAgentRequest(t.Context(), CreateAgentRequestRequest{
		Scope: scope, Kind: AgentRequestKindHandoff,
		Requester:   CollaborationParty{Type: OwnerTypeTeam, ID: "release-team"},
		Recipient:   CollaborationParty{Type: OwnerTypeAgent, ID: "specialist-agent"},
		SourceRunID: source.ID, Goal: "Prepare release evidence", SemanticRole: "specialist",
		AcceptanceCriteria: map[string]interface{}{"evidenceRequired": true}, IdempotencyKey: "prepare-release-evidence",
	})
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := service.RespondAgentRequest(t.Context(), RespondAgentRequestRequest{
		Scope: scope, RequestID: created.Request.ID, ExpectedRevision: created.Request.Revision,
		Decision: AgentRequestDecisionAccept, Principal: created.Request.Recipient,
		Actor: CollaborationParty{Type: OwnerTypeAgent, ID: "specialist-agent"}, AssignedAgentID: "specialist-agent",
	})
	if err != nil {
		t.Fatal(err)
	}
	completed, err := service.CompleteAgentRequest(t.Context(), CompleteAgentRequestRequest{
		Scope: scope, RequestID: accepted.Request.ID, ExpectedRevision: accepted.Request.Revision,
		ExpectedChildRevision: accepted.Child.Revision, Principal: accepted.Request.Recipient,
		Summary: "Release evidence prepared", AcceptanceEvidence: map[string]interface{}{"checks": "passed"},
		CompletionKey: "release-evidence-complete",
	})
	if err != nil || completed.Request.Status != AgentRequestStatusCompletionReview {
		t.Fatalf("completion submission = %#v, %v", completed, err)
	}
	reconciler, err := NewAgentRequestInboxReconciler(store)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := reconciler.Reconcile(t.Context(), scope, "reviewer-agent"); err != nil || result.CompletionReviewsCreated != 1 {
		t.Fatalf("reconcile = %#v, %v", result, err)
	}
	run := onlyRunWithSource(t, store.Store, scope, source.ID, RunSourceCompletionReview)
	if want := (&BudgetPolicy{
		MaxAttempts: 5, MaxTurns: 12, MaxInputTokens: 256000, MaxOutputTokens: 4096,
		MaxTotalTokens: 272384, MaxDurationMS: 180000,
	}); !reflect.DeepEqual(run.Budget, want) {
		t.Fatalf("review budget = %#v", run.Budget)
	}

	f := newRequestFinalFixture(t, store.Store, run,
		completionReviewCall("review", `{"decision":"approve","message":"The evidence meets the criteria."}`))
	authority := agentrun.ControlAuthority{AgentID: "reviewer-agent", FinalMode: agentrun.FinalModeReview}
	// The decision tool is not offered in a review Run.
	if specs := builtinControlSpecs(authority); len(specs) != 1 || specs[0].Name != ControlToolCompletionReview {
		t.Fatalf("review specs = %#v", specs)
	}
	result, err := f.exec.Settle(f.ctx, f.context("review", authority))
	if err != nil {
		t.Fatal(err)
	}
	final := requireFinal(t, result)
	want := map[string]interface{}{AgentRequestCompletionReviewOutputKey: map[string]interface{}{"decision": "approve", "message": "The evidence meets the criteria."}}
	if !reflect.DeepEqual(final.Output, want) {
		t.Fatalf("output = %#v", final.Output)
	}
	resolved, err := reconciler.ResolveCompletionReviewRun(t.Context(), f.completeWithFinal(final))
	if err != nil || !resolved {
		t.Fatalf("resolve = %v, %v", resolved, err)
	}
	current, err := service.GetAgentRequest(t.Context(), scope, created.Request.ID)
	if err != nil || current.Status != AgentRequestStatusCompleted || current.CompletionReviewer == nil ||
		current.CompletionReviewer.ID != "reviewer-agent" {
		t.Fatalf("request = %#v, %v", current, err)
	}

	// A review needs an evidence-based reason and a known verdict.
	for _, args := range []string{`{"decision":"approve","message":" "}`, `{"decision":"accept","message":"fine"}`} {
		other := newControlFixture(t, []agentrun.LoopToolCall{completionReviewCall("review", args)})
		registry := controlRegistry{}
		registry.register(ControlToolCompletionReview, completionReviewHandler{})
		other.exec.handlers = registry
		if _, err := other.exec.Settle(other.ctx, other.context("review", authority)); err != nil {
			t.Fatal(err)
		}
		requireControlError(t, other.stored("review").Tool.Result, ToolResultFailed, requestDecisionInvalidCode)
	}
}

func TestDecisionBudgetIsTwelveSteps(t *testing.T) {
	_, _, _, run := reconcileDecisionRun(t)
	want := &BudgetPolicy{
		MaxAttempts: 5, MaxTurns: 12, MaxInputTokens: 192000, MaxOutputTokens: 4096,
		MaxTotalTokens: 208384, MaxDurationMS: 180000,
	}
	if !reflect.DeepEqual(run.Budget, want) {
		t.Fatalf("decision budget = %#v", run.Budget)
	}
}

func TestRequestFinalToolSchemas(t *testing.T) {
	decision := builtinControlSpecs(agentrun.ControlAuthority{FinalMode: agentrun.FinalModeDecision})
	if len(decision) != 1 || decision[0].Name != ControlToolRequestDecision || decision[0].Phase != ControlPhaseSettle {
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
	review := builtinControlSpecs(agentrun.ControlAuthority{FinalMode: agentrun.FinalModeReview})
	properties, _ = schemaProperties(t, review[0].InputSchema)
	if enum := properties["decision"].(map[string]interface{})["enum"]; !reflect.DeepEqual(enum, []string{"approve", "reject"}) {
		t.Fatalf("review enum = %#v", enum)
	}
	// The plain-text note names the tool's required fields.
	if got := newControlRegistry(builtinControlTools()...).finalToolRequiredFields(ControlToolRequestDecision, agentrun.ControlAuthority{FinalMode: agentrun.FinalModeDecision}); got != "decision, message" {
		t.Fatalf("required fields = %q", got)
	}
}
