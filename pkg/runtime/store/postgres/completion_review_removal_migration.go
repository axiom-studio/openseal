package postgres

import (
	"context"
	"database/sql"
	"fmt"
)

// completionReviewRemovalMigrationVersion drops the completion_review
// AgentRequest status from the pending-work index. A Team member's finished
// request now completes directly, so no request may still wait for review.
const completionReviewRemovalMigrationVersion int64 = 88

// migrateCompletionReviewRemoval is PostgreSQL migration 88. A request still
// in completion_review has a source Run waiting on it that no code path wakes
// any more, so the migration stops with the count instead of guessing an
// outcome for that work.
func (s *Store) migrateCompletionReviewRemoval(ctx context.Context, tx *sql.Tx) error {
	applied, err := s.postgresMigrationApplied(ctx, tx, completionReviewRemovalMigrationVersion)
	if err != nil || applied {
		return err
	}
	var waiting int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+s.table("agent_requests")+` WHERE status = 'completion_review'`).Scan(&waiting); err != nil {
		return err
	}
	if waiting > 0 {
		return fmt.Errorf("%d agent requests are still in completion_review; resolve them before upgrading", waiting)
	}
	if _, err := tx.ExecContext(ctx, `DROP INDEX IF EXISTS `+s.table("agent_requests_pending_work_idx")+`;
		CREATE INDEX agent_requests_pending_work_idx ON `+s.table("agent_requests")+`(scope_kind,scope_id)
			WHERE status IN ('pending','accepted','clarification_requested')`); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+`(version, name)
		VALUES ($1, 'remove agent request completion review') ON CONFLICT (version) DO NOTHING`, completionReviewRemovalMigrationVersion)
	return err
}

// rollbackCompletionReviewRemoval restores the index predicate of migration
// 64's work outbox.
func (s *Store) rollbackCompletionReviewRemoval(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `DROP INDEX IF EXISTS `+s.table("agent_requests_pending_work_idx")+`;
		CREATE INDEX agent_requests_pending_work_idx ON `+s.table("agent_requests")+`(scope_kind,scope_id)
			WHERE status IN ('pending','accepted','clarification_requested','completion_review')`)
	return err
}
