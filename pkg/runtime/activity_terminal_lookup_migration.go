package runtime

import (
	"context"
	"database/sql"
)

const terminalActivityLookupMigrationVersion int64 = 40

func (s *PostgresStore) migrateTerminalActivityLookup(ctx context.Context, tx *sql.Tx) error {
	applied, err := s.postgresMigrationApplied(ctx, tx, terminalActivityLookupMigrationVersion)
	if err != nil || applied {
		return err
	}
	if _, err = tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS `+s.table("run_activity_terminal_lookup_idx")+`
		ON `+s.table("run_activity")+` (scope_kind, scope_id, run_id, event_type, created_at DESC, id DESC)`); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+`
		(version, name) VALUES ($1, 'index terminal Run resource outcomes')`, terminalActivityLookupMigrationVersion)
	return err
}
