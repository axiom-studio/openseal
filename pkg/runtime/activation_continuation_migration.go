package runtime

import (
	"context"
	"database/sql"
)

const activationContinuationMigrationVersion int64 = 31

// migrateActivationContinuations recovers the durable authoring continuation
// for deployments created before the reference became part of the portable
// contract. The applied inactive Change Set remains the source of truth; no
// lifecycle state or revision is changed by this migration.
func (s *PostgresStore) migrateActivationContinuations(ctx context.Context, tx *sql.Tx) error {
	for _, target := range []struct {
		table, resourceKind, statusPath, firstStatus, secondStatus string
	}{
		{"agent_deployments", "agent_deployment", "rolloutStatus", "pending", "paused"},
		{"team_deployments", "team_deployment", "status", "draft", "paused"},
	} {
		statement := `WITH ranked_continuations AS (
			SELECT changes.scope_kind, changes.scope_id, resource->>'id' AS deployment_id,
				changes.id AS change_set_id,
				row_number() OVER (
					PARTITION BY changes.scope_kind, changes.scope_id, resource->>'id'
					ORDER BY changes.updated_at DESC, changes.id DESC
				) AS position
			FROM ` + s.table("workforce_change_sets") + ` AS changes
			CROSS JOIN LATERAL jsonb_array_elements(
				COALESCE(changes.payload #> '{applyReceipt,resources}', '[]'::jsonb)
			) AS resource
			WHERE changes.status = 'applied'
				AND changes.payload #>> '{applyReceipt,activation}' = 'inactive'
				AND resource->>'kind' = $1
		)
		UPDATE ` + s.table(target.table) + ` AS deployment
		SET payload = jsonb_set(
			deployment.payload,
			'{activation}',
			jsonb_build_object('changeSetId', continuation.change_set_id),
			true
		)
		FROM ranked_continuations AS continuation
		WHERE continuation.position = 1
			AND continuation.scope_kind = deployment.scope_kind
			AND continuation.scope_id = deployment.scope_id
			AND continuation.deployment_id = deployment.id
			AND deployment.payload->'activation' IS NULL
			AND deployment.payload->>$2 IN ($3, $4)`
		if _, err := tx.ExecContext(
			ctx, statement, target.resourceKind, target.statusPath, target.firstStatus, target.secondStatus,
		); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+`
		(version, name) VALUES ($1, 'recover workforce activation continuations')
		ON CONFLICT(version) DO NOTHING`, activationContinuationMigrationVersion)
	return err
}

func migrateActivationContinuationsSQLite(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, target := range []struct {
		table, resourceKind, statusPath, firstStatus, secondStatus string
	}{
		{"agent_deployments", "agent_deployment", "$.rolloutStatus", "pending", "paused"},
		{"team_deployments", "team_deployment", "$.status", "draft", "paused"},
	} {
		statement := `UPDATE ` + target.table + `
		SET payload = json_set(
			payload,
			'$.activation.changeSetId',
			(
				SELECT changes.id
				FROM workforce_change_sets AS changes,
					json_each(COALESCE(json_extract(changes.payload, '$.applyReceipt.resources'), json('[]'))) AS resource
				WHERE changes.scope_kind = ` + target.table + `.scope_kind
					AND changes.scope_id = ` + target.table + `.scope_id
					AND changes.status = 'applied'
					AND json_extract(changes.payload, '$.applyReceipt.activation') = 'inactive'
					AND json_extract(resource.value, '$.kind') = ?
					AND json_extract(resource.value, '$.id') = ` + target.table + `.id
				ORDER BY changes.updated_at DESC, changes.id DESC
				LIMIT 1
			)
		)
		WHERE json_type(payload, '$.activation') IS NULL
			AND json_extract(payload, ?) IN (?, ?)
			AND EXISTS (
				SELECT 1
				FROM workforce_change_sets AS changes,
					json_each(COALESCE(json_extract(changes.payload, '$.applyReceipt.resources'), json('[]'))) AS resource
				WHERE changes.scope_kind = ` + target.table + `.scope_kind
					AND changes.scope_id = ` + target.table + `.scope_id
					AND changes.status = 'applied'
					AND json_extract(changes.payload, '$.applyReceipt.activation') = 'inactive'
					AND json_extract(resource.value, '$.kind') = ?
					AND json_extract(resource.value, '$.id') = ` + target.table + `.id
			)`
		if _, err = tx.Exec(
			statement,
			target.resourceKind, target.statusPath, target.firstStatus, target.secondStatus, target.resourceKind,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}
