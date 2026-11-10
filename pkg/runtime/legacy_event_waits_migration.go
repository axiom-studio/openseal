package runtime

import (
	"context"
	"database/sql"
)

// legacyEventWaitsMigrationVersion drops the durable event wait tables: the
// waits, their scope work rows, the event inbox, consumptions, notifications
// and retention cursors. Nothing creates an event wait once workflows and the
// runbook wait.match step went. Migrations 47, 49 and 51 that created them are
// retired.
const legacyEventWaitsMigrationVersion int64 = 81

// legacyEventWaitTables are dropped by migration 81, dependents first.
// run_event_retention_scopes only ever existed in SQLite.
var legacyEventWaitTables = []string{
	"run_event_notifications", "run_event_consumptions", "run_event_wait_scopes", "run_event_waits",
	"run_event_retention_cursor", "run_event_retention_scopes", "run_event_inbox",
}

// migrateRemoveEventWaits is PostgreSQL migration 81.
func (s *PostgresStore) migrateRemoveEventWaits(ctx context.Context, tx *sql.Tx) error {
	applied, err := s.postgresMigrationApplied(ctx, tx, legacyEventWaitsMigrationVersion)
	if err != nil || applied {
		return err
	}
	for _, table := range legacyEventWaitTables {
		if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS `+s.table(table)+` CASCADE`); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+`(version, name)
		VALUES ($1, 'remove durable event waits') ON CONFLICT (version) DO NOTHING`, legacyEventWaitsMigrationVersion)
	return err
}

// migrateRemoveEventWaitsSQLite mirrors PostgreSQL migration 81. Every
// statement is idempotent, so a later open does nothing.
func migrateRemoveEventWaitsSQLite(db *sql.DB) error {
	for _, table := range legacyEventWaitTables {
		if _, err := db.Exec(`DROP TABLE IF EXISTS ` + table); err != nil {
			return err
		}
	}
	return nil
}
