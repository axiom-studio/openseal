package runtime

import (
	"context"
	"database/sql"
)

// spaceAccessMigrationVersion gives a space who may see it: spaces.access,
// spaces.owner_id (renamed from created_by) and the people added to it in
// space_people.
const spaceAccessMigrationVersion int64 = 84

// migrateSpaceAccess is PostgreSQL migration 84. Every existing space becomes
// "owner": only its owner sees it until they share it.
func (s *PostgresStore) migrateSpaceAccess(ctx context.Context, tx *sql.Tx) error {
	applied, err := s.postgresMigrationApplied(ctx, tx, spaceAccessMigrationVersion)
	if err != nil || applied {
		return err
	}
	spaces, people := s.table("spaces"), s.table("space_people")
	// A database that lost only this ledger row is already renamed; its
	// spaces keep the access they have.
	var legacy bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = 'spaces' AND column_name = 'created_by')`, s.schema).Scan(&legacy); err != nil {
		return err
	}
	var statements []string
	if legacy {
		statements = []string{
			`ALTER TABLE ` + spaces + ` RENAME COLUMN created_by TO owner_id`,
			`ALTER TABLE ` + spaces + ` ADD COLUMN access TEXT NOT NULL DEFAULT 'owner' CHECK (access IN ('owner', 'invited', 'everyone'))`,
		}
	}
	for _, statement := range append(statements,
		`CREATE TABLE IF NOT EXISTS `+people+` (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, space_id TEXT NOT NULL, user_id TEXT NOT NULL,
			added_by TEXT NOT NULL, added_at TIMESTAMPTZ NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, space_id, user_id),
			FOREIGN KEY (scope_kind, scope_id, space_id) REFERENCES `+spaces+` (scope_kind, scope_id, id) ON DELETE CASCADE
		)`,
		`CREATE INDEX IF NOT EXISTS space_people_user_idx ON `+people+` (scope_kind, scope_id, user_id)`,
	) {
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
	// A rollback may run again after an earlier one already took the spaces
	// table or this migration's columns away.
	_, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS `+s.table("space_people")+`;
		ALTER TABLE IF EXISTS `+s.table("spaces")+` DROP COLUMN IF EXISTS access;
		DO $$ BEGIN
			IF EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = '`+s.schema+`' AND table_name = 'spaces' AND column_name = 'owner_id') THEN
				ALTER TABLE `+s.table("spaces")+` RENAME COLUMN owner_id TO created_by;
			END IF;
		END $$`)
	return err
}
