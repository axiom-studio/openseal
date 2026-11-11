package runtime

import (
	"context"
	"database/sql"
)

// spaceAccessMigrationVersion gives a space who may see it: spaces.access,
// spaces.owner_id (renamed from created_by) and the people added to it in
// space_people.
const spaceAccessMigrationVersion int64 = 84

// migrateSpaceAccess is PostgreSQL migration 84. Existing spaces become
// "owner". The Default space, owned by no person, becomes "everyone".
func (s *PostgresStore) migrateSpaceAccess(ctx context.Context, tx *sql.Tx) error {
	applied, err := s.postgresMigrationApplied(ctx, tx, spaceAccessMigrationVersion)
	if err != nil || applied {
		return err
	}
	spaces, people := s.table("spaces"), s.table("space_people")
	for _, statement := range []string{
		`ALTER TABLE ` + spaces + ` RENAME COLUMN created_by TO owner_id`,
		`ALTER TABLE ` + spaces + ` ADD COLUMN access TEXT NOT NULL DEFAULT 'owner' CHECK (access IN ('owner', 'invited', 'everyone'))`,
		`UPDATE ` + spaces + ` SET access = 'everyone' WHERE is_default`,
		`CREATE TABLE IF NOT EXISTS ` + people + ` (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, space_id TEXT NOT NULL, user_id TEXT NOT NULL,
			added_by TEXT NOT NULL, added_at TIMESTAMPTZ NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, space_id, user_id),
			FOREIGN KEY (scope_kind, scope_id, space_id) REFERENCES ` + spaces + ` (scope_kind, scope_id, id) ON DELETE CASCADE
		)`,
		`CREATE INDEX IF NOT EXISTS space_people_user_idx ON ` + people + ` (scope_kind, scope_id, user_id)`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+`(version, name)
		VALUES ($1, 'space access and people') ON CONFLICT (version) DO NOTHING`, spaceAccessMigrationVersion)
	return err
}

// rollbackSpaceAccess restores created_by. Each space's access and the
// people added to it are lost.
func (s *PostgresStore) rollbackSpaceAccess(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS `+s.table("space_people")+`;
		ALTER TABLE `+s.table("spaces")+` DROP COLUMN IF EXISTS access;
		ALTER TABLE `+s.table("spaces")+` RENAME COLUMN owner_id TO created_by`)
	return err
}

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
		`UPDATE spaces SET access = 'everyone' WHERE is_default = 1`,
	} {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	return tx.Commit()
}
