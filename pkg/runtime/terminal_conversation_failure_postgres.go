package runtime

import (
	"context"
	"database/sql"
	"errors"
)

const terminalConversationFailureMigrationVersion int64 = 59

// Migration 59 requires quiescing pre-59 terminal-report SQL readers/writers.
// The old three-column conflict/completion contract cannot represent two
// manual attempts safely. No pending or leased delivery is overwritten.
func (s *PostgresStore) migrateTerminalConversationFailures(ctx context.Context, tx *sql.Tx) error {
	applied, err := s.postgresMigrationApplied(ctx, tx, terminalConversationFailureMigrationVersion)
	if err != nil {
		return err
	}
	if !applied {
		_, err = tx.ExecContext(ctx, `ALTER TABLE `+s.table("run_terminal_reports")+`
			ADD COLUMN terminal_revision BIGINT NOT NULL DEFAULT 0;
			UPDATE `+s.table("run_terminal_reports")+` SET terminal_revision=COALESCE((payload->>'revision')::bigint,0);
			ALTER TABLE `+s.table("run_terminal_reports")+` DROP CONSTRAINT run_terminal_reports_pkey;
			ALTER TABLE `+s.table("run_terminal_reports")+` ADD PRIMARY KEY(scope_kind,scope_id,run_id,terminal_status,terminal_revision);
			DROP INDEX `+s.table("run_terminal_reports_pending_due_idx")+`;
			DROP INDEX `+s.table("run_terminal_reports_scoped_due_idx")+`;
			CREATE INDEX run_terminal_reports_pending_due_idx ON `+s.table("run_terminal_reports")+`
				(available_at,scope_kind,scope_id,run_id,terminal_status,terminal_revision) WHERE queue_state='pending';
			CREATE INDEX run_terminal_reports_scoped_due_idx ON `+s.table("run_terminal_reports")+`
				(scope_kind,scope_id,available_at,run_id,terminal_status,terminal_revision) WHERE queue_state='pending';`)
		if err != nil {
			return err
		}
	}
	// Reinstall after the original reporting migration when repairing a ledger
	// gap; its legacy trigger must never survive the committed schema 59.
	_, err = tx.ExecContext(ctx, `CREATE OR REPLACE FUNCTION `+s.table("enqueue_run_terminal_report")+`() RETURNS trigger LANGUAGE plpgsql AS $terminal_report$
		BEGIN
			IF (TG_OP='INSERT' OR OLD.status IS DISTINCT FROM NEW.status)
			AND NEW.status IN ('completed','failed','canceled')
			AND jsonb_typeof(NEW.payload #> '{context,conversationId}')='string'
			AND btrim(NEW.payload #>> '{context,conversationId}','`+runTerminalReportingWhitespace+`')<>''
			AND jsonb_typeof(NEW.payload #> '{context,triggerMessageId}')='string'
			AND btrim(NEW.payload #>> '{context,triggerMessageId}','`+runTerminalReportingWhitespace+`')<>''
			AND (
				(NEW.status='failed' AND NEW.payload->>'kind'='conversation' AND COALESCE(NEW.payload->>'parentRunId','')='')
				OR (jsonb_typeof(NEW.payload #> '{context,reportingRootRunId}')='string'
					AND btrim(NEW.payload #>> '{context,reportingRootRunId}','`+runTerminalReportingWhitespace+`')=NEW.id
					AND (NEW.payload #> '{context,reportingMilestones}') @>
					(CASE WHEN NEW.status='completed' THEN '["completed"]'::jsonb ELSE '["failed"]'::jsonb END))
			) THEN
				INSERT INTO `+s.table("run_terminal_reports")+`(scope_kind,scope_id,run_id,terminal_status,terminal_revision,available_at,payload)
				VALUES(NEW.scope_kind,NEW.scope_id,NEW.id,NEW.status,(NEW.payload->>'revision')::bigint,(NEW.payload->>'updatedAt')::timestamptz,NEW.payload)
				ON CONFLICT(scope_kind,scope_id,run_id,terminal_status,terminal_revision) DO NOTHING;
			END IF;
			RETURN NEW;
		END;
		$terminal_report$;
		INSERT INTO `+s.table("schema_migrations")+`(version,name)
			VALUES(59,'per-attempt terminal conversation failure replies') ON CONFLICT(version) DO NOTHING;`)
	return err
}

func (s *PostgresStore) rollbackTerminalConversationFailures(ctx context.Context, tx *sql.Tx) error {
	var ambiguous bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM `+s.table("run_terminal_reports")+`
		GROUP BY scope_kind,scope_id,run_id,terminal_status HAVING COUNT(*)>1)`).Scan(&ambiguous); err != nil {
		return err
	}
	if ambiguous {
		return errors.New("cannot roll back terminal reporting while multiple attempt receipts exist")
	}
	var unsupportedPending bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM `+s.table("run_terminal_reports")+`
		WHERE queue_state='pending' AND NOT COALESCE(
			jsonb_typeof(payload #> '{context,reportingRootRunId}')='string'
			AND btrim(payload #>> '{context,reportingRootRunId}','`+runTerminalReportingWhitespace+`')=run_id
			AND (payload #> '{context,reportingMilestones}') @>
			(CASE WHEN terminal_status='completed' THEN '["completed"]'::jsonb ELSE '["failed"]'::jsonb END),FALSE))`).Scan(&unsupportedPending); err != nil {
		return err
	}
	if unsupportedPending {
		return errors.New("cannot roll back terminal reporting before new conversation failure replies are delivered")
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE `+s.table("run_terminal_reports")+` DROP CONSTRAINT run_terminal_reports_pkey;
		ALTER TABLE `+s.table("run_terminal_reports")+` DROP COLUMN terminal_revision;
		ALTER TABLE `+s.table("run_terminal_reports")+` ADD PRIMARY KEY(scope_kind,scope_id,run_id,terminal_status);`); err != nil {
		return err
	}
	return s.migrateRunTerminalReporting(ctx, tx)
}
