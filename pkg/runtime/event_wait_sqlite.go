package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

func migrateRunEventWaitsSQLite(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS run_event_inbox (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, source TEXT NOT NULL, event_id TEXT NOT NULL,
			type TEXT NOT NULL, subject TEXT NOT NULL, occurred_at DATETIME NOT NULL, received_at DATETIME NOT NULL,
			digest TEXT NOT NULL, attributes TEXT NOT NULL, payload TEXT NOT NULL,
			PRIMARY KEY(scope_kind, scope_id, source, event_id)
		);
		CREATE INDEX IF NOT EXISTS idx_run_event_inbox_match
			ON run_event_inbox(scope_kind, scope_id, source, type, subject, occurred_at, received_at, event_id);
		CREATE INDEX IF NOT EXISTS idx_run_event_inbox_retention ON run_event_inbox(scope_kind, scope_id, received_at);
		CREATE INDEX IF NOT EXISTS idx_run_event_inbox_received ON run_event_inbox(received_at,scope_kind,scope_id,source,event_id);
		CREATE TABLE IF NOT EXISTS run_event_waits (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, run_id TEXT NOT NULL, wait_key TEXT NOT NULL,
			status TEXT NOT NULL, source TEXT NOT NULL, event_type TEXT NOT NULL, subject TEXT NOT NULL,
			after_at DATETIME NOT NULL, deadline DATETIME NOT NULL, attributes TEXT NOT NULL,
			available_at DATETIME, lease_owner TEXT NOT NULL DEFAULT '', lease_expires_at DATETIME, payload TEXT NOT NULL,
			PRIMARY KEY(scope_kind, scope_id, run_id, wait_key)
		);
		CREATE INDEX IF NOT EXISTS idx_run_event_waits_due
			ON run_event_waits(scope_kind, scope_id, status, available_at, run_id, wait_key, lease_expires_at);
		CREATE INDEX IF NOT EXISTS idx_run_event_waits_match
			ON run_event_waits(scope_kind, scope_id, source, event_type, subject, status, after_at, deadline);
		CREATE INDEX IF NOT EXISTS idx_run_event_waits_run_status
			ON run_event_waits(scope_kind,scope_id,run_id,status);
		CREATE INDEX IF NOT EXISTS idx_run_event_waits_unleased_due
			ON run_event_waits(scope_kind,scope_id,status,available_at,run_id,wait_key,lease_expires_at) WHERE lease_expires_at IS NULL;
		CREATE INDEX IF NOT EXISTS idx_run_event_waits_leased_due
			ON run_event_waits(scope_kind,scope_id,status,lease_expires_at,run_id,wait_key,available_at) WHERE lease_expires_at IS NOT NULL;
		CREATE TABLE IF NOT EXISTS run_event_wait_scopes (
			scope_kind TEXT NOT NULL,scope_id TEXT NOT NULL,available_at DATETIME,
			PRIMARY KEY(scope_kind,scope_id)
		);
		CREATE INDEX IF NOT EXISTS idx_run_event_wait_scopes_due ON run_event_wait_scopes(available_at,scope_kind,scope_id) WHERE available_at IS NOT NULL;
		CREATE TABLE IF NOT EXISTS run_event_consumptions (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, run_id TEXT NOT NULL, source TEXT NOT NULL, event_id TEXT NOT NULL,
			PRIMARY KEY(scope_kind, scope_id, run_id, source, event_id)
		);
		CREATE INDEX IF NOT EXISTS idx_run_event_consumptions_event
			ON run_event_consumptions(scope_kind, scope_id, source, event_id);
	`)
	return err
}

// JSON scalar types are checked as well as values: true must not match 1, and
// "1" must not match 1. JSON integer and real numbers share numeric semantics.
const sqliteRunEventAttributesMatch = `NOT EXISTS (
	SELECT 1 FROM json_each(w.attributes) expected
	LEFT JOIN json_each(e.attributes) actual ON actual.key = expected.key
	WHERE actual.key IS NULL
	OR json_extract(actual.value,'$[0]') IS NOT json_extract(expected.value,'$[0]')
	OR json_extract(actual.value,'$[1]') IS NOT json_extract(expected.value,'$[1]')
) AND (substr(w.source,1,8)<>'binding:' OR json_extract(e.attributes,'$.deploymentId[1]')=json_extract(w.payload,'$.assignedAgentId'))`

func (s *SQLiteStore) PublishRunEvent(ctx context.Context, receipt *RunEventReceipt) (bool, error) {
	if err := validateRunEventReceipt(receipt); err != nil {
		return false, err
	}
	conn, err := beginImmediateSQLite(ctx, s.db)
	if err != nil {
		return false, err
	}
	committed := false
	defer rollbackSQLiteConn(conn, &committed)
	inserted, err := publishSQLiteRunEventConn(ctx, conn, receipt)
	if err != nil {
		return false, err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return false, err
	}
	committed = true
	return inserted, nil
}

// Authentication receipts and observations can share this transaction-bound
// primitive, so an acknowledged provider delivery cannot disappear between
// receipt persistence and scheduling the executions that depend on it.
func publishSQLiteRunEventConn(ctx context.Context, conn *sql.Conn, receipt *RunEventReceipt) (bool, error) {
	if err := validateRunEventReceipt(receipt); err != nil {
		return false, err
	}
	e := receipt.Event
	var existingDigest string
	err := conn.QueryRowContext(ctx, `SELECT digest FROM run_event_inbox WHERE scope_kind=? AND scope_id=? AND source=? AND event_id=?`, e.Scope.Kind, e.Scope.ID, e.Source, e.ID).Scan(&existingDigest)
	if err == nil {
		if existingDigest != receipt.Digest {
			return false, ErrRunEventConflict
		}
		return false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	payload, err := json.Marshal(receipt)
	if err != nil {
		return false, err
	}
	attributes, err := sqliteRunEventMatchAttributes(e.Attributes)
	if err != nil {
		return false, err
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO run_event_inbox
		(scope_kind,scope_id,source,event_id,type,subject,occurred_at,received_at,digest,attributes,payload)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, e.Scope.Kind, e.Scope.ID, e.Source, e.ID, e.Type, e.Subject, e.OccurredAt.UTC(), receipt.ReceivedAt.UTC(), receipt.Digest, string(attributes), string(payload))
	if err != nil {
		return false, err
	}
	// Fanout is durable, paged work. Provider acknowledgements do not depend
	// on how many executions currently await this observation.
	if err := enqueueSQLiteRunEventNotificationConn(ctx, conn, receipt); err != nil {
		return false, err
	}
	if err := refreshSQLiteRunEventWaitScope(ctx, conn, e.Scope); err != nil {
		return false, err
	}
	return true, nil
}

func (s *SQLiteStore) ClaimRunEventWaits(ctx context.Context, request ClaimRunEventWaitsRequest) ([]*RunEventWait, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	request.Now = request.Now.UTC()
	conn, err := beginImmediateSQLite(ctx, s.db)
	if err != nil {
		return nil, err
	}
	committed := false
	defer rollbackSQLiteConn(conn, &committed)
	if err := drainSQLiteRunEventNotificationPageConn(ctx, conn, request.Scope, request.Now); err != nil {
		return nil, err
	}
	// Each branch reads at most a batch. A long prefix of held leases cannot
	// make acquiring the next batch scan every overdue Run in the tenant.
	rows, err := conn.QueryContext(ctx, `WITH ready AS (
		SELECT payload,available_at,lease_owner,lease_expires_at,run_id,wait_key FROM run_event_waits
		WHERE scope_kind=? AND scope_id=? AND status=? AND available_at<=? AND lease_expires_at IS NULL
		ORDER BY available_at,run_id,wait_key LIMIT ?
	), expired AS (
		SELECT payload,available_at,lease_owner,lease_expires_at,run_id,wait_key FROM run_event_waits
		WHERE scope_kind=? AND scope_id=? AND status=? AND lease_expires_at<=? AND available_at<=?
		ORDER BY lease_expires_at,run_id,wait_key LIMIT ?
	) SELECT payload,available_at,lease_owner,lease_expires_at FROM (
		SELECT * FROM ready UNION ALL SELECT * FROM expired
	) ORDER BY available_at,run_id,wait_key LIMIT ?`,
		request.Scope.Kind, request.Scope.ID, RunEventWaitPending, request.Now, request.Limit,
		request.Scope.Kind, request.Scope.ID, RunEventWaitPending, request.Now, request.Now, request.Limit, request.Limit)
	if err != nil {
		return nil, err
	}
	waits := make([]*RunEventWait, 0, request.Limit)
	for rows.Next() {
		wait, err := scanSQLiteRunEventWait(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		waits = append(waits, wait)
	}
	err = rows.Err()
	if closeErr := rows.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, err
	}
	expires := request.Now.Add(request.LeaseDuration)
	for _, wait := range waits {
		wait.LeaseOwner, wait.LeaseExpiresAt = request.WorkerID, &expires
		wait.AvailableAt = &expires
		if err := saveSQLiteRunEventWait(ctx, conn, wait); err != nil {
			return nil, err
		}
	}
	if err := refreshSQLiteRunEventWaitScope(ctx, conn, request.Scope); err != nil {
		return nil, err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, err
	}
	committed = true
	return waits, nil
}

func (s *SQLiteStore) ProcessRunEventWait(ctx context.Context, request ProcessRunEventWaitRequest) (*RunEventWaitResult, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	request.Now = request.Now.UTC()
	conn, err := beginImmediateSQLite(ctx, s.db)
	if err != nil {
		return nil, err
	}
	committed := false
	defer rollbackSQLiteConn(conn, &committed)
	wait, err := getSQLiteRunEventWait(ctx, conn, request.Scope, request.RunID, request.Key)
	if err != nil {
		return nil, err
	}
	if wait.LeaseOwner != request.WorkerID || wait.LeaseExpiresAt == nil || !wait.LeaseExpiresAt.Equal(request.LeaseExpiresAt) || !wait.LeaseExpiresAt.After(request.Now) || wait.Status != RunEventWaitPending {
		return nil, ErrLeaseLost
	}
	run, err := getSQLiteAgentRun(ctx, conn, request.Scope, request.RunID)
	if err != nil {
		return nil, err
	}
	receipt, err := findSQLiteRunEventReceipt(ctx, conn, wait)
	if err != nil {
		return nil, err
	}
	updated, resolved, activity, err := prepareRunEventWaitResolution(run, wait, receipt, request.Now)
	if err != nil {
		return nil, err
	}
	result := &RunEventWaitResult{Wait: resolved}
	if updated != nil {
		if receipt != nil {
			_, err = conn.ExecContext(ctx, `INSERT INTO run_event_consumptions(scope_kind,scope_id,run_id,source,event_id) VALUES(?,?,?,?,?)`, wait.Scope.Kind, wait.Scope.ID, wait.RunID, receipt.Event.Source, receipt.Event.ID)
			if err != nil {
				return nil, err
			}
			result.Event = &receipt.Event
		}
		if err := updateSQLiteAgentRunConn(ctx, conn, updated, run.Revision); err != nil {
			return nil, err
		}
		persisted, err := insertSQLiteActivityConn(ctx, conn, activity)
		if err != nil {
			return nil, err
		}
		result.Run, result.Activity = updated, persisted
	}
	if err := saveSQLiteRunEventWait(ctx, conn, resolved); err != nil {
		return nil, err
	}
	if err := refreshSQLiteRunEventWaitScope(ctx, conn, request.Scope); err != nil {
		return nil, err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, err
	}
	committed = true
	return result, nil
}

func (s *SQLiteStore) GetRunEventWait(ctx context.Context, scope Scope, runID, key string) (*RunEventWait, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return getSQLiteRunEventWait(ctx, s.db, scope, runID, key)
}

func (s *SQLiteStore) PruneRunEvents(ctx context.Context, scope Scope, now time.Time, limit int) (int, error) {
	return pruneSQLiteRunEventScope(ctx, s.db, scope, now, limit)
}

func findSQLiteRunEventReceipt(ctx context.Context, conn *sql.Conn, wait *RunEventWait) (*RunEventReceipt, error) {
	var payload string
	err := conn.QueryRowContext(ctx, `SELECT e.payload FROM run_event_inbox e JOIN run_event_waits w
		ON w.scope_kind=e.scope_kind AND w.scope_id=e.scope_id AND w.source=e.source AND w.event_type=e.type AND w.subject=e.subject
		WHERE w.scope_kind=? AND w.scope_id=? AND w.run_id=? AND w.wait_key=?
		AND e.occurred_at>=w.after_at AND e.received_at<=w.deadline AND `+sqliteRunEventAttributesMatch+`
		AND NOT EXISTS (SELECT 1 FROM run_event_consumptions c WHERE c.scope_kind=w.scope_kind AND c.scope_id=w.scope_id
		AND c.run_id=w.run_id AND c.source=e.source AND c.event_id=e.event_id)
		ORDER BY e.occurred_at,e.received_at,e.event_id LIMIT 1`, wait.Scope.Kind, wait.Scope.ID, wait.RunID, wait.Spec.Key).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var receipt RunEventReceipt
	decoder := json.NewDecoder(strings.NewReader(payload))
	decoder.UseNumber()
	if err := decoder.Decode(&receipt); err != nil {
		return nil, fmt.Errorf("decode run event receipt: %w", err)
	}
	return &receipt, nil
}

type sqliteRunEventWaitScanner interface {
	Scan(...interface{}) error
}

func scanSQLiteRunEventWait(scanner sqliteRunEventWaitScanner) (*RunEventWait, error) {
	var payload, owner string
	var available, expires sql.NullTime
	if err := scanner.Scan(&payload, &available, &owner, &expires); err != nil {
		return nil, err
	}
	var wait RunEventWait
	decoder := json.NewDecoder(strings.NewReader(payload))
	decoder.UseNumber()
	if err := decoder.Decode(&wait); err != nil {
		return nil, fmt.Errorf("decode run event wait: %w", err)
	}
	wait.AvailableAt, wait.LeaseOwner, wait.LeaseExpiresAt = nil, owner, nil
	if available.Valid {
		wait.AvailableAt = &available.Time
	}
	if expires.Valid {
		wait.LeaseExpiresAt = &expires.Time
	}
	return &wait, nil
}

func getSQLiteRunEventWait(ctx context.Context, queryer sqliteDependencyQueryer, scope Scope, runID, key string) (*RunEventWait, error) {
	wait, err := scanSQLiteRunEventWait(queryer.QueryRowContext(ctx, `SELECT payload,available_at,lease_owner,lease_expires_at FROM run_event_waits
		WHERE scope_kind=? AND scope_id=? AND run_id=? AND wait_key=?`, scope.Kind, scope.ID, runID, key))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrRunEventWaitNotFound
	}
	return wait, err
}

func saveSQLiteRunEventWait(ctx context.Context, conn *sql.Conn, wait *RunEventWait) error {
	payload, err := json.Marshal(wait)
	if err != nil {
		return err
	}
	attributes, err := sqliteRunEventMatchAttributes(wait.Spec.Attributes)
	if err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO run_event_waits
		(scope_kind,scope_id,run_id,wait_key,status,source,event_type,subject,after_at,deadline,attributes,available_at,lease_owner,lease_expires_at,payload)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(scope_kind,scope_id,run_id,wait_key) DO UPDATE SET
		status=excluded.status,available_at=excluded.available_at,lease_owner=excluded.lease_owner,lease_expires_at=excluded.lease_expires_at,payload=excluded.payload`,
		wait.Scope.Kind, wait.Scope.ID, wait.RunID, wait.Spec.Key, wait.Status, wait.Spec.Source, wait.Spec.Type, wait.Spec.Subject,
		wait.Spec.After.UTC(), wait.Spec.Deadline.UTC(), string(attributes), sqliteRunEventNullableTime(wait.AvailableAt), wait.LeaseOwner, sqliteRunEventNullableTime(wait.LeaseExpiresAt), string(payload))
	return err
}

