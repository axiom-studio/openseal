package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

const runbookActivationMigrationVersion int64 = 35

func (s *PostgresStore) migrateRunbookActivations(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS `+s.table("runbook_activations")+` (
			id TEXT NOT NULL,
			scope_kind TEXT NOT NULL,
			scope_id TEXT NOT NULL,
			owner_type TEXT NOT NULL,
			owner_id TEXT NOT NULL,
			objective_id TEXT NOT NULL,
			assigned_agent_id TEXT NOT NULL,
			status TEXT NOT NULL,
			next_run_at TIMESTAMPTZ,
			revision BIGINT NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL,
			idempotency_key_hash TEXT NOT NULL DEFAULT '',
			payload JSONB NOT NULL,
			PRIMARY KEY(scope_kind, scope_id, id),
			CHECK(revision > 0)
		);
		CREATE INDEX IF NOT EXISTS runbook_activations_due_idx ON `+s.table("runbook_activations")+`(scope_kind,scope_id,status,next_run_at,id);
		CREATE INDEX IF NOT EXISTS runbook_activations_objective_idx ON `+s.table("runbook_activations")+`(scope_kind,scope_id,objective_id,status,updated_at DESC);
		CREATE INDEX IF NOT EXISTS runbook_activations_owner_idx ON `+s.table("runbook_activations")+`(scope_kind,scope_id,owner_type,owner_id,status,updated_at DESC);
		CREATE UNIQUE INDEX IF NOT EXISTS runbook_activations_idempotency_idx ON `+s.table("runbook_activations")+`(scope_kind,scope_id,idempotency_key_hash) WHERE idempotency_key_hash <> '';
	`); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+`(version,name) VALUES($1,'first-class Runbook activations') ON CONFLICT(version) DO NOTHING`, runbookActivationMigrationVersion)
	return err
}

func (s *PostgresStore) CreateRunbookActivation(ctx context.Context, value *RunbookActivation) error {
	if err := value.Validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO `+s.table("runbook_activations")+`
		(id,scope_kind,scope_id,owner_type,owner_id,objective_id,assigned_agent_id,status,next_run_at,revision,updated_at,idempotency_key_hash,payload)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13::jsonb)`, value.ID, value.Scope.Kind, value.Scope.ID, value.Owner.Type,
		value.Owner.ID, value.ObjectiveID, value.AssignedAgentID, value.Status, value.NextRunAt, value.Revision, value.UpdatedAt, value.IdempotencyKeyHash, string(payload))
	return err
}

func (s *PostgresStore) GetRunbookActivation(ctx context.Context, scope Scope, id string) (*RunbookActivation, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	var payload string
	err := s.db.QueryRowContext(ctx, `SELECT payload FROM `+s.table("runbook_activations")+` WHERE scope_kind=$1 AND scope_id=$2 AND id=$3`, scope.Kind, scope.ID, id).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeRunbookActivation(payload)
}

func (s *PostgresStore) ListRunbookActivations(ctx context.Context, filter RunbookActivationFilter) ([]*RunbookActivation, error) {
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT payload::text FROM `+s.table("runbook_activations")+` WHERE scope_kind=$1 AND scope_id=$2`, filter.Scope.Kind, filter.Scope.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := make([]*RunbookActivation, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		value, err := decodeRunbookActivation(payload)
		if err != nil {
			return nil, err
		}
		if matchesRunbookActivationFilter(value, filter) {
			values = append(values, value)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return sortAndLimitRunbookActivations(values, filter.Limit), nil
}

func (s *PostgresStore) UpdateRunbookActivation(ctx context.Context, value *RunbookActivation, expectedRevision int64) error {
	if err := value.Validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE `+s.table("runbook_activations")+` SET owner_type=$1,owner_id=$2,objective_id=$3,assigned_agent_id=$4,status=$5,next_run_at=$6,revision=$7,updated_at=$8,payload=$9::jsonb WHERE scope_kind=$10 AND scope_id=$11 AND id=$12 AND revision=$13`,
		value.Owner.Type, value.Owner.ID, value.ObjectiveID, value.AssignedAgentID, value.Status, value.NextRunAt, value.Revision, value.UpdatedAt, string(payload), value.Scope.Kind, value.Scope.ID, value.ID, expectedRevision)
	if err != nil {
		return err
	}
	changed, _ := result.RowsAffected()
	if changed == 0 {
		return ErrRunbookActivationRevision
	}
	return nil
}

func (s *PostgresStore) ListRunbookActivationScopes(ctx context.Context) ([]Scope, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT scope_kind,scope_id FROM `+s.table("runbook_activations")+` ORDER BY scope_kind,scope_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := make([]Scope, 0)
	for rows.Next() {
		var scope Scope
		if err := rows.Scan(&scope.Kind, &scope.ID); err != nil {
			return nil, err
		}
		values = append(values, scope)
	}
	return values, rows.Err()
}
