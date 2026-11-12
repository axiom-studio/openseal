package runtime

import (
	"context"
	"database/sql"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
)

const credentialRequestMigrationVersion int64 = 62

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

func (s *PostgresStore) GetCredentialRequest(ctx context.Context, scope models.Scope, id string) (*models.CredentialRequest, error) {
	return sqlstore.GetCredentialRequest(ctx, s.db, s.table("credential_requests"), scope, id)
}

func (s *PostgresStore) ListCredentialRequests(ctx context.Context, scope models.Scope, deploymentID, conversationID string) ([]*models.CredentialRequest, error) {
	return sqlstore.ListCredentialRequests(ctx, s.db, s.table("credential_requests"), scope, deploymentID, conversationID)
}

func (s *PostgresStore) SaveCredentialRequest(ctx context.Context, r *models.CredentialRequest, expected int64) error {
	return sqlstore.SaveCredentialRequest(ctx, s.db, s.table("credential_requests"), r, expected)
}
