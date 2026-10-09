package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/axiom-studio/openseal/pkg/skillerror"
	"github.com/axiom-studio/openseal/pkg/workspace"
)

func feedbackReadCatalog(t *testing.T) (*skill.Catalog, Scope) {
	t.Helper()
	catalog := skill.NewCatalog()
	scope := Scope{Kind: "tenant", ID: "feedback"}
	definition := &skill.Definition{ID: "reader", Version: "1.0.0", Name: "Reader", Transport: skill.TransportReference{Kind: "http", Endpoint: "https://reader.invalid"},
		Actions: map[string]skill.Action{"list": {Name: "list", Description: "List records", SideEffect: skill.SideEffectRead, Risk: skill.RiskLevelRead,
			InputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"fields": map[string]interface{}{"type": "string"}}, "required": []interface{}{"fields"}},
			Credentials: []skill.CredentialRequirement{{Name: "token", Kind: "api-token"}}, Retry: skill.ActionRetryPolicy{MaxAttempts: 5}, Idempotency: skill.IdempotencySupported}}}
	if err := catalog.Register(t.Context(), definition); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Bind(t.Context(), &skill.Binding{ID: "reader-binding", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "reader-agent",
		SkillID: "reader", SkillVersion: "1.0.0", AllowedActions: []string{"list"}, MaximumRisk: skill.RiskLevelRead,
		Credentials: map[string]skill.CredentialReference{"token": {Kind: "api-token", ID: "reader-secret"}}, Revision: 1}); err != nil {
		t.Fatal(err)
	}
	return catalog, scope
}

func feedbackPropose(t *testing.T, store KernelStore, catalog *skill.Catalog, run *AgentRun, now time.Time, key, fields string, disposition ActionDisposition) *ActionProposalResult {
	t.Helper()
	coordinator := NewActionCoordinator(store, store, catalog, ActionPolicyEvaluatorFunc(func(context.Context, ActionPolicyInput) (ActionPolicyDecision, error) {
		return ActionPolicyDecision{Disposition: disposition, EligibleApprovers: []ApprovalPrincipal{{Type: "user", ID: "owner"}}, Reason: "Read access policy"}, nil
	}))
	coordinator.now = func() time.Time { return now }
	result, err := coordinator.Propose(t.Context(), ProposeActionRequest{Scope: run.Scope, RunID: run.ID, WorkerID: "agent-worker", DeploymentID: "reader-agent",
		SkillID: "reader", SkillVersion: "1.0.0", Action: "list", Arguments: map[string]interface{}{"fields": fields}, IdempotencyKey: key,
		Summary: "Read records", ContinuationCheckpoint: map[string]interface{}{ToolFeedbackCorrectionCheckpointKey: map[string]interface{}{"correctionsUsed": -100}}})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func feedbackClaim(t *testing.T, store KernelStore, scope Scope, now time.Time) *AgentRun {
	t.Helper()
	run, err := store.ClaimNextAgentRun(t.Context(), AgentRunClaim{Scope: scope, WorkerID: "agent-worker", Now: now, LeaseDuration: time.Minute, AgingInterval: time.Minute})
	if err != nil || run == nil {
		t.Fatalf("claim: %#v %v", run, err)
	}
	return run
}

func feedbackCreateRun(t *testing.T, store KernelStore, scope Scope, now time.Time) *AgentRun {
	t.Helper()
	portfolio := NewPortfolioService(store)
	portfolio.now = func() time.Time { return now }
	_, err := portfolio.CreateAgentRun(t.Context(), CreateAgentRunRequest{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "reader-agent"}, AssignedAgentID: "reader-agent", Goal: "Read records"})
	if err != nil {
		t.Fatal(err)
	}
	return feedbackClaim(t, store, scope, now)
}

// A failed dispatch goes back to the model as the action's result. The manifest
// retry policy never re-sends it, and only kernel state carries the allowance.
func TestToolFeedbackFirstFailureReturnsToTheModelAcrossStores(t *testing.T) {
	forActionLifecycleStores(t, func(t *testing.T, store KernelStore) {
		now := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
		catalog, scope := feedbackReadCatalog(t)
		run := feedbackCreateRun(t, store, scope, now)
		proposal := feedbackPropose(t, store, catalog, run, now, "initial", "bad", ActionDispositionAllow)
		if proposal.Call.MaxAttempts != 1 || !proposal.Call.AvailableAt.Equal(now) {
			t.Fatalf("manifest transport retry/delay remained enabled: %#v", proposal.Call)
		}
		dispatches := 0
		worker := feedbackFailingWorker(store, catalog, now, &dispatches)
		failed, err := worker.RunOnce(t.Context(), scope, "action-worker", time.Minute)
		if err != nil || failed == nil || failed.Call.Status != ActionCallStatusFailed || failed.Call.Attempt != 1 ||
			failed.Call.FailurePhase != ActionFailureAfterDispatch || dispatches != 1 {
			t.Fatalf("first failed dispatch: %#v %v count=%d", failed, err, dispatches)
		}
		feedback, active := ReadToolFeedbackCorrection(failed.Run.Checkpoint)
		if failed.Run.Status != AgentRunStatusQueued || failed.Run.CompletedAt != nil || requiresFinalFailureExplanation(failed.Run.Checkpoint) ||
			!active || feedback.CorrectionsRemaining != MaximumToolFeedbackCorrections || feedback.LastFailureID != failed.Call.ID ||
			toolFeedbackInteger(failed.Run.Checkpoint[toolFailureCountCheckpointKey]) != 1 {
			t.Fatalf("failure was not returned to the model: %#v", failed.Run)
		}
		if strings.Contains(fmt.Sprint(failed.Run.Checkpoint), "private-secret") {
			t.Fatal("failure feedback exposed credentials")
		}
		again, err := worker.RunOnce(t.Context(), scope, "action-worker", time.Minute)
		if err != nil || again != nil || dispatches != 1 {
			t.Fatalf("failed action retried: %#v %v dispatches=%d", again, err, dispatches)
		}
		// A durable failed receipt replayed in a later Turn is not fresh feedback.
		replay := checkpointTerminalAction(nil, failed.Call, map[string]interface{}{"idempotentReplay": true})
		if !requiresFinalFailureExplanation(replay) {
			t.Fatalf("durable receipt replay granted another correction: %#v", replay)
		}
	})
}

