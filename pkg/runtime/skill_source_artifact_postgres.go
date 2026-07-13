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

func (s *PostgresStore) migrateSkillSourceArtifacts(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS `+s.table("skill_source_artifacts")+` (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, digest TEXT NOT NULL,
			format TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL, payload JSONB NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, digest)
		);
		CREATE TABLE IF NOT EXISTS `+s.table("skill_source_artifact_references")+` (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, digest TEXT NOT NULL,
			id TEXT NOT NULL, kind TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL,
			expires_at TIMESTAMPTZ, payload JSONB NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, digest, id),
			FOREIGN KEY (scope_kind, scope_id, digest) REFERENCES `+s.table("skill_source_artifacts")+`(scope_kind, scope_id, digest) ON DELETE RESTRICT
		);
		CREATE INDEX IF NOT EXISTS skill_source_artifact_expiry_idx ON `+s.table("skill_source_artifact_references")+`(expires_at, scope_kind, scope_id, digest);
		CREATE UNIQUE INDEX IF NOT EXISTS skill_source_artifact_reference_owner_idx ON `+s.table("skill_source_artifact_references")+`(scope_kind, scope_id, id);
		CREATE INDEX IF NOT EXISTS skill_source_artifact_created_idx ON `+s.table("skill_source_artifacts")+`(created_at, scope_kind, scope_id, digest)`); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+` (version,name) VALUES (17,'skill source artifacts') ON CONFLICT(version) DO NOTHING`)
	return err
}

func (s *PostgresStore) ImportSourceArtifact(ctx context.Context, artifact *sourceartifact.Artifact, reference *sourceartifact.Reference) (bool, error) {
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, fmt.Sprintf("skill-source-reference:%d:%s:%d:%s:%s", len(reference.Scope.Kind), reference.Scope.Kind, len(reference.Scope.ID), reference.Scope.ID, reference.ID)); err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("skill_source_artifacts")+`(scope_kind,scope_id,digest,format,created_at,payload) VALUES($1,$2,$3,$4,$5,$6::jsonb) ON CONFLICT(scope_kind,scope_id,digest) DO NOTHING`, artifact.Scope.Kind, artifact.Scope.ID, artifact.Digest, artifact.Format, artifact.CreatedAt, string(artifactPayload))
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	created := rows == 1
	if !created {
		var existingPayload string
		if err := tx.QueryRowContext(ctx, `SELECT payload FROM `+s.table("skill_source_artifacts")+` WHERE scope_kind=$1 AND scope_id=$2 AND digest=$3 FOR UPDATE`, artifact.Scope.Kind, artifact.Scope.ID, artifact.Digest).Scan(&existingPayload); err != nil {
			return false, err
		}
		var existing sourceartifact.Artifact
		if json.Unmarshal([]byte(existingPayload), &existing) != nil || !sourceartifact.EquivalentArtifacts(&existing, artifact) {
			return false, sourceartifact.ErrImmutable
		}
	}
	var existingReferencePayload string
	err = tx.QueryRowContext(ctx, `SELECT payload FROM `+s.table("skill_source_artifact_references")+` WHERE scope_kind=$1 AND scope_id=$2 AND id=$3 FOR UPDATE`, reference.Scope.Kind, reference.Scope.ID, reference.ID).Scan(&existingReferencePayload)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("skill_source_artifact_references")+`(scope_kind,scope_id,digest,id,kind,created_at,expires_at,payload) VALUES($1,$2,$3,$4,$5,$6,$7,$8::jsonb)`, reference.Scope.Kind, reference.Scope.ID, reference.Digest, reference.ID, reference.Kind, reference.CreatedAt, reference.ExpiresAt, string(referencePayload)); err != nil {
			return false, err
		}
	case err != nil:
		return false, err
	default:
		var existing sourceartifact.Reference
		if json.Unmarshal([]byte(existingReferencePayload), &existing) != nil {
			return false, sourceartifact.ErrReferenceConflict
		}
		if existing.Digest != reference.Digest {
			if _, err := tx.ExecContext(ctx, `UPDATE `+s.table("skill_source_artifact_references")+` SET digest=$1,kind=$2,created_at=$3,expires_at=$4,payload=$5::jsonb WHERE scope_kind=$6 AND scope_id=$7 AND id=$8`, reference.Digest, reference.Kind, reference.CreatedAt, reference.ExpiresAt, string(referencePayload), reference.Scope.Kind, reference.Scope.ID, reference.ID); err != nil {
				return false, err
			}
		} else if existing.Origin.IsZero() && !reference.Origin.IsZero() {
			if _, err := tx.ExecContext(ctx, `UPDATE `+s.table("skill_source_artifact_references")+` SET kind=$1,created_at=$2,expires_at=$3,payload=$4::jsonb WHERE scope_kind=$5 AND scope_id=$6 AND id=$7`, reference.Kind, reference.CreatedAt, reference.ExpiresAt, string(referencePayload), reference.Scope.Kind, reference.Scope.ID, reference.ID); err != nil {
				return false, err
			}
		} else if !sourceartifact.EquivalentReferences(&existing, reference) {
			return false, sourceartifact.ErrReferenceConflict
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return created, nil
}

func (s *PostgresStore) ListSourceArtifactReferences(ctx context.Context, scope capability.ScopeReference, digest string) ([]sourceartifact.Reference, error) {
	if err := sourceartifact.ValidateKey(sourceartifact.Key{Scope: scope, Digest: digest}); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM `+s.table("skill_source_artifact_references")+` WHERE scope_kind=$1 AND scope_id=$2 AND digest=$3 ORDER BY id`, scope.Kind, scope.ID, digest)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]sourceartifact.Reference, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var reference sourceartifact.Reference
		if err := json.Unmarshal([]byte(payload), &reference); err != nil {
			return nil, err
		}
		result = append(result, reference)
	}
	return result, rows.Err()
}

