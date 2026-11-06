package runtime

import (
	"context"
	"database/sql"
)

const channelUmbrellaMigrationVersion int64 = 73

// migrateChannelUmbrellas gives every existing conversation gateway an Agent
// owner (all of them were Agent-owned) and moves parked event waits from the
// assigned Agent to the run's umbrella: the Team for a Team run.
func (s *PostgresStore) migrateChannelUmbrellas(ctx context.Context, tx *sql.Tx) error {
	applied, err := s.postgresMigrationApplied(ctx, tx, channelUmbrellaMigrationVersion)
	if err != nil || applied {
		return err
	}
	for _, statement := range []string{
		`UPDATE ` + s.table("external_conversation_gateways") + `
			SET payload = jsonb_set(payload, '{gateway,owner}', jsonb_build_object('type', 'agent', 'id', payload#>>'{gateway,deploymentId}'))
			WHERE payload#>'{gateway,owner}' IS NULL`,
		`UPDATE ` + s.table("run_event_waits") + ` w
			SET payload = (w.payload - 'assignedAgentId') || jsonb_build_object('umbrellaId',
				CASE WHEN r.payload#>>'{owner,type}' = 'team' THEN r.payload#>>'{owner,id}' ELSE w.payload->>'assignedAgentId' END)
			FROM ` + s.table("agent_runs") + ` r
			WHERE r.scope_kind = w.scope_kind AND r.scope_id = w.scope_id AND r.id = w.run_id AND w.payload ? 'assignedAgentId'`,
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
		`UPDATE run_event_waits
			SET payload = json_set(json_remove(payload, '$.assignedAgentId'), '$.umbrellaId', COALESCE((
				SELECT CASE WHEN json_extract(r.payload, '$.owner.type') = 'team' THEN json_extract(r.payload, '$.owner.id') END
				FROM agent_runs r WHERE r.scope_kind = run_event_waits.scope_kind AND r.scope_id = run_event_waits.scope_id AND r.id = run_event_waits.run_id
			), json_extract(payload, '$.assignedAgentId')))
			WHERE json_type(payload, '$.assignedAgentId') IS NOT NULL`,
	} {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	return tx.Commit()
}
