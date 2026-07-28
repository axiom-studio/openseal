package runtime

import (
	"context"
	"database/sql"
)

const projectRunRefsMigrationVersion int64 = 37

// migrateProjectRunRefs removes the former mutable Project-to-Run projection.
// Runs are derived from the Project's Objective references and their Runbook
// activations; retaining a second list allowed stale or invented execution
// lineage. The migration is intentionally destructive because runRefs was
// never authoritative.
func (s *PostgresStore) migrateProjectRunRefs(ctx context.Context, tx *sql.Tx) error {
	applied, err := s.postgresMigrationApplied(ctx, tx, projectRunRefsMigrationVersion)
	if err != nil || applied {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE `+s.table("projects")+` SET payload = payload - 'runRefs' WHERE payload ? 'runRefs'`); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+
		` (version, name) VALUES ($1, 'derive Project Runs from Objective lineage')`, projectRunRefsMigrationVersion)
	return err
}
