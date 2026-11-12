package runtime

import (
	"context"
	"database/sql"
)

// migrateRemoveKernelProjectsSQLite mirrors PostgreSQL migration 77. Every
// statement is idempotent, so a later open does nothing.
func migrateRemoveKernelProjectsSQLite(db *sql.DB) error {
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, table := range retiredKernelProjectTables {
		if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS `+table); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DROP INDEX IF EXISTS idx_run_activity_project_feed`); err != nil {
		return err
	}
	var present int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('run_activity') WHERE name = 'project_id'`).Scan(&present); err != nil {
		return err
	}
	if present > 0 {
		if _, err := tx.ExecContext(ctx, `ALTER TABLE run_activity DROP COLUMN project_id`); err != nil {
			return err
		}
	}
	return tx.Commit()
}
