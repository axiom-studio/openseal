package runtime

import (
	"context"
	"database/sql"

	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
)

var retiredChangeSetIndexesSQLite = []string{
	"idx_workforce_change_sets_parent", "idx_workforce_change_sets_status", "idx_workforce_change_sets_global_recovery",
}

// migrateAgentBuilderSQLite mirrors PostgreSQL migration 76. SQLite has no
// ledger: the retirement runs only while workforce_change_sets exists, so a
// later open does nothing.
func migrateAgentBuilderSQLite(db *sql.DB) error {
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DROP INDEX IF EXISTS idx_agent_runs_global_recovery`); err != nil {
		return err
	}
	var present int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, sqlstore.RetiredChangeSetTable).Scan(&present); err != nil {
		return err
	}
	if present == 0 {
		return tx.Commit()
	}
	for _, statement := range []string{
		`UPDATE agent_runs SET status = 'canceled', revision = revision + 1, lease_owner = '', lease_expires_at = NULL,
			payload = json_set(json_remove(payload, '$.leaseOwner', '$.leaseExpiresAt'), '$.status', 'canceled', '$.revision', revision + 1,
				'$.updatedAt', strftime('%Y-%m-%dT%H:%M:%fZ', 'now'), '$.completedAt', strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
			WHERE json_extract(payload, '$.kind') = 'workforce_authoring' AND status NOT IN ('completed', 'failed', 'canceled')`,
		`UPDATE agent_deployments SET payload = json_remove(payload, '$.activation') WHERE json_type(payload, '$.activation') IS NOT NULL`,
		`UPDATE team_deployments SET payload = json_remove(payload, '$.activation') WHERE json_type(payload, '$.activation') IS NOT NULL`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	if err := sqlstore.FoldChangeSetDrafts(ctx, tx, sqlstore.RetiredChangeSetTable,
		`SELECT scope_kind, scope_id, id, status, json_extract(payload, '$.parentId'), json_extract(payload, '$.prompt') FROM workforce_change_sets ORDER BY created_at, id`,
		`UPDATE workforce_change_sets SET payload = json_set(payload, '$.cutoverPrompts', json(?)) WHERE scope_kind = ? AND scope_id = ? AND id = ?`,
		`DELETE FROM workforce_change_sets WHERE scope_kind = ? AND scope_id = ? AND id = ?`,
	); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE workforce_change_sets RENAME TO `+sqlstore.AgentBuilderCutoverTable); err != nil {
		return err
	}
	for _, index := range retiredChangeSetIndexesSQLite {
		if _, err := tx.ExecContext(ctx, `DROP INDEX IF EXISTS `+index); err != nil {
			return err
		}
	}
	return tx.Commit()
}