func feedbackProposeCorrection(t *testing.T, store KernelStore, catalog *skill.Catalog, scope Scope, runID string, now time.Time, key, fields string) *ActionProposalResult {
	t.Helper()
	run := feedbackClaim(t, store, scope, now)
	if run.ID != runID {
		t.Fatalf("claimed %s, want %s", run.ID, runID)
	}
	return feedbackPropose(t, store, catalog, run, now, key, fields, ActionDispositionAllow)
}

// A corrected request runs once and a success ends the failure chain; the
// unchanged failed request is never sent again.
func TestToolFeedbackCorrectionRunsAndUnchangedRequestStopsAcrossStores(t *testing.T) {
	forActionLifecycleStores(t, func(t *testing.T, store KernelStore) {
		now := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
		catalog, scope := feedbackReadCatalog(t)
		run := feedbackCreateRun(t, store, scope, now)
		feedbackPropose(t, store, catalog, run, now, "initial", "bad", ActionDispositionAllow)
		dispatches := 0
		if _, err := feedbackFailingWorker(store, catalog, now, &dispatches).RunOnce(t.Context(), scope, "action-worker", time.Minute); err != nil {
			t.Fatal(err)
		}
		corrected := feedbackProposeCorrection(t, store, catalog, scope, run.ID, now, "corrected", "id")
		state, _ := corrected.Run.Checkpoint[ToolFeedbackCorrectionCheckpointKey].(map[string]interface{})
		if corrected.Call.Status != ActionCallStatusReady || state["admittedActionCallId"] != corrected.Call.ID {
			t.Fatalf("correction was not admitted: %#v", corrected)
		}
		worker := NewActionWorker(store, catalog, CredentialResolverFunc(func(context.Context, CredentialResolutionRequest) (map[string]string, error) {
			return map[string]string{"token": "private-secret"}, nil
		}), ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
			dispatches++
			return map[string]interface{}{"records": []interface{}{"one"}}, nil
		}))
		worker.now = func() time.Time { return now }
		succeeded, err := worker.RunOnce(t.Context(), scope, "action-worker", time.Minute)
		if err != nil || succeeded == nil || succeeded.Call.Status != ActionCallStatusSucceeded || dispatches != 2 {
			t.Fatalf("correction did not run: %#v %v", succeeded, err)
		}
		if _, active := ReadToolFeedbackCorrection(succeeded.Run.Checkpoint); active || succeeded.Run.Status != AgentRunStatusQueued ||
			toolFeedbackInteger(succeeded.Run.Checkpoint[toolFailureCountCheckpointKey]) != 1 {
			t.Fatalf("successful correction did not end the chain or lost the Run count: %#v", succeeded.Run)
		}

		// Another Run repeats its failed request unchanged.
		other := feedbackCreateRun(t, store, scope, now)
		feedbackPropose(t, store, catalog, other, now, "other-initial", "bad", ActionDispositionAllow)
		if _, err := feedbackFailingWorker(store, catalog, now, &dispatches).RunOnce(t.Context(), scope, "action-worker", time.Minute); err != nil {
			t.Fatal(err)
		}
		repeated := feedbackProposeCorrection(t, store, catalog, scope, other.ID, now, "other-repeat", "bad")
		if repeated.Call.Status != ActionCallStatusDenied || !requiresFinalFailureExplanation(repeated.Run.Checkpoint) || dispatches != 3 {
			t.Fatalf("unchanged failed request was admitted: %#v", repeated)
		}
	})
}

