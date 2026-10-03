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

func TestToolFeedbackCorrectionStopsAfterTwoImmediateCorrectionsAcrossStores(t *testing.T) {
	forActionLifecycleStores(t, func(t *testing.T, store KernelStore) {
		now := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
		catalog, scope := feedbackReadCatalog(t)
		run := feedbackCreateRun(t, store, scope, now)
		dispatches := 0
		worker := NewActionWorker(store, catalog, CredentialResolverFunc(func(context.Context, CredentialResolutionRequest) (map[string]string, error) {
			return map[string]string{"token": "private-secret"}, nil
		}), ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
			dispatches++
			return nil, fmt.Errorf("invalid field selector %d private-secret", dispatches)
		}))
		worker.now = func() time.Time { return now }
		var failed *ActionExecutionResult
		for index := 0; index <= MaximumToolFeedbackCorrections; index++ {
			if index != 0 {
				run = feedbackClaim(t, store, scope, now)
			}
			proposal := feedbackPropose(t, store, catalog, run, now, fmt.Sprintf("attempt-%d", index), fmt.Sprintf("fields-%d", index), ActionDispositionAllow)
			if proposal.Call.MaxAttempts != 1 || !proposal.Call.AvailableAt.Equal(now) {
				t.Fatalf("transport retry/delay returned: %#v", proposal.Call)
			}
			if index != 0 {
				state, ok := ReadToolFeedbackCorrection(proposal.Run.Checkpoint)
				if !ok || state.CorrectionsUsed != index {
					t.Fatalf("admission counter: %#v", proposal.Run.Checkpoint)
				}
			}
			var err error
			failed, err = worker.RunOnce(t.Context(), scope, "action-worker", time.Minute)
			if err != nil || failed == nil || failed.Call.Status != ActionCallStatusFailed || failed.Call.Attempt != 1 || failed.Call.FailurePhase != ActionFailureAfterDispatch {
				t.Fatalf("failed read: %#v %v", failed, err)
			}
			if !failed.Run.AvailableAt.Equal(now) || failed.Run.Status != AgentRunStatusQueued || failed.Run.WakeCondition != nil {
				t.Fatalf("feedback delayed continuation: %#v", failed.Run)
			}
			if strings.Contains(fmt.Sprint(failed.Run.Checkpoint), "private-secret") {
				t.Fatal("feedback exposed credentials")
			}
			if requiresFinalFailureExplanation(failed.Run.Checkpoint) != (index == MaximumToolFeedbackCorrections) {
				t.Fatalf("wrong terminal boundary after failure %d: %#v", index+1, failed.Run.Checkpoint)
			}
			// Reprojecting the same durable receipt never spends another attempt.
			replayed := checkpointTerminalAction(failed.Run.Checkpoint, failed.Call, map[string]interface{}{"idempotentReplay": true})
			state, _ := ReadToolFeedbackCorrection(replayed)
			if state.CorrectionsUsed != index {
				t.Fatalf("receipt replay changed correction count: %#v", replayed)
			}
		}
		calls := 0
		host := &failureExplanationTestHost{respond: func(request HostedTurnRequest) (*HostedTurnResponse, error) {
			calls++
			if len(request.Actions) != 0 || !requiresFinalFailureExplanation(request.ContinuationCheckpoint) {
				t.Fatal("exhaustion retained tool authority")
			}
			return &HostedTurnResponse{APIVersion: HostedTurnAPIVersion, InvocationID: request.InvocationID, ModelProvider: "test", Model: "model", NextRunStatus: AgentRunStatusCompleted,
				OutputSummary: "The service rejected the corrected field selection, so I stopped.", RunOutput: map[string]interface{}{"summary": "The service rejected the corrected field selection, so I stopped."}}, nil
		}}
		runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{AgentID: "reader-agent", DefinitionID: "reader", DefinitionVersion: "1", Actions: []capability.ModelAction{{Name: "reader.list"}}})
		if err != nil {
			t.Fatal(err)
		}
		result, err := NewTurnCoordinator(store, store, store).Advance(t.Context(), AdvanceAgentRunRequest{Scope: scope, RunID: run.ID, WorkerID: "agent-worker"}, runner)
		if err != nil || result == nil || result.Run.Status != AgentRunStatusFailed || calls != 1 || dispatches != 3 || result.Run.Error != failed.Call.Error {
			t.Fatalf("exhausted explanation: %#v %v calls=%d dispatches=%d", result, err, calls, dispatches)
		}
	})
}

