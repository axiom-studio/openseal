package postgres

import (
	"context"
	"database/sql"

	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
)

// legacyEventWaitsMigrationVersion drops the durable event wait tables: the
// waits, their scope work rows, the event inbox, consumptions, notifications
// and retention cursors. Nothing creates an event wait once workflows and the
// runbook wait.match step went. Migrations 47, 49 and 51 that created them are
// retired.
const legacyEventWaitsMigrationVersion int64 = 81

// migrateRemoveEventWaits is PostgreSQL migration 81.
func (s *Store) migrateRemoveEventWaits(ctx context.Context, tx *sql.Tx) error {
	applied, err := s.postgresMigrationApplied(ctx, tx, legacyEventWaitsMigrationVersion)
	if err != nil || applied {
		return err
	}
	for _, table := range sqlstore.LegacyEventWaitTables {
		if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS `+s.table(table)+` CASCADE`); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+`(version, name)
		VALUES ($1, 'remove durable event waits') ON CONFLICT (version) DO NOTHING`, legacyEventWaitsMigrationVersion)
	return err
}