// Corrections that keep failing stop after MaximumToolFeedbackCorrections.
func TestToolFeedbackFailingCorrectionsStopAtTheBoundAcrossStores(t *testing.T) {
	forActionLifecycleStores(t, func(t *testing.T, store KernelStore) {
		now := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
		catalog, scope := feedbackReadCatalog(t)
		run := feedbackCreateRun(t, store, scope, now)
		feedbackPropose(t, store, catalog, run, now, "initial", "bad", ActionDispositionAllow)
		dispatches := 0
		worker := feedbackFailingWorker(store, catalog, now, &dispatches)
		failed, err := worker.RunOnce(t.Context(), scope, "action-worker", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		for attempt := 1; attempt <= MaximumToolFeedbackCorrections; attempt++ {
			feedbackProposeCorrection(t, store, catalog, scope, run.ID, now, fmt.Sprint("correction-", attempt), fmt.Sprint("fields-", attempt))
			if failed, err = worker.RunOnce(t.Context(), scope, "action-worker", time.Minute); err != nil || failed == nil || failed.Call.Status != ActionCallStatusFailed {
				t.Fatalf("correction %d: %#v %v", attempt, failed, err)
			}
		}
		if failed.Run.Status != AgentRunStatusFailed || !requiresFinalFailureExplanation(failed.Run.Checkpoint) || dispatches != MaximumToolFeedbackCorrections+1 {
			t.Fatalf("failing corrections did not stop at the bound: %#v dispatches=%d", failed.Run, dispatches)
		}
		feedbackAssertKernelFailureReply(t, failed.Run, "action_failed")
		feedbackAssertNotClaimable(t, store, scope, now)
	})
}

// Failures separated by successful corrections stop at the Run's bound.
func TestToolFeedbackRunFailureBoundSurvivesSuccesses(t *testing.T) {
	checkpoint := map[string]interface{}{}
	for index := 1; index <= MaximumRecoveredToolFailures+1; index++ {
		failed := &ActionCall{ID: fmt.Sprint("failed-", index), Status: ActionCallStatusFailed, SideEffect: skill.SideEffectWrite,
			Action: "click", Arguments: map[string]interface{}{"target": index}, Error: "element moved"}
		checkpoint = checkpointTerminalAction(checkpoint, failed, nil)
		if stopped := requiresFinalFailureExplanation(checkpoint); stopped != (index > MaximumRecoveredToolFailures) {
			t.Fatalf("failure %d stopped=%v: %#v", index, stopped, checkpoint)
		}
		if index > MaximumRecoveredToolFailures {
			break
		}
		read := &ActionCall{ID: fmt.Sprint("read-", index), Status: ActionCallStatusReady, Action: "snapshot"}
		var denied string
		if checkpoint, denied = admitToolFeedbackCorrection(checkpoint, read); denied != "" {
			t.Fatalf("read after failure %d denied: %s", index, denied)
		}
		read.Status = ActionCallStatusSucceeded
		checkpoint = checkpointTerminalAction(checkpoint, read, nil)
		// The model cannot reset the kernel count through its checkpoint.
		checkpoint = preserveKernelActionHistory(checkpoint, map[string]interface{}{toolFailureCountCheckpointKey: 0})
		if _, active := ReadToolFeedbackCorrection(checkpoint); active || toolFeedbackInteger(checkpoint[toolFailureCountCheckpointKey]) != index {
			t.Fatalf("success after failure %d: %#v", index, checkpoint)
		}
	}
	if terminalFailureCodeFromCheckpoint(checkpoint) != "action_failed" {
		t.Fatalf("bounded stop lost its cause: %#v", checkpoint)
	}
}

func feedbackFailingWorker(store KernelStore, catalog *skill.Catalog, now time.Time, dispatches *int) *ActionWorker {
	worker := NewActionWorker(store, catalog, CredentialResolverFunc(func(context.Context, CredentialResolutionRequest) (map[string]string, error) {
		return map[string]string{"token": "private-secret"}, nil
	}), ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
		(*dispatches)++
		return nil, errors.New("invalid field selector private-secret")
	}))
	worker.now = func() time.Time { return now }
	return worker
}

// feedbackPlatformFailingWorker fails with a platform condition no correction
// can fix, which stops the attempt at once.
func feedbackPlatformFailingWorker(store KernelStore, catalog *skill.Catalog, now time.Time, dispatches *int) *ActionWorker {
	worker := NewActionWorker(store, catalog, CredentialResolverFunc(func(context.Context, CredentialResolutionRequest) (map[string]string, error) {
		return map[string]string{"token": "private-secret"}, nil
	}), ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
		(*dispatches)++
		return nil, skillerror.NewActionError("browser_proxy_unavailable", "", nil)
	}))
	worker.now = func() time.Time { return now }
	return worker
}

func feedbackAssertNotClaimable(t *testing.T, store KernelStore, scope Scope, now time.Time) {
	t.Helper()
	claimed, err := store.ClaimNextAgentRun(t.Context(), AgentRunClaim{Scope: scope, WorkerID: "unexpected-recovery", Now: now, LeaseDuration: time.Minute, AgingInterval: time.Minute})
	if err != nil || claimed != nil {
		t.Fatalf("failed attempt scheduled an automatic continuation: %#v %v", claimed, err)
	}
}

