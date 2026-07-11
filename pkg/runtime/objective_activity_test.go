package runtime

import (
	"context"
	"path/filepath"
	"testing"
)

func TestObjectiveLifecycleIsAtomicallyAudited(t *testing.T) {
	stores := map[string]func(*testing.T) KernelStore{
		"memory": func(*testing.T) KernelStore { return NewMemoryStore(20) },
		"sqlite": func(t *testing.T) KernelStore {
			store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
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
			portfolio := NewPortfolioService(store)
			scope := Scope{Kind: "tenant", ID: name}
			objective, err := portfolio.CreateObjective(ctx, CreateObjectiveRequest{
				Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "research"},
				Title: "Understand users", Goal: "Monitor feedback", Status: ObjectiveStatusActive,
				Actor: ActivityActor{Type: "user", ID: "7"}, IdempotencyKey: "research-objective",
			})
			if err != nil {
				t.Fatal(err)
			}
			events, err := store.ListActivity(ctx, ActivityFilter{Scope: scope, ObjectiveID: objective.ID, Descending: true})
			if err != nil || len(events) != 1 {
				t.Fatalf("created events = %#v, err = %v", events, err)
			}
			if events[0].EventType != "objective.created" || events[0].RunID != "" || events[0].TeamID != "research" || events[0].Actor.ID != "7" {
				t.Fatalf("created event = %#v", events[0])
			}
			paused := ObjectiveStatusPaused
			objective, err = portfolio.UpdateObjective(ctx, scope, objective.ID, UpdateObjectiveRequest{
				ExpectedRevision: objective.Revision, Status: &paused, Actor: ActivityActor{Type: "user", ID: "8"},
			})
			if err != nil {
				t.Fatal(err)
			}
			events, err = store.ListActivity(ctx, ActivityFilter{Scope: scope, ObjectiveID: objective.ID, Descending: true})
			if err != nil || len(events) != 2 || events[0].EventType != "objective.status_changed" || events[0].Payload["previousStatus"] != string(ObjectiveStatusActive) {
				t.Fatalf("updated events = %#v, err = %v", events, err)
			}
			if _, err = portfolio.UpdateObjective(ctx, scope, objective.ID, UpdateObjectiveRequest{ExpectedRevision: 1, Status: &paused}); err != ErrRevisionConflict {
				t.Fatalf("stale update = %v", err)
			}
			events, _ = store.ListActivity(ctx, ActivityFilter{Scope: scope, ObjectiveID: objective.ID, Descending: true})
			if len(events) != 2 {
				t.Fatalf("failed update appended event: %#v", events)
			}
		})
	}
}
