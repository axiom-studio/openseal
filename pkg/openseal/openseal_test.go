package openseal

import (
	"context"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime"
)

func TestEngineExposesObjectivePortfolio(t *testing.T) {
	engine, err := New(WithStore(runtime.NewMemoryStore(100)))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	scope := Scope{Kind: "local", ID: "test"}
	owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent-1"}
	objective, err := engine.CreateObjective(ctx, CreateObjectiveRequest{
		Scope: scope, Owner: owner, Title: "Operate", Goal: "Keep operating", Status: ObjectiveStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := engine.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, ObjectiveID: objective.ID, Owner: owner, AssignedAgentID: owner.ID,
		Goal: "Inspect current health", Source: RunSourceObjective,
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.ObjectiveID != objective.ID || run.RootRunID != run.ID {
		t.Fatalf("unexpected run: %#v", run)
	}
	objectives, err := engine.ListObjectives(ctx, ObjectiveFilter{Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	if len(objectives) != 1 || objectives[0].ID != objective.ID {
		t.Fatalf("unexpected objectives: %#v", objectives)
	}
	run, event, err := engine.TransitionAgentRun(ctx, scope, run.ID, RunTransitionRequest{
		ExpectedRevision: run.Revision, Status: AgentRunStatusRunning, Summary: "Health inspection started",
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != AgentRunStatusRunning || event.Sequence != 1 {
		t.Fatalf("unexpected transition: run=%#v event=%#v", run, event)
	}
	turn, err := engine.BeginAgentTurn(ctx, BeginAgentTurnRequest{
		Scope: scope, RunID: run.ID, DefinitionID: "operator", DefinitionVersion: "1", Model: "test-model", WorkerID: "test-worker",
	})
	if err != nil {
		t.Fatal(err)
	}
	turn, err = engine.FinishAgentTurn(ctx, scope, turn.ID, FinishAgentTurnRequest{
		ExpectedRevision: turn.Revision, Status: AgentTurnStatusCompleted, WorkerID: "test-worker",
		Decisions:     []TurnDecision{{Summary: "Inspect dependencies", EvidenceRefs: []string{"artifact:health"}}},
		OutputSummary: "Inspection plan ready", ContinuationCheckpoint: map[string]interface{}{"next": "inspect"},
	})
	if err != nil {
		t.Fatal(err)
	}
	listedTurns, err := engine.ListAgentTurns(ctx, AgentTurnFilter{Scope: scope, RunID: run.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(listedTurns) != 1 || listedTurns[0].ID != turn.ID || listedTurns[0].CompletedAt == nil {
		t.Fatalf("unexpected turns: %#v", listedTurns)
	}
	_, err = engine.AppendActivity(ctx, &ActivityEvent{
		Scope: scope, RunID: run.ID, EventType: "inspection.progress", Summary: "Checked the first subsystem",
		Visibility: ActivityVisibilityTeam,
	})
	if err != nil {
		t.Fatal(err)
	}
	events, err := engine.ListActivity(ctx, ActivityFilter{Scope: scope, RunID: run.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[1].Sequence != 2 || events[1].Visibility != ActivityVisibilityTeam {
		t.Fatalf("unexpected activity: %#v", events)
	}
	waiting, _, err := engine.TransitionAgentRun(ctx, scope, run.ID, RunTransitionRequest{
		ExpectedRevision: run.Revision, Status: AgentRunStatusWaitingForEvent,
		WakeCondition: &WakeCondition{Type: "event", Reference: "health.changed"},
	})
	if err != nil {
		t.Fatal(err)
	}
	woken, err := engine.WakeAgentRuns(ctx, WakeSignal{
		ID: "health-signal", Scope: scope, Type: "event", Reference: "health.changed",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(woken.Runs) != 1 || woken.Runs[0].Run.ID != waiting.ID || woken.Runs[0].Run.Status != AgentRunStatusQueued {
		t.Fatalf("unexpected wake result: %#v", woken)
	}
	autonomousRun, err := engine.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: owner, AssignedAgentID: "agent-2", Goal: "Complete one bounded step", Source: RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := engine.ClaimNextAgentRun(ctx, AgentRunClaimRequest{
		Scope: scope, WorkerID: "test-worker", AssignedAgentID: "agent-2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if claimed == nil || claimed.ID != autonomousRun.ID || claimed.LeaseOwner != "test-worker" {
		t.Fatalf("unexpected scheduled claim: %#v", claimed)
	}
	advanced, err := engine.AdvanceAgentRun(ctx, AdvanceAgentRunRequest{
		Scope: scope, RunID: autonomousRun.ID, WorkerID: "test-worker", Model: "test-model",
	}, TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
		return &TurnOutcome{
			NextRunStatus: AgentRunStatusCompleted, OutputSummary: "Bounded step complete",
			RunOutput: map[string]interface{}{"result": "ok"},
		}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if advanced.Run.Status != AgentRunStatusCompleted || advanced.Run.LastAppliedTurn != 1 || advanced.Event.TurnID != advanced.Turn.ID {
		t.Fatalf("unexpected bounded advance: %#v", advanced)
	}
}

func TestEngineRunsAutonomousAgentPortfolio(t *testing.T) {
	store := runtime.NewMemoryStore(20)
	scope := Scope{Kind: "local", ID: "autonomous"}
	resolver := TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*TurnRunnerBinding, error) {
		return &TurnRunnerBinding{
			DefinitionID: "test-agent", DefinitionVersion: "1", ModelProvider: "fake", Model: "deterministic",
			Runner: TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
				return &TurnOutcome{NextRunStatus: AgentRunStatusCompleted, OutputSummary: "Done"}, nil
			}),
		}, nil
	})
	engine, err := New(
		WithStore(store),
		WithAgentRunWorkers(AgentRunWorkerConfig{
			Scope: scope, AssignedAgentID: "agent", PollInterval: 5 * time.Millisecond,
			LeaseDuration: time.Second, TurnLeaseDuration: time.Second,
		}, resolver),
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run, err := engine.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent",
		Goal: "finish autonomously", Source: RunSourceObjective,
	})
	if err != nil {
		t.Fatal(err)
	}
	engine.Start(ctx)
	defer engine.Stop()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		current, err := engine.GetAgentRun(ctx, scope, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.Status == AgentRunStatusCompleted {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("autonomous Engine worker did not complete the run")
}