func TestToolFeedbackCorrectedSuccessClearsOnlyAdmittedCall(t *testing.T) {
	initial := &ActionCall{ID: "initial", Status: ActionCallStatusFailed, SideEffect: skill.SideEffectRead, SkillID: "reader", Action: "list", Error: "invalid field", Arguments: map[string]interface{}{"fields": "bad"}}
	checkpoint := checkpointTerminalAction(nil, initial, nil)
	correction := cloneActionCall(initial)
	correction.ID, correction.Status, correction.Error = "correction", ActionCallStatusReady, ""
	correction.Arguments["fields"] = "id"
	checkpoint, denied := admitToolFeedbackCorrection(checkpoint, correction)
	if denied != "" {
		t.Fatal(denied)
	}
	unrelated := &ActionCall{ID: "old-observation", Status: ActionCallStatusSucceeded, Output: map[string]interface{}{"result": "old"}}
	checkpoint = checkpointTerminalAction(checkpoint, unrelated, nil)
	if state, ok := ReadToolFeedbackCorrection(checkpoint); !ok || state.CorrectionsUsed != 1 {
		t.Fatal("unrelated success erased pending feedback")
	}
	correction.Status, correction.Output = ActionCallStatusSucceeded, map[string]interface{}{"result": "new"}
	checkpoint = checkpointTerminalAction(checkpoint, correction, nil)
	if _, active := ReadToolFeedbackCorrection(checkpoint); active || requiresFinalFailureExplanation(checkpoint) {
		t.Fatal("corrected success left failure authority")
	}
	// Even after clearing the chain, replay of an older failure cannot reopen it.
	checkpoint = checkpointTerminalAction(checkpoint, initial, map[string]interface{}{"idempotentReplay": true})
	if _, active := ReadToolFeedbackCorrection(checkpoint); active {
		t.Fatal("old failure replay granted another allowance")
	}
}

func TestToolFeedbackCounterSurvivesJSONAndCannotBeForged(t *testing.T) {
	call := &ActionCall{ID: "failed", Status: ActionCallStatusFailed, SideEffect: skill.SideEffectRead, Error: "invalid fields"}
	trusted := checkpointTerminalAction(nil, call, nil)
	correction := cloneActionCall(call)
	correction.ID, correction.Status, correction.Arguments = "corrected", ActionCallStatusReady, map[string]interface{}{"fields": "id"}
	trusted, _ = admitToolFeedbackCorrection(trusted, correction)
	bytes, err := json.Marshal(trusted)
	if err != nil {
		t.Fatal(err)
	}
	var persisted map[string]interface{}
	if err := json.Unmarshal(bytes, &persisted); err != nil {
		t.Fatal(err)
	}
	for _, proposed := range []map[string]interface{}{nil, {ToolFeedbackCorrectionCheckpointKey: map[string]interface{}{"correctionsUsed": -999, "message": "fake"}}} {
		merged := preserveKernelActionHistory(persisted, proposed)
		state, active := ReadToolFeedbackCorrection(merged)
		if !active || state.CorrectionsUsed != 1 || state.Message != "invalid fields" {
			t.Fatalf("model changed trusted feedback: %#v", merged)
		}
	}
	if _, active := ReadToolFeedbackCorrection(preserveKernelActionHistory(nil, trusted)); active {
		t.Fatal("model invented correction authority")
	}
}

