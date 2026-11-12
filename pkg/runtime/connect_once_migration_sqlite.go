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
	if err := (connectOnceMigration{db: tx, table: func(name string) string { return name }, sqlite: true}).run(context.Background()); err != nil {
		return err
	}
	return tx.Commit()
}
