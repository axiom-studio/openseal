package sqlite

import (
	"database/sql"
)

// migrateRoutineEventsSQLite mirrors PostgreSQL migration 80. Timestamps of
// the new tables are fixed-width UTC text (sqliteRoutineTime), so they sort
// and compare as text.
func migrateRoutineEventsSQLite(db *sql.DB) error {
	for _, column := range []string{"event_kind", "event_source_ref"} {
		exists, err := sqliteColumnExists(db, "runbook_activations", column)
		if err != nil {
			return err
		}
		if !exists {
			if _, err := db.Exec(`ALTER TABLE runbook_activations ADD COLUMN ` + column + ` TEXT`); err != nil {
				return err
			}
		}
	}
	_, err := db.Exec(`
		UPDATE runbook_activations SET event_kind = json_extract(payload, '$.task.event.kind'), event_source_ref = json_extract(payload, '$.task.event.sourceRef')
			WHERE event_kind IS NULL AND json_extract(payload, '$.trigger.kind') = 'event' AND json_extract(payload, '$.task.event.kind') IS NOT NULL;
		CREATE INDEX IF NOT EXISTS runbook_activations_event_idx ON runbook_activations (scope_kind, scope_id, event_kind, event_source_ref)
			WHERE status = 'active' AND event_kind IS NOT NULL;
		CREATE INDEX IF NOT EXISTS runbook_activations_event_kind_idx ON runbook_activations (event_kind, scope_kind, scope_id, id)
			WHERE status = 'active' AND event_kind IS NOT NULL;
		CREATE TABLE IF NOT EXISTS routine_events (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, activation_id TEXT NOT NULL, event_id TEXT NOT NULL,
			state TEXT NOT NULL, batch_id TEXT, dropped_reason TEXT,
			test INTEGER NOT NULL DEFAULT 0,
			received_at TEXT NOT NULL, occurred_at TEXT NOT NULL, payload TEXT,
			PRIMARY KEY (scope_kind, scope_id, activation_id, event_id));
		CREATE INDEX IF NOT EXISTS routine_events_work_idx ON routine_events (scope_kind, scope_id, activation_id, state, received_at, event_id);
		CREATE INDEX IF NOT EXISTS routine_events_prune_idx ON routine_events (received_at) WHERE state IN ('started','dropped');
		CREATE INDEX IF NOT EXISTS routine_events_pending_idx ON routine_events (scope_kind, scope_id, activation_id) WHERE state IN ('pending','batched');
		CREATE TABLE IF NOT EXISTS routine_event_batches (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, id TEXT NOT NULL, activation_id TEXT NOT NULL,
			event_ids TEXT NOT NULL, dropped INTEGER NOT NULL DEFAULT 0, test INTEGER NOT NULL DEFAULT 0, run_id TEXT,
			created_at TEXT NOT NULL, started_at TEXT,
			PRIMARY KEY (scope_kind, scope_id, id));
		CREATE UNIQUE INDEX IF NOT EXISTS routine_event_batches_open_idx ON routine_event_batches (scope_kind, scope_id, activation_id) WHERE started_at IS NULL;
		CREATE INDEX IF NOT EXISTS routine_event_batches_started_idx ON routine_event_batches (scope_kind, scope_id, activation_id, started_at);
		CREATE TABLE IF NOT EXISTS hooks (
			id TEXT PRIMARY KEY, scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, activation_id TEXT NOT NULL,
			revision INTEGER NOT NULL, path_token_enabled INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL, rotated_at TEXT NOT NULL,
			UNIQUE (scope_kind, scope_id, activation_id));
	`)
	return err
}
