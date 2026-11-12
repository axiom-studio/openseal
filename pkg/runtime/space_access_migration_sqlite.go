package runtime

import (
	"database/sql"
)

// migrateSpaceAccessSQLite mirrors PostgreSQL migration 84. It renames and
// backfills only while created_by is still there, so a later open does
// nothing.
func migrateSpaceAccessSQLite(db *sql.DB) error {
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS space_people (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, space_id TEXT NOT NULL, user_id TEXT NOT NULL,
			added_by TEXT NOT NULL, added_at DATETIME NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, space_id, user_id)
		);
		CREATE INDEX IF NOT EXISTS idx_space_people_user ON space_people(scope_kind, scope_id, user_id);
	`); err != nil {
		return err
	}
	legacy, err := sqliteColumnExists(db, "spaces", "created_by")
	if err != nil || !legacy {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, statement := range []string{
		`ALTER TABLE spaces RENAME COLUMN created_by TO owner_id`,
		`ALTER TABLE spaces ADD COLUMN access TEXT NOT NULL DEFAULT 'owner'`,
	} {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	return tx.Commit()
}