func (s *PostgresStore) GetSourceArtifact(ctx context.Context, scope capability.ScopeReference, digest string) (*sourceartifact.Artifact, error) {
	if err := sourceartifact.ValidateKey(sourceartifact.Key{Scope: scope, Digest: digest}); err != nil {
		return nil, err
	}
	var payload string
	if err := s.db.QueryRowContext(ctx, `SELECT payload FROM `+s.table("skill_source_artifacts")+` WHERE scope_kind=$1 AND scope_id=$2 AND digest=$3`, scope.Kind, scope.ID, digest).Scan(&payload); err != nil {
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

func (s *PostgresStore) DeleteSourceArtifactReference(ctx context.Context, scope capability.ScopeReference, digest, referenceID string) error {
	if err := sourceartifact.ValidateKey(sourceartifact.Key{Scope: scope, Digest: digest}); err != nil {
		return err
	}
	if referenceID == "" {
		return errors.New("skill source artifact reference id is required")
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM `+s.table("skill_source_artifact_references")+` WHERE scope_kind=$1 AND scope_id=$2 AND digest=$3 AND id=$4`, scope.Kind, scope.ID, digest, referenceID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err == nil && rows == 0 {
		return sourceartifact.ErrNotFound
	}
	return err
}

func (s *PostgresStore) PurgeExpiredSourceArtifactReferences(ctx context.Context, now time.Time, limit int) (int, error) {
	if limit <= 0 {
		limit = 100
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM `+s.table("skill_source_artifact_references")+` WHERE ctid IN (SELECT ctid FROM `+s.table("skill_source_artifact_references")+` WHERE expires_at IS NOT NULL AND expires_at<=$1 ORDER BY expires_at LIMIT $2 FOR UPDATE SKIP LOCKED)`, now, limit)
	if err != nil {
		return 0, err
	}
	rows, err := result.RowsAffected()
	return int(rows), err
}

func (s *PostgresStore) ListUnreferencedSourceArtifacts(ctx context.Context, before time.Time, limit int) ([]sourceartifact.Key, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT a.scope_kind,a.scope_id,a.digest FROM `+s.table("skill_source_artifacts")+` a WHERE a.created_at<=$1 AND NOT EXISTS (SELECT 1 FROM `+s.table("skill_source_artifact_references")+` r WHERE r.scope_kind=a.scope_kind AND r.scope_id=a.scope_id AND r.digest=a.digest) ORDER BY a.created_at,a.scope_kind,a.scope_id,a.digest LIMIT $2`, before, limit)
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

func (s *PostgresStore) DeleteSourceArtifactIfUnreferenced(ctx context.Context, key sourceartifact.Key, before time.Time) (bool, error) {
	if err := sourceartifact.ValidateKey(key); err != nil {
		return false, err
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM `+s.table("skill_source_artifacts")+` a WHERE scope_kind=$1 AND scope_id=$2 AND digest=$3 AND created_at<=$4 AND NOT EXISTS (SELECT 1 FROM `+s.table("skill_source_artifact_references")+` r WHERE r.scope_kind=a.scope_kind AND r.scope_id=a.scope_id AND r.digest=a.digest)`, key.Scope.Kind, key.Scope.ID, key.Digest, before)
	if err != nil {
		return false, fmt.Errorf("delete unreferenced source artifact: %w", err)
	}
	rows, err := result.RowsAffected()
	return rows == 1, err
}

var _ sourceartifact.Store = (*PostgresStore)(nil)