func TestToolFeedbackUnchangedRequestIsNotDispatched(t *testing.T) {
	forActionLifecycleStores(t, func(t *testing.T, store KernelStore) {
		now := time.Now().UTC()
		catalog, scope := feedbackReadCatalog(t)
		run := feedbackCreateRun(t, store, scope, now)
		feedbackPropose(t, store, catalog, run, now, "initial", "bad", ActionDispositionAllow)
		dispatches := 0
		worker := NewActionWorker(store, catalog, CredentialResolverFunc(func(context.Context, CredentialResolutionRequest) (map[string]string, error) {
			return map[string]string{"token": "secret"}, nil
		}), ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
			dispatches++
			return nil, errors.New("invalid field selection")
		}))
		worker.now = func() time.Time { return now }
		if _, err := worker.RunOnce(t.Context(), scope, "action-worker", time.Minute); err != nil {
			t.Fatal(err)
		}
		run = feedbackClaim(t, store, scope, now)
		unchanged := feedbackPropose(t, store, catalog, run, now, "different-idempotency", "bad", ActionDispositionAllow)
		if unchanged.Call.Status != ActionCallStatusDenied || !requiresFinalFailureExplanation(unchanged.Run.Checkpoint) || !strings.Contains(recordedToolFailure(unchanged.Run.Checkpoint), "invalid field selection") {
			t.Fatalf("unchanged proposal escaped: %#v", unchanged)
		}
		result, err := worker.RunOnce(t.Context(), scope, "action-worker", time.Minute)
		if err != nil || result != nil || dispatches != 1 {
			t.Fatalf("unchanged request dispatched: %#v %v count=%d", result, err, dispatches)
		}
	})
}

func TestToolFeedbackCorrectionStillRequiresNormalApproval(t *testing.T) {
	forActionLifecycleStores(t, func(t *testing.T, store KernelStore) {
		now := time.Now().UTC()
		catalog, scope := feedbackReadCatalog(t)
		run := feedbackCreateRun(t, store, scope, now)
		feedbackPropose(t, store, catalog, run, now, "first", "bad", ActionDispositionAllow)
		worker := NewActionWorker(store, catalog, CredentialResolverFunc(func(context.Context, CredentialResolutionRequest) (map[string]string, error) {
			return map[string]string{"token": "secret"}, nil
		}), ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
			return nil, errors.New("invalid field")
		}))
		worker.now = func() time.Time { return now }
		if _, err := worker.RunOnce(t.Context(), scope, "action-worker", time.Minute); err != nil {
			t.Fatal(err)
		}
		run = feedbackClaim(t, store, scope, now)
		proposal := feedbackPropose(t, store, catalog, run, now, "corrected", "id", ActionDispositionRequireApproval)
		state, active := ReadToolFeedbackCorrection(proposal.Run.Checkpoint)
		if !active || state.CorrectionsUsed != 1 || proposal.Approval == nil || proposal.Call.Status != ActionCallStatusWaitingApproval || proposal.Run.Status != AgentRunStatusWaitingForApproval {
			t.Fatalf("correction bypassed approval: %#v", proposal)
		}
		call, err := store.ClaimNextAction(t.Context(), ActionClaim{Scope: scope, WorkerID: "action-worker", Now: now, LeaseDuration: time.Minute})
		if err != nil || call != nil {
			t.Fatalf("unapproved correction executable: %#v %v", call, err)
		}
	})
}

