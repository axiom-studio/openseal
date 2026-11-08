package runtime

import (
	"context"
	"database/sql"
)

// kernelProjectRemovalMigrationVersion removes kernel Projects and the
// features that only hung off them: Project Skill references and their
// admission triggers, source monitors and their observations, outreach
// threads, and the Project activity projection.
const kernelProjectRemovalMigrationVersion int64 = 77

// retiredKernelProjectTables are dropped by migration 77, dependents first.
var retiredKernelProjectTables = []string{
	"project_skill_references", "outreach_threads", "source_monitor_checkpoints", "source_observations", "projects",
}

// migrateRemoveKernelProjects is PostgreSQL migration 77.
func (s *PostgresStore) migrateRemoveKernelProjects(ctx context.Context, tx *sql.Tx) error {
	applied, err := s.postgresMigrationApplied(ctx, tx, kernelProjectRemovalMigrationVersion)
	if err != nil || applied {
		return err
	}
	statements := []string{
		// The triggers go with the projects table; their functions do not.
		`DROP FUNCTION IF EXISTS ` + s.table("admit_project_skill_references") + `() CASCADE`,
		`DROP FUNCTION IF EXISTS ` + s.table("project_skill_references") + `() CASCADE`,
	}
	for _, table := range retiredKernelProjectTables {
		statements = append(statements, `DROP TABLE IF EXISTS `+s.table(table)+` CASCADE`)
	}
	statements = append(statements,
		`DROP INDEX IF EXISTS `+s.table("run_activity_project_feed_idx"),
		`ALTER TABLE `+s.table("run_activity")+` DROP COLUMN IF EXISTS project_id`,
	)
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+`(version, name)
		VALUES ($1, 'remove kernel Projects, source monitors and outreach') ON CONFLICT (version) DO NOTHING`, kernelProjectRemovalMigrationVersion)
	return err
}

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
