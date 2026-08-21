package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/lib/pq"
)

const embedInstallationsMigrationVersion int64 = 43

func (s *PostgresStore) migrateEmbedInstallations(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS `+s.table("embed_installations")+` (
			id TEXT NOT NULL, scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, deployment_id TEXT NOT NULL,
			public_route TEXT NOT NULL UNIQUE, status TEXT NOT NULL, revision BIGINT NOT NULL, created_at TIMESTAMPTZ NOT NULL,
			payload JSONB NOT NULL, PRIMARY KEY (scope_kind, scope_id, id));
		CREATE INDEX IF NOT EXISTS embed_installations_deployment_idx ON `+s.table("embed_installations")+` (scope_kind, scope_id, deployment_id, created_at);
		CREATE TABLE IF NOT EXISTS `+s.table("embed_sessions")+` (
			id TEXT NOT NULL, scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, installation_id TEXT NOT NULL,
			conversation_id TEXT NOT NULL, status TEXT NOT NULL, capability_hash TEXT NOT NULL,
			verified_claims JSONB NOT NULL DEFAULT '{}'::jsonb, message_count INTEGER NOT NULL DEFAULT 0,
			expires_at TIMESTAMPTZ NOT NULL, created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL,
			payload JSONB NOT NULL, PRIMARY KEY (scope_kind, scope_id, id), UNIQUE (scope_kind, scope_id, conversation_id));
		CREATE INDEX IF NOT EXISTS embed_sessions_installation_idx ON `+s.table("embed_sessions")+` (scope_kind, scope_id, installation_id, created_at)
	`); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+` (version, name) VALUES ($1, 'governed Agent embed installations and sessions') ON CONFLICT (version) DO NOTHING`, embedInstallationsMigrationVersion)
	return err
}

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

func (s *PostgresStore) CreateEmbedInstallation(ctx context.Context, value *EmbedInstallation) error {
	if err := value.Validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO `+s.table("embed_installations")+` (id,scope_kind,scope_id,deployment_id,public_route,status,revision,created_at,payload) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9::jsonb)`, value.ID, value.Scope.Kind, value.Scope.ID, value.DeploymentID, value.PublicRoute, value.Status, value.Revision, value.CreatedAt, string(payload))
	if pg, ok := err.(*pq.Error); ok && pg.Code == "23505" {
		return ErrEmbedRouteConflict
	}
	return err
}

func (s *PostgresStore) GetEmbedInstallation(ctx context.Context, scope Scope, id string) (*EmbedInstallation, error) {
	return scanPostgresEmbedInstallation(s.db.QueryRowContext(ctx, `SELECT payload FROM `+s.table("embed_installations")+` WHERE scope_kind=$1 AND scope_id=$2 AND id=$3`, scope.Kind, scope.ID, strings.TrimSpace(id)))
}

func (s *PostgresStore) GetEmbedInstallationByRoute(ctx context.Context, route string) (*EmbedInstallation, error) {
	return scanPostgresEmbedInstallation(s.db.QueryRowContext(ctx, `SELECT payload FROM `+s.table("embed_installations")+` WHERE public_route=$1`, strings.TrimSpace(route)))
}

func scanPostgresEmbedInstallation(row *sql.Row) (*EmbedInstallation, error) {
	var payload []byte
	if err := row.Scan(&payload); errors.Is(err, sql.ErrNoRows) {
		return nil, ErrEmbedInstallationNotFound
	} else if err != nil {
		return nil, err
	}
	var value EmbedInstallation
	if err := json.Unmarshal(payload, &value); err != nil {
		return nil, err
	}
	return &value, value.Validate()
}

func (s *PostgresStore) ListEmbedInstallations(ctx context.Context, filter EmbedInstallationFilter) ([]*EmbedInstallation, error) {
	query := `SELECT payload FROM ` + s.table("embed_installations") + ` WHERE scope_kind=$1 AND scope_id=$2`
	args := []interface{}{filter.Scope.Kind, filter.Scope.ID}
	if filter.DeploymentID != "" {
		query += ` AND deployment_id=$3`
		args = append(args, filter.DeploymentID)
	}
	query += ` ORDER BY created_at,id`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []*EmbedInstallation{}
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var value EmbedInstallation
		if err := json.Unmarshal(payload, &value); err != nil {
			return nil, err
		}
		result = append(result, &value)
	}
	return result, rows.Err()
}

