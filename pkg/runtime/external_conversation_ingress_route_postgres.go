package runtime

import (
	"context"
	"database/sql"
)

const externalConversationIngressRouteMigrationVersion int64 = 27

func (s *PostgresStore) migrateExternalConversationIngressRoutes(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `
		ALTER TABLE `+s.table("external_conversation_endpoints")+`
			ADD COLUMN IF NOT EXISTS ingress_route TEXT;
		UPDATE `+s.table("external_conversation_endpoints")+`
			SET ingress_route=gen_random_uuid()::text
			WHERE ingress_route IS NULL OR ingress_route='';
		UPDATE `+s.table("external_conversation_endpoints")+`
			SET payload=jsonb_set(payload,'{ingressRoute}',to_jsonb(ingress_route),true);
		ALTER TABLE `+s.table("external_conversation_endpoints")+`
			ALTER COLUMN ingress_route SET NOT NULL;
		CREATE UNIQUE INDEX IF NOT EXISTS external_conversation_endpoints_ingress_route_idx
			ON `+s.table("external_conversation_endpoints")+` (ingress_route)
	`); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+`
		(version,name) VALUES ($1,'opaque external conversation ingress routes')
		ON CONFLICT(version) DO NOTHING`, externalConversationIngressRouteMigrationVersion)
	return err
}
