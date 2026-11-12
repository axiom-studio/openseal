package runtime

import (
	"database/sql"

	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
)

// migrateOwnerDefaultSpacesSQLite mirrors PostgreSQL migration 86. SQLite has
// no ledger: it runs on every open, after owner_id exists, and changes
// nothing once every owner has a default space.
func migrateOwnerDefaultSpacesSQLite(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, statement := range []string{
		`DROP INDEX IF EXISTS idx_spaces_default`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_spaces_owner_default ON spaces(scope_kind, scope_id, owner_id) WHERE is_default = 1`,
		sqlstore.MarkOwnerDefaultSpaces("spaces", "1"),
	} {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	return tx.Commit()
}
