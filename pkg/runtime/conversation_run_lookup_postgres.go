package runtime

import (
	"context"
	"database/sql"
)

const conversationRunLookupMigrationVersion int64 = 44

func (s *PostgresStore) migrateConversationRunLookup(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS agent_runs_concurrency_idx ON `+s.table("agent_runs")+` (scope_kind, scope_id, (payload->>'concurrencyKey'))`); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+` (version, name) VALUES ($1, 'scoped conversation run lookup') ON CONFLICT (version) DO NOTHING`, conversationRunLookupMigrationVersion)
	return err
}
