package runtime

import (
	"context"
	"database/sql"
)

func sqliteTeamMembersMigration(tx *sql.Tx) teamMembersMigration {
	return teamMembersMigration{
		Ctx: context.Background(), DB: tx, Table: func(name string) string { return name }, Placeholder: func(int) string { return "?" },
		Exists: func(table string) (bool, error) {
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
	if err := sqliteTeamMembersMigration(tx).Up(); err != nil {
		return err
	}
	return tx.Commit()
}
