package sqlite

import (
	"context"
	"database/sql"

	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
)

// migrateInteractionUmbrellasSQLite mirrors PostgreSQL migration 72.
func migrateInteractionUmbrellasSQLite(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := sqlstore.StampInteractionUmbrellas(context.Background(), tx, func(name string) string { return name }, func(int) string { return "?" }); err != nil {
		return err
	}
	return tx.Commit()
}
