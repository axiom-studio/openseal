package runtime

import (
	"context"
	"database/sql"
)

const callbackWorkerMigrationVersion int64 = 42

func (s *PostgresStore) migrateCallbackWorker(ctx context.Context, tx *sql.Tx) error {
	applied, err := s.postgresMigrationApplied(ctx, tx, callbackWorkerMigrationVersion)
	if err != nil || applied {
		return err
	}
	if _, err = tx.ExecContext(ctx, `
		ALTER TABLE `+s.table("callback_events")+`
			ADD COLUMN IF NOT EXISTS available_at TIMESTAMPTZ,
			ADD COLUMN IF NOT EXISTS lease_owner TEXT NOT NULL DEFAULT '',
			ADD COLUMN IF NOT EXISTS lease_expires_at TIMESTAMPTZ,
			ADD COLUMN IF NOT EXISTS attempts INTEGER NOT NULL DEFAULT 0;
		UPDATE `+s.table("callback_events")+` SET available_at=updated_at WHERE available_at IS NULL;
		ALTER TABLE `+s.table("callback_events")+` ALTER COLUMN available_at SET NOT NULL;
		CREATE INDEX IF NOT EXISTS callback_events_dispatch_idx
			ON `+s.table("callback_events")+` (scope_kind,scope_id,status,available_at,lease_expires_at)
	`); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+`
		(version,name) VALUES ($1,'lease durable callback event dispatch')`, callbackWorkerMigrationVersion)
	return err
}