func feedbackAssertKernelFailureReply(t *testing.T, run *AgentRun, code string) {
	t.Helper()
	host := &failureExplanationTestHost{respond: func(HostedTurnRequest) (*HostedTurnResponse, error) {
		t.Fatal("terminal failure invoked the hosted model")
		return nil, errors.New("unexpected model request")
	}}
	runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{AgentID: "reader-agent", DefinitionID: "reader", DefinitionVersion: "1",
		Actions: []capability.ModelAction{{Name: "reader.list"}}, Workspace: &workspace.Authority{Workspace: workspace.DefaultSpec()},
		WorkspaceCredentials: map[string]capability.CredentialReference{"token": {Kind: "vault", ID: "workspace-secret"}}})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := runner.RunTurn(t.Context(), TurnExecutionContext{Run: run, Turn: &AgentTurn{ID: "stale-explanation"}})
	if err != nil || outcome == nil || host.calls != 0 || outcome.ModelProvider != "host" || outcome.Model != "terminal-failure-report" {
		t.Fatalf("failure was not handled without a model: %#v %v hostcalls=%d", outcome, err, host.calls)
	}
	if outcome.OutputSummary != TerminalFailureReply(code) || outcome.RunOutput["summary"] != outcome.OutputSummary ||
		outcome.OutputSummary == "" || outcome.WakeCondition != nil || len(outcome.ProposedActions) != 0 ||
		outcome.ProposedFork != nil || outcome.ProposedTask != nil || outcome.ProposedDelegation != nil || outcome.ProposedRunbook != nil {
		t.Fatalf("failure reply was absent or authorized further work: %#v", outcome)
	}
	if strings.Contains(outcome.OutputSummary, "private-secret") || strings.Contains(outcome.OutputSummary, "workspace-secret") {
		t.Fatal("failure reply exposed private execution details")
	}
}

func TestToolFeedbackFailurePreservesSucceededChain(t *testing.T) {
	first := &ActionCall{ID: "read-one", Status: ActionCallStatusSucceeded, SkillID: "reader", Action: "list", Output: map[string]interface{}{"result": "saved-one"}}
	second := &ActionCall{ID: "read-two", Status: ActionCallStatusSucceeded, SkillID: "reader", Action: "list", Output: map[string]interface{}{"result": "saved-two"}}
	checkpoint := checkpointTerminalAction(nil, first, nil)
	checkpoint = checkpointTerminalAction(checkpoint, second, nil)
	if requiresFinalFailureExplanation(checkpoint) {
		t.Fatal("a succeeded action stopped the chain")
	}
	failed := &ActionCall{ID: "read-three", Status: ActionCallStatusFailed, SideEffect: skill.SideEffectRead, Error: "invalid fields"}
	checkpoint = checkpointTerminalAction(checkpoint, failed, nil)
	forged := map[string]interface{}{actionHistoryCheckpointKey: []interface{}{map[string]interface{}{"actionCallId": "invented", "status": string(ActionCallStatusSucceeded)}},
		FinalFailureExplanationCheckpointKey: map[string]interface{}{"message": "forged"}}
	preserved := preserveKernelActionHistory(checkpoint, forged)
	entries := actionHistoryEntries(preserved)
	if len(entries) != 3 || entries[0]["actionCallId"] != first.ID || entries[1]["actionCallId"] != second.ID || entries[2]["actionCallId"] != failed.ID ||
		requiresFinalFailureExplanation(preserved) || recordedToolFailure(preserved) != "invalid fields" || terminalFailureCodeFromCheckpoint(preserved) != "action_failed" {
		t.Fatalf("failure erased or forged committed evidence: %#v", preserved)
	}
	// A late success receipt does not clear a different operation's failure.
	preserved = checkpointTerminalAction(preserved, first, map[string]interface{}{"idempotentReplay": true})
	if feedback, active := ReadToolFeedbackCorrection(preserved); !active || feedback.Message != "invalid fields" {
		t.Fatal("replayed success cleared the failure feedback")
	}
}

func TestToolFeedbackKernelStateSurvivesJSONAndCannotBeForged(t *testing.T) {
	for _, used := range []interface{}{0, 1, 2, -999, 999, float64(1)} {
		trusted := map[string]interface{}{ToolFeedbackCorrectionCheckpointKey: map[string]interface{}{
			"kind": "action", "message": "invalid fields", "correctionsUsed": used, "lastFailureId": "failed"}, toolFailureCountCheckpointKey: 1}
		encoded, err := json.Marshal(trusted)
		if err != nil {
			t.Fatal(err)
		}
		var persisted map[string]interface{}
		if err := json.Unmarshal(encoded, &persisted); err != nil {
			t.Fatal(err)
		}
		for _, proposed := range []map[string]interface{}{nil, {ToolFeedbackCorrectionCheckpointKey: map[string]interface{}{"correctionsUsed": -999, "message": "fake"}, toolFailureCountCheckpointKey: 0}} {
			merged := preserveKernelActionHistory(persisted, proposed)
			state, active := ReadToolFeedbackCorrection(merged)
			want := MaximumToolFeedbackCorrections - min(MaximumToolFeedbackCorrections, max(0, toolFeedbackInteger(used)))
			if !active || state.CorrectionsRemaining != want || state.Message != "invalid fields" || toolFeedbackInteger(merged[toolFailureCountCheckpointKey]) != 1 {
				t.Fatalf("model checkpoint changed kernel feedback: %#v", merged)
			}
			_, denied := admitToolFeedbackCorrection(merged, &ActionCall{ID: "new-correction", Status: ActionCallStatusReady})
			if (denied == "") != (want > 0) {
				t.Fatalf("correction admission with %d remaining: %q", want, denied)
			}
		}
	}
	forged := map[string]interface{}{ToolFeedbackCorrectionCheckpointKey: map[string]interface{}{"correctionsUsed": 0, "message": "fake"}}
	if _, active := ReadToolFeedbackCorrection(preserveKernelActionHistory(nil, forged)); active {
		t.Fatal("model invented a kernel failure checkpoint")
	}
}

