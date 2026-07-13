package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/axiom-studio/openseal/pkg/authoring"
	"github.com/axiom-studio/openseal/pkg/capability"
)

func (s *PostgresStore) migrateAuthoringChangeSets(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS `+s.table("workforce_change_sets")+` (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, id TEXT NOT NULL,
			parent_id TEXT NOT NULL DEFAULT '', status TEXT NOT NULL, revision BIGINT NOT NULL,
			idempotency_key TEXT NOT NULL, request_digest TEXT NOT NULL, candidate_digest TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL, payload JSONB NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, id),
			UNIQUE (scope_kind, scope_id, idempotency_key)
		);
		CREATE INDEX IF NOT EXISTS workforce_change_sets_parent_idx ON `+s.table("workforce_change_sets")+`
			(scope_kind, scope_id, parent_id, created_at);
		CREATE INDEX IF NOT EXISTS workforce_change_sets_status_idx ON `+s.table("workforce_change_sets")+`
			(scope_kind, scope_id, status, updated_at);
		CREATE INDEX IF NOT EXISTS workforce_change_sets_global_recovery_idx ON `+s.table("workforce_change_sets")+`
			(status, scope_kind, scope_id)`); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+` (version, name)
		VALUES (13, 'durable workforce change sets') ON CONFLICT (version) DO NOTHING`)
	return err
}

func (s *PostgresStore) GetChangeSetByIdempotency(ctx context.Context, scope capability.ScopeReference, key, requestDigest string) (*authoring.ChangeSet, bool, error) {
	var storedDigest, payload string
	err := s.db.QueryRowContext(ctx, `SELECT request_digest, payload FROM `+s.table("workforce_change_sets")+`
		WHERE scope_kind = $1 AND scope_id = $2 AND idempotency_key = $3`, scope.Kind, scope.ID, key).Scan(&storedDigest, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if storedDigest != requestDigest {
		return nil, false, authoring.ErrChangeSetIdempotency
	}
	value, err := decodeChangeSet(payload)
	return value, err == nil, err
}

func (s *PostgresStore) CreateChangeSet(ctx context.Context, value *authoring.ChangeSet, key, requestDigest string) (*authoring.ChangeSet, bool, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, false, err
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO `+s.table("workforce_change_sets")+`
		(scope_kind, scope_id, id, parent_id, status, revision, idempotency_key, request_digest, candidate_digest, created_at, updated_at, payload)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12::jsonb)
		ON CONFLICT DO NOTHING`,
		value.Scope.Kind, value.Scope.ID, value.ID, value.ParentID, value.Status, value.Revision, key, requestDigest,
		value.CandidateDigest, value.CreatedAt, value.UpdatedAt, string(payload))
	if err != nil {
		return nil, false, err
	}
	if affected, _ := result.RowsAffected(); affected == 1 {
		created, err := decodeChangeSet(string(payload))
		return created, false, err
	}
	replay, found, err := s.GetChangeSetByIdempotency(ctx, value.Scope, key, requestDigest)
	if err != nil || found {
		return replay, found, err
	}
	return nil, false, fmt.Errorf("create workforce change set: conflicting identifier")
}

func (s *PostgresStore) GetChangeSet(ctx context.Context, scope capability.ScopeReference, id string) (*authoring.ChangeSet, error) {
	var payload string
	if err := s.db.QueryRowContext(ctx, `SELECT payload FROM `+s.table("workforce_change_sets")+`
		WHERE scope_kind = $1 AND scope_id = $2 AND id = $3`, scope.Kind, scope.ID, id).Scan(&payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, authoring.ErrChangeSetNotFound
		}
		return nil, err
	}
	return decodeChangeSet(payload)
}

