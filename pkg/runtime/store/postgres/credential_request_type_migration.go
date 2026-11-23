package postgres

import (
	"context"
	"database/sql"
)

// credentialRequestTypeMigrationVersion renames a credential request's kind
// to the vault credential type it asks for. A request now names any host
// credential type; the two kinds it had map to Core's vault types
// (website_login is http_basic_auth, payment_card keeps its name).
const credentialRequestTypeMigrationVersion int64 = 89

// migrateCredentialRequestTypes is PostgreSQL migration 89.
func (s *Store) migrateCredentialRequestTypes(ctx context.Context, tx *sql.Tx) error {
	applied, err := s.postgresMigrationApplied(ctx, tx, credentialRequestTypeMigrationVersion)
	if err != nil || applied {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE `+s.table("credential_requests")+`
		SET payload = (payload - 'kind') || jsonb_build_object('credentialType',
			CASE payload->>'kind' WHEN 'website_login' THEN 'http_basic_auth' ELSE payload->>'kind' END)
		WHERE payload ? 'kind'`); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+`(version, name)
		VALUES ($1, 'credential requests name a credential type') ON CONFLICT (version) DO NOTHING`, credentialRequestTypeMigrationVersion)
	return err
}

// rollbackCredentialRequestTypes restores kind on the requests whose type had
// one. Requests for any other type have no kind to return to and are
// deleted with the cards they show.
func (s *Store) rollbackCredentialRequestTypes(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM `+s.table("credential_requests")+`
			WHERE payload->>'credentialType' NOT IN ('http_basic_auth', 'payment_card');
		UPDATE `+s.table("credential_requests")+`
			SET payload = (payload - 'credentialType' - 'service' - 'fields' - 'credential') || jsonb_build_object('kind',
				CASE payload->>'credentialType' WHEN 'http_basic_auth' THEN 'website_login' ELSE 'payment_card' END)
			WHERE payload ? 'credentialType'`)
	return err
}
