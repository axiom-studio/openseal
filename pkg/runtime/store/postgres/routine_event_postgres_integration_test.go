//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
	"github.com/google/uuid"
)

// Store test: it runs storetest.RunRoutineEventContract and reads the
// store's own routine event queries and tables.

func openPostgresRoutineEventStore(t *testing.T) *Store {
	t.Helper()
	pgSchema := storetest.NewPostgresSchema(t, "routine_events_")
	dsn := pgSchema.DSN
	ctx := context.Background()
	store, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(pgSchema.Name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pgSchema.DB.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+pgSchema.Quoted()+` CASCADE`)
		_ = store.Close()
	})
	return store
}

func TestPostgresRoutineEventStore(t *testing.T) {
	storetest.RunRoutineEventContract(t, func(t *testing.T) (models.KernelStore, storetest.RoutineEventPayloadProbe) {
		store := openPostgresRoutineEventStore(t)
		return store, func(t *testing.T, scope models.Scope, activationID, eventID string) bool {
			var hasPayload bool
			if err := store.db.QueryRow(`SELECT payload IS NOT NULL FROM `+store.table("routine_events")+`
				WHERE scope_kind = $1 AND scope_id = $2 AND activation_id = $3 AND event_id = $4`,
				scope.Kind, scope.ID, activationID, eventID).Scan(&hasPayload); err != nil {
				t.Fatalf("read event %s: %v", eventID, err)
			}
			return hasPayload
		}
	})

	t.Run("concurrent claims yield one open batch", func(t *testing.T) {
		store := openPostgresRoutineEventStore(t)
		ctx := t.Context()
		scope := models.Scope{Kind: "tenant", ID: "claims"}
		now := time.Now().UTC().Truncate(time.Microsecond)
		activation := storetest.RoutineEventActivation(scope, "racing", now)
		if err := store.CreateRunbookActivation(ctx, activation); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 20; i++ {
			event := storetest.RoutineEventFor(scope, activation, uuid.NewString(), now.Add(time.Duration(i)*time.Millisecond))
			if _, err := store.InsertRoutineEvent(ctx, &models.RoutineEventRecord{Scope: scope, ActivationID: activation.ID, Event: event,
				State: models.RoutineEventPending, ReceivedAt: event.OccurredAt}); err != nil {
				t.Fatal(err)
			}
		}
		const claimers = 8
		batches := make([]*models.RoutineEventBatch, claimers)
		errs := make([]error, claimers)
		var wait sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < claimers; i++ {
			wait.Add(1)
			go func(i int) {
				defer wait.Done()
				<-start
				batches[i], errs[i] = store.ClaimRoutineEventBatch(ctx, scope, activation.ID, 50, now)
			}(i)
		}
		close(start)
		wait.Wait()
		for i := range batches {
			if errs[i] != nil || batches[i] == nil || batches[i].ID != batches[0].ID || len(batches[i].EventIDs) != 20 {
				t.Fatalf("claim %d = %#v, %v; want batch %s", i, batches[i], errs[i], batches[0].ID)
			}
		}
		var open int
		if err := store.db.QueryRow(`SELECT COUNT(*) FROM ` + store.table("routine_event_batches") + ` WHERE started_at IS NULL`).Scan(&open); err != nil || open != 1 {
			t.Fatalf("open batches = %d, %v", open, err)
		}
	})

	t.Run("hot queries use indexes", func(t *testing.T) {
		store := openPostgresRoutineEventStore(t)
		ctx := t.Context()
		now := time.Now().UTC()
		checks := []struct {
			table string
			query string
			args  []interface{}
		}{
			{"routine_events", store.pendingRoutineEventsQuery(), []interface{}{"tenant", "one", "activation"}},
			{"routine_events", store.pendingRoutineEventsInScopeQuery(), []interface{}{"tenant", "one"}},
			{"routine_events", store.routineEventWorkQuery(), []interface{}{"tenant", "one", "", 10}},
			{"routine_events", store.routineEventWorkScopesQuery(), nil},
			{"routine_events", store.pruneRoutineEventsQuery(), []interface{}{now, 10}},
			{"runbook_activations", store.routineEventActivationsQuery(), []interface{}{"tenant", "one", "webhook", "hook:hk_x"}},
			{"runbook_activations", store.routineEventSourceKeysQuery(), []interface{}{"tenant", "one"}},
			{"routine_event_batches", store.activeRoutineEventRunsQuery(), []interface{}{"tenant", "one", now}},
		}
		for _, check := range checks {
			plan := explainWithoutSeqScan(t, ctx, store, check.query, check.args...)
			if strings.Contains(plan, "Seq Scan on "+check.table) {
				t.Fatalf("query scans %s sequentially:\n%s\n%s", check.table, check.query, plan)
			}
		}
		// The scope-wide queries read only waiting rows, never the week of
		// started and dropped history under the work index.
		for _, query := range []string{store.pendingRoutineEventsInScopeQuery(), store.routineEventWorkScopesQuery()} {
			args := []interface{}{"tenant", "one"}
			if query == store.routineEventWorkScopesQuery() {
				args = nil
			}
			if plan := explainWithoutSeqScan(t, ctx, store, query, args...); !strings.Contains(plan, "routine_events_pending_idx") {
				t.Fatalf("query does not use the pending index:\n%s\n%s", query, plan)
			}
		}
		if plan := explainWithoutSeqScan(t, ctx, store, store.routineEventWorkQuery(), "tenant", "one", "", 10); !strings.Contains(plan, "routine_events_pending_idx") {
			t.Fatalf("work query does not use the pending index:\n%s", plan)
		}
	})
}