func (s *PostgresStore) UpdateChangeSet(ctx context.Context, value *authoring.ChangeSet, expectedRevision int64) (*authoring.ChangeSet, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE `+s.table("workforce_change_sets")+` SET status = $1, revision = $2, updated_at = $3, payload = $4::jsonb
		WHERE scope_kind = $5 AND scope_id = $6 AND id = $7 AND revision = $8 AND candidate_digest = $9`,
		value.Status, value.Revision, value.UpdatedAt, string(payload), value.Scope.Kind, value.Scope.ID, value.ID, expectedRevision, value.CandidateDigest)
	if err != nil {
		return nil, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		if _, err := s.GetChangeSet(ctx, value.Scope, value.ID); err != nil {
			return nil, err
		}
		return nil, authoring.ErrChangeSetRevision
	}
	return decodeChangeSet(string(payload))
}

func (s *PostgresStore) CompleteChangeSetGeneration(ctx context.Context, value *authoring.ChangeSet, expectedRevision int64) (*authoring.ChangeSet, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE `+s.table("workforce_change_sets")+` SET status = $1, revision = $2, candidate_digest = $3, updated_at = $4, payload = $5::jsonb
		WHERE scope_kind = $6 AND scope_id = $7 AND id = $8 AND revision = $9 AND status = $10 AND candidate_digest = ''`,
		value.Status, value.Revision, value.CandidateDigest, value.UpdatedAt, string(payload), value.Scope.Kind, value.Scope.ID, value.ID, expectedRevision, authoring.ChangeSetEvaluating)
	if err != nil {
		return nil, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		if _, err := s.GetChangeSet(ctx, value.Scope, value.ID); err != nil {
			return nil, err
		}
		return nil, authoring.ErrChangeSetRevision
	}
	return decodeChangeSet(string(payload))
}

func (s *PostgresStore) ListPendingChangeSetGenerations(ctx context.Context, scope capability.ScopeReference, limit int) ([]*authoring.ChangeSet, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM `+s.table("workforce_change_sets")+`
		WHERE scope_kind = $1 AND scope_id = $2 AND status = $3 ORDER BY created_at, id LIMIT $4`,
		scope.Kind, scope.ID, authoring.ChangeSetEvaluating, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := make([]*authoring.ChangeSet, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		value, err := decodeChangeSet(payload)
		if err != nil {
			return nil, err
		}
		if value.Generation != nil {
			values = append(values, value)
		}
	}
	return values, rows.Err()
}

func (s *PostgresStore) ListPendingChangeSetEvaluations(ctx context.Context, scope capability.ScopeReference, limit int) ([]*authoring.ChangeSet, error) {
	return s.listChangeSetsByStatus(ctx, scope, authoring.ChangeSetReview, limit)
}

func (s *PostgresStore) ListWorkforceAuthoringRecoveryScopes(ctx context.Context, after Scope, limit int) ([]Scope, error) {
	if limit <= 0 {
		limit = 256
	}
	now := time.Now().UTC()
	rows, err := s.db.QueryContext(ctx, `
		SELECT scope_kind, scope_id FROM (
			SELECT scope_kind, scope_id FROM `+s.table("workforce_change_sets")+`
			WHERE status IN ($1, $2)
			UNION
			SELECT scope_kind, scope_id FROM `+s.table("agent_runs")+`
			WHERE COALESCE(payload->>'kind', 'agent_work') = $3
			AND ((status = $4 AND available_at <= $6 AND (lease_expires_at IS NULL OR lease_expires_at <= $6))
			  OR (status = $5 AND (lease_expires_at IS NULL OR lease_expires_at <= $6)))
		) AS recovery
		WHERE ($7 = '' OR scope_kind > $7 OR (scope_kind = $7 AND scope_id > $8))
		ORDER BY scope_kind, scope_id LIMIT $9`,
		authoring.ChangeSetEvaluating, authoring.ChangeSetReview, RunKindWorkforceAuthoring,
		AgentRunStatusQueued, AgentRunStatusRunning, now, after.Kind, after.ID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]Scope, 0, limit)
	for rows.Next() {
		var scope Scope
		if err := rows.Scan(&scope.Kind, &scope.ID); err != nil {
			return nil, err
		}
		result = append(result, scope)
	}
	return result, rows.Err()
}

func (s *PostgresStore) listChangeSetsByStatus(ctx context.Context, scope capability.ScopeReference, status authoring.ChangeSetStatus, limit int) ([]*authoring.ChangeSet, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM `+s.table("workforce_change_sets")+`
		WHERE scope_kind = $1 AND scope_id = $2 AND status = $3 ORDER BY updated_at, id LIMIT $4`,
		scope.Kind, scope.ID, status, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := make([]*authoring.ChangeSet, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		value, err := decodeChangeSet(payload)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func decodeChangeSet(payload string) (*authoring.ChangeSet, error) {
	var value authoring.ChangeSet
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return nil, err
	}
	return &value, nil
}

var _ authoring.ChangeSetStore = (*PostgresStore)(nil)
