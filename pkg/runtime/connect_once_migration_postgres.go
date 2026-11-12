package runtime

import (
	"context"
	"database/sql"
)

const connectOnceMigrationVersion int64 = 74

func (s *PostgresStore) migrateConnectOnce(ctx context.Context, tx *sql.Tx) error {
	applied, err := s.postgresMigrationApplied(ctx, tx, connectOnceMigrationVersion)
	if err != nil || applied {
		return err
	}
	if err := (connectOnceMigration{db: tx, table: s.table}).run(ctx); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+`(version,name) VALUES($1,'connect once: event waits, setup copies, amendments, gateway owners, routine creators') ON CONFLICT(version) DO NOTHING`, connectOnceMigrationVersion)
	return err
}
