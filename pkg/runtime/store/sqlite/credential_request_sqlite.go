package sqlite

import (
	"context"
	"database/sql"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
)

func migrateCredentialRequests(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS credential_requests (scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, id TEXT NOT NULL, deployment_id TEXT NOT NULL, conversation_id TEXT NOT NULL, status TEXT NOT NULL, revision INTEGER NOT NULL, created_at DATETIME NOT NULL, payload TEXT NOT NULL, PRIMARY KEY(scope_kind,scope_id,id))`)
	return err
}

func (s *Store) GetCredentialRequest(ctx context.Context, scope models.Scope, id string) (*models.CredentialRequest, error) {
	return sqlstore.GetCredentialRequest(ctx, s.db, "credential_requests", scope, id)
}

func (s *Store) ListCredentialRequests(ctx context.Context, scope models.Scope, deploymentID, conversationID string) ([]*models.CredentialRequest, error) {
	return sqlstore.ListCredentialRequests(ctx, s.db, "credential_requests", scope, deploymentID, conversationID)
}

func (s *Store) SaveCredentialRequest(ctx context.Context, r *models.CredentialRequest, expected int64) error {
	return sqlstore.SaveCredentialRequest(ctx, s.db, "credential_requests", r, expected)
}
