package runtime

import (
	"context"
	"database/sql"
)

const activityProjectionRepairMigrationVersion int64 = 38

// migrateActivityProjectionRepair reconciles the indexed activity projection
// with its migration ledger. Some restored databases retained migrations 6 and
// 23 while missing one or more physical columns, which made activity writes
// fail even though the schema appeared current.
func (s *PostgresStore) migrateActivityProjectionRepair(ctx context.Context, tx *sql.Tx) error {
	applied, err := s.postgresMigrationApplied(ctx, tx, activityProjectionRepairMigrationVersion)
	if err != nil || applied {
		return err
	}
	if _, err = tx.ExecContext(ctx, `ALTER TABLE `+s.table("run_activity")+`
		ADD COLUMN IF NOT EXISTS agent_id TEXT NOT NULL DEFAULT '',
		ADD COLUMN IF NOT EXISTS objective_id TEXT NOT NULL DEFAULT '',
		ADD COLUMN IF NOT EXISTS project_id TEXT NOT NULL DEFAULT '',
		ADD COLUMN IF NOT EXISTS team_id TEXT NOT NULL DEFAULT '',
		ADD COLUMN IF NOT EXISTS severity TEXT NOT NULL DEFAULT 'info',
		ADD COLUMN IF NOT EXISTS visibility TEXT NOT NULL DEFAULT 'scope'`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE `+s.table("run_activity")+` SET
		agent_id = COALESCE(NULLIF(agent_id, ''), payload->>'agentId', ''),
		objective_id = COALESCE(NULLIF(objective_id, ''), payload->>'objectiveId', ''),
		project_id = COALESCE(NULLIF(project_id, ''), payload->>'projectId', ''),
		team_id = COALESCE(NULLIF(team_id, ''), payload->>'teamId', ''),
		severity = COALESCE(NULLIF(severity, ''), payload->>'severity', 'info'),
		visibility = COALESCE(NULLIF(visibility, ''), payload->>'visibility', 'scope')`); err != nil {
		return err
	}
	for _, statement := range []string{
		`CREATE INDEX IF NOT EXISTS run_activity_agent_feed_idx ON ` + s.table("run_activity") + ` (scope_kind, scope_id, agent_id, created_at DESC, id DESC)`,
		`CREATE INDEX IF NOT EXISTS run_activity_objective_feed_idx ON ` + s.table("run_activity") + ` (scope_kind, scope_id, objective_id, created_at DESC, id DESC)`,
		`CREATE INDEX IF NOT EXISTS run_activity_project_feed_idx ON ` + s.table("run_activity") + ` (scope_kind, scope_id, project_id, created_at DESC, id DESC)`,
		`CREATE INDEX IF NOT EXISTS run_activity_team_feed_idx ON ` + s.table("run_activity") + ` (scope_kind, scope_id, team_id, created_at DESC, id DESC)`,
	} {
		if _, err = tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+
		` (version, name) VALUES ($1, 'reconcile indexed activity projections')`, activityProjectionRepairMigrationVersion)
	return err
}
