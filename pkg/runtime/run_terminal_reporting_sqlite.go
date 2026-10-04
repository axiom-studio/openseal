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

var _ RunTerminalReportingStore = (*SQLiteStore)(nil)

// Match strings.TrimSpace at the SQL eligibility boundary. SQL TRIM's default
// only handles an ordinary space, which could enqueue an empty destination or
// omit a valid reporting root containing another Unicode whitespace character.
const runTerminalReportingWhitespace = " \t\n\v\f\r\u0085\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000"

// Queue timestamps use integer microseconds, matching PostgreSQL precision.
// Trigger-generated RFC3339 values must not be compared as strings against
// database/sql's different time encoding. Parsing whole seconds and the leading
// fractional digits also preserves offsets without floating point rounding.
const sqliteTerminalReportAvailableAt = `CAST(strftime('%s',CASE
	WHEN substr(json_extract(NEW.payload,'$.updatedAt'),20,1)='.' THEN
		substr(json_extract(NEW.payload,'$.updatedAt'),1,19)||
		ltrim(substr(json_extract(NEW.payload,'$.updatedAt'),21),'0123456789')
	ELSE json_extract(NEW.payload,'$.updatedAt') END) AS INTEGER)*1000000
	+ CASE WHEN substr(json_extract(NEW.payload,'$.updatedAt'),20,1)='.' THEN
		CAST(substr(substr(substr(json_extract(NEW.payload,'$.updatedAt'),21),1,
			length(substr(json_extract(NEW.payload,'$.updatedAt'),21))-
			length(ltrim(substr(json_extract(NEW.payload,'$.updatedAt'),21),'0123456789')))
			||'000000',1,6) AS INTEGER) ELSE 0 END`

const sqliteTerminalReportEligible = `NEW.status IN ('completed','failed','canceled')
	AND json_type(NEW.payload,'$.context.conversationId')='text'
	AND trim(json_extract(NEW.payload,'$.context.conversationId'),'` + runTerminalReportingWhitespace + `')<>''
	AND json_type(NEW.payload,'$.context.triggerMessageId')='text'
	AND trim(json_extract(NEW.payload,'$.context.triggerMessageId'),'` + runTerminalReportingWhitespace + `')<>''
	AND ((NEW.status='failed' AND json_extract(NEW.payload,'$.kind')='conversation'
		AND COALESCE(json_extract(NEW.payload,'$.parentRunId'),'')='')
	OR (json_type(NEW.payload,'$.context.reportingRootRunId')='text'
		AND trim(json_extract(NEW.payload,'$.context.reportingRootRunId'),'` + runTerminalReportingWhitespace + `')=NEW.id
		AND EXISTS (SELECT 1 FROM json_each(CASE
			WHEN json_type(NEW.payload,'$.context.reportingMilestones')='array'
			THEN json_extract(NEW.payload,'$.context.reportingMilestones') ELSE '[]' END) milestone
			WHERE milestone.type='text' AND milestone.value=CASE
				WHEN NEW.status='completed' THEN 'completed' ELSE 'failed' END)))`

