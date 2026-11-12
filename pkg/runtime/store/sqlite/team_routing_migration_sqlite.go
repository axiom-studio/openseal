package sqlite

import (
	"database/sql"

	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
)

// migrateTeamRoutingSQLite is safe to replay on every SQLite open.
func migrateTeamRoutingSQLite(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, path := range sqlstore.TeamCoordinationPaths {
		if _, err = tx.Exec(`UPDATE `+path.Table+`
			SET payload = json_remove(payload, ?)
			WHERE json_type(payload, ?) IS NOT NULL`, path.SQLite, path.SQLite); err != nil {
			return err
		}
	}
	return tx.Commit()
}