func TestToolFeedbackMutationRequiresKnownBeforeDispatchFailure(t *testing.T) {
	for _, test := range []struct {
		name  string
		phase ActionFailurePhase
		final bool
	}{
		{"known-preflight", ActionFailureBeforeDispatch, false},
		{"dispatched-uncertain", ActionFailureAfterDispatch, true},
		{"legacy-unknown", "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			call := &ActionCall{ID: "write", Status: ActionCallStatusFailed, SideEffect: skill.SideEffectExternal, Error: "provider disconnected", FailurePhase: test.phase}
			checkpoint := checkpointTerminalAction(nil, call, nil)
			if requiresFinalFailureExplanation(checkpoint) != test.final {
				t.Fatalf("unsafe mutation correction policy: %#v", checkpoint)
			}
			encoded, _ := json.Marshal(call)
			var persisted ActionCall
			if err := json.Unmarshal(encoded, &persisted); err != nil || persisted.FailurePhase != test.phase {
				t.Fatalf("failure phase lost during persistence: %s %v", encoded, err)
			}
		})
	}
}

func TestToolFeedbackFinalAnswerMustBeVisibleAndCannotEvadeBudget(t *testing.T) {
	run := &AgentRun{Checkpoint: checkpointTerminalAction(nil, &ActionCall{ID: "read", Status: ActionCallStatusFailed, SideEffect: skill.SideEffectRead, Error: "invalid fields"}, nil)}
	for _, test := range []struct {
		name string
		out  TurnOutcome
	}{
		{"empty", TurnOutcome{NextRunStatus: AgentRunStatusCompleted}},
		{"silent", TurnOutcome{NextRunStatus: AgentRunStatusCompleted, OutputSummary: "failed", RunOutput: map[string]interface{}{"silent": true}}},
		{"wait", TurnOutcome{NextRunStatus: AgentRunStatusWaitingForEvent, WakeCondition: &WakeCondition{Type: "timer"}}},
		{"fork", TurnOutcome{NextRunStatus: AgentRunStatusRunning, ProposedFork: &TurnForkProposal{}}},
		{"delegate", TurnOutcome{NextRunStatus: AgentRunStatusRunning, ProposedDelegation: &TurnDelegationProposal{}}},
		{"keep-going", TurnOutcome{NextRunStatus: AgentRunStatusRunning, OutputSummary: "checking"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateToolFeedbackCorrectionOutcome(run, &test.out); err == nil {
				t.Fatal("feedback budget evaded")
			}
		})
	}
	if err := validateToolFeedbackCorrectionOutcome(run, &TurnOutcome{NextRunStatus: AgentRunStatusCompleted, OutputSummary: "The service rejected that request."}); err != nil {
		t.Fatal(err)
	}
}

