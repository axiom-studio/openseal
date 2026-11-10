package runtime

import (
	"context"
	"database/sql"
)

const channelUmbrellaMigrationVersion int64 = 73

// migrateChannelUmbrellas gives every existing conversation gateway an Agent
// owner (all of them were Agent-owned).
func (s *PostgresStore) migrateChannelUmbrellas(ctx context.Context, tx *sql.Tx) error {
	applied, err := s.postgresMigrationApplied(ctx, tx, channelUmbrellaMigrationVersion)
	if err != nil || applied {
		return err
	}
	for _, statement := range []string{
		`UPDATE ` + s.table("external_conversation_gateways") + `
			SET payload = jsonb_set(payload, '{gateway,owner}', jsonb_build_object('type', 'agent', 'id', payload#>>'{gateway,deploymentId}'))
			WHERE payload#>'{gateway,owner}' IS NULL`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+`(version,name) VALUES($1,'channels and event waits belong to the run umbrella') ON CONFLICT(version) DO NOTHING`, channelUmbrellaMigrationVersion)
	return err
}

// migrateChannelUmbrellasSQLite mirrors PostgreSQL migration 73 and is safe
// to replay on every open.
func migrateChannelUmbrellasSQLite(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, statement := range []string{
		`UPDATE external_conversation_gateways
			SET payload = json_set(payload, '$.gateway.owner', json_object('type', 'agent', 'id', json_extract(payload, '$.gateway.deploymentId')))
			WHERE json_type(payload, '$.gateway.owner') IS NULL`,
	} {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	return tx.Commit()
}
