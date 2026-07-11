package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

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

var _ authoring.ChangeSetStore = (*SQLiteStore)(nil)
