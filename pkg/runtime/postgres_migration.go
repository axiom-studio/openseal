package runtime

import (
	"context"
	"errors"
	"fmt"
)

const currentPostgresSchemaVersion int64 = 8

// PostgresSchemaVersion returns the highest applied OpenSeal migration.
func (s *PostgresStore) PostgresSchemaVersion(ctx context.Context) (int64, error) {
	if s == nil || s.db == nil {
		return 0, errors.New("PostgreSQL store is not configured")
	}
	var version int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM `+s.table("schema_migrations")).Scan(&version); err != nil {
		return 0, err
	}
	return version, nil
}

// RollbackPostgresMigrations removes schema objects introduced after target.
// It is intentionally explicit and primarily intended for controlled release
// rollback and isolated migration verification.
func (s *PostgresStore) RollbackPostgresMigrations(ctx context.Context, target int64) error {
	if target < 0 || target > currentPostgresSchemaVersion {
		return fmt.Errorf("PostgreSQL migration target must be between 0 and %d", currentPostgresSchemaVersion)
	}
	if s == nil || s.db == nil {
		return errors.New("PostgreSQL store is not configured")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "openseal:migrate:"+s.schema); err != nil {
		return err
	}
	down := map[int64][]string{
		8: {"artifacts"},
		7: {"agent_requests"},
		5: {"approval_checkpoints", "action_calls"},
		4: {"skill_bindings", "skill_definitions", "agent_definition_amendments", "agent_definition_activations", "agent_deployments", "agent_definitions"},
		3: {"agent_turns", "run_activity"},
		2: {"agent_runs", "objectives"},
		1: {"runs"},
	}
	for version := currentPostgresSchemaVersion; version > target; version-- {
		if version == 6 {
			if _, err := tx.ExecContext(ctx, `ALTER TABLE `+s.table("run_activity")+`
				DROP COLUMN IF EXISTS agent_id,
				DROP COLUMN IF EXISTS objective_id,
				DROP COLUMN IF EXISTS team_id,
				DROP COLUMN IF EXISTS severity,
				DROP COLUMN IF EXISTS visibility`); err != nil {
				return err
			}
		}
		for _, table := range down[version] {
			if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS `+s.table(table)+` CASCADE`); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+s.table("schema_migrations")+` WHERE version = $1`, version); err != nil {
			return err
		}
	}
	if target == 0 {
		if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS `+s.table("schema_migrations")+` CASCADE`); err != nil {
			return err
		}
	}
	return tx.Commit()
}