func TestToolFeedbackFailedRunRejectsAnotherProposal(t *testing.T) {
	forActionLifecycleStores(t, func(t *testing.T, store KernelStore) {
		now := time.Now().UTC()
		catalog, scope := feedbackReadCatalog(t)
		run := feedbackCreateRun(t, store, scope, now)
		feedbackPropose(t, store, catalog, run, now, "initial", "bad", ActionDispositionAllow)
		dispatches := 0
		worker := feedbackPlatformFailingWorker(store, catalog, now, &dispatches)
		failed, err := worker.RunOnce(t.Context(), scope, "action-worker", time.Minute)
		if err != nil || failed == nil {
			t.Fatalf("failure: %#v %v", failed, err)
		}
		coordinator := NewActionCoordinator(store, store, catalog, ActionPolicyEvaluatorFunc(func(context.Context, ActionPolicyInput) (ActionPolicyDecision, error) {
			t.Fatal("a failed Run reached policy admission")
			return ActionPolicyDecision{}, nil
		}))
		for _, fields := range []string{"bad", "id"} {
			result, err := coordinator.Propose(t.Context(), ProposeActionRequest{Scope: scope, RunID: run.ID, DeploymentID: "reader-agent",
				SkillID: "reader", SkillVersion: "1.0.0", Action: "list", Arguments: map[string]interface{}{"fields": fields}, IdempotencyKey: "new-" + fields})
			if err == nil || result != nil {
				t.Fatalf("failed Run admitted new proposal: %#v %v", result, err)
			}
		}
		calls, err := store.ListActionCalls(t.Context(), ActionFilter{Scope: scope, RunID: run.ID})
		if err != nil || len(calls) != 1 || dispatches != 1 {
			t.Fatalf("unexpected correction receipt: %#v %v", calls, err)
		}
		current, err := store.GetAgentRun(t.Context(), scope, run.ID)
		if err != nil || current.Revision != failed.Run.Revision || current.Status != AgentRunStatusFailed {
			t.Fatalf("rejected proposal mutated failed attempt: %#v %v", current, err)
		}
	})
}

func TestToolFeedbackNewUserAttemptStillRequiresNormalApproval(t *testing.T) {
	forActionLifecycleStores(t, func(t *testing.T, store KernelStore) {
		now := time.Now().UTC()
		catalog, scope := feedbackReadCatalog(t)
		failedRun := feedbackCreateRun(t, store, scope, now)
		feedbackPropose(t, store, catalog, failedRun, now, "first", "bad", ActionDispositionAllow)
		dispatches := 0
		failed, err := feedbackPlatformFailingWorker(store, catalog, now, &dispatches).RunOnce(t.Context(), scope, "action-worker", time.Minute)
		if err != nil || failed == nil {
			t.Fatalf("failure: %#v %v", failed, err)
		}
		// A new user request creates a fresh attempt, not an automatic recovery.
		next := feedbackCreateRun(t, store, scope, now)
		if next.ID == failed.Run.ID || requiresFinalFailureExplanation(next.Checkpoint) {
			t.Fatal("new request reused failed authority")
		}
		proposal := feedbackPropose(t, store, catalog, next, now, "user-request", "id", ActionDispositionRequireApproval)
		if proposal.Approval == nil || proposal.Call.Status != ActionCallStatusWaitingApproval || proposal.Run.Status != AgentRunStatusWaitingForApproval {
			t.Fatalf("new user attempt bypassed approval: %#v", proposal)
		}
		call, err := store.ClaimNextAction(t.Context(), ActionClaim{Scope: scope, WorkerID: "action-worker", Now: now, LeaseDuration: time.Minute})
		if err != nil || call != nil {
			t.Fatalf("unapproved action executable: %#v %v", call, err)
		}
		old, err := store.GetAgentRun(t.Context(), scope, failed.Run.ID)
		if err != nil || old.Status != AgentRunStatusFailed || old.Revision != failed.Run.Revision || dispatches != 1 {
			t.Fatalf("new user request resumed old failed attempt: %#v %v", old, err)
		}
	})
}

// A failed change may already have taken effect: the model gets the failure
// (and is told to verify before redoing it) rather than a blind retry.
func TestToolFeedbackMutationFailureReturnsAndPreservesPhase(t *testing.T) {
	for _, phase := range []ActionFailurePhase{ActionFailureBeforeDispatch, ActionFailureAfterDispatch, ""} {
		t.Run(string(phase), func(t *testing.T) {
			call := &ActionCall{ID: "write", Status: ActionCallStatusFailed, SideEffect: skill.SideEffectExternal, Error: "provider disconnected", FailurePhase: phase}
			checkpoint := checkpointTerminalAction(nil, call, nil)
			if feedback, active := ReadToolFeedbackCorrection(checkpoint); requiresFinalFailureExplanation(checkpoint) || !active || feedback.Message != "provider disconnected" {
				t.Fatalf("mutation failure was not returned: %#v", checkpoint)
			}
			if last, _ := checkpoint["lastAction"].(map[string]interface{}); phase != "" && last["failurePhase"] != phase {
				t.Fatalf("failure phase lost: %#v", last)
			}
			encoded, err := json.Marshal(call)
			if err != nil {
				t.Fatal(err)
			}
			var persisted ActionCall
			if err := json.Unmarshal(encoded, &persisted); err != nil || persisted.FailurePhase != phase {
				t.Fatalf("failure phase lost during persistence: %s %v", encoded, err)
			}
		})
	}
}

