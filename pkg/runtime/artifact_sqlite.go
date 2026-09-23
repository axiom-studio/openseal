package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
)

func migrateArtifacts(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS artifacts (
			scope_kind TEXT NOT NULL,
			scope_id TEXT NOT NULL,
			id TEXT NOT NULL,
			version INTEGER NOT NULL,
			type TEXT NOT NULL DEFAULT '',
			media_type TEXT NOT NULL DEFAULT '',
			classification TEXT NOT NULL,
			digest TEXT NOT NULL,
			content_ref TEXT NOT NULL,
			producer_run_id TEXT NOT NULL DEFAULT '',
			producer_request_id TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL,
			payload TEXT NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, id, version),
			CHECK (version > 0)
		);
		CREATE INDEX IF NOT EXISTS idx_artifacts_latest
			ON artifacts(scope_kind, scope_id, id, version DESC);
		CREATE INDEX IF NOT EXISTS idx_artifacts_catalog
			ON artifacts(scope_kind, scope_id, type, media_type, classification, created_at DESC);
		CREATE INDEX IF NOT EXISTS idx_artifacts_provenance
			ON artifacts(scope_kind, scope_id, producer_run_id, producer_request_id, created_at DESC);
		CREATE INDEX IF NOT EXISTS idx_artifacts_owner_page
			ON artifacts(scope_kind, scope_id,
				json_extract(payload, '$.provenance.owner.type'),
				json_extract(payload, '$.provenance.owner.id'),
				created_at DESC, id, version DESC);
	`)
	return err
}

func (s *SQLiteStore) CreateArtifactVersion(ctx context.Context, artifact *Artifact, expectedLatestVersion int64) error {
	if err := artifact.Validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(artifact)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO artifacts
		(scope_kind, scope_id, id, version, type, media_type, classification, digest, content_ref,
		 producer_run_id, producer_request_id, created_at, payload)
		SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
		WHERE ? = COALESCE((SELECT MAX(version) FROM artifacts WHERE scope_kind = ? AND scope_id = ? AND id = ?), 0)`,
		artifact.Scope.Kind, artifact.Scope.ID, artifact.ID, artifact.Version, artifact.Type, artifact.MediaType,
		artifact.Classification, artifact.Digest, artifact.ContentRef, artifact.Provenance.RunID,
		artifact.Provenance.RequestID, artifact.CreatedAt, string(payload), expectedLatestVersion,
		artifact.Scope.Kind, artifact.Scope.ID, artifact.ID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrArtifactVersionConflict
	}
	return nil
}

func (s *SQLiteStore) GetArtifact(ctx context.Context, scope Scope, id string, version int64) (*Artifact, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	query := `SELECT payload FROM artifacts WHERE scope_kind = ? AND scope_id = ? AND id = ?`
	args := []interface{}{scope.Kind, scope.ID, id}
	if version > 0 {
		query += ` AND version = ?`
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

func (s *SQLiteStore) ListArtifacts(ctx context.Context, filter ArtifactFilter) ([]*Artifact, error) {
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	if filter.EvidenceTarget == "" && filter.RetentionDueBefore == nil {
		query, args := artifactListQuery(filter, "artifacts", false)
		return queryArtifactPage(ctx, s.db, query, args)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM artifacts WHERE scope_kind = ? AND scope_id = ?`, filter.Scope.Kind, filter.Scope.ID)
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

func decodeArtifact(payload string) (*Artifact, error) {
	var artifact Artifact
	if err := json.Unmarshal([]byte(payload), &artifact); err != nil {
		return nil, err
	}
	return &artifact, nil
}

var _ ArtifactStore = (*SQLiteStore)(nil)