// syncSQLiteRunEventWaitConn runs inside the same transaction as every Run
// write. No observer can see a persisted waiting Run without its expectation.
func syncSQLiteRunEventWaitConn(ctx context.Context, conn *sql.Conn, run *AgentRun) error {
	wait, err := runEventWaitForRun(run)
	if err != nil {
		return err
	}
	if wait != nil {
		existing, err := getSQLiteRunEventWait(ctx, conn, run.Scope, run.ID, wait.Spec.Key)
		if err != nil && !errors.Is(err, ErrRunEventWaitNotFound) {
			return err
		}
		if existing != nil {
			if existing.AssignedAgentID != wait.AssignedAgentID || !sameRunEventWaitSpec(&existing.Spec, &wait.Spec) || (existing.Status != RunEventWaitPending && existing.Status != RunEventWaitPaused) {
				return ErrRunEventConflict
			}
			if existing.Status == RunEventWaitPending && wait.Status == RunEventWaitPending {
				wait.AvailableAt, wait.LeaseOwner, wait.LeaseExpiresAt = existing.AvailableAt, existing.LeaseOwner, existing.LeaseExpiresAt
			}
		}
	}
	// At most one unresolved external wait is active for a Run. Historic waits
	// remain immutable so accidental reuse of a completed key is rejected.
	query := `UPDATE run_event_waits SET status=?,available_at=NULL,lease_owner='',lease_expires_at=NULL,
		payload=json_set(payload,'$.status',?,'$.availableAt',NULL)
		WHERE scope_kind=? AND scope_id=? AND run_id=? AND status IN (?,?)`
	args := []interface{}{RunEventWaitCanceled, RunEventWaitCanceled, run.Scope.Kind, run.Scope.ID, run.ID, RunEventWaitPending, RunEventWaitPaused}
	if wait != nil {
		query += ` AND wait_key<>?`
		args = append(args, wait.Spec.Key)
	}
	if _, err := conn.ExecContext(ctx, query, args...); err != nil {
		return err
	}
	if wait != nil {
		if err := saveSQLiteRunEventWait(ctx, conn, wait); err != nil {
			return err
		}
	}
	return refreshSQLiteRunEventWaitScope(ctx, conn, run.Scope)
}

