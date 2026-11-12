package runtime

import (
	"context"
	"database/sql"
)

// migrateTeamSkillGrantRemovalSQLite mirrors PostgreSQL migration 71 and is
// safe to replay on every open.
func migrateTeamSkillGrantRemovalSQLite(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := stripTeamSkillGrants(context.Background(), tx, func(name string) string { return name }, func(int) string { return "?" }); err != nil {
		return err
	}
	return tx.Commit()
}
