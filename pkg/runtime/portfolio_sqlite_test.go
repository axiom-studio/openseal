package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

func TestSQLitePortfolioRoundTripAndScopeIsolation(t *testing.T) {
	store, err := sqlite.NewSQLiteStore(filepath.Join(t.TempDir(), "portfolio.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := portfolio.NewPortfolioService(store)
	ctx := context.Background()
	scope := models.Scope{Kind: "tenant", ID: "acme"}
	otherScope := models.Scope{Kind: "tenant", ID: "other"}
	owner := models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "gtm"}

	objective, err := service.CreateObjective(ctx, portfolio.CreateObjectiveRequest{
		Scope: scope, Owner: owner, Title: "Launch", Goal: "Launch the product",
		Status: models.ObjectiveStatusActive, Priority: 100,
		Budget:          &models.BudgetPolicy{MaxTotalTokens: 50000},
		SuccessCriteria: map[string]interface{}{"qualifiedLeads": float64(25)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.GetObjective(ctx, otherScope, objective.ID); !errors.Is(err, models.ErrObjectiveNotFound) {
		t.Fatalf("cross-scope objective read error = %v", err)
	}

	run, err := service.CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, ObjectiveID: objective.ID, Owner: owner, AssignedAgentID: "marketing",
		Goal: "Draft launch content", Source: models.RunSourceObjective, Priority: 50,
		Context:       map[string]interface{}{"release": "v2"},
		Plan:          map[string]interface{}{"next": "draft"},
		Checkpoint:    map[string]interface{}{"turn": float64(3)},
		WakeCondition: &models.WakeCondition{Type: "event", Reference: "approval:launch"},
		Budget:        &models.BudgetPolicy{MaxTotalTokens: 25000},
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := service.GetAgentRun(ctx, scope, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.RootRunID != run.ID || loaded.Context["release"] != "v2" ||
		loaded.Plan["next"] != "draft" || loaded.Checkpoint["turn"] != float64(3) ||
		loaded.WakeCondition == nil || loaded.WakeCondition.Reference != "approval:launch" {
		t.Fatalf("run did not round-trip: %#v", loaded)
	}
	if _, err := service.GetAgentRun(ctx, otherScope, run.ID); !errors.Is(err, models.ErrRunNotFound) {
		t.Fatalf("cross-scope run read error = %v", err)
	}

	summary := "Launch copy drafted"
	allocatedObjective, err := service.GetObjective(ctx, scope, objective.ID)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := service.UpdateObjective(ctx, scope, objective.ID, portfolio.UpdateObjectiveRequest{ExpectedRevision: allocatedObjective.Revision, ProgressSummary: &summary})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Revision != allocatedObjective.Revision+1 || updated.ProgressSummary != summary {
		t.Fatalf("objective update did not round-trip: %#v", updated)
	}
	if _, err := service.UpdateObjective(ctx, scope, objective.ID, portfolio.UpdateObjectiveRequest{ExpectedRevision: 1, ProgressSummary: &summary}); !errors.Is(err, models.ErrRevisionConflict) {
		t.Fatalf("stale update error = %v", err)
	}
}
