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
}
