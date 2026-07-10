package openseal

import (
	"context"
	"testing"
)

func TestPublicDependencyFacadeCoordinatesFanIn(t *testing.T) {
	t.Parallel()
	engine, err := New()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	scope := Scope{Kind: "local", ID: "fan-in"}
	source, err := engine.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "research"}, AssignedAgentID: "lead",
		Goal: "Synthesize findings", Source: RunSourceObjective,
	})
	if err != nil {
		t.Fatal(err)
	}
	created, err := engine.CreateRunDependencyGroup(ctx, CreateRunDependencyGroupRequest{
		Scope: scope, SourceRunID: source.ID, ExpectedSourceRevision: source.Revision,
		Policy: RunDependencyPolicy{Mode: FanInModeAll, FailureMode: DependencyFailureFailFast},
		Dependencies: []RunDependencySpec{
			{ID: "forums", Kind: RunDependencyKindAgentRequest, RequestID: "forums-request"},
			{ID: "reviews", Kind: RunDependencyKindAgentRequest, RequestID: "reviews-request"},
		},
		IdempotencyKey: "research-wave-1", Actor: ActivityActor{Type: "agent", ID: "lead"},
		Visibility: ActivityVisibilityTeam,
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := engine.ResolveRunDependency(ctx, ResolveRunDependencyRequest{
		Scope: scope, GroupID: created.Group.ID, DependencyID: "forums", ExpectedDependencyRevision: 1,
		State: RunDependencyStateSatisfied, Actor: ActivityActor{Type: "agent", ID: "forums"},
		Visibility: ActivityVisibilityTeam,
	})
	if err != nil || first.Evaluation.Wake {
		t.Fatalf("first resolution = %#v, %v", first, err)
	}
	second, err := engine.ResolveRunDependency(ctx, ResolveRunDependencyRequest{
		Scope: scope, GroupID: created.Group.ID, DependencyID: "reviews", ExpectedDependencyRevision: 1,
		State: RunDependencyStateSatisfied, Actor: ActivityActor{Type: "agent", ID: "reviews"},
		Visibility: ActivityVisibilityTeam,
	})
	if err != nil || !second.Evaluation.Wake || second.Source.Status != AgentRunStatusQueued {
		t.Fatalf("second resolution = %#v, %v", second, err)
	}
	group, err := engine.GetRunDependencyGroup(ctx, scope, created.Group.ID)
	if err != nil || group.Status != RunDependencyGroupSatisfied {
		t.Fatalf("group = %#v, %v", group, err)
	}
	edges, err := engine.ListRunDependencies(ctx, scope, created.Group.ID)
	if err != nil || len(edges) != 2 {
		t.Fatalf("edges = %#v, %v", edges, err)
	}
}