func migrateRunTerminalReportingSQLite(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='run_terminal_reports'`).Scan(&exists); err != nil {
		return err
	}
	if exists != 0 {
		rows, err := tx.Query(`PRAGMA table_info(run_terminal_reports)`)
		if err != nil {
			return err
		}
		hasRevision := false
		for rows.Next() {
			var cid, notnull, pk int
			var name, typ string
			var defaultValue sql.NullString
			if err := rows.Scan(&cid, &name, &typ, &notnull, &defaultValue, &pk); err != nil {
				rows.Close()
				return err
			}
			hasRevision = hasRevision || name == "terminal_revision"
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if !hasRevision {
			if _, err := tx.Exec(`DROP TRIGGER IF EXISTS agent_runs_terminal_reporting_insert;
				DROP TRIGGER IF EXISTS agent_runs_terminal_reporting_update;
				DROP INDEX IF EXISTS idx_run_terminal_reports_pending_due;
				DROP INDEX IF EXISTS idx_run_terminal_reports_scoped_due;
				ALTER TABLE run_terminal_reports RENAME TO run_terminal_reports_v58;
				CREATE TABLE run_terminal_reports (
					scope_kind TEXT NOT NULL,scope_id TEXT NOT NULL,run_id TEXT NOT NULL,terminal_status TEXT NOT NULL,
					terminal_revision BIGINT NOT NULL,queue_state TEXT NOT NULL DEFAULT 'pending',available_at BIGINT NOT NULL,
					attempts INTEGER NOT NULL DEFAULT 0,lease_owner TEXT NOT NULL DEFAULT '',lease_expires_at BIGINT,
					delivered_at BIGINT,payload TEXT,PRIMARY KEY(scope_kind,scope_id,run_id,terminal_status,terminal_revision),
					CHECK(terminal_status IN ('completed','failed','canceled')),CHECK(queue_state IN ('pending','delivered')));
				INSERT INTO run_terminal_reports SELECT scope_kind,scope_id,run_id,terminal_status,
					COALESCE(json_extract(payload,'$.revision'),0),queue_state,available_at,attempts,lease_owner,lease_expires_at,delivered_at,payload
					FROM run_terminal_reports_v58;
				DROP TABLE run_terminal_reports_v58;`); err != nil {
				return err
			}
		}
	}
	_, err = tx.Exec(`
		CREATE TABLE IF NOT EXISTS run_terminal_reports (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, run_id TEXT NOT NULL,
			terminal_revision BIGINT NOT NULL,
			terminal_status TEXT NOT NULL CHECK(terminal_status IN ('completed','failed','canceled')),
			queue_state TEXT NOT NULL DEFAULT 'pending' CHECK(queue_state IN ('pending','delivered')),
			available_at BIGINT NOT NULL, attempts INTEGER NOT NULL DEFAULT 0,
			lease_owner TEXT NOT NULL DEFAULT '', lease_expires_at BIGINT,
			delivered_at BIGINT, payload TEXT,
			PRIMARY KEY(scope_kind,scope_id,run_id,terminal_status,terminal_revision)
		);
		CREATE INDEX IF NOT EXISTS idx_run_terminal_reports_pending_due
			ON run_terminal_reports(available_at,scope_kind,scope_id,run_id,terminal_status,terminal_revision)
			WHERE queue_state='pending';
		CREATE INDEX IF NOT EXISTS idx_run_terminal_reports_scoped_due
			ON run_terminal_reports(scope_kind,scope_id,available_at,run_id,terminal_status,terminal_revision)
			WHERE queue_state='pending';
		DROP TRIGGER IF EXISTS agent_runs_terminal_reporting_insert;
		CREATE TRIGGER agent_runs_terminal_reporting_insert
		AFTER INSERT ON agent_runs WHEN ` + sqliteTerminalReportEligible + ` BEGIN
			INSERT INTO run_terminal_reports(scope_kind,scope_id,run_id,terminal_status,terminal_revision,available_at,payload)
			VALUES(NEW.scope_kind,NEW.scope_id,NEW.id,NEW.status,json_extract(NEW.payload,'$.revision'),` + sqliteTerminalReportAvailableAt + `,NEW.payload)
			ON CONFLICT(scope_kind,scope_id,run_id,terminal_status,terminal_revision) DO NOTHING;
		END;
		DROP TRIGGER IF EXISTS agent_runs_terminal_reporting_update;
		CREATE TRIGGER agent_runs_terminal_reporting_update
		AFTER UPDATE OF status,payload ON agent_runs WHEN OLD.status<>NEW.status AND ` + sqliteTerminalReportEligible + ` BEGIN
			INSERT INTO run_terminal_reports(scope_kind,scope_id,run_id,terminal_status,terminal_revision,available_at,payload)
			VALUES(NEW.scope_kind,NEW.scope_id,NEW.id,NEW.status,json_extract(NEW.payload,'$.revision'),` + sqliteTerminalReportAvailableAt + `,NEW.payload)
			ON CONFLICT(scope_kind,scope_id,run_id,terminal_status,terminal_revision) DO NOTHING;
		END;
	`)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLiteStore) ClaimRunTerminalReports(ctx context.Context, request RunTerminalReportingClaim) ([]*RunTerminalReport, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	expires := request.Now.Add(request.LeaseDuration).UTC().Truncate(time.Microsecond)
	if !expires.After(request.Now) {
		return nil, ErrInvalidRunTerminalReport
	}
	conn, err := beginImmediateSQLite(ctx, s.db)
	if err != nil {
		return nil, err
	}
	committed := false
	defer rollbackSQLiteConn(conn, &committed)
	query := `SELECT scope_kind,scope_id,run_id,terminal_status,terminal_revision,payload,available_at,attempts,lease_owner,lease_expires_at,delivered_at
		FROM run_terminal_reports WHERE queue_state='pending' AND available_at<=?
		AND (lease_expires_at IS NULL OR lease_expires_at<=?)`
	args := []interface{}{request.Now.UnixMicro(), request.Now.UnixMicro()}
	if request.Scope != nil {
		query += ` AND scope_kind=? AND scope_id=?`
		args = append(args, request.Scope.Kind, request.Scope.ID)
	}
	query += ` ORDER BY available_at,scope_kind,scope_id,run_id,terminal_status,terminal_revision LIMIT ?`
	args = append(args, request.Limit)
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	reports := make([]*RunTerminalReport, 0, request.Limit)
	for rows.Next() {
		report, scanErr := scanSQLiteRunTerminalReport(rows)
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
	for _, report := range reports {
		// Moving the indexed due time to the expiry removes held leases from
		// every worker's ready prefix; recovery remains an indexed due claim.
		_, err = conn.ExecContext(ctx, `UPDATE run_terminal_reports
			SET available_at=?,attempts=attempts+1,lease_owner=?,lease_expires_at=?
			WHERE scope_kind=? AND scope_id=? AND run_id=? AND terminal_status=? AND terminal_revision=?`,
			expires.UnixMicro(), request.WorkerID, expires.UnixMicro(), report.Scope.Kind, report.Scope.ID, report.RunID, report.Status, report.TerminalRevision)
		if err != nil {
			return nil, err
		}
		report.AvailableAt, report.LeaseOwner, report.LeaseExpiresAt = expires, request.WorkerID, &expires
		report.Attempts++
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, err
	}
	committed = true
	return reports, nil
}

func (s *SQLiteStore) CompleteRunTerminalReport(ctx context.Context, request RunTerminalReportingCompletion) error {
	if err := request.Validate(); err != nil {
		return err
	}
	if !request.LeaseExpiresAt.Equal(request.LeaseExpiresAt.Truncate(time.Microsecond)) {
		return ErrLeaseLost
	}
	result, err := s.db.ExecContext(ctx, `UPDATE run_terminal_reports
		SET queue_state='delivered',delivered_at=?,lease_owner='',lease_expires_at=NULL,payload=NULL
		WHERE scope_kind=? AND scope_id=? AND run_id=? AND terminal_status=? AND queue_state='pending'
		AND lease_owner=? AND lease_expires_at=? AND lease_expires_at>? AND terminal_revision=?`,
		request.Now.UnixMicro(), request.Scope.Kind, request.Scope.ID, request.RunID, request.Status,
		request.WorkerID, request.LeaseExpiresAt.UnixMicro(), request.Now.UnixMicro(), request.TerminalRevision)
	return runTerminalReportLeaseResult(result, err)
}

func (s *SQLiteStore) RetryRunTerminalReport(ctx context.Context, request RunTerminalReportingRetry) error {
	if err := request.Validate(); err != nil {
		return err
	}
	if !request.LeaseExpiresAt.Equal(request.LeaseExpiresAt.Truncate(time.Microsecond)) {
		return ErrLeaseLost
	}
	if !request.AvailableAt.Truncate(time.Microsecond).After(request.Now) {
		return ErrInvalidRunTerminalReport
	}
	result, err := s.db.ExecContext(ctx, `UPDATE run_terminal_reports
		SET available_at=?,lease_owner='',lease_expires_at=NULL
		WHERE scope_kind=? AND scope_id=? AND run_id=? AND terminal_status=? AND queue_state='pending'
		AND lease_owner=? AND lease_expires_at=? AND lease_expires_at>? AND terminal_revision=?`,
		request.AvailableAt.UnixMicro(), request.Scope.Kind, request.Scope.ID, request.RunID, request.Status,
		request.WorkerID, request.LeaseExpiresAt.UnixMicro(), request.Now.UnixMicro(), request.TerminalRevision)
	return runTerminalReportLeaseResult(result, err)
}

func (s *SQLiteStore) GetRunTerminalReport(ctx context.Context, scope Scope, runID string, status AgentRunStatus) (*RunTerminalReport, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if !validOpaqueIdentifier(runID, 128) || !isTerminalAgentRunStatus(status) {
		return nil, ErrInvalidRunTerminalReport
	}
	report, err := scanSQLiteRunTerminalReport(s.db.QueryRowContext(ctx, `SELECT scope_kind,scope_id,run_id,terminal_status,terminal_revision,
		payload,available_at,attempts,lease_owner,lease_expires_at,delivered_at FROM run_terminal_reports
		WHERE scope_kind=? AND scope_id=? AND run_id=? AND terminal_status=? ORDER BY terminal_revision DESC LIMIT 1`, scope.Kind, scope.ID, runID, status))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrRunTerminalReportNotFound
	}
	return report, err
}

func scanSQLiteRunTerminalReport(scanner interface{ Scan(...interface{}) error }) (*RunTerminalReport, error) {
	var report RunTerminalReport
	var payload sql.NullString
	var available int64
	var expires, delivered sql.NullInt64
	if err := scanner.Scan(&report.Scope.Kind, &report.Scope.ID, &report.RunID, &report.Status, &report.TerminalRevision,
		&payload, &available, &report.Attempts, &report.LeaseOwner, &expires, &delivered); err != nil {
		return nil, err
	}
	report.AvailableAt = time.UnixMicro(available).UTC()
	if expires.Valid {
		at := time.UnixMicro(expires.Int64).UTC()
		report.LeaseExpiresAt = &at
	}
	if delivered.Valid {
		at := time.UnixMicro(delivered.Int64).UTC()
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

func runTerminalReportLeaseResult(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrLeaseLost
	}
	return nil
}
