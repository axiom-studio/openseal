package sqlite

import (
	"database/sql"

	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
)

// migrateRemoveEventWaitsSQLite mirrors PostgreSQL migration 81. Every
// statement is idempotent, so a later open does nothing.
func migrateRemoveEventWaitsSQLite(db *sql.DB) error {
	for _, table := range sqlstore.LegacyEventWaitTables {
		if _, err := db.Exec(`DROP TABLE IF EXISTS ` + table); err != nil {
			return err
		}
	}
	return nil
}
