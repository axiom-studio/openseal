package runtime

import (
	"context"
	"database/sql"
)

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
