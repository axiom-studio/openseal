package runtime

import (
	"context"
	"database/sql"
)

// legacyEventSourcesMigrationVersion drops the event source subscriptions,
// their health rows and the event source checkpoints. Nothing produced or read
// them once the Runbook event router went. Migrations 20 and 22 that created
// them are retired.
const legacyEventSourcesMigrationVersion int64 = 79

// migrateRemoveEventSources is PostgreSQL migration 79.
func (s *PostgresStore) migrateRemoveEventSources(ctx context.Context, tx *sql.Tx) error {
	applied, err := s.postgresMigrationApplied(ctx, tx, legacyEventSourcesMigrationVersion)
	if err != nil || applied {
		return err
	}
	for _, table := range legacyEventSourceTables {
		if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS `+s.table(table)); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+`(version, name)
		VALUES ($1, 'remove event source subscriptions and checkpoints') ON CONFLICT (version) DO NOTHING`, legacyEventSourcesMigrationVersion)
	return err
}
