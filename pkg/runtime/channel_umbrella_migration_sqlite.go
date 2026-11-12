package runtime

import (
	"database/sql"
)

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
