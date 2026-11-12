package runtime

import (
	"context"
	"database/sql"
)

func sqliteTeamMembersMigration(tx *sql.Tx) teamMembersMigration {
	return teamMembersMigration{
		ctx: context.Background(), db: tx, table: func(name string) string { return name }, placeholder: func(int) string { return "?" },
		exists: func(table string) (bool, error) {
			var count int
			err := tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&count)
			return count > 0, err
		},
	}
}

// migrateTeamMembersSQLite mirrors PostgreSQL migration 85. It only touches
// payloads that still carry role slots, so a later open does nothing.
func migrateTeamMembersSQLite(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := sqliteTeamMembersMigration(tx).up(); err != nil {
		return err
	}
	return tx.Commit()
}