func TestToolFeedbackStoppedAttemptPublishesWithoutModelCallAcrossStores(t *testing.T) {
	forActionLifecycleStores(t, func(t *testing.T, store KernelStore) {
		now := time.Now().UTC()
		_, scope := feedbackReadCatalog(t)
		portfolio := NewPortfolioService(store)
		portfolio.now = func() time.Time { return now }
		stopped := checkpointTerminalAction(nil, &ActionCall{ID: "failed-read", Status: ActionCallStatusFailed, SideEffect: skill.SideEffectRead, Error: "invalid fields"}, nil)
		stopped = checkpointTerminalFailure(checkpointFinalFailureExplanation(stopped, "action", "invalid fields"), "action_failed")
		if _, err := portfolio.CreateAgentRun(t.Context(), CreateAgentRunRequest{Scope: scope,
			Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "reader-agent"}, AssignedAgentID: "reader-agent", Goal: "Read records",
			Checkpoint: stopped}); err != nil {
			t.Fatal(err)
		}
		run := feedbackClaim(t, store, scope, now)
		host := &failureExplanationTestHost{respond: func(HostedTurnRequest) (*HostedTurnResponse, error) {
			t.Fatal("stopped attempt invoked an explanatory model")
			return nil, nil
		}}
		runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{AgentID: "reader-agent", DefinitionID: "reader", DefinitionVersion: "1"})
		if err != nil {
			t.Fatal(err)
		}
		result, err := NewTurnCoordinator(store, store, store).Advance(t.Context(), AdvanceAgentRunRequest{Scope: scope, RunID: run.ID, WorkerID: "agent-worker"}, runner)
		if err != nil || result == nil || result.Run.Status != AgentRunStatusFailed || result.Run.Error != "invalid fields" ||
			result.Run.Output["summary"] != TerminalFailureReply("action_failed") || host.calls != 0 || (result.Turn.Usage.InputTokens != 0 || result.Turn.Usage.OutputTokens != 0 || result.Turn.Usage.Cost != 0 || result.Turn.Usage.RepairAttempts != 0) {
			t.Fatalf("stopped attempt was not terminal without model usage: %#v %v calls=%d", result, err, host.calls)
		}
		feedbackAssertNotClaimable(t, store, scope, now)
	})
}

type feedbackProposalConflictStore struct {
	KernelStore
	conflict bool
}

func (s *feedbackProposalConflictStore) CreateActionProposal(ctx context.Context, record ActionProposalRecord) (*ActionProposalResult, error) {
	if s.conflict {
		s.conflict = false
		return nil, ErrRevisionConflict
	}
	return s.KernelStore.CreateActionProposal(ctx, record)
}

func TestToolFeedbackNewUserAttemptAdmissionConflictIsAtomic(t *testing.T) {
	forActionLifecycleStores(t, func(t *testing.T, store KernelStore) {
		now := time.Now().UTC()
		catalog, scope := feedbackReadCatalog(t)
		run := feedbackCreateRun(t, store, scope, now)
		feedbackPropose(t, store, catalog, run, now, "first", "bad", ActionDispositionAllow)
		dispatches := 0
		failed, err := feedbackPlatformFailingWorker(store, catalog, now, &dispatches).RunOnce(t.Context(), scope, "action-worker", time.Minute)
		if err != nil || failed == nil {
			t.Fatalf("failure: %#v %v", failed, err)
		}
		next := feedbackCreateRun(t, store, scope, now)
		wrapped := &feedbackProposalConflictStore{KernelStore: store, conflict: true}
		coordinator := NewActionCoordinator(store, wrapped, catalog, ActionPolicyEvaluatorFunc(func(context.Context, ActionPolicyInput) (ActionPolicyDecision, error) {
			return ActionPolicyDecision{Disposition: ActionDispositionAllow}, nil
		}))
		coordinator.now = func() time.Time { return now }
		request := ProposeActionRequest{Scope: scope, RunID: next.ID, WorkerID: "agent-worker", DeploymentID: "reader-agent", SkillID: "reader", SkillVersion: "1.0.0", Action: "list",
			Arguments: map[string]interface{}{"fields": "id"}, IdempotencyKey: "new-user-request"}
		if result, err := coordinator.Propose(t.Context(), request); !errors.Is(err, ErrRevisionConflict) || result != nil {
			t.Fatalf("missing proposal CAS conflict: %#v %v", result, err)
		}
		persisted, err := store.GetAgentRun(t.Context(), scope, next.ID)
		calls, callsErr := store.ListActionCalls(t.Context(), ActionFilter{Scope: scope, RunID: next.ID})
		if err != nil || callsErr != nil || persisted.Revision != next.Revision || persisted.Status != AgentRunStatusRunning || len(calls) != 0 {
			t.Fatalf("conflicted admission partially committed: %#v %#v %v %v", persisted, calls, err, callsErr)
		}
		result, err := coordinator.Propose(t.Context(), request)
		if err != nil || result == nil || !result.Created || result.Run.Status != AgentRunStatusWaitingForDependency || requiresFinalFailureExplanation(result.Run.Checkpoint) {
			t.Fatalf("new user request failed after CAS retry: %#v %v", result, err)
		}
		old, err := store.GetAgentRun(t.Context(), scope, failed.Run.ID)
		if err != nil || old.Revision != failed.Run.Revision || old.Status != AgentRunStatusFailed || dispatches != 1 {
			t.Fatalf("new admission mutated old failure: %#v %v", old, err)
		}
	})
}

