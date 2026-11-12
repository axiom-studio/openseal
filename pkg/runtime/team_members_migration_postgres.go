package runtime

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
)

// teamMembersMigrationVersion removes Team role slots. What a role carried
// (name, purpose, channel participation, required Skills, approver and lead
// designation) moves onto the roster members that held it.
const teamMembersMigrationVersion int64 = 85

func (s *PostgresStore) teamMembersMigration(ctx context.Context, tx *sql.Tx) sqlstore.TeamMembersMigration {
	return sqlstore.TeamMembersMigration{
		Ctx: ctx, DB: tx, Table: s.table, Placeholder: func(n int) string { return fmt.Sprintf("$%d", n) },
		Exists: func(table string) (bool, error) {
			var relation sql.NullString
			err := tx.QueryRowContext(ctx, `SELECT to_regclass($1)`, s.table(table)).Scan(&relation)
			return relation.Valid, err
		},
	}
}

// migrateTeamMembers is PostgreSQL migration 85.
func (s *PostgresStore) migrateTeamMembers(ctx context.Context, tx *sql.Tx) error {
	applied, err := s.postgresMigrationApplied(ctx, tx, teamMembersMigrationVersion)
	if err != nil || applied {
		return err
	}
	if err := s.teamMembersMigration(ctx, tx).Up(); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+`(version, name)
		VALUES ($1, 'Team members replace role slots') ON CONFLICT (version) DO NOTHING`, teamMembersMigrationVersion)
	return err
}

// rollbackTeamMembers restores role slots for a kernel that requires them.
func (s *PostgresStore) rollbackTeamMembers(ctx context.Context, tx *sql.Tx) error {
	return s.teamMembersMigration(ctx, tx).Down()
}
