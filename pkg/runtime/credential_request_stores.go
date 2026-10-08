package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
)

const credentialRequestMigrationVersion int64 = 62

func (s *MemoryStore) GetCredentialRequest(_ context.Context, scope Scope, id string) (*CredentialRequest, error) {
	if scope.Validate() != nil {
		return nil, ErrInvalidCredentialRequest
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneCredentialRequest(s.credentialRequests[portfolioKey(scope, id)]), nil
}

func (s *MemoryStore) ListCredentialRequests(_ context.Context, scope Scope, deploymentID, conversationID string) ([]*CredentialRequest, error) {
	if scope.Validate() != nil || deploymentID == "" || conversationID == "" {
		return nil, ErrInvalidCredentialRequest
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := []*CredentialRequest{}
	for _, r := range s.credentialRequests {
		if r.Scope == scope && r.DeploymentID == deploymentID && r.ConversationID == conversationID {
			result = append(result, cloneCredentialRequest(r))
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].CreatedAt.Equal(result[j].CreatedAt) {
			return result[i].ID < result[j].ID
		}
		return result[i].CreatedAt.Before(result[j].CreatedAt)
	})
	return result, nil
}

func (s *MemoryStore) SaveCredentialRequest(_ context.Context, r *CredentialRequest, expected int64) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if r.Revision != expected+1 {
		return ErrInvalidCredentialRequest
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := portfolioKey(r.Scope, r.ID)
	current := s.credentialRequests[key]
	if current == nil && expected != 0 || current != nil && (current.Revision != expected || current.Status != CredentialRequestStatusPending) {
		return ErrCredentialRequestConflict
	}
	s.credentialRequests[key] = cloneCredentialRequest(r)
	return nil
}

func (s *PostgresStore) migrateCredentialRequests(ctx context.Context, tx *sql.Tx) error {
	applied, err := s.postgresMigrationApplied(ctx, tx, credentialRequestMigrationVersion)
	if err != nil || applied {
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS `+s.table("credential_requests")+` (scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, id TEXT NOT NULL, deployment_id TEXT NOT NULL, conversation_id TEXT NOT NULL, status TEXT NOT NULL, revision BIGINT NOT NULL, created_at TIMESTAMPTZ NOT NULL, payload JSONB NOT NULL, PRIMARY KEY(scope_kind,scope_id,id))`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS credential_requests_conversation_idx ON `+s.table("credential_requests")+`(scope_kind,scope_id,deployment_id,conversation_id,created_at)`); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+`(version,name) VALUES($1,'durable in-chat credential requests') ON CONFLICT(version) DO NOTHING`, credentialRequestMigrationVersion)
	return err
}

func (s *PostgresStore) GetCredentialRequest(ctx context.Context, scope Scope, id string) (*CredentialRequest, error) {
	return getCredentialRequest(ctx, s.db, s.table("credential_requests"), scope, id)
}

func (s *PostgresStore) ListCredentialRequests(ctx context.Context, scope Scope, deploymentID, conversationID string) ([]*CredentialRequest, error) {
	return listCredentialRequests(ctx, s.db, s.table("credential_requests"), scope, deploymentID, conversationID)
}

func (s *PostgresStore) SaveCredentialRequest(ctx context.Context, r *CredentialRequest, expected int64) error {
	return saveCredentialRequest(ctx, s.db, s.table("credential_requests"), r, expected)
}

func migrateCredentialRequests(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS credential_requests (scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, id TEXT NOT NULL, deployment_id TEXT NOT NULL, conversation_id TEXT NOT NULL, status TEXT NOT NULL, revision INTEGER NOT NULL, created_at DATETIME NOT NULL, payload TEXT NOT NULL, PRIMARY KEY(scope_kind,scope_id,id))`)
	return err
}

func (s *SQLiteStore) GetCredentialRequest(ctx context.Context, scope Scope, id string) (*CredentialRequest, error) {
	return getCredentialRequest(ctx, s.db, "credential_requests", scope, id)
}

func (s *SQLiteStore) ListCredentialRequests(ctx context.Context, scope Scope, deploymentID, conversationID string) ([]*CredentialRequest, error) {
	return listCredentialRequests(ctx, s.db, "credential_requests", scope, deploymentID, conversationID)
}

func (s *SQLiteStore) SaveCredentialRequest(ctx context.Context, r *CredentialRequest, expected int64) error {
	return saveCredentialRequest(ctx, s.db, "credential_requests", r, expected)
}

// The $n placeholders are accepted by both the PostgreSQL and SQLite drivers.
func getCredentialRequest(ctx context.Context, db *sql.DB, table string, scope Scope, id string) (*CredentialRequest, error) {
	if scope.Validate() != nil {
		return nil, ErrInvalidCredentialRequest
	}
	var payload []byte
	err := db.QueryRowContext(ctx, `SELECT payload FROM `+table+` WHERE scope_kind=$1 AND scope_id=$2 AND id=$3`, scope.Kind, scope.ID, id).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r CredentialRequest
	if err := json.Unmarshal(payload, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func listCredentialRequests(ctx context.Context, db *sql.DB, table string, scope Scope, deploymentID, conversationID string) ([]*CredentialRequest, error) {
	if scope.Validate() != nil || deploymentID == "" || conversationID == "" {
		return nil, ErrInvalidCredentialRequest
	}
	rows, err := db.QueryContext(ctx, `SELECT payload FROM `+table+` WHERE scope_kind=$1 AND scope_id=$2 AND deployment_id=$3 AND conversation_id=$4 ORDER BY created_at,id`, scope.Kind, scope.ID, deploymentID, conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []*CredentialRequest{}
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var r CredentialRequest
		if err := json.Unmarshal(payload, &r); err != nil {
			return nil, err
		}
		result = append(result, &r)
	}
	return result, rows.Err()
}

func saveCredentialRequest(ctx context.Context, db *sql.DB, table string, r *CredentialRequest, expected int64) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if r.Revision != expected+1 {
		return ErrInvalidCredentialRequest
	}
	payload, err := json.Marshal(r)
	if err != nil {
		return err
	}
	var result sql.Result
	if expected == 0 {
		result, err = db.ExecContext(ctx, `INSERT INTO `+table+`(scope_kind,scope_id,id,deployment_id,conversation_id,status,revision,created_at,payload) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT DO NOTHING`, r.Scope.Kind, r.Scope.ID, r.ID, r.DeploymentID, r.ConversationID, r.Status, r.Revision, r.CreatedAt, string(payload))
	} else {
		result, err = db.ExecContext(ctx, `UPDATE `+table+` SET status=$1,revision=$2,payload=$3 WHERE scope_kind=$4 AND scope_id=$5 AND id=$6 AND revision=$7 AND status='pending'`, r.Status, r.Revision, string(payload), r.Scope.Kind, r.Scope.ID, r.ID, expected)
	}
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrCredentialRequestConflict
	}
	return nil
}
