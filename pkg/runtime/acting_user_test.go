package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestAgentRunReplayActingUser(t *testing.T) {
	ctx := context.Background()
	store := newTestKernelStore(t)
	service := NewRunCommandService(store)
	scope := Scope{Kind: "tenant", ID: "1"}
	request := func(key, actingUser string) CreateAgentRunRequest {
		return CreateAgentRunRequest{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent",
			Goal: "work", Source: RunSourceManual, IdempotencyKey: key, ActingUser: actingUser}
	}
	created, err := service.CreateAgentRun(ctx, request("stamped", "7"))
	if err != nil {
		t.Fatal(err)
	}
	if created.Run.ActingUser != "7" {
		t.Fatalf("ActingUser = %q", created.Run.ActingUser)
	}
	if replayed, err := service.CreateAgentRun(ctx, request("stamped", "7")); err != nil || replayed.Run.ID != created.Run.ID {
		t.Fatalf("exact replay = %#v, %v", replayed, err)
	}
	if _, err := service.CreateAgentRun(ctx, request("stamped", "8")); !errors.Is(err, ErrRunIdempotency) {
		t.Fatalf("other acting user replay error = %v", err)
	}
	if _, err := service.CreateAgentRun(ctx, request("stamped", "")); !errors.Is(err, ErrRunIdempotency) {
		t.Fatalf("missing acting user replay error = %v", err)
	}
	// A run stored before ActingUser existed replays for any caller.
	legacy, err := service.CreateAgentRun(ctx, request("legacy", ""))
	if err != nil {
		t.Fatal(err)
	}
	if replayed, err := service.CreateAgentRun(ctx, request("legacy", "7")); err != nil || replayed.Run.ID != legacy.Run.ID || replayed.Run.ActingUser != "" {
		t.Fatalf("replay with empty stored ActingUser = %#v, %v", replayed, err)
	}
}

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
	dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OPENSEAL_TEST_POSTGRES_DSN to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	store, err := NewPostgresStore(ctx, dsn, WithPostgresSchema("openseal_acting_user_"+uuid.NewString()[:8]))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.db.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+store.quotedSchema()+` CASCADE`)
		_ = store.Close()
	})
	return store
}
