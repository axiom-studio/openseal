package runtime

import (
	"context"
	"database/sql"
)

const externalOperationReceiptMigrationVersion int64 = 33

func (s *PostgresStore) migrateExternalOperationReceipts(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `
		ALTER TABLE `+s.table("action_calls")+`
			ADD COLUMN IF NOT EXISTS external_operation_digest TEXT NOT NULL DEFAULT '';
		CREATE UNIQUE INDEX IF NOT EXISTS action_calls_external_operation_idx
			ON `+s.table("action_calls")+` (scope_kind, scope_id, external_operation_digest)
			WHERE external_operation_digest <> '' AND status IN ('ready','waiting_for_approval','running','succeeded','compensating','compensated')
	`); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+`
		(version, name) VALUES ($1, 'durable external operation receipts') ON CONFLICT(version) DO NOTHING`, externalOperationReceiptMigrationVersion)
	return err
}