// explainWithoutSeqScan returns the plan with sequential scans priced out, so
// a Seq Scan in it means no index can serve the query.
func explainWithoutSeqScan(t *testing.T, ctx context.Context, store *Store, query string, args ...interface{}) string {
	t.Helper()
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.QueryContext(ctx, `EXPLAIN `+query, args...)
	if err != nil {
		t.Fatalf("explain %s: %v", query, err)
	}
	defer rows.Close()
	lines := make([]string, 0)
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(lines, "\n")
}

// Migration 80 creates the routine event tables on a fresh database and on
// one at 79, fills the index columns of event Tasks written before it, and
// rolls back by dropping the tables while keeping the two columns.
func TestPostgresMigration80RoutineEvents(t *testing.T) {
	pgSchema := storetest.NewPostgresSchema(t, "routine_events_migration_")
	dsn := pgSchema.DSN
	schema := pgSchema.Name
	ctx := context.Background()
	store, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pgSchema.DB.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+pgSchema.Quoted()+` CASCADE`)
		_ = store.Close()
	})
	assertRoutineEventTables(t, ctx, store, true)
	assertSchemaCurrent(t, ctx, store)

	// Back to 79, as the previous release left it.
	if err := store.RollbackPostgresMigrations(ctx, 79); err != nil {
		t.Fatal(err)
	}
	if version, err := store.PostgresSchemaVersion(ctx); err != nil || version != 79 {
		t.Fatalf("version after rollback = %d, %v", version, err)
	}
	assertRoutineEventTables(t, ctx, store, false)
	// An event Task written by a release that did not fill the columns.
	scope := models.Scope{Kind: "tenant", ID: "migrated"}
	activation := storetest.RoutineEventActivation(scope, "written-at-79", time.Now().UTC())
	if err := store.CreateRunbookActivation(ctx, activation); err != nil {
		t.Fatal(err)
	}
	if _, err := pgSchema.DB.ExecContext(ctx, `UPDATE `+pgSchema.Table("runbook_activations")+` SET event_kind = NULL, event_source_ref = NULL`); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()

	reopened, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	assertRoutineEventTables(t, ctx, reopened, true)
	assertSchemaCurrent(t, ctx, reopened)
	keys, err := reopened.ListRoutineEventSourceKeysInScope(ctx, scope)
	if err != nil || len(keys) != 1 || keys[0].Kind != models.RoutineEventWebhook || keys[0].SourceRef != activation.Task.Event.SourceRef {
		t.Fatalf("keys after migrating = %#v, %v", keys, err)
	}
	var indexes int
	if err := pgSchema.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM pg_indexes WHERE schemaname = $1
		AND indexname IN ('runbook_activations_event_idx', 'runbook_activations_event_kind_idx', 'routine_events_work_idx',
			'routine_events_prune_idx', 'routine_event_batches_open_idx', 'routine_event_batches_started_idx')`, schema).Scan(&indexes); err != nil || indexes != 6 {
		t.Fatalf("indexes = %d, %v", indexes, err)
	}
}

func assertRoutineEventTables(t *testing.T, ctx context.Context, store *Store, want bool) {
	t.Helper()
	for _, table := range routineEventsTables {
		var relation sql.NullString
		if err := store.db.QueryRowContext(ctx, `SELECT to_regclass($1)::text`, store.schema+"."+table).Scan(&relation); err != nil || relation.Valid != want {
			t.Fatalf("table %s exists = %v, want %v (%v)", table, relation.Valid, want, err)
		}
	}
	for _, column := range []string{"event_kind", "event_source_ref"} {
		var exists bool
		if err := store.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns
			WHERE table_schema = $1 AND table_name = 'runbook_activations' AND column_name = $2)`, store.schema, column).Scan(&exists); err != nil || !exists {
			t.Fatalf("column %s exists = %v, %v", column, exists, err)
		}
	}
}
