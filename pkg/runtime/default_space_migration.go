package runtime

import (
	"context"
	"database/sql"

	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
)

// defaultSpaceMigrationVersion renames each scope's Personal space to its
// Default space: the column is_personal becomes is_default, the id prefix
// personal- becomes default-, and a space still named Personal is named
// Default. Members, conversations and artifact provenance follow the new id.
const defaultSpaceMigrationVersion int64 = 78

// migrateDefaultSpace is PostgreSQL migration 78.
func (s *PostgresStore) migrateDefaultSpace(ctx context.Context, tx *sql.Tx) error {
	applied, err := s.postgresMigrationApplied(ctx, tx, defaultSpaceMigrationVersion)
	if err != nil || applied {
		return err
	}
	var legacy bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = 'spaces' AND column_name = 'is_personal')`, s.schema).Scan(&legacy); err != nil {
		return err
	}
	var statements []string
	if legacy {
		statements = append(statements,
			`ALTER TABLE `+s.table("spaces")+` RENAME COLUMN is_personal TO is_default`,
			`ALTER INDEX IF EXISTS `+s.table("spaces_personal_idx")+` RENAME TO spaces_default_idx`,
		)
	}
	statements = append(statements, sqlstore.DefaultSpaceRewrites(s.table("spaces"), s.table("space_members"), s.table("conversations"), s.table("artifacts"), true)...)
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+`(version, name)
		VALUES ($1, 'rename the Personal space to the Default space') ON CONFLICT (version) DO NOTHING`, defaultSpaceMigrationVersion)
	return err
}
