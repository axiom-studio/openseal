package runtime

import (
	"context"
	"errors"
	"testing"
)

func TestMemoryPortfolioSupportsMultipleObjectivesAndRunLineage(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore(100)
	service := NewPortfolioService(store)
	scope := Scope{Kind: "tenant", ID: "acme"}
	owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "marketing"}

	content, err := service.CreateObjective(ctx, CreateObjectiveRequest{
		Scope: scope, Owner: owner, Title: "Market releases", Goal: "Turn releases into content", Status: ObjectiveStatusActive, Priority: 80,
	})
	if err != nil {
		t.Fatal(err)
	}
	leads, err := service.CreateObjective(ctx, CreateObjectiveRequest{
		Scope: scope, Owner: owner, Title: "Follow up leads", Goal: "Convert qualified leads", Status: ObjectiveStatusActive, Priority: 90,
	})
	if err != nil {
		t.Fatal(err)
	}

	objectives, err := service.ListObjectives(ctx, ObjectiveFilter{Scope: scope, Owner: &owner})
	if err != nil {
		t.Fatal(err)
	}
	if len(objectives) != 2 || objectives[0].ID != leads.ID || objectives[1].ID != content.ID {
		t.Fatalf("unexpected objective order: %#v", objectives)
	}

	root, err := service.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, ObjectiveID: content.ID, Owner: owner, AssignedAgentID: "marketing", Goal: "Draft launch post", Source: RunSourceObjective,
	})
	if err != nil {
		t.Fatal(err)
	}
	child, err := service.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, ObjectiveID: content.ID, ParentRunID: root.ID, Owner: owner, AssignedAgentID: "analyst", Goal: "Collect launch evidence", Source: RunSourceHandoff,
	})
	if err != nil {
		t.Fatal(err)
	}
	if child.ParentRunID != root.ID || child.RootRunID != root.ID {
		t.Fatalf("lineage not preserved: %#v", child)
	}
}

func TestMemoryPortfolioFailsClosedAcrossScopesAndOnStaleRevision(t *testing.T) {
	ctx := context.Background()
	service := NewPortfolioService(NewMemoryStore(100))
	scopeA := Scope{Kind: "tenant", ID: "a"}
	scopeB := Scope{Kind: "tenant", ID: "b"}
	owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}
	objective, err := service.CreateObjective(ctx, CreateObjectiveRequest{Scope: scopeA, Owner: owner, Title: "A", Goal: "A"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.GetObjective(ctx, scopeB, objective.ID); !errors.Is(err, ErrObjectiveNotFound) {
		t.Fatalf("cross-scope read error = %v", err)
	}
	updatedTitle := "updated"
	if _, err := service.UpdateObjective(ctx, scopeA, objective.ID, UpdateObjectiveRequest{ExpectedRevision: 1, Title: &updatedTitle}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.UpdateObjective(ctx, scopeA, objective.ID, UpdateObjectiveRequest{ExpectedRevision: 1, Title: &updatedTitle}); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale update error = %v", err)
	}
}