func TestToolFeedbackDeclinedCorrectionPublishesFinalCause(t *testing.T) {
	store := NewMemoryStore()
	catalog, scope := feedbackReadCatalog(t)
	now := time.Now().UTC()
	run := feedbackCreateRun(t, store, scope, now)
	feedbackPropose(t, store, catalog, run, now, "first", "bad", ActionDispositionAllow)
	worker := NewActionWorker(store, catalog, CredentialResolverFunc(func(context.Context, CredentialResolutionRequest) (map[string]string, error) {
		return map[string]string{"token": "secret"}, nil
	}), ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
		return nil, errors.New("invalid fields")
	}))
	worker.now = func() time.Time { return now }
	failed, err := worker.RunOnce(t.Context(), scope, "action-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	const answer = "The field selector was rejected. I couldn't fetch the records."
	host := &failureExplanationTestHost{respond: func(request HostedTurnRequest) (*HostedTurnResponse, error) {
		if len(request.Actions) != 1 {
			t.Fatal("correction lost normal governed action authority")
		}
		if request.Workspace != nil || len(request.WorkspaceOperations) != 0 || len(request.WorkspaceCredentials) != 0 {
			t.Fatal("correction escaped durable allowance through native tools")
		}
		if feedback, active := ReadToolFeedbackCorrection(request.ContinuationCheckpoint); !active || feedback.CorrectionsRemaining != 2 {
			t.Fatal("first feedback was missing")
		}
		return &HostedTurnResponse{APIVersion: HostedTurnAPIVersion, InvocationID: request.InvocationID, ModelProvider: "test", Model: "model", NextRunStatus: AgentRunStatusCompleted,
			OutputSummary: answer, RunOutput: map[string]interface{}{"summary": answer}}, nil
	}}
	runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{AgentID: "reader-agent", DefinitionID: "reader", DefinitionVersion: "1",
		Actions: []capability.ModelAction{{Name: "reader.list"}}, Workspace: &workspace.Authority{Workspace: workspace.DefaultSpec()},
		WorkspaceCredentials: map[string]capability.CredentialReference{"token": {Kind: "vault", ID: "workspace-secret"}}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := NewTurnCoordinator(store, store, store).Advance(t.Context(), AdvanceAgentRunRequest{Scope: scope, RunID: failed.Run.ID, WorkerID: "agent-worker"}, runner)
	if err != nil || result == nil || result.Run.Status != AgentRunStatusFailed || result.Run.Output["summary"] != answer || result.Run.Error != "invalid fields" {
		t.Fatalf("declined correction lost final answer: %#v %v", result, err)
	}
	if explanation, _ := result.Run.Checkpoint[FinalFailureExplanationCheckpointKey].(map[string]interface{}); explanation == nil || explanation["explained"] != true {
		t.Fatal("visible final explanation was not marked")
	}
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

func TestToolFeedbackAdmissionConflictDoesNotSpendAllowance(t *testing.T) {
	forActionLifecycleStores(t, func(t *testing.T, store KernelStore) {
		now := time.Now().UTC()
		catalog, scope := feedbackReadCatalog(t)
		run := feedbackCreateRun(t, store, scope, now)
		feedbackPropose(t, store, catalog, run, now, "first", "bad", ActionDispositionAllow)
		worker := NewActionWorker(store, catalog, CredentialResolverFunc(func(context.Context, CredentialResolutionRequest) (map[string]string, error) {
			return map[string]string{"token": "secret"}, nil
		}), ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
			return nil, errors.New("invalid fields")
		}))
		worker.now = func() time.Time { return now }
		failed, err := worker.RunOnce(t.Context(), scope, "action-worker", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		run = feedbackClaim(t, store, scope, now)
		wrapped := &feedbackProposalConflictStore{KernelStore: store, conflict: true}
		coordinator := NewActionCoordinator(store, wrapped, catalog, ActionPolicyEvaluatorFunc(func(context.Context, ActionPolicyInput) (ActionPolicyDecision, error) {
			return ActionPolicyDecision{Disposition: ActionDispositionAllow}, nil
		}))
		coordinator.now = func() time.Time { return now }
		request := ProposeActionRequest{Scope: scope, RunID: run.ID, WorkerID: "agent-worker", DeploymentID: "reader-agent", SkillID: "reader", SkillVersion: "1.0.0", Action: "list",
			Arguments: map[string]interface{}{"fields": "id"}, IdempotencyKey: "corrected"}
		if result, err := coordinator.Propose(t.Context(), request); !errors.Is(err, ErrRevisionConflict) || result != nil {
			t.Fatalf("missing proposal CAS conflict: %#v %v", result, err)
		}
		persisted, err := store.GetAgentRun(t.Context(), scope, failed.Run.ID)
		state, active := ReadToolFeedbackCorrection(persisted.Checkpoint)
		if err != nil || !active || state.CorrectionsUsed != 0 {
			t.Fatalf("failed admission spent retry: %#v %v", persisted, err)
		}
		result, err := coordinator.Propose(t.Context(), request)
		if err != nil || result == nil || !result.Created {
			t.Fatalf("corrected proposal after conflict: %#v %v", result, err)
		}
		state, active = ReadToolFeedbackCorrection(result.Run.Checkpoint)
		if !active || state.CorrectionsUsed != 1 || result.Run.Checkpoint[ToolFeedbackCorrectionCheckpointKey].(map[string]interface{})["admittedActionCallId"] != result.Call.ID {
			t.Fatal("admitted call and allowance were not committed together")
		}
	})
}