// Due scope discovery is independent from tenant enumeration and bounded by
// the requested batch. The short durable reservation gives other tenants a
// turn without holding a worker or relying on process-local fairness state.
func (s *SQLiteStore) ListRunEventWaitWorkScopes(ctx context.Context, now time.Time, limit int) ([]Scope, error) {
	if now.IsZero() || limit < 1 || limit > 100 {
		return nil, ErrInvalidRunEventWait
	}
	now = now.UTC()
	conn, err := beginImmediateSQLite(ctx, s.db)
	if err != nil {
		return nil, err
	}
	committed := false
	defer rollbackSQLiteConn(conn, &committed)
	rows, err := conn.QueryContext(ctx, `SELECT scope_kind,scope_id FROM run_event_wait_scopes
		WHERE available_at<=? ORDER BY available_at,scope_kind,scope_id LIMIT ?`, now, limit)
	if err != nil {
		return nil, err
	}
	scopes := make([]Scope, 0, limit)
	for rows.Next() {
		var scope Scope
		if err := rows.Scan(&scope.Kind, &scope.ID); err != nil {
			rows.Close()
			return nil, err
		}
		scopes = append(scopes, scope)
	}
	err = rows.Err()
	if closeErr := rows.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, err
	}
	for _, scope := range scopes {
		if _, err := conn.ExecContext(ctx, `UPDATE run_event_wait_scopes SET available_at=? WHERE scope_kind=? AND scope_id=?`, now.Add(time.Second), scope.Kind, scope.ID); err != nil {
			return nil, err
		}
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, err
	}
	committed = true
	return scopes, nil
}

