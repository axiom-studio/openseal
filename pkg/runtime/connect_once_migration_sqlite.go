package runtime

import (
	"context"
	"database/sql"
)

// migrateConnectOnceSQLite mirrors PostgreSQL migration 74 and is safe to
// replay on every open.
func migrateConnectOnceSQLite(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := (connectOnceMigration{DB: tx, Table: func(name string) string { return name }, SQLite: true}).Run(context.Background()); err != nil {
		return err
	}
	return tx.Commit()
}
