package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
)

func migrateRunbookActivationsSQLite(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS runbook_activations (
			id TEXT NOT NULL,
			scope_kind TEXT NOT NULL,
			scope_id TEXT NOT NULL,
			owner_type TEXT NOT NULL,
			owner_id TEXT NOT NULL,
			objective_id TEXT NOT NULL,
			assigned_agent_id TEXT NOT NULL,
			status TEXT NOT NULL,
			next_run_at DATETIME,
			revision INTEGER NOT NULL,
			updated_at DATETIME NOT NULL,
			idempotency_key_hash TEXT NOT NULL DEFAULT '',
			payload TEXT NOT NULL,
			PRIMARY KEY(scope_kind, scope_id, id)
		);
		CREATE INDEX IF NOT EXISTS idx_runbook_activations_due
			ON runbook_activations(scope_kind, scope_id, status, next_run_at, id);
		CREATE INDEX IF NOT EXISTS idx_runbook_activations_objective
			ON runbook_activations(scope_kind, scope_id, objective_id, status, updated_at DESC);
		CREATE INDEX IF NOT EXISTS idx_runbook_activations_owner
			ON runbook_activations(scope_kind, scope_id, owner_type, owner_id, status, updated_at DESC);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_runbook_activations_idempotency
			ON runbook_activations(scope_kind, scope_id, idempotency_key_hash) WHERE idempotency_key_hash <> '';
	`)
	return err
}

func (s *SQLiteStore) CreateRunbookActivation(ctx context.Context, value *models.RunbookActivation) error {
	if err := value.Validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	eventKind, eventSourceRef := sqlstore.NullableRoutineEventIndex(value)
	_, err = s.db.ExecContext(ctx, `INSERT INTO runbook_activations
		(id,scope_kind,scope_id,owner_type,owner_id,objective_id,assigned_agent_id,status,next_run_at,revision,updated_at,idempotency_key_hash,payload,event_kind,event_source_ref)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, value.ID, value.Scope.Kind, value.Scope.ID, value.Owner.Type, value.Owner.ID,
		value.ObjectiveID, value.AssignedAgentID, value.Status, value.NextRunAt, value.Revision, value.UpdatedAt, value.IdempotencyKeyHash, string(payload), eventKind, eventSourceRef)
	return err
}

func (s *SQLiteStore) GetRunbookActivation(ctx context.Context, scope models.Scope, id string) (*models.RunbookActivation, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	var payload string
	err := s.db.QueryRowContext(ctx, `SELECT payload FROM runbook_activations WHERE scope_kind=? AND scope_id=? AND id=?`, scope.Kind, scope.ID, id).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return sqlstore.DecodeRunbookActivation(payload)
}

func (s *SQLiteStore) ListRunbookActivations(ctx context.Context, filter models.RunbookActivationFilter) ([]*models.RunbookActivation, error) {
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM runbook_activations WHERE scope_kind=? AND scope_id=?`, filter.Scope.Kind, filter.Scope.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := make([]*models.RunbookActivation, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		value, err := sqlstore.DecodeRunbookActivation(payload)
		if err != nil {
			return nil, err
		}
		if models.MatchesRunbookActivationFilter(value, filter) {
			values = append(values, value)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return models.SortAndLimitRunbookActivations(values, filter.Limit, filter.Offset), nil
}

func (s *SQLiteStore) UpdateRunbookActivation(ctx context.Context, value *models.RunbookActivation, expectedRevision int64) error {
	if err := value.Validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	eventKind, eventSourceRef := sqlstore.NullableRoutineEventIndex(value)
	result, err := s.db.ExecContext(ctx, `UPDATE runbook_activations SET owner_type=?,owner_id=?,objective_id=?,assigned_agent_id=?,status=?,next_run_at=?,revision=?,updated_at=?,payload=?,event_kind=?,event_source_ref=? WHERE scope_kind=? AND scope_id=? AND id=? AND revision=?`,
		value.Owner.Type, value.Owner.ID, value.ObjectiveID, value.AssignedAgentID, value.Status, value.NextRunAt, value.Revision, value.UpdatedAt, string(payload), eventKind, eventSourceRef, value.Scope.Kind, value.Scope.ID, value.ID, expectedRevision)
	if err != nil {
		return err
	}
	changed, _ := result.RowsAffected()
	if changed == 0 {
		return models.ErrRunbookActivationRevision
	}
	return nil
}

func (s *SQLiteStore) ListRunbookActivationScopes(ctx context.Context) ([]models.Scope, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT scope_kind,scope_id FROM runbook_activations ORDER BY scope_kind,scope_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := make([]models.Scope, 0)
	for rows.Next() {
		var scope models.Scope
		if err := rows.Scan(&scope.Kind, &scope.ID); err != nil {
			return nil, err
		}
		values = append(values, scope)
	}
	return values, rows.Err()
}
