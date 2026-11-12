package sqlite

import (
	"crypto/md5"
	"database/sql"
	"encoding/hex"
)

// migrateSpaceAccessSQLite mirrors PostgreSQL migrations 84 and 87. It
// renames and backfills only while created_by is still there, so a later open
// does nothing.
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
	if err := openSystemSpacesSQLite(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// openSystemSpacesSQLite mirrors PostgreSQL migration 87: every space owned by
// "system" or by nobody, and every scope's former one default space, is
// "everyone". Service-account owners are opened by axiomcloud, which can tell
// them from people.
func openSystemSpacesSQLite(tx *sql.Tx) error {
	rows, err := tx.Query(`SELECT scope_kind, scope_id, id, COALESCE(owner_id, '') FROM spaces WHERE access <> 'everyone'`)
	if err != nil {
		return err
	}
	type key struct{ kind, scope, id string }
	var open []key
	for rows.Next() {
		var space key
		var ownerID string
		if err := rows.Scan(&space.kind, &space.scope, &space.id, &ownerID); err != nil {
			rows.Close()
			return err
		}
		digest := md5.Sum([]byte(space.kind + ":" + space.scope))
		if ownerID == "system" || ownerID == "" || space.id == "default-"+hex.EncodeToString(digest[:]) {
			open = append(open, space)
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, space := range open {
		if _, err := tx.Exec(`UPDATE spaces SET access = 'everyone' WHERE scope_kind = ? AND scope_id = ? AND id = ?`,
			space.kind, space.scope, space.id); err != nil {
			return err
		}
	}
	return nil
}