// The user command, unlike a failed tool checkpoint, may explicitly start a
// supported retry of a failed reply which has no uncertain tool side effects.
func TestToolFeedbackExplicitUserRetryClearsLegacyBoundaryAcrossStores(t *testing.T) {
	forActionLifecycleStores(t, func(t *testing.T, store KernelStore) {
		commands, ok := store.(RunCommandStore)
		if !ok {
			t.Fatal("built-in store lacks user retry command support")
		}
		service, failed := failedReplyFixture(t, commands)
		result, err := service.RetryConversationRun(t.Context(), AgentRunCommandRequest{
			Scope: failed.Scope, RunID: failed.ID, ExpectedRevision: failed.Revision,
			Actor: ActivityActor{Type: "user", ID: "user"},
		})
		if err != nil || result == nil || result.Run.Status != AgentRunStatusQueued || result.Run.ID != failed.ID {
			t.Fatalf("explicit user retry rejected: %#v %v", result, err)
		}
		if requiresFinalFailureExplanation(result.Run.Checkpoint) || result.Run.Checkpoint[ToolFeedbackCorrectionCheckpointKey] != nil ||
			result.Run.Checkpoint[proposalRecoveryCheckpointKey] != nil || result.Event.EventType != "run.retried" {
			t.Fatalf("explicit retry retained failure authority or lost audit: %#v", result)
		}
		if result.Run.LastAppliedTurn != failed.LastAppliedTurn || result.Run.BudgetUsage != failed.BudgetUsage || result.Run.Checkpoint["saved"] != "context" {
			t.Fatal("user retry lost usage, committed cursor or saved context")
		}
		if _, err := service.RetryConversationRun(t.Context(), AgentRunCommandRequest{
			Scope: failed.Scope, RunID: failed.ID, ExpectedRevision: failed.Revision,
			Actor: ActivityActor{Type: "user", ID: "user"},
		}); !errors.Is(err, ErrRevisionConflict) {
			t.Fatalf("replayed user retry admitted another attempt: %v", err)
		}
	})
}

type feedbackExecutionCatalogProbe struct {
	ActionExecutionCatalog
	resolutions int
}

func (c *feedbackExecutionCatalogProbe) Resolve(ctx context.Context, scope skill.ScopeReference, deploymentID, skillID, version, action string, selection ...skill.BindingReference) (*skill.BoundAction, error) {
	c.resolutions++
	return c.ActionExecutionCatalog.Resolve(ctx, scope, deploymentID, skillID, version, action, selection...)
}

// Only the call correction admission recorded may run while a failure awaits
// its correction.
func TestToolFeedbackUnadmittedReadyCallCannotDispatchAcrossStores(t *testing.T) {
	forActionLifecycleStores(t, func(t *testing.T, store KernelStore) {
		now := time.Now().UTC()
		catalog, scope := feedbackReadCatalog(t)
		run := feedbackCreateRun(t, store, scope, now)
		proposal := feedbackPropose(t, store, catalog, run, now, "unadmitted", "id", ActionDispositionAllow)
		pending := cloneAgentRun(proposal.Run)
		pending.Checkpoint = appendActionHistory(pending.Checkpoint, &ActionCall{
			ID: "prior-failed-read", Scope: scope, RunID: run.ID, Status: ActionCallStatusFailed,
			SkillID: "reader", SkillVersion: "1.0.0", Action: "list", SideEffect: skill.SideEffectRead,
			Arguments: map[string]interface{}{"fields": "bad"}, Error: "invalid fields",
		})
		pending.Checkpoint[ToolFeedbackCorrectionCheckpointKey] = map[string]interface{}{
			"kind": "action", "message": "invalid fields", "correctionsUsed": 1,
			"lastFailureId": "prior-failed-read", "admittedActionCallId": "another-call",
		}
		pending.Revision++
		pending.UpdatedAt = now
		_, err := store.UpdateAgentRunWithEvent(t.Context(), pending, proposal.Run.Revision, &ActivityEvent{
			ID: "restore-pending-feedback", Scope: scope, RunID: run.ID, AgentID: "reader-agent",
			EventType: "test.feedback_checkpoint_restored", Severity: ActivitySeverityInfo, Visibility: ActivityVisibilityScope,
			Actor: ActivityActor{Type: "worker", ID: "fixture"}, Summary: "Restored a pending correction checkpoint", CreatedAt: now,
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		credentialCalls, dispatches := 0, 0
		executionCatalog := &feedbackExecutionCatalogProbe{ActionExecutionCatalog: catalog}
		worker := NewActionWorker(store, executionCatalog, CredentialResolverFunc(func(context.Context, CredentialResolutionRequest) (map[string]string, error) {
			credentialCalls++
			return map[string]string{"token": "private-secret"}, nil
		}), ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
			dispatches++
			return map[string]interface{}{"result": "unexpected correction"}, nil
		}))
		worker.now = func() time.Time { return now }
		stopped, err := worker.RunOnce(t.Context(), scope, "action-worker", time.Minute)
		if err != nil || stopped == nil || stopped.Call.Status != ActionCallStatusFailed || stopped.Call.Error != errActionNotAdmittedAfterFailure.Error() ||
			stopped.Call.FailurePhase != ActionFailureBeforeDispatch || executionCatalog.resolutions != 0 || credentialCalls != 0 || dispatches != 0 {
			t.Fatalf("unadmitted call crossed dispatch boundary: %#v %v catalogcalls=%d credentialcalls=%d dispatches=%d", stopped, err, executionCatalog.resolutions, credentialCalls, dispatches)
		}
		foundPrior := false
		for _, entry := range actionHistoryEntries(stopped.Run.Checkpoint) {
			if entry["actionCallId"] == "prior-failed-read" && entry["error"] == "invalid fields" {
				foundPrior = true
			}
		}
		if !foundPrior {
			t.Fatalf("blocked call lost the original failure receipt: %#v", stopped.Run.Checkpoint)
		}
		if again, err := worker.RunOnce(t.Context(), scope, "action-worker", time.Minute); err != nil || again != nil || dispatches != 0 {
			t.Fatalf("blocked call was requeued: %#v %v dispatches=%d", again, err, dispatches)
		}
	})
}

