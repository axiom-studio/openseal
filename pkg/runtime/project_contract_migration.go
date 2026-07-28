package runtime

import (
	"context"
	"database/sql"
)

const projectContractMigrationVersion int64 = 36

// migrateProjectContract is the clean break from the former campaign container
// to optional Projects. The old research projections cannot be decoded as the
// new projectId contract, so their derived state is removed and rebuilt by the
// normal Project, source-monitor, and outreach migrations in this transaction.
func (s *PostgresStore) migrateProjectContract(ctx context.Context, tx *sql.Tx) error {
	applied, err := s.postgresMigrationApplied(ctx, tx, projectContractMigrationVersion)
	if err != nil || applied {
		return err
	}
	if _, err = tx.ExecContext(ctx, `
		DROP TABLE IF EXISTS `+s.table("initiatives")+` CASCADE;
		DROP TABLE IF EXISTS `+s.table("source_monitor_checkpoints")+` CASCADE;
		DROP TABLE IF EXISTS `+s.table("source_observations")+` CASCADE;
		DROP TABLE IF EXISTS `+s.table("outreach_threads")+` CASCADE
	`); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+
		` (version, name) VALUES ($1, 'replace initiatives with optional projects')`, projectContractMigrationVersion)
	return err
}
