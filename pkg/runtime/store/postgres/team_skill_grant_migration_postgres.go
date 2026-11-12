package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
)

const teamSkillGrantRemovalMigrationVersion int64 = 71

func (s *PostgresStore) migrateTeamSkillGrantRemoval(ctx context.Context, tx *sql.Tx) error {
	applied, err := s.postgresMigrationApplied(ctx, tx, teamSkillGrantRemovalMigrationVersion)
	if err != nil || applied {
		return err
	}
	if err := sqlstore.StripTeamSkillGrants(ctx, tx, s.table, func(n int) string { return fmt.Sprintf("$%d", n) }); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+`(version,name) VALUES($1,'Team role Skill grants removed') ON CONFLICT(version) DO NOTHING`, teamSkillGrantRemovalMigrationVersion)
	return err
}
