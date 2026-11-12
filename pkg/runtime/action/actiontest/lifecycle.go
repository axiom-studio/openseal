package actiontest

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/store/postgres"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

// ForEachStore runs test on a fresh store of each backend ForEachBackend runs.
func ForEachStore(t *testing.T, test func(*testing.T, models.KernelStore)) {
	t.Helper()
	ForEachBackend(t, func(t *testing.T, store models.KernelStore, _ Backend) { test(t, store) })
}

// ParentRun reads the Run that call belongs to.
func ParentRun(t *testing.T, store models.KernelStore, call *models.ActionCall) *models.AgentRun {
	t.Helper()
	run, err := store.GetAgentRun(t.Context(), call.Scope, call.RunID)
	if err != nil || run == nil {
		t.Fatalf("get parent Run = %#v, %v", run, err)
	}
	return run
}

// CallEvent is a worker activity event about call, carrying its status.
func CallEvent(call *models.ActionCall, id, eventType string, now time.Time) *models.ActivityEvent {
	return &models.ActivityEvent{
		ID: id, Scope: call.Scope, RunID: call.RunID, AgentID: call.DeploymentID,
		EventType: eventType, Severity: models.ActivitySeverityInfo, Summary: "Record action lifecycle state",
		Actor: models.ActivityActor{Type: "worker", ID: "lifecycle-worker"}, Visibility: models.ActivityVisibilityScope,
		CreatedAt: now, Payload: map[string]interface{}{"actionCallId": call.ID, "status": call.Status},
	}
}

// AssertUnclaimable fails the test when a worker could claim another action
// in call's scope at now.
func AssertUnclaimable(t *testing.T, store models.KernelStore, call *models.ActionCall, now time.Time) {
	t.Helper()
	claimed, err := store.ClaimNextAction(t.Context(), models.ActionClaim{Scope: call.Scope, WorkerID: "later-worker", Now: now, LeaseDuration: time.Minute})
	if err != nil || claimed != nil {
		t.Fatalf("terminal action was eligible again: %#v, %v", claimed, err)
	}
}

// ForEachBackend runs test on SQLite, and on Postgres when
// OPENSEAL_TEST_POSTGRES_DSN is set.
func ForEachBackend(t *testing.T, test func(*testing.T, models.KernelStore, Backend)) {
	t.Helper()
	for _, testCase := range []struct {
		name string
		open func(*testing.T) (models.KernelStore, Backend)
	}{
		{name: "sqlite", open: func(t *testing.T) (models.KernelStore, Backend) {
			path := filepath.Join(t.TempDir(), "action-lifecycle.db")
			store, err := sqlite.NewSQLiteStore(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return store, Backend{DB: openTestSQLiteDB(t, path), Table: func(name string) string { return name }, Reopen: func(t *testing.T) models.KernelStore {
				if err := store.Close(); err != nil {
					t.Fatal(err)
				}
				reopened, err := sqlite.NewSQLiteStore(path)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = reopened.Close() })
				return reopened
			}}
		}},
		{name: "postgres", open: func(t *testing.T) (models.KernelStore, Backend) {
			pgSchema := storetest.NewPostgresSchema(t, "openseal_action_lifecycle_")
			dsn := pgSchema.DSN
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			store, err := postgres.NewPostgresStore(ctx, dsn, postgres.WithPostgresSchema(pgSchema.Name))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_, _ = pgSchema.DB.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+pgSchema.Quoted()+` CASCADE`)
				_ = store.Close()
			})
			return store, Backend{Postgres: true, DB: pgSchema.DB, Table: pgSchema.Table, Reopen: func(t *testing.T) models.KernelStore {
				return openPostgresStoreOn(t, pgSchema)
			}}
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			store, backend := testCase.open(t)
			test(t, store, backend)
		})
	}
}

// Backend is the database behind one lifecycle store as the
// test owns it: its own connection and table names for rows no store method
// writes, and Reopen, which opens the store again on the same data as a
// restart would.
type Backend struct {
	Postgres bool
	DB       *sql.DB
	Table    func(name string) string
	Reopen   func(*testing.T) models.KernelStore
}

// TransitionParentRun moves call's Run to status through the run activity
// service, as a person changing the workstream would.
func TransitionParentRun(t *testing.T, store models.KernelStore, call *models.ActionCall, status models.AgentRunStatus, wake *models.WakeCondition, now time.Time) *models.AgentRun {
	t.Helper()
	run := ParentRun(t, store, call)
	service := activity.NewRunActivityService(store, store, models.WithClock(func() time.Time { return now }))
	updated, _, err := service.TransitionRun(t.Context(), call.Scope, call.RunID, activity.RunTransitionRequest{
		ExpectedRevision: run.Revision, Status: status, WakeCondition: wake,
		Actor: models.ActivityActor{Type: "user", ID: "alice"}, Summary: "Update the current workstream",
	})
	if err != nil {
		t.Fatal(err)
	}
	return updated
}

// MutateParentRun applies mutate to call's Run and stores it with a new
// revision, outside any service.
func MutateParentRun(t *testing.T, store models.KernelStore, call *models.ActionCall, now time.Time, mutate func(*models.AgentRun)) *models.AgentRun {
	t.Helper()
	run := ParentRun(t, store, call)
	expected := run.Revision
	mutate(run)
	run.Revision++
	run.UpdatedAt = now
	event := CallEvent(call, fmt.Sprintf("lifecycle-mutation-%d", run.Revision), "run.updated", now)
	if _, err := store.UpdateAgentRunWithEvent(t.Context(), run, expected, event, nil); err != nil {
		t.Fatal(err)
	}
	return run
}
