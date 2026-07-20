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

func migrateAuthoringChangeSets(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS workforce_change_sets (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, id TEXT NOT NULL,
			parent_id TEXT NOT NULL DEFAULT '', status TEXT NOT NULL, revision INTEGER NOT NULL,
			idempotency_key TEXT NOT NULL, request_digest TEXT NOT NULL, candidate_digest TEXT NOT NULL,
			created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL, payload TEXT NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, id),
			UNIQUE (scope_kind, scope_id, idempotency_key)
		);
		CREATE INDEX IF NOT EXISTS idx_workforce_change_sets_parent
			ON workforce_change_sets(scope_kind, scope_id, parent_id, created_at);
		CREATE INDEX IF NOT EXISTS idx_workforce_change_sets_status
			ON workforce_change_sets(scope_kind, scope_id, status, updated_at);
		CREATE INDEX IF NOT EXISTS idx_workforce_change_sets_global_recovery
			ON workforce_change_sets(status, scope_kind, scope_id);
	`)
	return err
}

func (s *SQLiteStore) GetChangeSetByIdempotency(ctx context.Context, scope capability.ScopeReference, key, requestDigest string) (*authoring.ChangeSet, bool, error) {
	var storedDigest, payload string
	err := s.db.QueryRowContext(ctx, `SELECT request_digest, payload FROM workforce_change_sets
		WHERE scope_kind = ? AND scope_id = ? AND idempotency_key = ?`, scope.Kind, scope.ID, key).Scan(&storedDigest, &payload)
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

func (s *SQLiteStore) CreateChangeSet(ctx context.Context, value *authoring.ChangeSet, key, requestDigest string) (*authoring.ChangeSet, bool, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, false, err
	}
	result, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO workforce_change_sets
		(scope_kind, scope_id, id, parent_id, status, revision, idempotency_key, request_digest, candidate_digest, created_at, updated_at, payload)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, value.Scope.Kind, value.Scope.ID, value.ID, value.ParentID, value.Status,
		value.Revision, key, requestDigest, value.CandidateDigest, value.CreatedAt, value.UpdatedAt, string(payload))
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

func (s *SQLiteStore) GetChangeSet(ctx context.Context, scope capability.ScopeReference, id string) (*authoring.ChangeSet, error) {
	var payload string
	if err := s.db.QueryRowContext(ctx, `SELECT payload FROM workforce_change_sets
		WHERE scope_kind = ? AND scope_id = ? AND id = ?`, scope.Kind, scope.ID, id).Scan(&payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, authoring.ErrChangeSetNotFound
		}
		return nil, err
	}
	return decodeChangeSet(payload)
}

func (s *SQLiteStore) UpdateChangeSet(ctx context.Context, value *authoring.ChangeSet, expectedRevision int64) (*authoring.ChangeSet, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE workforce_change_sets SET status = ?, revision = ?, updated_at = ?, payload = ?
		WHERE scope_kind = ? AND scope_id = ? AND id = ? AND revision = ? AND candidate_digest = ?`,
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

func (s *SQLiteStore) CompleteChangeSetGeneration(ctx context.Context, value *authoring.ChangeSet, expectedRevision int64) (*authoring.ChangeSet, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	expectedDigest := ""
	if value.Generation != nil {
		expectedDigest = value.Generation.PreviousCandidateDigest
	}
	result, err := s.db.ExecContext(ctx, `UPDATE workforce_change_sets SET status = ?, revision = ?, candidate_digest = ?, updated_at = ?, payload = ?
		WHERE scope_kind = ? AND scope_id = ? AND id = ? AND revision = ? AND status = ? AND candidate_digest = ?`,
		value.Status, value.Revision, value.CandidateDigest, value.UpdatedAt, string(payload), value.Scope.Kind, value.Scope.ID, value.ID, expectedRevision, authoring.ChangeSetEvaluating, expectedDigest)
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

func (s *SQLiteStore) ListPendingChangeSetGenerations(ctx context.Context, scope capability.ScopeReference, limit int) ([]*authoring.ChangeSet, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM workforce_change_sets
		WHERE scope_kind = ? AND scope_id = ? AND status = ? ORDER BY created_at, id LIMIT ?`,
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

func (s *SQLiteStore) ListPendingChangeSetEvaluations(ctx context.Context, scope capability.ScopeReference, limit int) ([]*authoring.ChangeSet, error) {
	return s.listChangeSetsByStatus(ctx, scope, authoring.ChangeSetReview, limit)
}

func (s *SQLiteStore) ListWorkforceAuthoringRecoveryScopes(ctx context.Context, after Scope, limit int) ([]Scope, error) {
	if limit <= 0 {
		limit = 256
	}
	now := time.Now().UTC()
	rows, err := s.db.QueryContext(ctx, `
		SELECT scope_kind, scope_id FROM (
			SELECT scope_kind, scope_id FROM workforce_change_sets
			WHERE status IN (?, ?)
			UNION
			SELECT scope_kind, scope_id FROM agent_runs
			WHERE COALESCE(json_extract(payload, '$.kind'), 'agent_work') = ?
			AND ((status = ? AND available_at <= ? AND (lease_expires_at IS NULL OR lease_expires_at <= ?))
			  OR (status = ? AND (lease_expires_at IS NULL OR lease_expires_at <= ?)))
		) AS recovery
		WHERE (? = '' OR scope_kind > ? OR (scope_kind = ? AND scope_id > ?))
		ORDER BY scope_kind, scope_id LIMIT ?`,
		authoring.ChangeSetEvaluating, authoring.ChangeSetReview, RunKindWorkforceAuthoring,
		AgentRunStatusQueued, now, now, AgentRunStatusRunning, now,
		after.Kind, after.Kind, after.Kind, after.ID, limit)
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

func (s *SQLiteStore) listChangeSetsByStatus(ctx context.Context, scope capability.ScopeReference, status authoring.ChangeSetStatus, limit int) ([]*authoring.ChangeSet, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM workforce_change_sets
		WHERE scope_kind = ? AND scope_id = ? AND status = ? ORDER BY updated_at, id LIMIT ?`,
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

var _ authoring.ChangeSetStore = (*SQLiteStore)(nil)