func (s *PostgresStore) UpdateEmbedInstallation(ctx context.Context, value *EmbedInstallation, expected int64) error {
	if err := value.Validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE `+s.table("embed_installations")+` SET status=$1,revision=$2,payload=$3::jsonb WHERE scope_kind=$4 AND scope_id=$5 AND id=$6 AND revision=$7 AND deployment_id=$8 AND public_route=$9`, value.Status, value.Revision, string(payload), value.Scope.Kind, value.Scope.ID, value.ID, expected, value.DeploymentID, value.PublicRoute)
	if err != nil {
		return err
	}
	affected, _ := result.RowsAffected()
	if affected != 1 {
		return ErrEmbedRevisionConflict
	}
	return nil
}

func (s *PostgresStore) CreateEmbedSession(ctx context.Context, value *EmbedSession) error {
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
	_, err = s.db.ExecContext(ctx, `INSERT INTO `+s.table("embed_sessions")+` (id,scope_kind,scope_id,installation_id,conversation_id,status,capability_hash,verified_claims,message_count,expires_at,created_at,updated_at,payload) VALUES ($1,$2,$3,$4,$5,$6,$7,$8::jsonb,$9,$10,$11,$12,$13::jsonb)`, value.ID, value.Scope.Kind, value.Scope.ID, value.InstallationID, value.ConversationID, value.Status, value.CapabilityHash, string(claims), value.MessageCount, value.ExpiresAt, value.CreatedAt, value.UpdatedAt, string(payload))
	return err
}

func (s *PostgresStore) GetEmbedSession(ctx context.Context, scope Scope, id string) (*EmbedSession, error) {
	return scanPostgresEmbedSession(s.db.QueryRowContext(ctx, `SELECT payload,capability_hash,verified_claims,message_count,status,updated_at FROM `+s.table("embed_sessions")+` WHERE scope_kind=$1 AND scope_id=$2 AND id=$3`, scope.Kind, scope.ID, strings.TrimSpace(id)))
}
func (s *PostgresStore) GetEmbedSessionByConversation(ctx context.Context, scope Scope, id string) (*EmbedSession, error) {
	return scanPostgresEmbedSession(s.db.QueryRowContext(ctx, `SELECT payload,capability_hash,verified_claims,message_count,status,updated_at FROM `+s.table("embed_sessions")+` WHERE scope_kind=$1 AND scope_id=$2 AND conversation_id=$3`, scope.Kind, scope.ID, strings.TrimSpace(id)))
}

func scanPostgresEmbedSession(row *sql.Row) (*EmbedSession, error) {
	var payload, claims []byte
	var hash string
	var count int
	var status EmbedSessionStatus
	var updated time.Time
	if err := row.Scan(&payload, &hash, &claims, &count, &status, &updated); errors.Is(err, sql.ErrNoRows) {
		return nil, ErrEmbedSessionNotFound
	} else if err != nil {
		return nil, err
	}
	var value EmbedSession
	if err := json.Unmarshal(payload, &value); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(claims, &value.VerifiedClaims); err != nil {
		return nil, err
	}
	value.CapabilityHash, value.MessageCount, value.Status, value.UpdatedAt = hash, count, status, updated
	return &value, value.Validate()
}

func (s *PostgresStore) ConsumeEmbedSessionMessage(ctx context.Context, scope Scope, id string, maximum int, now time.Time) (*EmbedSession, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE `+s.table("embed_sessions")+` SET message_count=message_count+1,updated_at=$1 WHERE scope_kind=$2 AND scope_id=$3 AND id=$4 AND status=$5 AND expires_at>$1 AND message_count<$6`, now, scope.Kind, scope.ID, strings.TrimSpace(id), EmbedSessionActive, maximum)
	if err != nil {
		return nil, err
	}
	affected, _ := result.RowsAffected()
	if affected != 1 {
		return nil, ErrEmbedLimitExceeded
	}
	return s.GetEmbedSession(ctx, scope, id)
}

