package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
)

func migrateEmbedInstallationsSQLite(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS embed_installations (
			id TEXT NOT NULL, scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, deployment_id TEXT NOT NULL,
			public_route TEXT NOT NULL UNIQUE, status TEXT NOT NULL, revision INTEGER NOT NULL, created_at DATETIME NOT NULL,
			payload TEXT NOT NULL, PRIMARY KEY (scope_kind, scope_id, id));
		CREATE INDEX IF NOT EXISTS idx_embed_installations_deployment ON embed_installations(scope_kind, scope_id, deployment_id, created_at);
		CREATE TABLE IF NOT EXISTS embed_sessions (
			id TEXT NOT NULL, scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, installation_id TEXT NOT NULL,
			conversation_id TEXT NOT NULL, status TEXT NOT NULL, capability_hash TEXT NOT NULL, verified_claims TEXT NOT NULL DEFAULT '{}',
			message_count INTEGER NOT NULL DEFAULT 0, expires_at DATETIME NOT NULL, created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL,
			payload TEXT NOT NULL, PRIMARY KEY (scope_kind, scope_id, id), UNIQUE (scope_kind, scope_id, conversation_id));
		CREATE INDEX IF NOT EXISTS idx_embed_sessions_installation ON embed_sessions(scope_kind, scope_id, installation_id, created_at);
	`)
	return err
}

func (s *SQLiteStore) CreateEmbedInstallation(ctx context.Context, value *models.EmbedInstallation) error {
	if err := value.Validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO embed_installations (id,scope_kind,scope_id,deployment_id,public_route,status,revision,created_at,payload) VALUES (?,?,?,?,?,?,?,?,?)`, value.ID, value.Scope.Kind, value.Scope.ID, value.DeploymentID, value.PublicRoute, value.Status, value.Revision, value.CreatedAt, string(payload))
	if err != nil && strings.Contains(err.Error(), "public_route") {
		return models.ErrEmbedRouteConflict
	}
	return err
}

func (s *SQLiteStore) GetEmbedInstallation(ctx context.Context, scope models.Scope, id string) (*models.EmbedInstallation, error) {
	return scanSQLiteEmbedInstallation(s.db.QueryRowContext(ctx, `SELECT payload FROM embed_installations WHERE scope_kind=? AND scope_id=? AND id=?`, scope.Kind, scope.ID, strings.TrimSpace(id)))
}

func (s *SQLiteStore) GetEmbedInstallationByRoute(ctx context.Context, route string) (*models.EmbedInstallation, error) {
	return scanSQLiteEmbedInstallation(s.db.QueryRowContext(ctx, `SELECT payload FROM embed_installations WHERE public_route=?`, strings.TrimSpace(route)))
}

func scanSQLiteEmbedInstallation(row *sql.Row) (*models.EmbedInstallation, error) {
	var payload string
	if err := row.Scan(&payload); errors.Is(err, sql.ErrNoRows) {
		return nil, models.ErrEmbedInstallationNotFound
	} else if err != nil {
		return nil, err
	}
	var value models.EmbedInstallation
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return nil, err
	}
	return &value, value.Validate()
}

func (s *SQLiteStore) ListEmbedInstallations(ctx context.Context, filter models.EmbedInstallationFilter) ([]*models.EmbedInstallation, error) {
	query := `SELECT payload FROM embed_installations WHERE scope_kind=? AND scope_id=?`
	args := []interface{}{filter.Scope.Kind, filter.Scope.ID}
	if filter.DeploymentID != "" {
		query += ` AND deployment_id=?`
		args = append(args, filter.DeploymentID)
	}
	query += ` ORDER BY created_at,id`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []*models.EmbedInstallation{}
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var value models.EmbedInstallation
		if err := json.Unmarshal([]byte(payload), &value); err != nil {
			return nil, err
		}
		result = append(result, &value)
	}
	return result, rows.Err()
}

func (s *SQLiteStore) UpdateEmbedInstallation(ctx context.Context, value *models.EmbedInstallation, expected int64) error {
	if err := value.Validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE embed_installations SET status=?,revision=?,payload=? WHERE scope_kind=? AND scope_id=? AND id=? AND revision=? AND deployment_id=? AND public_route=?`, value.Status, value.Revision, string(payload), value.Scope.Kind, value.Scope.ID, value.ID, expected, value.DeploymentID, value.PublicRoute)
	if err != nil {
		return err
	}
	affected, _ := result.RowsAffected()
	if affected != 1 {
		return models.ErrEmbedRevisionConflict
	}
	return nil
}

func (s *SQLiteStore) CreateEmbedSession(ctx context.Context, value *models.EmbedSession) error {
	if err := value.Validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	claims, err := json.Marshal(value.VerifiedClaims)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO embed_sessions (id,scope_kind,scope_id,installation_id,conversation_id,status,capability_hash,verified_claims,message_count,expires_at,created_at,updated_at,payload) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`, value.ID, value.Scope.Kind, value.Scope.ID, value.InstallationID, value.ConversationID, value.Status, value.CapabilityHash, string(claims), value.MessageCount, value.ExpiresAt, value.CreatedAt, value.UpdatedAt, string(payload))
	return err
}

func (s *SQLiteStore) GetEmbedSession(ctx context.Context, scope models.Scope, id string) (*models.EmbedSession, error) {
	return scanSQLiteEmbedSession(s.db.QueryRowContext(ctx, `SELECT payload,capability_hash,verified_claims,message_count,status,updated_at FROM embed_sessions WHERE scope_kind=? AND scope_id=? AND id=?`, scope.Kind, scope.ID, strings.TrimSpace(id)))
}

func (s *SQLiteStore) GetEmbedSessionByConversation(ctx context.Context, scope models.Scope, id string) (*models.EmbedSession, error) {
	return scanSQLiteEmbedSession(s.db.QueryRowContext(ctx, `SELECT payload,capability_hash,verified_claims,message_count,status,updated_at FROM embed_sessions WHERE scope_kind=? AND scope_id=? AND conversation_id=?`, scope.Kind, scope.ID, strings.TrimSpace(id)))
}

func scanSQLiteEmbedSession(row *sql.Row) (*models.EmbedSession, error) {
	var payload, claims, hash string
	var count int
	var status models.EmbedSessionStatus
	var updated time.Time
	if err := row.Scan(&payload, &hash, &claims, &count, &status, &updated); errors.Is(err, sql.ErrNoRows) {
		return nil, models.ErrEmbedSessionNotFound
	} else if err != nil {
		return nil, err
	}
	var value models.EmbedSession
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(claims), &value.VerifiedClaims); err != nil {
		return nil, err
	}
	value.CapabilityHash, value.MessageCount, value.Status, value.UpdatedAt = hash, count, status, updated
	return &value, value.Validate()
}

func (s *SQLiteStore) ConsumeEmbedSessionMessage(ctx context.Context, scope models.Scope, id string, maximum int, now time.Time) (*models.EmbedSession, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE embed_sessions SET message_count=message_count+1,updated_at=? WHERE scope_kind=? AND scope_id=? AND id=? AND status=? AND expires_at>? AND message_count<?`, now, scope.Kind, scope.ID, strings.TrimSpace(id), models.EmbedSessionActive, now, maximum)
	if err != nil {
		return nil, err
	}
	affected, _ := result.RowsAffected()
	if affected != 1 {
		return nil, models.ErrEmbedLimitExceeded
	}
	return s.GetEmbedSession(ctx, scope, id)
}
