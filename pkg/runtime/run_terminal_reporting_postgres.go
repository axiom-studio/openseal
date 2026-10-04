package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var _ RunTerminalReportingStore = (*PostgresStore)(nil)

func (s *PostgresStore) migrateRunTerminalReporting(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS `+s.table("run_terminal_reports")+` (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, run_id TEXT NOT NULL,
			terminal_status TEXT NOT NULL CHECK(terminal_status IN ('completed','failed','canceled')),
			queue_state TEXT NOT NULL DEFAULT 'pending' CHECK(queue_state IN ('pending','delivered')),
			available_at TIMESTAMPTZ NOT NULL, attempts INTEGER NOT NULL DEFAULT 0,
			lease_owner TEXT NOT NULL DEFAULT '', lease_expires_at TIMESTAMPTZ,
			delivered_at TIMESTAMPTZ, payload JSONB,
			PRIMARY KEY(scope_kind,scope_id,run_id,terminal_status)
		);
		CREATE INDEX IF NOT EXISTS run_terminal_reports_pending_due_idx
			ON `+s.table("run_terminal_reports")+`(available_at,scope_kind,scope_id,run_id,terminal_status)
			WHERE queue_state='pending';
		CREATE INDEX IF NOT EXISTS run_terminal_reports_scoped_due_idx
			ON `+s.table("run_terminal_reports")+`(scope_kind,scope_id,available_at,run_id,terminal_status)
			WHERE queue_state='pending';
		CREATE OR REPLACE FUNCTION `+s.table("enqueue_run_terminal_report")+`() RETURNS trigger LANGUAGE plpgsql AS $terminal_report$
		BEGIN
			IF NEW.status IN ('completed','failed','canceled')
			AND jsonb_typeof(NEW.payload #> '{context,reportingRootRunId}')='string'
			AND btrim(NEW.payload #>> '{context,reportingRootRunId}','`+runTerminalReportingWhitespace+`')=NEW.id
			AND jsonb_typeof(NEW.payload #> '{context,conversationId}')='string'
			AND btrim(NEW.payload #>> '{context,conversationId}','`+runTerminalReportingWhitespace+`')<>''
			AND jsonb_typeof(NEW.payload #> '{context,triggerMessageId}')='string'
			AND btrim(NEW.payload #>> '{context,triggerMessageId}','`+runTerminalReportingWhitespace+`')<>''
			AND (NEW.payload #> '{context,reportingMilestones}') @>
				(CASE WHEN NEW.status='completed' THEN '["completed"]'::jsonb ELSE '["failed"]'::jsonb END)
			THEN
				INSERT INTO `+s.table("run_terminal_reports")+`(scope_kind,scope_id,run_id,terminal_status,available_at,payload)
				VALUES(NEW.scope_kind,NEW.scope_id,NEW.id,NEW.status,(NEW.payload->>'updatedAt')::timestamptz,NEW.payload)
				ON CONFLICT(scope_kind,scope_id,run_id,terminal_status) DO NOTHING;
			END IF;
			RETURN NEW;
		END;
		$terminal_report$;
		DROP TRIGGER IF EXISTS agent_runs_terminal_reporting ON `+s.table("agent_runs")+`;
		CREATE TRIGGER agent_runs_terminal_reporting AFTER INSERT OR UPDATE OF status,payload
			ON `+s.table("agent_runs")+` FOR EACH ROW EXECUTE FUNCTION `+s.table("enqueue_run_terminal_report")+`();
		INSERT INTO `+s.table("schema_migrations")+`(version,name)
			VALUES(50,'durable terminal Run reporting outbox') ON CONFLICT(version) DO NOTHING;
	`)
	return err
}

func (s *PostgresStore) ClaimRunTerminalReports(ctx context.Context, request RunTerminalReportingClaim) ([]*RunTerminalReport, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	expires := request.Now.Add(request.LeaseDuration).UTC().Truncate(time.Microsecond)
	if !expires.After(request.Now) {
		return nil, ErrInvalidRunTerminalReport
	}
	args := []interface{}{request.Now.UTC().Truncate(time.Microsecond), request.Limit, request.WorkerID, expires}
	filter := ""
	if request.Scope != nil {
		filter = ` AND scope_kind=$5 AND scope_id=$6`
		args = append(args, request.Scope.Kind, request.Scope.ID)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `WITH candidates AS MATERIALIZED (
		SELECT scope_kind,scope_id,run_id,terminal_status,terminal_revision FROM `+s.table("run_terminal_reports")+`
		WHERE queue_state='pending' AND available_at<=$1 AND (lease_expires_at IS NULL OR lease_expires_at<=$1)`+filter+`
		ORDER BY available_at,scope_kind,scope_id,run_id,terminal_status,terminal_revision LIMIT $2 FOR UPDATE SKIP LOCKED
	), claimed AS (
		UPDATE `+s.table("run_terminal_reports")+` report
		SET available_at=$4,attempts=report.attempts+1,lease_owner=$3,lease_expires_at=$4
		FROM candidates WHERE report.scope_kind=candidates.scope_kind AND report.scope_id=candidates.scope_id
		AND report.run_id=candidates.run_id AND report.terminal_status=candidates.terminal_status AND report.terminal_revision=candidates.terminal_revision
		RETURNING report.scope_kind,report.scope_id,report.run_id,report.terminal_status,
			report.terminal_revision,report.payload,report.available_at,report.attempts,report.lease_owner,report.lease_expires_at,report.delivered_at
	) SELECT * FROM claimed ORDER BY available_at,scope_kind,scope_id,run_id,terminal_status,terminal_revision`, args...)
	if err != nil {
		return nil, err
	}
	reports := make([]*RunTerminalReport, 0, request.Limit)
	for rows.Next() {
		report, scanErr := scanPostgresRunTerminalReport(rows)
		if scanErr != nil {
			_ = rows.Close()
			return nil, scanErr
		}
		reports = append(reports, report)
	}
	err = rows.Err()
	if closeErr := rows.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return reports, nil
}

func (s *PostgresStore) CompleteRunTerminalReport(ctx context.Context, request RunTerminalReportingCompletion) error {
	if err := request.Validate(); err != nil {
		return err
	}
	if !request.LeaseExpiresAt.Equal(request.LeaseExpiresAt.Truncate(time.Microsecond)) {
		return ErrLeaseLost
	}
	result, err := s.db.ExecContext(ctx, `UPDATE `+s.table("run_terminal_reports")+`
		SET queue_state='delivered',delivered_at=$1,lease_owner='',lease_expires_at=NULL,payload=NULL
		WHERE scope_kind=$2 AND scope_id=$3 AND run_id=$4 AND terminal_status=$5 AND queue_state='pending'
		AND lease_owner=$6 AND lease_expires_at=$7 AND lease_expires_at>$1 AND terminal_revision=$8`,
		request.Now.UTC().Truncate(time.Microsecond), request.Scope.Kind, request.Scope.ID, request.RunID, request.Status,
		request.WorkerID, request.LeaseExpiresAt.UTC(), request.TerminalRevision)
	return runTerminalReportLeaseResult(result, err)
}

func (s *PostgresStore) RetryRunTerminalReport(ctx context.Context, request RunTerminalReportingRetry) error {
	if err := request.Validate(); err != nil {
		return err
	}
	if !request.LeaseExpiresAt.Equal(request.LeaseExpiresAt.Truncate(time.Microsecond)) {
		return ErrLeaseLost
	}
	if !request.AvailableAt.Truncate(time.Microsecond).After(request.Now) {
		return ErrInvalidRunTerminalReport
	}
	result, err := s.db.ExecContext(ctx, `UPDATE `+s.table("run_terminal_reports")+`
		SET available_at=$1,lease_owner='',lease_expires_at=NULL
		WHERE scope_kind=$2 AND scope_id=$3 AND run_id=$4 AND terminal_status=$5 AND queue_state='pending'
		AND lease_owner=$6 AND lease_expires_at=$7 AND lease_expires_at>$8 AND terminal_revision=$9`,
		request.AvailableAt.UTC().Truncate(time.Microsecond), request.Scope.Kind, request.Scope.ID, request.RunID, request.Status,
		request.WorkerID, request.LeaseExpiresAt.UTC(), request.Now.UTC().Truncate(time.Microsecond), request.TerminalRevision)
	return runTerminalReportLeaseResult(result, err)
}

func (s *PostgresStore) GetRunTerminalReport(ctx context.Context, scope Scope, runID string, status AgentRunStatus) (*RunTerminalReport, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if !validOpaqueIdentifier(runID, 128) || !isTerminalAgentRunStatus(status) {
		return nil, ErrInvalidRunTerminalReport
	}
	report, err := scanPostgresRunTerminalReport(s.db.QueryRowContext(ctx, `SELECT scope_kind,scope_id,run_id,terminal_status,terminal_revision,
		payload,available_at,attempts,lease_owner,lease_expires_at,delivered_at FROM `+s.table("run_terminal_reports")+`
		WHERE scope_kind=$1 AND scope_id=$2 AND run_id=$3 AND terminal_status=$4 ORDER BY terminal_revision DESC LIMIT 1`, scope.Kind, scope.ID, runID, status))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrRunTerminalReportNotFound
	}
	return report, err
}

func scanPostgresRunTerminalReport(scanner interface{ Scan(...interface{}) error }) (*RunTerminalReport, error) {
	var report RunTerminalReport
	var payload sql.NullString
	var expires, delivered sql.NullTime
	if err := scanner.Scan(&report.Scope.Kind, &report.Scope.ID, &report.RunID, &report.Status, &report.TerminalRevision,
		&payload, &report.AvailableAt, &report.Attempts, &report.LeaseOwner, &expires, &delivered); err != nil {
		return nil, err
	}
	report.AvailableAt = report.AvailableAt.UTC()
	if expires.Valid {
		at := expires.Time.UTC()
		report.LeaseExpiresAt = &at
	}
	if delivered.Valid {
		at := delivered.Time.UTC()
		report.DeliveredAt = &at
	}
	if payload.Valid {
		report.Run = &AgentRun{}
		decoder := json.NewDecoder(strings.NewReader(payload.String))
		decoder.UseNumber()
		if err := decoder.Decode(report.Run); err != nil {
			return nil, fmt.Errorf("decode terminal Run report: %w", err)
		}
	}
	return &report, nil
}