func (s *SQLiteStore) CreateEmbedInstallation(ctx context.Context, value *EmbedInstallation) error {
	if err := value.Validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO embed_installations (id,scope_kind,scope_id,deployment_id,public_route,status,revision,created_at,payload) VALUES (?,?,?,?,?,?,?,?,?)`, value.ID, value.Scope.Kind, value.Scope.ID, value.DeploymentID, value.PublicRoute, value.Status, value.Revision, value.CreatedAt, string(payload))
	if err != nil && strings.Contains(err.Error(), "public_route") {
		return ErrEmbedRouteConflict
	}
	return err
}
func (s *SQLiteStore) GetEmbedInstallation(ctx context.Context, scope Scope, id string) (*EmbedInstallation, error) {
	return scanSQLiteEmbedInstallation(s.db.QueryRowContext(ctx, `SELECT payload FROM embed_installations WHERE scope_kind=? AND scope_id=? AND id=?`, scope.Kind, scope.ID, strings.TrimSpace(id)))
}
func (s *SQLiteStore) GetEmbedInstallationByRoute(ctx context.Context, route string) (*EmbedInstallation, error) {
	return scanSQLiteEmbedInstallation(s.db.QueryRowContext(ctx, `SELECT payload FROM embed_installations WHERE public_route=?`, strings.TrimSpace(route)))
}
func scanSQLiteEmbedInstallation(row *sql.Row) (*EmbedInstallation, error) {
	var payload string
	if err := row.Scan(&payload); errors.Is(err, sql.ErrNoRows) {
		return nil, ErrEmbedInstallationNotFound
	} else if err != nil {
		return nil, err
	}
	var value EmbedInstallation
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return nil, err
	}
	return &value, value.Validate()
}
func (s *SQLiteStore) ListEmbedInstallations(ctx context.Context, filter EmbedInstallationFilter) ([]*EmbedInstallation, error) {
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
	result := []*EmbedInstallation{}
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var value EmbedInstallation
		if err := json.Unmarshal([]byte(payload), &value); err != nil {
			return nil, err
		}
		result = append(result, &value)
	}
	return result, rows.Err()
}
func (s *SQLiteStore) UpdateEmbedInstallation(ctx context.Context, value *EmbedInstallation, expected int64) error {
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
		return ErrEmbedRevisionConflict
	}
	return nil
}
func (s *SQLiteStore) CreateEmbedSession(ctx context.Context, value *EmbedSession) error {
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
func (s *SQLiteStore) GetEmbedSession(ctx context.Context, scope Scope, id string) (*EmbedSession, error) {
	return scanSQLiteEmbedSession(s.db.QueryRowContext(ctx, `SELECT payload,capability_hash,verified_claims,message_count,status,updated_at FROM embed_sessions WHERE scope_kind=? AND scope_id=? AND id=?`, scope.Kind, scope.ID, strings.TrimSpace(id)))
}
func (s *SQLiteStore) GetEmbedSessionByConversation(ctx context.Context, scope Scope, id string) (*EmbedSession, error) {
	return scanSQLiteEmbedSession(s.db.QueryRowContext(ctx, `SELECT payload,capability_hash,verified_claims,message_count,status,updated_at FROM embed_sessions WHERE scope_kind=? AND scope_id=? AND conversation_id=?`, scope.Kind, scope.ID, strings.TrimSpace(id)))
}
func scanSQLiteEmbedSession(row *sql.Row) (*EmbedSession, error) {
	var payload, claims, hash string
	var count int
	var status EmbedSessionStatus
	var updated time.Time
	if err := row.Scan(&payload, &hash, &claims, &count, &status, &updated); errors.Is(err, sql.ErrNoRows) {
		return nil, ErrEmbedSessionNotFound
	} else if err != nil {
		return nil, err
	}
	var value EmbedSession
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(claims), &value.VerifiedClaims); err != nil {
		return nil, err
	}
	value.CapabilityHash, value.MessageCount, value.Status, value.UpdatedAt = hash, count, status, updated
	return &value, value.Validate()
}
func (s *SQLiteStore) ConsumeEmbedSessionMessage(ctx context.Context, scope Scope, id string, maximum int, now time.Time) (*EmbedSession, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE embed_sessions SET message_count=message_count+1,updated_at=? WHERE scope_kind=? AND scope_id=? AND id=? AND status=? AND expires_at>? AND message_count<?`, now, scope.Kind, scope.ID, strings.TrimSpace(id), EmbedSessionActive, now, maximum)
	if err != nil {
		return nil, err
	}
	affected, _ := result.RowsAffected()
	if affected != 1 {
		return nil, ErrEmbedLimitExceeded
	}
	return s.GetEmbedSession(ctx, scope, id)
}
