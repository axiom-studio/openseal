package sqlite

import (
	"database/sql"
)

func migrateSkillBindingLifecycleRepairSQLite(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`UPDATE skill_bindings
		SET payload = json_set(payload, '$.createdAt', json_extract(payload, '$.updatedAt'))
		WHERE json_array_length(COALESCE(json_extract(payload, '$.lifecycle'), json('[]'))) > 0
			AND (json_extract(payload, '$.createdAt') IS NULL OR json_extract(payload, '$.createdAt') = '0001-01-01T00:00:00Z')
			AND json_extract(payload, '$.updatedAt') IS NOT NULL
			AND json_extract(payload, '$.updatedAt') <> '0001-01-01T00:00:00Z'`); err != nil {
		return err
	}
	return tx.Commit()
}
