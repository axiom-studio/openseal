package runtime

import (
	"context"
	"database/sql"
)

const skillBindingLifecycleRepairMigrationVersion int64 = 32

// migrateSkillBindingLifecycleRepair repairs bindings upgraded by the first
// reference-upgrade implementation. Atomic workforce bindings did not yet
// have management timestamps; that implementation appended lifecycle revision
// 2 without establishing createdAt, making the otherwise valid binding
// unreadable. updatedAt is the authoritative first management boundary.
func (s *PostgresStore) migrateSkillBindingLifecycleRepair(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `UPDATE `+s.table("skill_bindings")+`
		SET payload = jsonb_set(payload, '{createdAt}', payload->'updatedAt', true)
		WHERE jsonb_array_length(COALESCE(payload->'lifecycle', '[]'::jsonb)) > 0
			AND (payload->>'createdAt' IS NULL OR payload->>'createdAt' = '0001-01-01T00:00:00Z')
			AND payload->>'updatedAt' IS NOT NULL
			AND payload->>'updatedAt' <> '0001-01-01T00:00:00Z'`); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+`
		(version, name) VALUES ($1, 'repair managed Skill binding lifecycle timestamps')
		ON CONFLICT(version) DO NOTHING`, skillBindingLifecycleRepairMigrationVersion)
	return err
}

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
