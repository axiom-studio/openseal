package runtime

import (
	"database/sql"
)

// migrateTeamRoutingSQLite is safe to replay on every SQLite open.
func migrateTeamRoutingSQLite(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, path := range teamCoordinationPaths {
		if _, err = tx.Exec(`UPDATE `+path.table+`
			SET payload = json_remove(payload, ?)
			WHERE json_type(payload, ?) IS NOT NULL`, path.sqlite, path.sqlite); err != nil {
			return err
		}
	}
	return tx.Commit()
}
