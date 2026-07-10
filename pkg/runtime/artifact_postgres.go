package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
)

func (s *PostgresStore) migrateArtifacts(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS `+s.table("artifacts")+` (
			scope_kind TEXT NOT NULL,
			scope_id TEXT NOT NULL,
			id TEXT NOT NULL,
			version BIGINT NOT NULL,
			type TEXT NOT NULL DEFAULT '',
			media_type TEXT NOT NULL DEFAULT '',
			classification TEXT NOT NULL,
			digest TEXT NOT NULL,
			content_ref TEXT NOT NULL,
			producer_run_id TEXT NOT NULL DEFAULT '',
			producer_request_id TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL,
			payload JSONB NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, id, version),
			CHECK (version > 0)
		)`); err != nil {
		return err
	}
	statements := []string{
		`CREATE INDEX IF NOT EXISTS artifacts_latest_idx ON ` + s.table("artifacts") + ` (scope_kind, scope_id, id, version DESC)`,
		`CREATE INDEX IF NOT EXISTS artifacts_catalog_idx ON ` + s.table("artifacts") + ` (scope_kind, scope_id, type, media_type, classification, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS artifacts_provenance_idx ON ` + s.table("artifacts") + ` (scope_kind, scope_id, producer_run_id, producer_request_id, created_at DESC)`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+` (version, name) VALUES (8, 'artifact and evidence catalog') ON CONFLICT (version) DO NOTHING`)
	return err
}

func (s *PostgresStore) CreateArtifactVersion(ctx context.Context, artifact *Artifact, expectedLatestVersion int64) error {
	if err := artifact.Validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(artifact)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	lockKey := hashString(artifactStorageKey(artifact.Scope, artifact.ID))
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, lockKey); err != nil {
		return err
	}
	var latest int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM `+s.table("artifacts")+`
		WHERE scope_kind = $1 AND scope_id = $2 AND id = $3`, artifact.Scope.Kind, artifact.Scope.ID, artifact.ID).Scan(&latest); err != nil {
		return err
	}
	if latest != expectedLatestVersion {
		return ErrArtifactVersionConflict
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("artifacts")+`
		(scope_kind, scope_id, id, version, type, media_type, classification, digest, content_ref,
		 producer_run_id, producer_request_id, created_at, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13::jsonb)`,
		artifact.Scope.Kind, artifact.Scope.ID, artifact.ID, artifact.Version, artifact.Type, artifact.MediaType,
		artifact.Classification, artifact.Digest, artifact.ContentRef, artifact.Provenance.RunID,
		artifact.Provenance.RequestID, artifact.CreatedAt, string(payload)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *PostgresStore) GetArtifact(ctx context.Context, scope Scope, id string, version int64) (*Artifact, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	query := `SELECT payload FROM ` + s.table("artifacts") + ` WHERE scope_kind = $1 AND scope_id = $2 AND id = $3`
	args := []interface{}{scope.Kind, scope.ID, id}
	if version > 0 {
		query += ` AND version = $4`
		args = append(args, version)
	} else {
		query += ` ORDER BY version DESC LIMIT 1`
	}
	var payload string
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&payload); errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	return decodeArtifact(payload)
}

func (s *PostgresStore) ListArtifacts(ctx context.Context, filter ArtifactFilter) ([]*Artifact, error) {
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM `+s.table("artifacts")+` WHERE scope_kind = $1 AND scope_id = $2`, filter.Scope.Kind, filter.Scope.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	all := make([]*Artifact, 0)
	latest := make(map[string]int64)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		artifact, err := decodeArtifact(payload)
		if err != nil {
			return nil, err
		}
		all = append(all, artifact)
		if artifact.Version > latest[artifact.ID] {
			latest[artifact.ID] = artifact.Version
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	result := make([]*Artifact, 0, len(all))
	for _, artifact := range all {
		if filter.LatestOnly && artifact.Version != latest[artifact.ID] {
			continue
		}
		if matchesArtifactFilter(artifact, filter) {
			result = append(result, artifact)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].CreatedAt.Equal(result[j].CreatedAt) {
			if result[i].ID == result[j].ID {
				return result[i].Version > result[j].Version
			}
			return result[i].ID < result[j].ID
		}
		return result[i].CreatedAt.After(result[j].CreatedAt)
	})
	start := min(filter.Offset, len(result))
	end := min(start+filter.Limit, len(result))
	return result[start:end], nil
}

var _ ArtifactStore = (*PostgresStore)(nil)
