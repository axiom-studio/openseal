package sqlite

import (
	"database/sql"

	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
)

// migrateRemoveEventSourcesSQLite mirrors PostgreSQL migration 79. Every
// statement is idempotent, so a later open does nothing.
func migrateRemoveEventSourcesSQLite(db *sql.DB) error {
	for _, table := range sqlstore.LegacyEventSourceTables {
		if _, err := db.Exec(`DROP TABLE IF EXISTS ` + table); err != nil {
			return err
		}
	}
	return nil
}
