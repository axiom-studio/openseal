package runtime

import (
	"context"
	"database/sql"
)

// migrateWorkspaceSearch records the indexed search schema after the five
// expression indexes have been installed by their owning table migrations.
func (s *PostgresStore) migrateWorkspaceSearch(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+` (version, name) VALUES (46, 'indexed workspace search') ON CONFLICT (version) DO NOTHING`)
	return err
}
