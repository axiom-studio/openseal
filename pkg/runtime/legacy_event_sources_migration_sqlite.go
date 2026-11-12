package runtime

import (
	"database/sql"
)

// migrateRemoveEventSourcesSQLite mirrors PostgreSQL migration 79. Every
// statement is idempotent, so a later open does nothing.
func migrateRemoveEventSourcesSQLite(db *sql.DB) error {
	for _, table := range legacyEventSourceTables {
		if _, err := db.Exec(`DROP TABLE IF EXISTS ` + table); err != nil {
			return err
		}
	}
	return nil
}