func TestToolFeedbackNormalSucceededActionsStillContinueAcrossStores(t *testing.T) {
	forActionLifecycleStores(t, func(t *testing.T, store KernelStore) {
		now := time.Now().UTC()
		catalog, scope := feedbackReadCatalog(t)
		run := feedbackCreateRun(t, store, scope, now)
		dispatches := 0
		worker := NewActionWorker(store, catalog, CredentialResolverFunc(func(context.Context, CredentialResolutionRequest) (map[string]string, error) {
			return map[string]string{"token": "private-secret"}, nil
		}), ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
			dispatches++
			return map[string]interface{}{"result": "authorized observation"}, nil
		}))
		worker.now = func() time.Time { return now }
		for index, fields := range []string{"id", "subject"} {
			if index != 0 {
				run = feedbackClaim(t, store, scope, now)
			}
			feedbackPropose(t, store, catalog, run, now, fmt.Sprintf("normal-read-%d", index), fields, ActionDispositionAllow)
			succeeded, err := worker.RunOnce(t.Context(), scope, "action-worker", time.Minute)
			if err != nil || succeeded == nil || succeeded.Call.Status != ActionCallStatusSucceeded || succeeded.Run.Status != AgentRunStatusQueued ||
				requiresFinalFailureExplanation(succeeded.Run.Checkpoint) || len(actionHistoryEntries(succeeded.Run.Checkpoint)) != index+1 {
				t.Fatalf("terminal failure policy stopped a succeeded chain: %#v %v", succeeded, err)
			}
		}
		if dispatches != 2 {
			t.Fatalf("normal action chain dispatched %d times", dispatches)
		}
	})
}

func TestToolFeedbackOutcomeIsOneCorrectionOrAFinalExplanation(t *testing.T) {
	failed := &ActionCall{ID: "failed", Status: ActionCallStatusFailed, SideEffect: skill.SideEffectWrite, Action: "pay", Error: "amount must be the order total"}
	run := &AgentRun{Status: AgentRunStatusRunning, Checkpoint: checkpointTerminalAction(nil, failed, nil)}
	action := TurnAction{Type: "skill", Capability: "browser.pay"}
	for name, test := range map[string]struct {
		outcome *TurnOutcome
		valid   bool
	}{
		"correction":  {&TurnOutcome{NextRunStatus: AgentRunStatusRunning, ProposedActions: []TurnAction{action}}, true},
		"explanation": {&TurnOutcome{NextRunStatus: AgentRunStatusCompleted, OutputSummary: "The payment amount was wrong."}, true},
		"two actions": {&TurnOutcome{NextRunStatus: AgentRunStatusRunning, ProposedActions: []TurnAction{action, action}}, false},
		"fork":        {&TurnOutcome{NextRunStatus: AgentRunStatusRunning, ProposedFork: &TurnForkProposal{}}, false},
		"wait":        {&TurnOutcome{NextRunStatus: AgentRunStatusWaitingForEvent, WakeCondition: &WakeCondition{Type: "timer"}}, false},
		"silent":      {&TurnOutcome{NextRunStatus: AgentRunStatusCompleted, RunOutput: map[string]interface{}{"silent": true}}, false},
	} {
		if err := validateToolFeedbackCorrectionOutcome(run, test.outcome); (err == nil) != test.valid {
			t.Errorf("%s: valid=%v err=%v", name, test.valid, err)
		}
	}
}
