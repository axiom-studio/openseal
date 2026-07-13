package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill/sourceartifact"
)

func migrateSkillSourceArtifacts(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS skill_source_artifacts (
			scope_kind TEXT NOT NULL,
			scope_id TEXT NOT NULL,
			digest TEXT NOT NULL,
			format TEXT NOT NULL,
			created_at DATETIME NOT NULL,
			payload TEXT NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, digest)
		);
		CREATE TABLE IF NOT EXISTS skill_source_artifact_references (
			scope_kind TEXT NOT NULL,
			scope_id TEXT NOT NULL,
			digest TEXT NOT NULL,
			id TEXT NOT NULL,
			kind TEXT NOT NULL,
			created_at DATETIME NOT NULL,
			expires_at DATETIME,
			payload TEXT NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, digest, id),
			FOREIGN KEY (scope_kind, scope_id, digest)
				REFERENCES skill_source_artifacts(scope_kind, scope_id, digest) ON DELETE RESTRICT
		);
		CREATE INDEX IF NOT EXISTS idx_skill_source_artifact_expiry
			ON skill_source_artifact_references(expires_at, scope_kind, scope_id, digest);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_skill_source_artifact_reference_owner
			ON skill_source_artifact_references(scope_kind, scope_id, id);
		CREATE INDEX IF NOT EXISTS idx_skill_source_artifact_created
			ON skill_source_artifacts(created_at, scope_kind, scope_id, digest);
	`)
	return err
}

func (s *SQLiteStore) ImportSourceArtifact(ctx context.Context, artifact *sourceartifact.Artifact, reference *sourceartifact.Reference) (bool, error) {
	if err := sourceartifact.ValidateArtifact(artifact); err != nil {
		return false, err
	}
	if err := sourceartifact.ValidateReference(reference); err != nil {
		return false, err
	}
	if artifact.Scope != reference.Scope || artifact.Digest != reference.Digest {
		return false, errors.New("skill source artifact and reference identity must match")
	}
	artifactPayload, err := json.Marshal(artifact)
	if err != nil {
		return false, err
	}
	referencePayload, err := json.Marshal(reference)
	if err != nil {
		return false, err
	}
	conn, err := beginImmediateSQLite(ctx, s.db)
	if err != nil {
		return false, err
	}
	committed := false
	defer rollbackSQLiteConn(conn, &committed)
	created := false
	var existingPayload string
	err = conn.QueryRowContext(ctx, `SELECT payload FROM skill_source_artifacts WHERE scope_kind=? AND scope_id=? AND digest=?`, artifact.Scope.Kind, artifact.Scope.ID, artifact.Digest).Scan(&existingPayload)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := conn.ExecContext(ctx, `INSERT INTO skill_source_artifacts(scope_kind,scope_id,digest,format,created_at,payload) VALUES(?,?,?,?,?,?)`, artifact.Scope.Kind, artifact.Scope.ID, artifact.Digest, artifact.Format, artifact.CreatedAt, string(artifactPayload)); err != nil {
			return false, err
		}
		created = true
	case err != nil:
		return false, err
	default:
		var existing sourceartifact.Artifact
		if json.Unmarshal([]byte(existingPayload), &existing) != nil || !sourceartifact.EquivalentArtifacts(&existing, artifact) {
			return false, sourceartifact.ErrImmutable
		}
	}
	err = conn.QueryRowContext(ctx, `SELECT payload FROM skill_source_artifact_references WHERE scope_kind=? AND scope_id=? AND id=?`, reference.Scope.Kind, reference.Scope.ID, reference.ID).Scan(&existingPayload)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := conn.ExecContext(ctx, `INSERT INTO skill_source_artifact_references(scope_kind,scope_id,digest,id,kind,created_at,expires_at,payload) VALUES(?,?,?,?,?,?,?,?)`, reference.Scope.Kind, reference.Scope.ID, reference.Digest, reference.ID, reference.Kind, reference.CreatedAt, reference.ExpiresAt, string(referencePayload)); err != nil {
			return false, err
		}
	case err != nil:
		return false, err
	default:
		var existing sourceartifact.Reference
		if json.Unmarshal([]byte(existingPayload), &existing) != nil {
			return false, sourceartifact.ErrReferenceConflict
		}
		if existing.Digest != reference.Digest {
			if _, err := conn.ExecContext(ctx, `UPDATE skill_source_artifact_references SET digest=?,kind=?,created_at=?,expires_at=?,payload=? WHERE scope_kind=? AND scope_id=? AND id=?`, reference.Digest, reference.Kind, reference.CreatedAt, reference.ExpiresAt, string(referencePayload), reference.Scope.Kind, reference.Scope.ID, reference.ID); err != nil {
				return false, err
			}
		} else if !sourceartifact.EquivalentReferences(&existing, reference) {
			return false, sourceartifact.ErrReferenceConflict
		}
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return false, err
	}
	committed = true
	return created, nil
}

func (s *SQLiteStore) GetSourceArtifact(ctx context.Context, scope capability.ScopeReference, digest string) (*sourceartifact.Artifact, error) {
	if err := sourceartifact.ValidateKey(sourceartifact.Key{Scope: scope, Digest: digest}); err != nil {
		return nil, err
	}
	var payload string
	if err := s.db.QueryRowContext(ctx, `SELECT payload FROM skill_source_artifacts WHERE scope_kind=? AND scope_id=? AND digest=?`, scope.Kind, scope.ID, digest).Scan(&payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	var artifact sourceartifact.Artifact
	if err := json.Unmarshal([]byte(payload), &artifact); err != nil {
		return nil, err
	}
	return &artifact, nil
}

func (s *SQLiteStore) DeleteSourceArtifactReference(ctx context.Context, scope capability.ScopeReference, digest, referenceID string) error {
	if err := sourceartifact.ValidateKey(sourceartifact.Key{Scope: scope, Digest: digest}); err != nil {
		return err
	}
	if referenceID == "" {
		return errors.New("skill source artifact reference id is required")
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM skill_source_artifact_references WHERE scope_kind=? AND scope_id=? AND digest=? AND id=?`, scope.Kind, scope.ID, digest, referenceID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err == nil && rows == 0 {
		return sourceartifact.ErrNotFound
	}
	return err
}

