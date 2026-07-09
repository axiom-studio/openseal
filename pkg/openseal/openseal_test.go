package openseal

import (
	"context"
	"testing"

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
}
