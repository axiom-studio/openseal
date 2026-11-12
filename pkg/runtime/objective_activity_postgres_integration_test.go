//go:build integration

package runtime

import (
	"context"
	"github.com/axiom-studio/openseal/pkg/models"
	runtimeportfolio "github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"testing"
	"time"
)

func TestPostgresObjectiveLifecycleCommitsActivityAtomically(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, _ := openTestPostgresStore(t, "openseal_objective_activity_")
	portfolio := runtimeportfolio.NewPortfolioService(store)
	scope := models.Scope{Kind: "tenant", ID: "audit"}
	objective, err := portfolio.CreateObjective(ctx, runtimeportfolio.CreateObjectiveRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, Title: "Audit", Goal: "Stay visible",
	})
	if err != nil {
		t.Fatal(err)
	}
	active := models.ObjectiveStatusActive
	objective, err = portfolio.UpdateObjective(ctx, scope, objective.ID, runtimeportfolio.UpdateObjectiveRequest{ExpectedRevision: objective.Revision, Status: &active})
	if err != nil {
		t.Fatal(err)
	}
	events, err := store.ListActivity(ctx, models.ActivityFilter{Scope: scope, ObjectiveID: objective.ID, Descending: true})
	if err != nil || len(events) != 2 || events[0].EventType != "objective.status_changed" || events[1].EventType != "objective.created" {
		t.Fatalf("events = %#v, err = %v", events, err)
	}
}
