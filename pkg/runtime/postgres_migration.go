package runtime

import (
	"context"
	"errors"
	"fmt"
)

const currentPostgresSchemaVersion int64 = 23

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
	if _, err := s.acquireMigrationLock(ctx, tx); err != nil {
		return err
	}
	down := map[int64][]string{
		22: {"source_policy_lifecycle_events", "source_policy_lifecycles", "source_policy_versions"},
		21: {"action_credential_lease_redemptions"},
		20: {"event_source_checkpoints"},
		17: {"skill_source_artifact_references", "skill_source_artifacts"},
		16: {"source_monitor_checkpoints", "source_observations"},
		15: {"agent_definition_compilations"},
		14: {"initiatives"},
		13: {"workforce_change_sets"},
		12: {"team_definition_amendments"},
		11: {"team_definition_activations", "team_deployments", "team_definitions"},
		10: {"conversation_presence", "conversation_cursors", "participation_rounds", "channel_messages", "conversations"},
		9:  {"run_dependencies", "run_dependency_groups"},
		8:  {"artifacts"},
		7:  {"agent_requests"},
		5:  {"approval_checkpoints", "action_calls"},
		4:  {"skill_bindings", "skill_definitions", "agent_definition_amendments", "agent_definition_activations", "agent_deployments", "agent_definitions"},
		3:  {"agent_turns", "run_activity"},
		2:  {"agent_runs", "objectives"},
		1:  {"runs"},
	}
	for version := currentPostgresSchemaVersion; version > target; version-- {
		if version == 23 {
			if _, err := tx.ExecContext(ctx, `
				DROP INDEX IF EXISTS `+s.table("run_activity_initiative_feed_idx")+`;
				ALTER TABLE `+s.table("run_activity")+` DROP COLUMN IF EXISTS initiative_id
			`); err != nil {
				return err
			}
		}
		if version == 19 {
			var hasCollisions bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
				SELECT 1 FROM `+s.table("skill_definitions")+` GROUP BY id, version HAVING COUNT(*) > 1
			)`).Scan(&hasCollisions); err != nil {
				return err
			}
			if hasCollisions {
				return errors.New("cannot roll back source-qualified Skill variants while publisher-colliding definitions exist")
			}
			if _, err := tx.ExecContext(ctx, `
				ALTER TABLE `+s.table("skill_definitions")+` DROP CONSTRAINT IF EXISTS skill_definitions_pkey;
				ALTER TABLE `+s.table("skill_definitions")+` ADD PRIMARY KEY (id, version);
				ALTER TABLE `+s.table("skill_definitions")+` DROP COLUMN IF EXISTS source_identity;
				ALTER TABLE `+s.table("skill_bindings")+` DROP COLUMN IF EXISTS source_identity;
			`); err != nil {
				return err
			}
		}
		if version == 18 {
			if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS `+s.table("outreach_threads")+` CASCADE`); err != nil {
				return err
			}
		}
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
