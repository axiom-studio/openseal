package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Store test: each store keeps a Run's ActingUser fixed, and the postgres
// subtest also checks the Postgres store's own in-transaction update. It
// moves with the stores in U2.8/U2.9, one copy per backend: the postgres
// subtest (with actingUserPostgresStore) to store/postgres, the sqlite one to
// store/sqlite.

func TestAgentRunActingUserIsImmutable(t *testing.T) {
	stores := map[string]func(*testing.T) KernelStore{
		"sqlite": func(t *testing.T) KernelStore {
			store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "acting-user.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return store
		},
		"postgres": func(t *testing.T) KernelStore { return actingUserPostgresStore(t) },
	}
	for name, open := range stores {
		t.Run(name, func(t *testing.T) {
			store := open(t)
			ctx := context.Background()
			scope := Scope{Kind: "tenant", ID: "1"}
			run, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
				Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent",
				Goal: "work", Source: RunSourceManual, ActingUser: "7",
			})
			if err != nil {
				t.Fatal(err)
			}
			update := func(actingUser string) error {
				current, err := store.GetAgentRun(ctx, scope, run.ID)
				if err != nil {
					return err
				}
				expected := current.Revision
				current.ActingUser = actingUser
				current.Priority++
				current.Revision++
				current.UpdatedAt = current.UpdatedAt.Add(time.Second)
				event := &ActivityEvent{ID: uuid.NewString(), Scope: scope, EventType: "run.updated", Severity: ActivitySeverityInfo,
					AgentID: "agent", RunID: run.ID, Actor: ActivityActor{Type: "system", ID: "openseal"}, Summary: "Run updated",
					Visibility: ActivityVisibilityScope, CreatedAt: current.UpdatedAt}
				_, err = store.UpdateAgentRunWithEvent(ctx, current, expected, event, nil)
				return err
			}
			for _, changed := range []string{"8", ""} {
				if err := update(changed); !errors.Is(err, ErrRevisionConflict) {
					t.Fatalf("update to ActingUser %q error = %v", changed, err)
				}
			}
			if err := update("7"); err != nil {
				t.Fatalf("update keeping ActingUser: %v", err)
			}
			stored, err := store.GetAgentRun(ctx, scope, run.ID)
			if err != nil || stored.ActingUser != "7" {
				t.Fatalf("stored run = %#v, %v", stored, err)
			}
			if postgres, ok := store.(*PostgresStore); ok {
				stored.ActingUser = "8"
				expected := stored.Revision
				stored.Revision++
				tx, err := postgres.db.BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				if err := postgres.updateAgentRunTx(ctx, tx, stored, expected); !errors.Is(err, ErrRevisionConflict) {
					t.Fatalf("updateAgentRunTx changing ActingUser error = %v", err)
				}
			}
		})
	}
}

func actingUserPostgresStore(t *testing.T) *PostgresStore {
	t.Helper()
	pgSchema := newTestPostgresSchema(t, "openseal_acting_user_")
	dsn := pgSchema.dsn
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	store, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(pgSchema.name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})
	return store
}
