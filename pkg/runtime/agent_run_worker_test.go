package runtime

import (
	"context"
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
	store := NewMemoryStore(50)
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
		{name: "memory", store: func(*testing.T) KernelStore { return NewMemoryStore(50) }},
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
	store := NewMemoryStore(50)
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
	store := NewMemoryStore(20)
	catalog, scope := governedActionCatalog(t)
	if err := catalog.Bind(t.Context(), &skill.Binding{
		ID: "team-release-binding", Scope: capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "release-team",
		SkillID: "release", SkillVersion: "1.0.0", AllowedActions: []string{"deploy"}, MaximumRisk: skill.RiskLevelProduction,
		Credentials: map[string]skill.CredentialReference{"token": {Kind: "vault", ID: "release-secret"}}, Revision: 1,
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

func TestAgentRunWorkerMaterializesDurableFork(t *testing.T) {
	store := NewMemoryStore(20)
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
			store := NewMemoryStore(50)
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
	store := NewMemoryStore(50)
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
	resolver := TurnRunnerResolverFunc(func(_ context.Context, run *AgentRun) (*TurnRunnerBinding, error) {
		if run.AssignedAgentID == "manager" {
			return &TurnRunnerBinding{DefinitionID: "manager", DefinitionVersion: "1", Runner: parentRunner}, nil
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
		t.Fatalf("parent=%#v", completed)
	}
	children, err := store.ListAgentRuns(t.Context(), AgentRunFilter{Scope: scope, ParentRunID: parent.ID, Limit: 10})
	if err != nil || len(children) != 1 || children[0].AssignedAgentID != "specialist" || children[0].Source != RunSourceFork || children[0].Status != AgentRunStatusCompleted {
		t.Fatalf("children=%#v error=%v", children, err)
	}
	if children[0].Budget == nil || children[0].Budget.MaxTurns != 2 || children[0].Budget.MaxDurationMS != 60000 || len(completed.BudgetAllocations) != 1 {
		t.Fatalf("budgeted parent=%#v children=%#v", completed, children)
	}
	turns, err := store.ListAgentTurns(t.Context(), AgentTurnFilter{Scope: scope, RunID: parent.ID})
	if err != nil || len(turns) != 2 || turns[0].RequestedDelegation == nil || turns[0].RequestedDelegation.AssignedAgentID != "specialist" {
		t.Fatalf("turns=%#v error=%v", turns, err)
	}
}

func TestAgentRunWorkerPoolYieldsBetweenTurnSlices(t *testing.T) {
	store := NewMemoryStore(20)
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
	for _, event := range events {
		foundYield = foundYield || event.EventType == "run.yielded"
	}
	if !foundYield {
		t.Fatalf("yield activity was not recorded: %#v", events)
	}
}

func TestAgentRunWorkerPoolRenewsLeaseDuringLongTurn(t *testing.T) {
	store := NewMemoryStore(20)
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
