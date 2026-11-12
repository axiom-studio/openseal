package postgres

import (
	"context"
	"database/sql"
)

// openSystemSpacesMigrationVersion opens to everyone in the tenant every space
// that no person owns. Migration 84 made every space "owner", which hid the
// spaces a whole tenant shared before spaces had owners: the scope-wide
// default space (migration 67/78) and the "Personal" space created by
// "system" that holds most of a tenant's chats.
const openSystemSpacesMigrationVersion int64 = 87

// noPersonOwnsSpace matches a space owned by "system" or by nobody, or a
// scope's former one default space, whose id migration 67 derived from the
// scope ('personal-' then, renamed 'default-' by migration 78). A space owned
// by a service account looks like a person's here: the kernel cannot see
// whose account it is, so axiomcloud opens those when it records them.
const noPersonOwnsSpace = `(owner_id IS NULL OR owner_id IN ('system', '')
	OR id = 'default-' || md5(scope_kind || ':' || scope_id))`

// migrateOpenSystemSpaces is PostgreSQL migration 87. Owners stay as they are.
func (s *Store) migrateOpenSystemSpaces(ctx context.Context, tx *sql.Tx) error {
	applied, err := s.postgresMigrationApplied(ctx, tx, openSystemSpacesMigrationVersion)
	if err != nil || applied {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE `+s.table("spaces")+` SET access = 'everyone'
		WHERE access <> 'everyone' AND `+noPersonOwnsSpace); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+`(version, name)
		VALUES ($1, 'open the spaces no person owns') ON CONFLICT (version) DO NOTHING`, openSystemSpacesMigrationVersion)
	return err
}

// rollbackOpenSystemSpaces makes the matching spaces "owner" again. It cannot
// tell a space this migration opened from one a person opened later, and
// closes both.
func (s *Store) rollbackOpenSystemSpaces(ctx context.Context, tx *sql.Tx) error {
	// A rollback may run again after an earlier one already took the access
	// column or the spaces table away.
	_, err := tx.ExecContext(ctx, `DO $$ BEGIN
			IF EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = '`+s.schema+`' AND table_name = 'spaces' AND column_name = 'access') THEN
				UPDATE `+s.table("spaces")+` SET access = 'owner' WHERE access = 'everyone' AND `+noPersonOwnsSpace+`;
			END IF;
		END $$`)
	return err
}
