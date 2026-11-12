package runtime

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	runtimeportfolio "github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

func TestObjectiveLifecycleIsAtomicallyAudited(t *testing.T) {
	stores := map[string]func(*testing.T) models.KernelStore{
		"sqlite": func(t *testing.T) models.KernelStore {
			store, err := sqlite.New(filepath.Join(t.TempDir(), "kernel.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return store
		},
	}
	for name, createStore := range stores {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store := createStore(t)
			portfolio := runtimeportfolio.NewPortfolioService(store)
			scope := models.Scope{Kind: "tenant", ID: name}
			objective, err := portfolio.CreateObjective(ctx, runtimeportfolio.CreateObjectiveRequest{
				Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "research"},
				Title: "Understand users", Goal: "Monitor feedback", Status: models.ObjectiveStatusActive,
				Actor: models.ActivityActor{Type: "user", ID: "7"}, IdempotencyKey: "research-objective",
			})
			if err != nil {
				t.Fatal(err)
			}
			events, err := store.ListActivity(ctx, models.ActivityFilter{Scope: scope, ObjectiveID: objective.ID, Descending: true})
			if err != nil || len(events) != 1 {
				t.Fatalf("created events = %#v, err = %v", events, err)
			}
			if events[0].EventType != "objective.created" || events[0].RunID != "" || events[0].TeamID != "research" || events[0].Actor.ID != "7" {
				t.Fatalf("created event = %#v", events[0])
			}
			paused := models.ObjectiveStatusPaused
			objective, err = portfolio.UpdateObjective(ctx, scope, objective.ID, runtimeportfolio.UpdateObjectiveRequest{
				ExpectedRevision: objective.Revision, Status: &paused, Actor: models.ActivityActor{Type: "user", ID: "8"},
			})
			if err != nil {
				t.Fatal(err)
			}
			events, err = store.ListActivity(ctx, models.ActivityFilter{Scope: scope, ObjectiveID: objective.ID, Descending: true})
			if err != nil || len(events) != 2 || events[0].EventType != "objective.status_changed" || events[0].Payload["previousStatus"] != string(models.ObjectiveStatusActive) {
				t.Fatalf("updated events = %#v, err = %v", events, err)
			}
			if _, err = portfolio.UpdateObjective(ctx, scope, objective.ID, runtimeportfolio.UpdateObjectiveRequest{ExpectedRevision: 1, Status: &paused}); err != models.ErrRevisionConflict {
				t.Fatalf("stale update = %v", err)
			}
			events, _ = store.ListActivity(ctx, models.ActivityFilter{Scope: scope, ObjectiveID: objective.ID, Descending: true})
			if len(events) != 2 {
				t.Fatalf("failed update appended event: %#v", events)
			}
		})
	}
}
