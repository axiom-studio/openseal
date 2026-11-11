package runtime

import (
	"context"
	"database/sql"
)

// ownerDefaultSpaceMigrationVersion makes the default space a per-owner
// flag: is_default is unique per (scope, owner) instead of per scope, and
// every owner of spaces has one.
const ownerDefaultSpaceMigrationVersion int64 = 86

// markOwnerDefaultSpaces marks, for every (scope, owner) that owns spaces and
// has no default space, their oldest space by (created_at, id). An owner who
// has a default space keeps it, so running it again changes nothing.
func markOwnerDefaultSpaces(spaces, isTrue string) string {
	sameOwner := func(alias string) string {
		return alias + `.scope_kind = s.scope_kind AND ` + alias + `.scope_id = s.scope_id AND ` + alias + `.owner_id = s.owner_id`
	}
	return `UPDATE ` + spaces + ` AS s SET is_default = ` + isTrue + `
		WHERE NOT EXISTS (SELECT 1 FROM ` + spaces + ` d WHERE ` + sameOwner("d") + ` AND d.is_default = ` + isTrue + `)
		AND NOT EXISTS (SELECT 1 FROM ` + spaces + ` o WHERE ` + sameOwner("o") + `
			AND (o.created_at < s.created_at OR (o.created_at = s.created_at AND o.id < s.id)))`
}

// migrateOwnerDefaultSpace is PostgreSQL migration 86. The spaces that were
// a scope's one default keep the flag for the owner they have.
func (s *PostgresStore) migrateOwnerDefaultSpace(ctx context.Context, tx *sql.Tx) error {
	applied, err := s.postgresMigrationApplied(ctx, tx, ownerDefaultSpaceMigrationVersion)
	if err != nil || applied {
		return err
	}
	spaces := s.table("spaces")
	// Every statement is safe on a database that lost only this ledger row.
	for _, statement := range []string{
		`DROP INDEX IF EXISTS ` + s.table("spaces_default_idx"),
		`CREATE UNIQUE INDEX IF NOT EXISTS spaces_owner_default_idx ON ` + spaces + ` (scope_kind, scope_id, owner_id) WHERE is_default`,
		markOwnerDefaultSpaces(spaces, "TRUE"),
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+`(version, name)
		VALUES ($1, 'a default space per owner') ON CONFLICT (version) DO NOTHING`, ownerDefaultSpaceMigrationVersion)
	return err
}

// rollbackOwnerDefaultSpace restores one default space per scope: the oldest
// default space of the scope keeps the flag and every other one loses it.
func (s *PostgresStore) rollbackOwnerDefaultSpace(ctx context.Context, tx *sql.Tx) error {
	spaces := s.table("spaces")
	// A rollback may run again after an earlier one already took the spaces
	// table away.
	_, err := tx.ExecContext(ctx, `DROP INDEX IF EXISTS `+s.table("spaces_owner_default_idx")+`;
		DO $$ BEGIN
			IF EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = '`+s.schema+`' AND table_name = 'spaces' AND column_name = 'is_default') THEN
				UPDATE `+spaces+` AS s SET is_default = FALSE
					WHERE s.is_default AND EXISTS (SELECT 1 FROM `+spaces+` o
						WHERE o.scope_kind = s.scope_kind AND o.scope_id = s.scope_id AND o.is_default
						AND (o.created_at < s.created_at OR (o.created_at = s.created_at AND o.id < s.id)));
				CREATE UNIQUE INDEX IF NOT EXISTS spaces_default_idx ON `+spaces+` (scope_kind, scope_id) WHERE is_default;
			END IF;
		END $$`)
	return err
}

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
		markOwnerDefaultSpaces("spaces", "1"),
	} {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	return tx.Commit()
}
