package runtime

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/skill"
)

type workerBudgetPlanningRunner struct {
	planned BudgetUsage
	seen    chan BudgetUsage
}

func (r *workerBudgetPlanningRunner) PlanTurnBudget(context.Context, TurnExecutionContext) (BudgetUsage, error) {
	return r.planned, nil
}

func (r *workerBudgetPlanningRunner) RunTurn(_ context.Context, input TurnExecutionContext) (*TurnOutcome, error) {
	reservation := input.Run.BudgetReservations[input.Turn.ID].Usage
	r.seen <- reservation
	return &TurnOutcome{
		NextRunStatus: AgentRunStatusCompleted,
		OutputSummary: "bounded work complete",
		Usage:         TurnUsage{InputTokens: int(reservation.InputTokens), OutputTokens: int(reservation.OutputTokens)},
	}, nil
}

func TestAgentRunWorkerPreservesRunnerBudgetPlanning(t *testing.T) {
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "7"}
	run, err := NewPortfolioService(store).CreateAgentRun(t.Context(), CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "researcher"}, AssignedAgentID: "researcher",
		Goal: "Synthesize bounded evidence", Source: RunSourceManual,
		Budget: &BudgetPolicy{MaxTurns: 2, MaxInputTokens: 8000, MaxOutputTokens: 2000, MaxTotalTokens: 10000},
	})
	if err != nil {
		t.Fatal(err)
	}
	runner := &workerBudgetPlanningRunner{planned: BudgetUsage{InputTokens: 5000, OutputTokens: 1000}, seen: make(chan BudgetUsage, 1)}
	resolver := TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*TurnRunnerBinding, error) {
		return &TurnRunnerBinding{DefinitionID: "researcher", DefinitionVersion: "1", Runner: runner}, nil
	})
	pool, err := NewAgentRunWorkerPool(store, resolver, nil, AgentRunWorkerConfig{
		Scope: scope, AssignedAgentID: "researcher", Concurrency: 1, MaxTurnsPerClaim: 1,
		PollInterval: 10 * time.Millisecond, LeaseDuration: time.Second, TurnLeaseDuration: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.Start(ctx)
	defer pool.Stop()
	select {
	case reservation := <-runner.seen:
		if reservation.Turns != 1 || reservation.InputTokens != 5000 || reservation.OutputTokens != 1000 {
			t.Fatalf("worker discarded planned reservation: %#v", reservation)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not execute the bounded turn")
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		completed, getErr := NewPortfolioService(store).GetAgentRun(ctx, scope, run.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if completed.Status == AgentRunStatusCompleted {
			if completed.BudgetUsage.Turns != 1 || completed.BudgetUsage.InputTokens != 5000 || completed.BudgetUsage.OutputTokens != 1000 || len(completed.BudgetReservations) != 0 {
				t.Fatalf("planned reservation was not settled exactly: %#v", completed)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("bounded worker Run did not complete")
}

func TestAgentRunWorkerPoolAdvancesSleepsAndResumes(t *testing.T) {
	tests := []struct {
		name  string
		store func(*testing.T) KernelStore
	}{
		{name: "memory", store: func(*testing.T) KernelStore { return NewMemoryStore() }},
		{name: "sqlite", store: func(t *testing.T) KernelStore {
			store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "workers.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return store
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := test.store(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			scope := Scope{Kind: "local", ID: "test"}
			run, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
				Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent",
				Goal: "sleep once, then finish", Source: RunSourceObjective,
			})
			if err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			resolver := TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*TurnRunnerBinding, error) {
				return &TurnRunnerBinding{
					DefinitionID: "test-agent", DefinitionVersion: "1", ModelProvider: "fake", Model: "deterministic",
					Runner: TurnRunnerFunc(func(_ context.Context, input TurnExecutionContext) (*TurnOutcome, error) {
						call := calls.Add(1)
						if call == 1 {
							due := time.Now().Add(40 * time.Millisecond)
							return &TurnOutcome{
								NextRunStatus: AgentRunStatusSleeping, OutputSummary: "Waiting for the next turn",
								WakeCondition:          &WakeCondition{Type: "timer", WakeAt: &due, Reference: "next-turn"},
								ContinuationCheckpoint: map[string]interface{}{"step": float64(1)},
							}, nil
						}
						if input.Run.LastAppliedTurn != 1 || input.Run.Checkpoint["step"] != float64(1) {
							return nil, fmt.Errorf("second turn did not resume from its checkpoint: %#v", input.Run)
						}
						return &TurnOutcome{
							NextRunStatus: AgentRunStatusCompleted, OutputSummary: "Work complete",
							RunOutput: map[string]interface{}{"winner": "agent"},
						}, nil
					}),
				}, nil
			})
			pool, err := NewAgentRunWorkerPool(store, resolver, nil, AgentRunWorkerConfig{
				Scope: scope, AssignedAgentID: "agent", Concurrency: 1, MaxTurnsPerClaim: 1,
				PollInterval: 10 * time.Millisecond, LeaseDuration: time.Second, TurnLeaseDuration: time.Second,
			})
			if err != nil {
				t.Fatal(err)
			}
			pool.Start(ctx)
			defer pool.Stop()
			deadline := time.Now().Add(3 * time.Second)
			var completed *AgentRun
			for time.Now().Before(deadline) {
				completed, err = NewPortfolioService(store).GetAgentRun(ctx, scope, run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if completed.Status == AgentRunStatusCompleted {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if completed == nil || completed.Status != AgentRunStatusCompleted || completed.LastAppliedTurn != 2 ||
				completed.Output["winner"] != "agent" || calls.Load() != 2 {
				t.Fatalf("autonomous run did not complete across sleep: run=%#v calls=%d", completed, calls.Load())
			}
			turns, err := NewAgentTurnService(store, store).ListTurns(ctx, AgentTurnFilter{Scope: scope, RunID: run.ID})
			if err != nil {
				t.Fatal(err)
			}
			if len(turns) != 2 || turns[0].Sequence != 1 || turns[1].Sequence != 2 {
				t.Fatalf("unexpected durable turn history: %#v", turns)
			}
		})
	}
}

func TestAgentRunWorkerCompletesArtifactFreeHandoffFromTerminalChild(t *testing.T) {
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "7"}
	portfolio := NewPortfolioService(store)
	source, err := portfolio.CreateAgentRun(t.Context(), CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "developer"}, AssignedAgentID: "developer",
		Goal: "Build the feature", Source: RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	collaboration := NewCollaborationService(store)
	created, err := collaboration.CreateAgentRequest(t.Context(), CreateAgentRequestRequest{
		Scope: scope, Kind: AgentRequestKindHandoff, Requester: CollaborationParty{Type: OwnerTypeAgent, ID: "developer"},
		Recipient: CollaborationParty{Type: OwnerTypeAgent, ID: "marketing"}, SourceRunID: source.ID,
		Goal: "Draft the launch follow-up", AcceptanceCriteria: map[string]interface{}{"required": "one follow-up"},
		IdempotencyKey: "handoff-worker-completion",
	})
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := collaboration.RespondAgentRequest(t.Context(), RespondAgentRequestRequest{
		Scope: scope, RequestID: created.Request.ID, ExpectedRevision: created.Request.Revision,
		Decision: AgentRequestDecisionAccept, Principal: CollaborationParty{Type: OwnerTypeAgent, ID: "marketing"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resolver := TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*TurnRunnerBinding, error) {
		return &TurnRunnerBinding{DefinitionID: "marketing", DefinitionVersion: "1", ModelProvider: "fake", Model: "deterministic",
			Runner: TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
				return &TurnOutcome{NextRunStatus: AgentRunStatusCompleted, OutputSummary: "Launch follow-up ready",
					RunOutput: map[string]interface{}{"followUp": "Publish the evidence-backed launch note"}}, nil
			})}, nil
	})
	pool, err := NewAgentRunWorkerPool(store, resolver, nil, AgentRunWorkerConfig{
		Scope: scope, AssignedAgentID: "marketing", Concurrency: 1, MaxTurnsPerClaim: 1,
		PollInterval: 10 * time.Millisecond, LeaseDuration: time.Second, TurnLeaseDuration: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.Start(ctx)
	defer pool.Stop()
	deadline := time.Now().Add(3 * time.Second)
	var completed *AgentRequest
	for time.Now().Before(deadline) {
		completed, err = collaboration.GetAgentRequest(ctx, scope, created.Request.ID)
		if err != nil {
			t.Fatal(err)
		}
		if completed.Status == AgentRequestStatusCompleted {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if completed == nil || completed.Status != AgentRequestStatusCompleted || completed.ChildRunID != accepted.Child.ID ||
		completed.CompletionSummary != "Publish the evidence-backed launch note" || completed.AcceptanceEvidence["runOutput"] == nil {
		t.Fatalf("completed handoff = %#v", completed)
	}
	child, err := portfolio.GetAgentRun(ctx, scope, accepted.Child.ID)
	if err != nil {
		t.Fatal(err)
	}
	refreshedSource, err := portfolio.GetAgentRun(ctx, scope, source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if child.Status != AgentRunStatusCompleted || refreshedSource.Status != AgentRunStatusCompleted ||
		refreshedSource.Output["handoffRequestId"] != completed.ID {
		t.Fatalf("source=%#v child=%#v", refreshedSource, child)
	}
}

func TestAgentRunWorkerMaterializesOneGovernedAction(t *testing.T) {
	store := NewMemoryStore()
	catalog, scope := governedActionCatalog(t)
	if err := catalog.Bind(t.Context(), &skill.Binding{
		ID: "team-release-binding", Scope: capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "release-team",
		SkillID: "release", SkillVersion: "1.0.0", AllowedActions: []string{"deploy"}, MaximumRisk: skill.RiskLevelProduction,
		Credentials: map[string]skill.CredentialReference{"token": {Kind: "api-token", ID: "release-secret"}}, Revision: 1,
	}); err != nil {
		t.Fatal(err)
	}
	run, err := NewPortfolioService(store).CreateAgentRun(t.Context(), CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "release-team"}, AssignedAgentID: "release-agent",
		Goal: "deploy staging", Source: RunSourceObjective,
	})
	if err != nil {
		t.Fatal(err)
	}
	modelActions, err := catalog.ListModelActions(t.Context(), capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, "release-team")
	if err != nil {
		t.Fatal(err)
	}
	prepared := skill.PreparedRuntime{
		PreparationID: "sha256:" + strings.Repeat("a", 64), RuntimeID: "oci://runtime.test/release@sha256:" + strings.Repeat("b", 64),
		Revision: "sha256:" + strings.Repeat("c", 64), Adapter: "oci-builder/v1", OperatingSystem: "linux", Architecture: "amd64",
		Executables: []string{"release"},
	}
	resolver := TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*TurnRunnerBinding, error) {
		return &TurnRunnerBinding{
			DeploymentID: "release-agent", ActionDeploymentID: "release-team", DefinitionID: "release-agent", DefinitionVersion: "1", ModelActions: modelActions,
			PreparedRuntimes: []PreparedSkillRuntime{{
				DeploymentID: modelActions[0].DeploymentID, BindingID: modelActions[0].BindingID, BindingRevision: modelActions[0].BindingRevision,
				SkillID: modelActions[0].SkillID, SkillVersion: modelActions[0].Version, Runtime: prepared,
			}},
			Runner: TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
				return &TurnOutcome{
					NextRunStatus: AgentRunStatusRunning, OutputSummary: "Proposed staging deployment",
					ContinuationCheckpoint: map[string]interface{}{"actionInputs": map[string]interface{}{
						"deploy": map[string]interface{}{"environment": "staging"},
					}},
					ProposedActions: []TurnAction{{
						Type: "skill_action", Capability: "release.deploy", Summary: "Deploy release to staging", InputRef: "/actionInputs/deploy",
						EvidenceRefs:    []string{"artifact:release-plan"},
						PreparedRuntime: &skill.PreparedRuntime{RuntimeID: "model-controlled-runtime"},
					}},
				}, nil
			}),
		}, nil
	})
	pool, err := NewAgentRunWorkerPool(store, resolver, nil, AgentRunWorkerConfig{
		Scope: scope, AssignedAgentID: "release-agent", Concurrency: 1, MaxTurnsPerClaim: 1,
		PollInterval: 5 * time.Millisecond, LeaseDuration: time.Second, TurnLeaseDuration: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	pool.SetActionCoordinator(NewActionCoordinator(store, store, catalog, ActionPolicyEvaluatorFunc(func(context.Context, ActionPolicyInput) (ActionPolicyDecision, error) {
		return ActionPolicyDecision{Disposition: ActionDispositionAllow, Reason: "staging is allowed"}, nil
	})))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.Start(ctx)
	defer pool.Stop()
	deadline := time.Now().Add(2 * time.Second)
	var waiting *AgentRun
	for time.Now().Before(deadline) {
		waiting, err = store.GetAgentRun(t.Context(), scope, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if waiting.Status == AgentRunStatusWaitingForDependency {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if waiting == nil || waiting.Status != AgentRunStatusWaitingForDependency || waiting.WakeCondition == nil {
		t.Fatalf("run did not wait on governed action: %#v", waiting)
	}
	calls, err := store.ListActionCalls(t.Context(), ActionFilter{Scope: scope, RunID: run.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].Status != ActionCallStatusReady || calls[0].DeploymentID != "release-team" || calls[0].SkillID != "release" ||
		calls[0].Action != "deploy" || calls[0].Arguments["environment"] != "staging" || calls[0].IdempotencyKey == "" ||
		calls[0].PreparedRuntime == nil || calls[0].PreparedRuntime.RuntimeID != prepared.RuntimeID ||
		len(calls[0].EvidenceRefs) != 1 || calls[0].EvidenceRefs[0] != "artifact:release-plan" {
		t.Fatalf("governed action mismatch: %#v", calls)
	}
	turns, err := NewAgentTurnService(store, store).ListTurns(t.Context(), AgentTurnFilter{Scope: scope, RunID: run.ID})
	if err != nil || len(turns) != 1 || turns[0].RequestedActions[0].Capability != "release.deploy" ||
		turns[0].RequestedActions[0].PreparedRuntime == nil || turns[0].RequestedActions[0].PreparedRuntime.RuntimeID != prepared.RuntimeID {
		t.Fatalf("durable proposal Turn mismatch: %#v, %v", turns, err)
	}
}

func TestAgentRunWorkerResumesCompletedIdempotentActionReplay(t *testing.T) {
	store := NewMemoryStore()
	catalog, scope := governedActionCatalog(t)
	now := time.Now().UTC()
	claimed := claimedActionRun(t, store, scope, now, "worker")
	actions := NewActionCoordinator(store, store, catalog, ActionPolicyEvaluatorFunc(func(context.Context, ActionPolicyInput) (ActionPolicyDecision, error) {
		return ActionPolicyDecision{Disposition: ActionDispositionDeny, Reason: "test denial"}, nil
	}))
	actions.now = func() time.Time { return now }
	first, err := actions.Propose(t.Context(), ProposeActionRequest{
		Scope: scope, RunID: claimed.ID, TurnID: "turn-1", WorkerID: "worker", DeploymentID: "release-agent",
		BindingID: "release-binding", BindingRevision: 1, SkillID: "release", SkillVersion: "1.0.0", Action: "deploy",
		Arguments: map[string]interface{}{"environment": "staging"}, IdempotencyKey: "deploy-staging", Summary: "Deploy staging",
	})
	if err != nil || first == nil || first.Call == nil || first.Run == nil || first.Call.Status != ActionCallStatusDenied {
		t.Fatalf("first proposal = %#v, %v", first, err)
	}
	claimed, err = store.ClaimNextAgentRun(t.Context(), AgentRunClaim{
		Scope: scope, WorkerID: "worker", Now: now.Add(time.Second), LeaseDuration: time.Minute, AgingInterval: time.Minute,
	})
	if err != nil || claimed == nil {
		t.Fatalf("reclaimed Run = %#v, %v", claimed, err)
	}
	modelActions, err := catalog.ListModelActions(t.Context(), capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, "release-agent")
	if err != nil {
		t.Fatal(err)
	}
	pool, err := NewAgentRunWorkerPool(store, TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*TurnRunnerBinding, error) {
		return nil, errors.New("unused")
	}), nil, AgentRunWorkerConfig{
		Scope: scope, AssignedAgentID: "release-agent", Concurrency: 1, MaxTurnsPerClaim: 1,
		PollInterval: time.Second, LeaseDuration: time.Minute, TurnLeaseDuration: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	pool.SetActionCoordinator(actions)
	turn := &AgentTurn{
		ID: "turn-2", RequestedActions: []TurnAction{{
			Type: "skill_action", Capability: "release.deploy", Summary: "Deploy staging",
			IdempotencyKey: "deploy-staging", InputRef: "/actionInputs/call",
		}},
		ContinuationCheckpoint: map[string]interface{}{"actionInputs": map[string]interface{}{
			"call": map[string]interface{}{"environment": "staging"},
		}},
	}
	resumed, err := pool.materializeTurnAction(t.Context(), "worker", claimed, turn, &TurnRunnerBinding{
		DeploymentID: "release-agent", ActionDeploymentID: "release-agent", ModelActions: modelActions,
	})
	if err != nil {
		t.Fatal(err)
	}
	last, _ := resumed.Checkpoint["lastAction"].(map[string]interface{})
	if resumed.Status != AgentRunStatusQueued || fmt.Sprint(last["actionCallId"]) != first.Call.ID || last["idempotentReplay"] != true {
		t.Fatalf("replayed Run = %#v", resumed)
	}
	calls, err := store.ListActionCalls(t.Context(), ActionFilter{Scope: scope, RunID: claimed.ID})
	if err != nil || len(calls) != 1 {
		t.Fatalf("replay created duplicate calls = %#v, %v", calls, err)
	}
	events, err := store.ListActivity(t.Context(), ActivityFilter{Scope: scope, RunID: claimed.ID, EventTypes: []string{"action.replayed"}})
	if err != nil || len(events) != 1 {
		t.Fatalf("replay activity = %#v, %v", events, err)
	}
	claimed, err = store.ClaimNextAgentRun(t.Context(), AgentRunClaim{
		Scope: scope, WorkerID: "worker", Now: now.Add(2 * time.Second), LeaseDuration: time.Minute, AgingInterval: time.Minute,
	})
	if err != nil || claimed == nil {
		t.Fatalf("claimed conflicting replay Run = %#v, %v", claimed, err)
	}
	turn.ID = "turn-3"
	turn.ContinuationCheckpoint = map[string]interface{}{"actionInputs": map[string]interface{}{
		"call": map[string]interface{}{"environment": "production"},
	}}
	if _, err := pool.materializeTurnAction(t.Context(), "worker", claimed, turn, &TurnRunnerBinding{
		DeploymentID: "release-agent", ActionDeploymentID: "release-agent", ModelActions: modelActions,
	}); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("conflicting replay error = %v", err)
	}
}

func TestAgentRunWorkerRequeuesConversationMaterializationFailureForProjection(t *testing.T) {
	store := NewMemoryStore()
	ctx := t.Context()
	scope := Scope{Kind: "tenant", ID: "proposal-failure"}
	run, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Kind: RunKindConversation, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"},
		AssignedAgentID: "agent", Goal: "Pause the draft objective", Source: RunSourceChat,
	})
	if err != nil {
		t.Fatal(err)
	}
	const workerID = "conversation-worker"
	claimed, err := store.ClaimNextAgentRun(ctx, AgentRunClaim{
		Scope: scope, Kind: RunKindConversation, WorkerID: workerID, Now: time.Now(),
		LeaseDuration: time.Minute, AgingInterval: time.Minute,
	})
	if err != nil || claimed == nil || claimed.ID != run.ID {
		t.Fatalf("claimed Run = %#v, %v", claimed, err)
	}
	pool, err := NewAgentRunWorkerPool(store, TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*TurnRunnerBinding, error) {
		return nil, nil
	}), nil, AgentRunWorkerConfig{Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	turn := &AgentTurn{ID: "turn-pause", ContinuationCheckpoint: map[string]interface{}{
		"actionInputs": map[string]interface{}{"call": map[string]interface{}{"objectiveId": "objective-draft", "expectedRevision": float64(2)}},
	}, RequestedActions: []TurnAction{{
		Type: "skill_action", Capability: "openseal.objectives.pause", InputRef: "/actionInputs/call",
		BindingID: "bundled:objectives", BindingRevision: 1,
	}}}
	pool.failMaterialization(ctx, workerID, claimed, turn, fmt.Errorf("%w: draft -> paused", ErrInvalidObjectiveTransition))

	requeued, err := store.GetAgentRun(ctx, scope, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	last, _ := requeued.Checkpoint["lastAction"].(map[string]interface{})
	if requeued.Status != AgentRunStatusQueued || requeued.Error != "" || requeued.LeaseOwner != "" ||
		fmt.Sprint(last["status"]) != governedActionProposalFailedStatus || fmt.Sprint(last["action"]) != ObjectiveActionPause {
		t.Fatalf("requeued proposal failure = %#v", requeued)
	}
	events, err := store.ListActivity(ctx, ActivityFilter{Scope: scope, RunID: run.ID})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.EventType == "action.materialization_failed" {
			found = true
			if reason := fmt.Sprint(event.Payload["reason"]); strings.Contains(reason, "<nil>") || reason == "" {
				t.Fatalf("unsafe materialization failure reason = %q", reason)
			}
		}
	}
	if !found {
		t.Fatalf("materialization failure activity missing: %#v", events)
	}
}

func TestAgentRunWorkerRequeuesAgentProposalFailureWithinBoundedRecovery(t *testing.T) {
	store := NewMemoryStore()
	ctx := t.Context()
	scope := Scope{Kind: "tenant", ID: "agent-proposal-recovery"}
	run, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Kind: RunKindAgentWork, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"},
		AssignedAgentID: "agent", Goal: "Post one reviewed comment", Source: RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	const workerID = "agent-worker"
	claimed, err := store.ClaimNextAgentRun(ctx, AgentRunClaim{
		Scope: scope, Kind: RunKindAgentWork, WorkerID: workerID, Now: time.Now(),
		LeaseDuration: time.Minute, AgingInterval: time.Minute,
	})
	if err != nil || claimed == nil || claimed.ID != run.ID {
		t.Fatalf("claimed Run = %#v, %v", claimed, err)
	}
	pool, err := NewAgentRunWorkerPool(store, TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*TurnRunnerBinding, error) {
		return nil, nil
	}), nil, AgentRunWorkerConfig{Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	turn := &AgentTurn{ID: "turn-commit", ContinuationCheckpoint: map[string]interface{}{
		"actionInputs": map[string]interface{}{"call": map[string]interface{}{"target": "s4:e9"}},
	}, RequestedActions: []TurnAction{{
		Type: "skill_action", Capability: "skill-browser.camoufox-commit", Summary: "Publish comment", InputRef: "/actionInputs/call",
	}}}
	pool.failMaterialization(ctx, workerID, claimed, turn, errors.New("target requires a current observation"))

	requeued, err := store.GetAgentRun(ctx, scope, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	recovery, _ := requeued.Checkpoint[proposalRecoveryCheckpointKey].(map[string]interface{})
	if requeued.Status != AgentRunStatusQueued || requeued.Error != "" || requeued.LeaseOwner != "" ||
		fmt.Sprint(recovery["attempt"]) != "1" || recovery["capability"] != "skill-browser.camoufox-commit" ||
		recovery["error"] != "target requires a current observation" {
		t.Fatalf("requeued proposal recovery = %#v", requeued)
	}
	second, ok := checkpointGovernedAgentProposalFailure(requeued, turn, "target still requires a current observation")
	if !ok || fmt.Sprint(second[proposalRecoveryCheckpointKey].(map[string]interface{})["attempt"]) != "2" {
		t.Fatalf("second proposal recovery = %#v, ok=%v", second, ok)
	}
	requeued.Checkpoint = second
	if _, ok = checkpointGovernedAgentProposalFailure(requeued, turn, "target still invalid"); ok {
		t.Fatal("proposal recovery exceeded its bounded allowance")
	}
}

func TestAgentRunWorkerMaterializesDurableFork(t *testing.T) {
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "fork-worker"}
	run, err := NewPortfolioService(store).CreateAgentRun(t.Context(), CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent", Goal: "fork", Source: RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	resolver := TurnRunnerResolverFunc(func(_ context.Context, run *AgentRun) (*TurnRunnerBinding, error) {
		return &TurnRunnerBinding{Runner: TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
			if run.ParentRunID != "" {
				due := time.Now().Add(time.Hour)
				return &TurnOutcome{NextRunStatus: AgentRunStatusSleeping, WakeCondition: &WakeCondition{Type: "timer", WakeAt: &due}, OutputSummary: "branch parked"}, nil
			}
			return &TurnOutcome{
				NextRunStatus: AgentRunStatusRunning, OutputSummary: "fork proposed", ContinuationCheckpoint: map[string]interface{}{"fork": "pending"},
				ProposedFork: &TurnForkProposal{
					ForkID: "wave", Policy: RunDependencyPolicy{Mode: FanInModeAll, FailureMode: DependencyFailureFailFast},
					Branches: []RunForkBranch{{ID: "a", Goal: "A", Checkpoint: map[string]interface{}{"branch": "a"}}, {ID: "b", Goal: "B", Checkpoint: map[string]interface{}{"branch": "b"}}},
				},
			}, nil
		})}, nil
	})
	pool, err := NewAgentRunWorkerPool(store, resolver, nil, AgentRunWorkerConfig{
		Scope: scope, AssignedAgentID: "agent", Concurrency: 1, MaxTurnsPerClaim: 1,
		PollInterval: 5 * time.Millisecond, LeaseDuration: time.Second, TurnLeaseDuration: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.Start(ctx)
	defer pool.Stop()
	deadline := time.Now().Add(2 * time.Second)
	var waiting *AgentRun
	for time.Now().Before(deadline) {
		waiting, err = store.GetAgentRun(t.Context(), scope, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if waiting.Status == AgentRunStatusWaitingForDependency {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if waiting == nil || waiting.Status != AgentRunStatusWaitingForDependency || waiting.WakeCondition == nil || waiting.Checkpoint["fork"] != "pending" {
		t.Fatalf("source=%#v", waiting)
	}
	group, err := NewDependencyCoordinator(store).GetRunDependencyGroup(t.Context(), scope, waiting.WakeCondition.Reference)
	if err != nil || group.ExpectedCount != 2 || group.Policy.Mode != FanInModeAll {
		t.Fatalf("group=%#v error=%v", group, err)
	}
	edges, err := NewDependencyCoordinator(store).ListRunDependencies(t.Context(), scope, group.ID)
	if err != nil || len(edges) != 2 {
		t.Fatalf("edges=%#v error=%v", edges, err)
	}
	turns, err := NewAgentTurnService(store, store).ListTurns(t.Context(), AgentTurnFilter{Scope: scope, RunID: run.ID})
	if err != nil || len(turns) != 1 || turns[0].RequestedFork == nil || turns[0].RequestedFork.ForkID != "wave" {
		t.Fatalf("turns=%#v error=%v", turns, err)
	}
}

func TestAgentRunWorkersExecuteJoinAllAndJoinAnyConcurrently(t *testing.T) {
	for _, mode := range []runbook.JoinMode{runbook.JoinAll, runbook.JoinAny} {
		t.Run(string(mode), func(t *testing.T) {
			store := NewMemoryStore()
			scope := Scope{Kind: "tenant", ID: "fork-" + string(mode)}
			steps := map[string]runbook.Step{
				"fork": {Kind: runbook.StepFork, Fork: &runbook.ForkStep{Branches: map[string]string{"fast": "fast", "slow": "slow"}, Join: "join"}},
				"fast": {Kind: runbook.StepTransform, Transform: &runbook.TransformStep{Assignments: map[string]runbook.Value{"/state/fast": runbookLiteral(true)}, Next: "join"}},
				"slow": {Kind: runbook.StepTransform, Transform: &runbook.TransformStep{Assignments: map[string]runbook.Value{"/state/slow": runbookLiteral(true)}, Next: "join"}},
				"join": {Kind: runbook.StepJoin, Join: &runbook.JoinStep{Fork: "fork", Mode: mode, Next: "done"}},
				"done": {Kind: runbook.StepEnd, End: &runbook.EndStep{Outputs: map[string]runbook.Value{"state": {Ref: "/state"}}}},
			}
			if mode == runbook.JoinAny {
				steps["slow"] = runbook.Step{Kind: runbook.StepWait, Wait: &runbook.WaitStep{Duration: time.Hour, Next: "join"}}
			}
			definition := &runbook.Definition{APIVersion: runbook.APIVersion, ID: "concurrent", Version: "1", Name: "Concurrent", Entrypoints: map[string]string{"manual": "fork"}, Steps: steps}
			runner, err := NewRunbookTurnRunner(definition, "manual")
			if err != nil {
				t.Fatal(err)
			}
			parent, err := NewPortfolioService(store).CreateAgentRun(t.Context(), CreateAgentRunRequest{
				Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent", Goal: "concurrent", Source: RunSourceManual,
			})
			if err != nil {
				t.Fatal(err)
			}
			pool, err := NewAgentRunWorkerPool(store, TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*TurnRunnerBinding, error) {
				return &TurnRunnerBinding{DefinitionID: "concurrent", DefinitionVersion: "1", Runner: runner}, nil
			}), nil, AgentRunWorkerConfig{
				Scope: scope, AssignedAgentID: "agent", Concurrency: 4, MaxTurnsPerClaim: 1,
				PollInterval: 5 * time.Millisecond, LeaseDuration: time.Second, TurnLeaseDuration: time.Second,
			})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			pool.Start(ctx)
			defer pool.Stop()
			deadline := time.Now().Add(3 * time.Second)
			var completed *AgentRun
			for time.Now().Before(deadline) {
				completed, err = store.GetAgentRun(t.Context(), scope, parent.ID)
				if err != nil {
					t.Fatal(err)
				}
				if completed.Status == AgentRunStatusCompleted || completed.Status == AgentRunStatusFailed {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			if completed == nil || completed.Status != AgentRunStatusCompleted {
				t.Fatalf("parent=%#v", completed)
			}
			state := completed.Output["state"].(map[string]interface{})
			if state["fast"] != true || mode == runbook.JoinAll && state["slow"] != true {
				t.Fatalf("state=%#v", state)
			}
			children, err := store.ListAgentRuns(t.Context(), AgentRunFilter{Scope: scope, ParentRunID: parent.ID, Limit: 10})
			if err != nil || len(children) != 2 || children[0].ID == children[1].ID {
				t.Fatalf("children=%#v error=%v", children, err)
			}
			if mode == runbook.JoinAny {
				cancellationDeadline := time.Now().Add(2 * time.Second)
				canceled := countAgentRunsWithStatus(children, AgentRunStatusCanceled)
				for canceled != 1 && time.Now().Before(cancellationDeadline) {
					time.Sleep(5 * time.Millisecond)
					children, err = store.ListAgentRuns(t.Context(), AgentRunFilter{Scope: scope, ParentRunID: parent.ID, Limit: 10})
					if err != nil || len(children) != 2 {
						t.Fatalf("join_any children=%#v error=%v", children, err)
					}
					canceled = countAgentRunsWithStatus(children, AgentRunStatusCanceled)
				}
				if canceled != 1 {
					t.Fatalf("join_any children=%#v", children)
				}
			}
		})
	}
}

func countAgentRunsWithStatus(runs []*AgentRun, status AgentRunStatus) int {
	count := 0
	for _, run := range runs {
		if run.Status == status {
			count++
		}
	}
	return count
}

func TestAgentRunWorkersExecuteDurableDelegation(t *testing.T) {
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "delegation"}
	definition := &runbook.Definition{APIVersion: runbook.APIVersion, ID: "parent", Version: "1", Name: "Parent", Entrypoints: map[string]string{"manual": "delegate"}, Steps: map[string]runbook.Step{
		"delegate": {Kind: runbook.StepDelegate, Delegate: &runbook.DelegateStep{
			AgentID: runbookLiteral("specialist"), Goal: runbookLiteral("Analyze the release"),
			Context: map[string]runbook.Value{"release": runbookLiteral("2026.07")}, Mode: runbook.DelegateReason, ResultPath: "/steps/delegate", Timeout: time.Minute,
			Budget: &runbook.BudgetAllocation{MaxTurns: 2, MaxDurationMS: 60000}, Next: "done",
		}},
		"done": {Kind: runbook.StepEnd, End: &runbook.EndStep{Outputs: map[string]runbook.Value{"answer": {Ref: "/steps/delegate/answer"}}}},
	}}
	parentRunner, err := NewRunbookTurnRunner(definition, "manual")
	if err != nil {
		t.Fatal(err)
	}
	parent, err := NewPortfolioService(store).CreateAgentRun(t.Context(), CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "manager"}, AssignedAgentID: "manager", Goal: "delegate", Source: RunSourceManual,
		Budget: &BudgetPolicy{MaxTurns: 6, MaxDurationMS: 180000},
	})
	if err != nil {
		t.Fatal(err)
	}
	var collaborationResultObserved atomic.Bool
	resolver := TurnRunnerResolverFunc(func(_ context.Context, run *AgentRun) (*TurnRunnerBinding, error) {
		if run.Source == RunSourceRequestDecision {
			if run.AssignedAgentID != "specialist" || run.Context[AgentRequestInboxContextKey] == nil {
				return nil, fmt.Errorf("request decision run mismatch: %#v", run)
			}
			return &TurnRunnerBinding{DefinitionID: "specialist", DefinitionVersion: "1", Runner: TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
				return &TurnOutcome{
					NextRunStatus: AgentRunStatusCompleted,
					OutputSummary: "Request accepted",
					RunOutput: map[string]interface{}{
						AgentRequestDecisionOutputKey: map[string]interface{}{
							"decision": string(AgentRequestDecisionAccept),
							"message":  "The request is clear and relevant.",
						},
					},
				}, nil
			})}, nil
		}
		if run.AssignedAgentID == "manager" {
			return &TurnRunnerBinding{DefinitionID: "manager", DefinitionVersion: "1", Runner: TurnRunnerFunc(func(ctx context.Context, input TurnExecutionContext) (*TurnOutcome, error) {
				if results, _ := input.Run.Output["collaborationResults"].(map[string]interface{}); len(results) > 0 {
					for _, raw := range results {
						result, _ := raw.(map[string]interface{})
						output, _ := result["output"].(map[string]interface{})
						if output["answer"] != "ship" {
							return nil, fmt.Errorf("delegated output mismatch: %#v", results)
						}
						if output["collaborationResults"] != nil {
							return nil, fmt.Errorf("delegated output contains recipient lifecycle metadata: %#v", output)
						}
						collaborationResultObserved.Store(true)
					}
					if groups, _ := input.Run.Output["dependencyGroups"].(map[string]interface{}); len(groups) != 0 {
						return nil, fmt.Errorf("single delegation created parallel dependency groups: %#v", groups)
					}
				}
				return parentRunner.RunTurn(ctx, input)
			})}, nil
		}
		if run.AssignedAgentID != "specialist" || run.Context["release"] != "2026.07" || run.Context[DelegationModeContextKey] != string(runbook.DelegateReason) || run.Goal != "Analyze the release" {
			return nil, fmt.Errorf("delegated child mismatch: %#v", run)
		}
		return &TurnRunnerBinding{DefinitionID: "specialist", DefinitionVersion: "1", Runner: TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
			return &TurnOutcome{NextRunStatus: AgentRunStatusCompleted, OutputSummary: "Analysis complete", RunOutput: map[string]interface{}{"answer": "ship"}}, nil
		})}, nil
	})
	pool, err := NewAgentRunWorkerPool(store, resolver, nil, AgentRunWorkerConfig{
		Scope: scope, Concurrency: 2, MaxTurnsPerClaim: 1,
		PollInterval: 5 * time.Millisecond, LeaseDuration: time.Second, TurnLeaseDuration: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.Start(ctx)
	defer pool.Stop()
	deadline := time.Now().Add(3 * time.Second)
	var completed *AgentRun
	for time.Now().Before(deadline) {
		completed, err = store.GetAgentRun(t.Context(), scope, parent.ID)
		if err != nil {
			t.Fatal(err)
		}
		if completed.Status == AgentRunStatusCompleted || completed.Status == AgentRunStatusFailed {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if completed == nil || completed.Status != AgentRunStatusCompleted || completed.Output["answer"] != "ship" {
		events, _ := store.ListActivity(t.Context(), ActivityFilter{Scope: scope, RunID: parent.ID, Limit: 50})
		runs, _ := store.ListAgentRuns(t.Context(), AgentRunFilter{Scope: scope, ParentRunID: parent.ID, Limit: 50})
		requests, _ := store.ListAgentRequests(t.Context(), AgentRequestFilter{Scope: scope, SourceRunID: parent.ID, Limit: 50})
		var latest *ActivityEvent
		if len(events) > 0 {
			latest = events[len(events)-1]
		}
		var child, request interface{}
		if len(runs) > 0 {
			child = *runs[0]
		}
		if len(requests) > 0 {
			request = *requests[0]
		}
		t.Fatalf("parent=%#v latest=%#v child=%#v request=%#v", completed, latest, child, request)
	}
	if !collaborationResultObserved.Load() {
		t.Fatal("parent did not observe the durable AgentRequest child output")
	}
	children, err := store.ListAgentRuns(t.Context(), AgentRunFilter{Scope: scope, ParentRunID: parent.ID, Limit: 10})
	if err != nil || len(children) != 2 {
		t.Fatalf("children=%#v error=%v", children, err)
	}
	var decision, delegated *AgentRun
	for _, child := range children {
		switch child.Source {
		case RunSourceRequestDecision:
			decision = child
		case RunSourceRequest:
			delegated = child
		}
	}
	if decision == nil || decision.AssignedAgentID != "specialist" || decision.Status != AgentRunStatusCompleted ||
		delegated == nil || delegated.AssignedAgentID != "specialist" || delegated.Status != AgentRunStatusCompleted {
		t.Fatalf("children=%#v", children)
	}
	if delegated.Budget == nil || delegated.Budget.MaxTurns != 2 || delegated.Budget.MaxDurationMS != 60000 || len(completed.BudgetAllocations) != 1 {
		t.Fatalf("budgeted parent=%#v children=%#v", completed, children)
	}
	turns, err := store.ListAgentTurns(t.Context(), AgentTurnFilter{Scope: scope, RunID: parent.ID})
	if err != nil || len(turns) != 2 || turns[0].RequestedDelegation == nil || turns[0].RequestedDelegation.AssignedAgentID != "specialist" {
		t.Fatalf("turns=%#v error=%v", turns, err)
	}
	requests, err := store.ListAgentRequests(t.Context(), AgentRequestFilter{Scope: scope, SourceRunID: parent.ID, Limit: 10})
	if err != nil || len(requests) != 1 || requests[0].Status != AgentRequestStatusCompleted ||
		requests[0].AcceptancePolicy != AgentRequestAcceptanceRecipientReview ||
		requests[0].Requester != (CollaborationParty{Type: OwnerTypeAgent, ID: "manager"}) ||
		requests[0].Recipient != (CollaborationParty{Type: OwnerTypeAgent, ID: "specialist"}) {
		t.Fatalf("requests=%#v error=%v", requests, err)
	}
}

func TestDelegatedRunbookOriginPreservesImmutableIdentity(t *testing.T) {
	origin := delegatedRunbookOrigin(map[string]interface{}{
		"runbookDefinitionId": "hourly-scan", "runbookDefinitionVersion": "2.0.2",
		"runbookActivationId": "activation-one", "runbookTriggerId": "hourly",
	})
	if origin["runbookDefinitionId"] != "hourly-scan" || origin["runbookDefinitionVersion"] != "2.0.2" ||
		origin["runbookActivationId"] != "activation-one" || origin["runbookTriggerId"] != "hourly" {
		t.Fatalf("delegated Runbook origin = %#v", origin)
	}
	if delegatedRunbookOrigin(map[string]interface{}{"runbookDefinitionId": "hourly-scan"}) != nil {
		t.Fatal("incomplete Runbook origin was propagated")
	}
}

func TestAgentRunWorkerRecoversPendingDelegationMaterializationIdempotently(t *testing.T) {
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "delegation-recovery"}
	source, err := NewPortfolioService(store).CreateAgentRun(t.Context(), CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "manager"}, AssignedAgentID: "manager",
		Goal: "delegate safely", Source: RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	proposal := &TurnDelegationProposal{
		StepID: "analyze", AssignedAgentID: "specialist", Goal: "Analyze evidence",
		Context:    map[string]interface{}{"evidence": "artifact:one"},
		Checkpoint: map[string]interface{}{"phase": "analysis"},
		Budget:     &BudgetPolicy{MaxTurns: 2},
		Mode:       "reason",
	}
	key := strings.Join([]string{"delegation", source.ID, proposal.StepID}, ":")
	service := NewCollaborationService(store)
	pending, err := service.CreateAgentRequest(t.Context(), CreateAgentRequestRequest{
		ID: stableCollaborationID(scope, key, "request"), Scope: scope, Kind: AgentRequestKindRequest,
		Requester:   CollaborationParty{Type: OwnerTypeAgent, ID: "manager"},
		Recipient:   CollaborationParty{Type: OwnerTypeAgent, ID: "specialist"},
		SourceRunID: source.ID, Goal: proposal.Goal,
		SharedContext:   map[string]interface{}{"evidence": "artifact:one", DelegationModeContextKey: "reason"},
		ChildCheckpoint: proposal.Checkpoint, BudgetAllocation: proposal.Budget, IdempotencyKey: key,
		AcceptancePolicy: AgentRequestAcceptancePreauthorized,
	})
	if err != nil || pending.Request.Status != AgentRequestStatusPending {
		t.Fatalf("pending request = %#v, %v", pending, err)
	}
	pool, err := NewAgentRunWorkerPool(store, TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*TurnRunnerBinding, error) {
		return nil, nil
	}), nil, AgentRunWorkerConfig{Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	turn := &AgentTurn{ID: "turn", RequestedDelegation: proposal}
	waiting, err := pool.materializeTurnDelegation(t.Context(), "worker", source, turn)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := pool.materializeTurnDelegation(t.Context(), "worker", waiting, turn)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.ID != source.ID || replayed.Status != AgentRunStatusWaitingForAgent {
		t.Fatalf("replayed source = %#v", replayed)
	}
	reconciled, err := pool.requestInbox.Reconcile(t.Context(), scope, "specialist")
	if err != nil || reconciled.RequestsAccepted != 1 {
		t.Fatalf("reconciled inbox = %#v, %v", reconciled, err)
	}
	replayed, err = store.GetAgentRun(t.Context(), scope, source.ID)
	if err != nil || replayed.Status != AgentRunStatusWaitingForDependency {
		t.Fatalf("accepted source = %#v, %v", replayed, err)
	}
	requests, err := store.ListAgentRequests(t.Context(), AgentRequestFilter{Scope: scope, SourceRunID: source.ID, Limit: 10})
	if err != nil || len(requests) != 1 || requests[0].Status != AgentRequestStatusAccepted {
		t.Fatalf("requests = %#v, %v", requests, err)
	}
	children, err := store.ListAgentRuns(t.Context(), AgentRunFilter{Scope: scope, ParentRunID: source.ID, Limit: 10})
	if err != nil || len(children) != 1 || children[0].Checkpoint["phase"] != "analysis" ||
		children[0].Context[DelegationModeContextKey] != "reason" {
		t.Fatalf("children = %#v, %v", children, err)
	}
}

func TestAgentRunWorkerCompletesPartialBoundedDelegationBudget(t *testing.T) {
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "partial-delegation-budget"}
	source, err := NewPortfolioService(store).CreateAgentRun(t.Context(), CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "manager"}, AssignedAgentID: "manager",
		Goal: "delegate bounded browser work", Source: RunSourceSchedule,
		Budget: &BudgetPolicy{
			MaxAttempts: 3, MaxTurns: 30, MaxInputTokens: 120000, MaxOutputTokens: 60000,
			MaxTotalTokens: 180000, MaxCostMicros: 900000, MaxDurationMS: 600000, MaxActions: 60,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := NewAgentRunWorkerPool(store, TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*TurnRunnerBinding, error) {
		return nil, nil
	}), nil, AgentRunWorkerConfig{Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	proposal := &TurnDelegationProposal{
		StepID: "perform", AssignedAgentID: "browser", Goal: "Perform the bounded browser task",
		Checkpoint: map[string]interface{}{}, Budget: &BudgetPolicy{MaxTurns: 24, MaxTotalTokens: 150000},
	}
	waiting, err := pool.materializeTurnDelegation(t.Context(), "worker", source, &AgentTurn{ID: "turn", RequestedDelegation: proposal})
	if err != nil || waiting.Status != AgentRunStatusWaitingForAgent {
		t.Fatalf("materialized delegation = %#v, %v", waiting, err)
	}
	requests, err := store.ListAgentRequests(t.Context(), AgentRequestFilter{Scope: scope, SourceRunID: source.ID, Limit: 10})
	if err != nil || len(requests) != 1 {
		t.Fatalf("requests = %#v, %v", requests, err)
	}
	budget := requests[0].BudgetAllocation
	if budget == nil || budget.MaxAttempts != 3 || budget.MaxTurns != 24 || budget.MaxInputTokens != 120000 ||
		budget.MaxOutputTokens != 60000 || budget.MaxTotalTokens != 150000 || budget.MaxCostMicros != 900000 ||
		budget.MaxDurationMS != 600000 || budget.MaxActions != 60 {
		t.Fatalf("completed child budget = %#v", budget)
	}

	overAllocated := *proposal
	overAllocated.StepID = "perform-too-much"
	overAllocated.Budget = &BudgetPolicy{MaxAttempts: 4, MaxTurns: 24, MaxTotalTokens: 150000}
	if _, err := pool.materializeTurnDelegation(t.Context(), "worker", source, &AgentTurn{ID: "turn-two", RequestedDelegation: &overAllocated}); err == nil || !strings.Contains(err.Error(), "exceeds parent remaining capacity") {
		t.Fatalf("explicit over-allocation error = %v", err)
	}
}

func TestAgentRunWorkerRejectsExplicitRunbookDelegationOverAllocation(t *testing.T) {
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "runbook-delegation-budget"}
	source, err := NewPortfolioService(store).CreateAgentRun(t.Context(), CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "manager"}, AssignedAgentID: "manager",
		Goal: "run the authored delegation", Source: RunSourceSchedule,
		Plan:   map[string]interface{}{"runbook": map[string]interface{}{"id": "daily-browser-task", "version": "1.0.0"}},
		Budget: &BudgetPolicy{MaxAttempts: 2, MaxTurns: 24, MaxTotalTokens: 200000, MaxDurationMS: 1800000, MaxActions: 60},
	})
	if err != nil {
		t.Fatal(err)
	}
	source.BudgetUsage = BudgetUsage{Attempts: 1, Turns: 1}
	pool, err := NewAgentRunWorkerPool(store, TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*TurnRunnerBinding, error) {
		return nil, nil
	}), nil, AgentRunWorkerConfig{Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	proposal := &TurnDelegationProposal{
		StepID: "perform", AssignedAgentID: "browser", Goal: "Perform the browser task", Checkpoint: map[string]interface{}{},
		Budget: &BudgetPolicy{MaxAttempts: 2, MaxTurns: 24, MaxTotalTokens: 200000, MaxDurationMS: 1800000, MaxActions: 60},
	}
	if _, err := pool.materializeTurnDelegation(t.Context(), "worker", source, &AgentTurn{ID: "turn", RequestedDelegation: proposal}); err == nil || !strings.Contains(err.Error(), "exceeds parent remaining capacity") {
		t.Fatalf("explicit Runbook over-allocation error = %v", err)
	}
	requests, err := store.ListAgentRequests(t.Context(), AgentRequestFilter{Scope: scope, SourceRunID: source.ID, Limit: 10})
	if err != nil || len(requests) != 0 {
		t.Fatalf("requests = %#v, %v", requests, err)
	}
}

func TestAgentRunWorkerCompletesDelegationClarificationRoundTrip(t *testing.T) {
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "delegation-clarification"}
	source, err := NewPortfolioService(store).CreateAgentRun(t.Context(), CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "manager"}, AssignedAgentID: "manager",
		Goal: "delegate with enough context", Source: RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	proposal := &TurnDelegationProposal{
		StepID: "research", AssignedAgentID: "specialist", Goal: "Research customer feedback",
		Context: map[string]interface{}{"release": "2026.07"}, Mode: "reason",
	}
	resolver := TurnRunnerResolverFunc(func(_ context.Context, run *AgentRun) (*TurnRunnerBinding, error) {
		switch run.Source {
		case RunSourceRequestDecision:
			inbox, _ := run.Context[AgentRequestInboxContextKey].(map[string]interface{})
			revision, revisionErr := positiveInt64(inbox["requestRevision"])
			if revisionErr != nil {
				return nil, revisionErr
			}
			decision := AgentRequestDecisionRequestClarification
			message := "Which customer segment should I prioritize?"
			if revision > 1 {
				if inbox["clarificationQuestion"] != message || inbox["clarificationResponse"] != "Prioritize enterprise platform teams." {
					return nil, fmt.Errorf("clarification context mismatch: %#v", inbox)
				}
				decision = AgentRequestDecisionAccept
				message = "The clarified request is relevant and specific."
			}
			return &TurnRunnerBinding{DefinitionID: "specialist", DefinitionVersion: "1", Runner: TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
				return &TurnOutcome{
					NextRunStatus: AgentRunStatusCompleted, OutputSummary: "Request reviewed",
					RunOutput: map[string]interface{}{AgentRequestDecisionOutputKey: map[string]interface{}{
						"decision": string(decision), "message": message,
					}},
				}, nil
			})}, nil
		case RunSourceRequest:
			return &TurnRunnerBinding{DefinitionID: "specialist", DefinitionVersion: "1", Runner: TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
				return &TurnOutcome{
					NextRunStatus: AgentRunStatusCompleted, OutputSummary: "Research complete",
					RunOutput: map[string]interface{}{"summary": "Enterprise platform teams need clearer rollout evidence."},
				}, nil
			})}, nil
		default:
			return nil, fmt.Errorf("unexpected claimed run source %s", run.Source)
		}
	})
	pool, err := NewAgentRunWorkerPool(store, resolver, nil, AgentRunWorkerConfig{
		Scope: scope, Kind: RunKindAgentWork, AssignedAgentID: "specialist",
		Concurrency: 1, MaxTurnsPerClaim: 1, PollInterval: 5 * time.Millisecond,
		LeaseDuration: time.Second, TurnLeaseDuration: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	waiting, err := pool.materializeTurnDelegation(t.Context(), "worker", source, &AgentTurn{ID: "turn-1", RequestedDelegation: proposal})
	if err != nil || waiting.Status != AgentRunStatusWaitingForAgent {
		t.Fatalf("initial delegation = %#v, %v", waiting, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	pool.Start(ctx)
	defer func() {
		cancel()
		pool.Stop()
	}()
	service := NewCollaborationService(store)
	requestID := stableCollaborationID(scope, "delegation:"+source.ID+":"+proposal.StepID, "request")
	deadline := time.Now().Add(3 * time.Second)
	var request *AgentRequest
	for time.Now().Before(deadline) {
		request, err = service.GetAgentRequest(t.Context(), scope, requestID)
		if err != nil {
			t.Fatal(err)
		}
		if request.Status == AgentRequestStatusClarificationRequested {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if request == nil || request.Status != AgentRequestStatusClarificationRequested {
		t.Fatalf("clarification request = %#v", request)
	}
	resumed, err := store.GetAgentRun(t.Context(), scope, source.ID)
	if err != nil || resumed.Status != AgentRunStatusQueued || resumed.WakeCondition != nil {
		t.Fatalf("source awaiting clarification response = %#v, %v", resumed, err)
	}
	followUp := *proposal
	followUp.Clarification = "Prioritize enterprise platform teams."
	waiting, err = pool.materializeTurnDelegation(t.Context(), "worker", resumed, &AgentTurn{ID: "turn-2", RequestedDelegation: &followUp})
	if err != nil || waiting.Status != AgentRunStatusWaitingForAgent {
		t.Fatalf("clarification response = %#v, %v", waiting, err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		request, err = service.GetAgentRequest(t.Context(), scope, requestID)
		if err != nil {
			t.Fatal(err)
		}
		if request.Status == AgentRequestStatusCompleted {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if request == nil || request.Status != AgentRequestStatusCompleted {
		t.Fatalf("completed clarified request = %#v", request)
	}
	resumed, err = store.GetAgentRun(t.Context(), scope, source.ID)
	if err != nil || resumed.Status != AgentRunStatusQueued || resumed.WakeCondition != nil {
		t.Fatalf("completed source = %#v, %v", resumed, err)
	}
	children, err := store.ListAgentRuns(t.Context(), AgentRunFilter{Scope: scope, ParentRunID: source.ID, Limit: 10})
	if err != nil || len(children) != 3 {
		t.Fatalf("clarification children = %#v, %v", children, err)
	}
	decisionRuns := 0
	workRuns := 0
	for _, child := range children {
		switch child.Source {
		case RunSourceRequestDecision:
			decisionRuns++
		case RunSourceRequest:
			workRuns++
		}
	}
	if decisionRuns != 2 || workRuns != 1 {
		t.Fatalf("clarification children = %#v", children)
	}
}

func TestAgentRunWorkerPoolYieldsBetweenTurnSlices(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	scope := Scope{Kind: "local", ID: "test"}
	run, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent",
		Goal: "take two turns", Source: RunSourceObjective,
	})
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	pool, err := NewAgentRunWorkerPool(store, TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*TurnRunnerBinding, error) {
		return &TurnRunnerBinding{Runner: TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
			if calls.Add(1) == 1 {
				return &TurnOutcome{NextRunStatus: AgentRunStatusRunning, OutputSummary: "More work remains"}, nil
			}
			return &TurnOutcome{NextRunStatus: AgentRunStatusCompleted, OutputSummary: "Done"}, nil
		})}, nil
	}), nil, AgentRunWorkerConfig{
		Scope: scope, AssignedAgentID: "agent", MaxTurnsPerClaim: 1,
		PollInterval: 5 * time.Millisecond, LeaseDuration: time.Second, TurnLeaseDuration: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	pool.Start(ctx)
	defer pool.Stop()
	deadline := time.Now().Add(2 * time.Second)
	var completed *AgentRun
	for time.Now().Before(deadline) {
		completed, err = NewPortfolioService(store).GetAgentRun(ctx, scope, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if completed.Status == AgentRunStatusCompleted {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if completed == nil || completed.Status != AgentRunStatusCompleted || completed.Attempt != 2 || calls.Load() != 2 {
		t.Fatalf("run was not fairly yielded and reclaimed: run=%#v calls=%d", completed, calls.Load())
	}
	events, err := NewRunActivityService(store, store).ListActivity(ctx, ActivityFilter{Scope: scope, RunID: run.ID})
	if err != nil {
		t.Fatal(err)
	}
	foundYield := false
	claimEvents := 0
	for _, event := range events {
		foundYield = foundYield || event.EventType == "run.yielded"
		if event.EventType == "run.claimed" {
			claimEvents++
			if event.UsageDelta != nil {
				t.Fatalf("run.claimed duplicated scheduler-owned attempt usage: %#v", event)
			}
		}
	}
	if !foundYield || claimEvents != 2 {
		t.Fatalf("yield activity was not recorded: %#v", events)
	}
}

func TestAgentRunWorkerNeverChargesPastAttemptLimit(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	scope := Scope{Kind: "local", ID: "attempt-limit"}
	run, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent",
		Goal: "use exactly one autonomous attempt", Source: RunSourceObjective,
		Budget: &BudgetPolicy{MaxAttempts: 1, MaxTurns: 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	pool, err := NewAgentRunWorkerPool(store, TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*TurnRunnerBinding, error) {
		return &TurnRunnerBinding{Runner: TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
			calls.Add(1)
			return &TurnOutcome{NextRunStatus: AgentRunStatusRunning, OutputSummary: "More work remains"}, nil
		})}, nil
	}), nil, AgentRunWorkerConfig{
		Scope: scope, AssignedAgentID: "agent", MaxTurnsPerClaim: 1,
		PollInterval: 5 * time.Millisecond, LeaseDuration: time.Second, TurnLeaseDuration: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	pool.Start(ctx)
	defer pool.Stop()
	deadline := time.Now().Add(2 * time.Second)
	var paused *AgentRun
	for time.Now().Before(deadline) {
		paused, err = NewPortfolioService(store).GetAgentRun(ctx, scope, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if paused.Status == AgentRunStatusPaused {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if paused == nil || paused.Status != AgentRunStatusPaused || paused.BudgetUsage.Attempts != 1 || calls.Load() != 1 {
		t.Fatalf("attempt ceiling was not exact: run=%#v calls=%d", paused, calls.Load())
	}
}

func TestAgentRunWorkerPoolRenewsLeaseDuringLongTurn(t *testing.T) {
	store := NewMemoryStore()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	scope := Scope{Kind: "local", ID: "test"}
	run, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent",
		Goal: "finish a long bounded turn", Source: RunSourceObjective,
	})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := NewAgentRunWorkerPool(store, TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*TurnRunnerBinding, error) {
		return &TurnRunnerBinding{Runner: TurnRunnerFunc(func(ctx context.Context, _ TurnExecutionContext) (*TurnOutcome, error) {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(220 * time.Millisecond):
				return &TurnOutcome{NextRunStatus: AgentRunStatusCompleted, OutputSummary: "Done"}, nil
			}
		})}, nil
	}), nil, AgentRunWorkerConfig{
		Scope: scope, AssignedAgentID: "agent", PollInterval: 5 * time.Millisecond,
		LeaseDuration: 90 * time.Millisecond, TurnLeaseDuration: 90 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	pool.Start(ctx)
	defer pool.Stop()
	deadline := time.Now().Add(2 * time.Second)
	var completed *AgentRun
	for time.Now().Before(deadline) {
		completed, err = NewPortfolioService(store).GetAgentRun(ctx, scope, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if completed.Status == AgentRunStatusCompleted {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if completed == nil || completed.Status != AgentRunStatusCompleted || completed.LastAppliedTurn != 1 || completed.Revision < 5 {
		t.Fatalf("long turn was not protected by renewable ownership: %#v", completed)
	}
}
