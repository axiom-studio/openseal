package runtime

import (
	"context"
	"database/sql"
	"fmt"
)

const interactionUmbrellaMigrationVersion int64 = 72

func (s *PostgresStore) migrateInteractionUmbrellas(ctx context.Context, tx *sql.Tx) error {
	applied, err := s.postgresMigrationApplied(ctx, tx, interactionUmbrellaMigrationVersion)
	if err != nil || applied {
		return err
	}
	if err := stampInteractionUmbrellas(ctx, tx, s.table, func(n int) string { return fmt.Sprintf("$%d", n) }); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+`(version,name) VALUES($1,'setup and credential requests belong to the conversation umbrella') ON CONFLICT(version) DO NOTHING`, interactionUmbrellaMigrationVersion)
	return err
}
