package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
)

func TestPortfolioSupportsMultipleObjectivesAndRunLineage(t *testing.T) {
	ctx := context.Background()
	store := kerneltest.NewStore(t)
	service := portfolio.NewPortfolioService(store)
	scope := models.Scope{Kind: "tenant", ID: "acme"}
	owner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "marketing"}

	content, err := service.CreateObjective(ctx, portfolio.CreateObjectiveRequest{
		Scope: scope, Owner: owner, Title: "Market releases", Goal: "Turn releases into content", Status: models.ObjectiveStatusActive, Priority: 80,
	})
	if err != nil {
		t.Fatal(err)
	}
	leads, err := service.CreateObjective(ctx, portfolio.CreateObjectiveRequest{
		Scope: scope, Owner: owner, Title: "Follow up leads", Goal: "Convert qualified leads", Status: models.ObjectiveStatusActive, Priority: 90,
	})
	if err != nil {
		t.Fatal(err)
	}

	objectives, err := service.ListObjectives(ctx, models.ObjectiveFilter{Scope: scope, Owner: &owner})
	if err != nil {
		t.Fatal(err)
	}
	if len(objectives) != 2 || objectives[0].ID != leads.ID || objectives[1].ID != content.ID {
		t.Fatalf("unexpected objective order: %#v", objectives)
	}

	root, err := service.CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, ObjectiveID: content.ID, Owner: owner, AssignedAgentID: "marketing", Goal: "Draft launch post", Source: models.RunSourceObjective,
	})
	if err != nil {
		t.Fatal(err)
	}
	child, err := service.CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, ObjectiveID: content.ID, ParentRunID: root.ID, Owner: owner, AssignedAgentID: "analyst", Goal: "Collect launch evidence", Source: models.RunSourceHandoff,
	})
	if err != nil {
		t.Fatal(err)
	}
	if child.ParentRunID != root.ID || child.RootRunID != root.ID {
		t.Fatalf("lineage not preserved: %#v", child)
	}
}

func TestPortfolioFailsClosedAcrossScopesAndOnStaleRevision(t *testing.T) {
	ctx := context.Background()
	service := portfolio.NewPortfolioService(kerneltest.NewStore(t))
	scopeA := models.Scope{Kind: "tenant", ID: "a"}
	scopeB := models.Scope{Kind: "tenant", ID: "b"}
	owner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}
	objective, err := service.CreateObjective(ctx, portfolio.CreateObjectiveRequest{Scope: scopeA, Owner: owner, Title: "A", Goal: "A"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.GetObjective(ctx, scopeB, objective.ID); !errors.Is(err, models.ErrObjectiveNotFound) {
		t.Fatalf("cross-scope read error = %v", err)
	}
	updatedTitle := "updated"
	if _, err := service.UpdateObjective(ctx, scopeA, objective.ID, portfolio.UpdateObjectiveRequest{ExpectedRevision: 1, Title: &updatedTitle}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.UpdateObjective(ctx, scopeA, objective.ID, portfolio.UpdateObjectiveRequest{ExpectedRevision: 1, Title: &updatedTitle}); !errors.Is(err, models.ErrRevisionConflict) {
		t.Fatalf("stale update error = %v", err)
	}
}
