package runtime

import (
	"context"
	"database/sql"

	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
)

// kernelProjectRemovalMigrationVersion removes kernel Projects and the
// features that only hung off them: Project Skill references and their
// admission triggers, source monitors and their observations, the source
// policy registry that only authorized source observation, outreach threads,
// and the Project activity projection.
const kernelProjectRemovalMigrationVersion int64 = 77

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
	for _, table := range sqlstore.RetiredKernelProjectTables {
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
		VALUES ($1, 'remove kernel Projects, source monitors, source policies and outreach') ON CONFLICT (version) DO NOTHING`, kernelProjectRemovalMigrationVersion)
	return err
}
