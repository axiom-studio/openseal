package runtime

import (
	"context"
	"database/sql"
)

const dependencyReconciliationMigrationVersion int64 = 56

func (s *PostgresStore) migrateDependencyReconciliation(ctx context.Context, tx *sql.Tx) error {
	applied, err := s.postgresMigrationApplied(ctx, tx, dependencyReconciliationMigrationVersion)
	if err != nil || applied {
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS run_dependency_groups_waiting_page_idx
		ON `+s.table("run_dependency_groups")+` (scope_kind,scope_id,id) WHERE status='waiting'`); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+` (version,name)
		VALUES ($1,'indexed waiting dependency group reconciliation') ON CONFLICT (version) DO NOTHING`, dependencyReconciliationMigrationVersion)
	return err
}
