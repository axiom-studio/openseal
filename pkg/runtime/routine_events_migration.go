package runtime

import (
	"context"
	"database/sql"
)

// routineEventsMigrationVersion adds the routine event tables (events,
// batches, hooks) and the event index columns of runbook_activations.
const routineEventsMigrationVersion int64 = 80

// routineEventsTables are dropped when migration 80 is rolled back, dependents first.
var routineEventsTables = []string{"hooks", "routine_event_batches", "routine_events"}

// migrateRoutineEvents is PostgreSQL migration 80.
func (s *PostgresStore) migrateRoutineEvents(ctx context.Context, tx *sql.Tx) error {
	applied, err := s.postgresMigrationApplied(ctx, tx, routineEventsMigrationVersion)
	if err != nil || applied {
		return err
	}
	activations := s.table("runbook_activations")
	statements := []string{
		`ALTER TABLE ` + activations + ` ADD COLUMN IF NOT EXISTS event_kind TEXT`,
		`ALTER TABLE ` + activations + ` ADD COLUMN IF NOT EXISTS event_source_ref TEXT`,
		// Fill the columns of event Tasks written before them.
		`UPDATE ` + activations + ` SET event_kind = payload->'task'->'event'->>'kind', event_source_ref = payload->'task'->'event'->>'sourceRef'
			WHERE payload->'trigger'->>'kind' = 'event' AND payload->'task'->'event' IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS runbook_activations_event_idx ON ` + activations + ` (scope_kind, scope_id, event_kind, event_source_ref)
			WHERE status = 'active' AND event_kind IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS runbook_activations_event_kind_idx ON ` + activations + ` (event_kind, scope_kind, scope_id, id)
			WHERE status = 'active' AND event_kind IS NOT NULL`,
		`CREATE TABLE IF NOT EXISTS ` + s.table("routine_events") + ` (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, activation_id TEXT NOT NULL, event_id TEXT NOT NULL,
			state TEXT NOT NULL, batch_id TEXT, dropped_reason TEXT,
			test BOOLEAN NOT NULL DEFAULT false,
			received_at TIMESTAMPTZ NOT NULL, occurred_at TIMESTAMPTZ NOT NULL, payload JSONB,
			PRIMARY KEY (scope_kind, scope_id, activation_id, event_id))`,
		`CREATE INDEX IF NOT EXISTS routine_events_work_idx ON ` + s.table("routine_events") + ` (scope_kind, scope_id, activation_id, state, received_at, event_id)`,
		`CREATE INDEX IF NOT EXISTS routine_events_prune_idx ON ` + s.table("routine_events") + ` (received_at) WHERE state IN ('started','dropped')`,
		// Scope-wide work and queue-depth queries touch only waiting rows,
		// never the week of started and dropped history.
		`CREATE INDEX IF NOT EXISTS routine_events_pending_idx ON ` + s.table("routine_events") + ` (scope_kind, scope_id, activation_id) WHERE state IN ('pending','batched')`,
		`CREATE TABLE IF NOT EXISTS ` + s.table("routine_event_batches") + ` (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, id TEXT NOT NULL, activation_id TEXT NOT NULL,
			event_ids JSONB NOT NULL, dropped INTEGER NOT NULL DEFAULT 0, test BOOLEAN NOT NULL DEFAULT false, run_id TEXT,
			created_at TIMESTAMPTZ NOT NULL, started_at TIMESTAMPTZ,
			PRIMARY KEY (scope_kind, scope_id, id))`,
		`CREATE UNIQUE INDEX IF NOT EXISTS routine_event_batches_open_idx ON ` + s.table("routine_event_batches") + ` (scope_kind, scope_id, activation_id) WHERE started_at IS NULL`,
		`CREATE INDEX IF NOT EXISTS routine_event_batches_started_idx ON ` + s.table("routine_event_batches") + ` (scope_kind, scope_id, activation_id, started_at)`,
		`CREATE TABLE IF NOT EXISTS ` + s.table("hooks") + ` (
			id TEXT PRIMARY KEY, scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, activation_id TEXT NOT NULL,
			revision BIGINT NOT NULL, path_token_enabled BOOLEAN NOT NULL DEFAULT false, created_at TIMESTAMPTZ NOT NULL, rotated_at TIMESTAMPTZ NOT NULL,
			UNIQUE (scope_kind, scope_id, activation_id))`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+`(version, name)
		VALUES ($1, 'routine events, batches and hooks') ON CONFLICT (version) DO NOTHING`, routineEventsMigrationVersion)
	return err
}

// rollbackRoutineEvents drops the two activation indexes of migration 80.
// The tables go through the down map; the two index columns stay, and
// migrating again fills them anew.
func (s *PostgresStore) rollbackRoutineEvents(ctx context.Context, tx *sql.Tx) error {
	for _, index := range []string{"runbook_activations_event_idx", "runbook_activations_event_kind_idx"} {
		if _, err := tx.ExecContext(ctx, `DROP INDEX IF EXISTS `+s.table(index)); err != nil {
			return err
		}
	}
	return nil
}