func (s *SQLiteStore) PurgeExpiredSourceArtifactReferences(ctx context.Context, now time.Time, limit int) (int, error) {
	if limit <= 0 {
		limit = 100
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM skill_source_artifact_references WHERE rowid IN (SELECT rowid FROM skill_source_artifact_references WHERE expires_at IS NOT NULL AND expires_at<=? ORDER BY expires_at LIMIT ?)`, now, limit)
	if err != nil {
		return 0, err
	}
	rows, err := result.RowsAffected()
	return int(rows), err
}

func (s *SQLiteStore) ListUnreferencedSourceArtifacts(ctx context.Context, before time.Time, limit int) ([]sourceartifact.Key, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT a.scope_kind,a.scope_id,a.digest FROM skill_source_artifacts a WHERE a.created_at<=? AND NOT EXISTS (SELECT 1 FROM skill_source_artifact_references r WHERE r.scope_kind=a.scope_kind AND r.scope_id=a.scope_id AND r.digest=a.digest) ORDER BY a.created_at,a.scope_kind,a.scope_id,a.digest LIMIT ?`, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keys := make([]sourceartifact.Key, 0)
	for rows.Next() {
		var key sourceartifact.Key
		if err := rows.Scan(&key.Scope.Kind, &key.Scope.ID, &key.Digest); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

func (s *SQLiteStore) DeleteSourceArtifactIfUnreferenced(ctx context.Context, key sourceartifact.Key, before time.Time) (bool, error) {
	if err := sourceartifact.ValidateKey(key); err != nil {
		return false, err
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM skill_source_artifacts AS a WHERE scope_kind=? AND scope_id=? AND digest=? AND created_at<=? AND NOT EXISTS (SELECT 1 FROM skill_source_artifact_references r WHERE r.scope_kind=a.scope_kind AND r.scope_id=a.scope_id AND r.digest=a.digest)`, key.Scope.Kind, key.Scope.ID, key.Digest, before)
	if err != nil {
		return false, fmt.Errorf("delete unreferenced source artifact: %w", err)
	}
	rows, err := result.RowsAffected()
	return rows == 1, err
}

var _ sourceartifact.Store = (*SQLiteStore)(nil)