func refreshSQLiteRunEventWaitScope(ctx context.Context, conn *sql.Conn, scope Scope) error {
	// Claiming happens only when available_at<=now and places lease expiry in
	// the future, so for a held lease the next work time is its expiry. Two
	// partial-index probes avoid scanning every waiting Run in a busy tenant.
	var unleased, leased sql.NullTime
	err := conn.QueryRowContext(ctx, `SELECT available_at FROM run_event_waits
		WHERE scope_kind=? AND scope_id=? AND status=? AND available_at IS NOT NULL AND lease_expires_at IS NULL
		ORDER BY available_at LIMIT 1`, scope.Kind, scope.ID, RunEventWaitPending).Scan(&unleased)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	err = conn.QueryRowContext(ctx, `SELECT lease_expires_at FROM run_event_waits
		WHERE scope_kind=? AND scope_id=? AND status=? AND lease_expires_at IS NOT NULL
		ORDER BY lease_expires_at LIMIT 1`, scope.Kind, scope.ID, RunEventWaitPending).Scan(&leased)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var available *time.Time
	if unleased.Valid {
		available = &unleased.Time
	}
	if leased.Valid && (available == nil || leased.Time.Before(*available)) {
		available = &leased.Time
	}
	var notification sql.NullTime
	err = conn.QueryRowContext(ctx, `SELECT available_at FROM run_event_notifications
		WHERE scope_kind=? AND scope_id=? ORDER BY available_at,source,event_id LIMIT 1`, scope.Kind, scope.ID).Scan(&notification)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if notification.Valid && (available == nil || notification.Time.Before(*available)) {
		available = &notification.Time
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO run_event_wait_scopes(scope_kind,scope_id,available_at) VALUES(?,?,?)
		ON CONFLICT(scope_kind,scope_id) DO UPDATE SET available_at=excluded.available_at`, scope.Kind, scope.ID, available)
	return err
}

var _ RunEventWaitStore = (*SQLiteStore)(nil)

// SQLite's JSON numeric atoms use int64 or float64. Matching metadata encodes
// numbers as exact rationals to preserve all JSON scalar precision; public
// receipts and checkpoints retain their original values in the payload.
func sqliteRunEventMatchAttributes(attributes map[string]interface{}) ([]byte, error) {
	canonical := make(map[string]interface{}, len(attributes))
	for key, value := range attributes {
		switch value := value.(type) {
		case string:
			canonical[key] = []interface{}{"string", value}
		case bool:
			canonical[key] = []interface{}{"boolean", value}
		case nil:
			canonical[key] = []interface{}{"null", nil}
		default:
			encoded, err := json.Marshal(value)
			if err != nil {
				return nil, err
			}
			number, ok := new(big.Rat).SetString(string(encoded))
			if !ok {
				return nil, ErrInvalidRunEventWait
			}
			canonical[key] = []interface{}{"number", number.RatString()}
		}
	}
	return json.Marshal(canonical)
}

func sqliteRunEventNullableTime(value *time.Time) interface{} {
	if value == nil {
		return nil
	}
	return value.UTC()
}
