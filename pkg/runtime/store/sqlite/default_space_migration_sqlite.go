package sqlite

import (
	"database/sql"
	"strings"

	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
)

// migrateDefaultSpaceSQLite mirrors PostgreSQL migration 78. It runs before
// the spaces tables are ensured, whose index names is_default, and does
// nothing once is_personal is gone.
func migrateDefaultSpaceSQLite(db *sql.DB) error {
	legacy, err := sqliteColumnExists(db, "spaces", "is_personal")
	if err != nil || !legacy {
		return err
	}
	// The artifact store may keep its own database.
	artifacts, err := sqliteColumnExists(db, "artifacts", "payload")
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	statements := append([]string{
		`DROP INDEX IF EXISTS idx_spaces_personal`,
		`ALTER TABLE spaces RENAME COLUMN is_personal TO is_default`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_spaces_default ON spaces(scope_kind, scope_id) WHERE is_default = 1`,
	}, sqlstore.DefaultSpaceRewrites("spaces", "space_members", "conversations", "artifacts", false)...)
	if !artifacts {
		statements = removeArtifactRewrite(statements)
	}
	for _, statement := range statements {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func removeArtifactRewrite(statements []string) []string {
	kept := statements[:0]
	for _, statement := range statements {
		if !strings.HasPrefix(statement, "UPDATE artifacts ") {
			kept = append(kept, statement)
		}
	}
	return kept
}
