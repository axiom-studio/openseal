package postgres

import (
	"context"
	"database/sql"

	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
)

// agentBuilderMigrationVersion retires workforce_change_sets (spec §3.6): the
// change-set work family, its outbox triggers and its Runs go, and the leaf
// ready/review drafts are kept in agent_builder_cutover_drafts until
// Engine.AgentBuilderCutover turns each into a builder conversation.
const agentBuilderMigrationVersion int64 = 76

// retiredChangeSetIndexes are the change-set indexes; a rename keeps them,
// so the migration drops them by name. The SQLite names carry the idx_
// prefix.
var retiredChangeSetIndexes = []string{
	"workforce_change_sets_pending_work_idx", "workforce_change_sets_parent_idx",
	"workforce_change_sets_status_idx", "workforce_change_sets_global_recovery_idx",
}

// migrateAgentBuilder is PostgreSQL migration 76 (spec §3.6).
func (s *Store) migrateAgentBuilder(ctx context.Context, tx *sql.Tx) error {
	applied, err := s.postgresMigrationApplied(ctx, tx, agentBuilderMigrationVersion)
	if err != nil || applied {
		return err
	}
	var relation sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT to_regclass($1)::text`, s.schema+"."+sqlstore.RetiredChangeSetTable).Scan(&relation); err != nil {
		return err
	}
	changeSets := relation.Valid
	statements := []string{}
	for _, name := range []string{"work_outbox_insert", "work_outbox_update"} {
		trigger := workOutboxTrigger{table: sqlstore.RetiredChangeSetTable, name: name}
		if changeSets {
			statements = append(statements, `DROP TRIGGER IF EXISTS `+name+` ON `+s.table(sqlstore.RetiredChangeSetTable))
		}
		statements = append(statements, `DROP FUNCTION IF EXISTS `+s.table(trigger.function())+`()`)
	}
	// The agent_runs outbox functions still route workforce_authoring Runs
	// to the authoring family; replace them with the current bodies.
	for _, trigger := range workOutboxTriggers() {
		if trigger.table == "agent_runs" {
			statements = append(statements, s.workOutboxTriggerStatements(trigger)...)
		}
	}
	statements = append(statements,
		`DROP INDEX IF EXISTS `+s.table("agent_runs_global_recovery_idx"),
		`UPDATE `+s.table("agent_runs")+` SET status = 'canceled', revision = revision + 1, lease_owner = '', lease_expires_at = NULL,
			payload = (payload - 'leaseOwner' - 'leaseExpiresAt') || jsonb_build_object('status', 'canceled', 'revision', revision + 1,
				'updatedAt', to_jsonb(now()), 'completedAt', to_jsonb(now()))
			WHERE payload->>'kind' = 'workforce_authoring' AND status NOT IN ('completed', 'failed', 'canceled')`,
		`UPDATE `+s.table("agent_deployments")+` SET payload = payload - 'activation' WHERE payload ? 'activation'`,
		`UPDATE `+s.table("team_deployments")+` SET payload = payload - 'activation' WHERE payload ? 'activation'`,
	)
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	if changeSets {
		table := s.table(sqlstore.RetiredChangeSetTable)
		if err := sqlstore.FoldChangeSetDrafts(ctx, tx, sqlstore.RetiredChangeSetTable,
			`SELECT scope_kind, scope_id, id, status, payload->>'parentId', payload->>'prompt' FROM `+table+` ORDER BY created_at, id`,
			`UPDATE `+table+` SET payload = jsonb_set(payload, '{cutoverPrompts}', $1::jsonb, true) WHERE scope_kind = $2 AND scope_id = $3 AND id = $4`,
			`DELETE FROM `+table+` WHERE scope_kind = $1 AND scope_id = $2 AND id = $3`,
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `ALTER TABLE `+table+` RENAME TO `+sqlstore.AgentBuilderCutoverTable); err != nil {
			return err
		}
		for _, index := range retiredChangeSetIndexes {
			if _, err := tx.ExecContext(ctx, `DROP INDEX IF EXISTS `+s.table(index)); err != nil {
				return err
			}
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+`(version, name)
		VALUES ($1, 'agent builder: retire workforce change sets') ON CONFLICT (version) DO NOTHING`, agentBuilderMigrationVersion)
	return err
}
