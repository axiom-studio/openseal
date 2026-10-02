package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

func enqueueSQLiteRunEventNotificationConn(ctx context.Context, conn *sql.Conn, receipt *RunEventReceipt) error {
	notification := runEventNotificationForReceipt(receipt)
	_, err := conn.ExecContext(ctx, `INSERT INTO run_event_notifications(scope_kind,scope_id,source,event_id,available_at)
		VALUES(?,?,?,?,?)`, notification.Scope.Kind, notification.Scope.ID, notification.Source, notification.EventID, notification.AvailableAt.UTC())
	return err
}

// One acquisition visit drains a bounded descriptor batch with a shared wait
// candidate budget. Empty notifications cannot indefinitely delay real work,
// and a burst of fanout cannot multiply the per-claim row bound.
func drainSQLiteRunEventNotificationPageConn(ctx context.Context, conn *sql.Conn, scope Scope, now time.Time) error {
	remaining := runEventNotificationPageLimit
	for descriptors := 0; descriptors < runEventNotificationBatchLimit && remaining > 0; descriptors++ {
		candidates, processed, err := drainSQLiteRunEventNotificationConn(ctx, conn, scope, now, remaining)
		if err != nil {
			return err
		}
		if !processed {
			break
		}
		remaining -= candidates
	}
	return nil
}

func drainSQLiteRunEventNotificationConn(ctx context.Context, conn *sql.Conn, scope Scope, now time.Time, candidateLimit int) (int, bool, error) {
	var notification runEventNotification
	notification.Scope = scope
	err := conn.QueryRowContext(ctx, `SELECT source,event_id,after_run_id,after_wait_key,available_at FROM run_event_notifications
		WHERE scope_kind=? AND scope_id=? AND available_at<=? ORDER BY available_at,source,event_id LIMIT 1`,
		scope.Kind, scope.ID, now.UTC()).Scan(&notification.Source, &notification.EventID, &notification.AfterRunID, &notification.AfterWaitKey, &notification.AvailableAt)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	var payload string
	err = conn.QueryRowContext(ctx, `SELECT payload FROM run_event_inbox WHERE scope_kind=? AND scope_id=? AND source=? AND event_id=?`,
		scope.Kind, scope.ID, notification.Source, notification.EventID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		// SQLite may run without FK enforcement. A bounded orphan removal is
		// harmless and prevents an old maintenance record keeping a scope due.
		return 0, true, deleteSQLiteRunEventNotificationConn(ctx, conn, notification)
	}
	if err != nil {
		return 0, true, err
	}
	var receipt RunEventReceipt
	decoder := json.NewDecoder(strings.NewReader(payload))
	decoder.UseNumber()
	if err := decoder.Decode(&receipt); err != nil {
		return 0, true, err
	}
	if err := validateRunEventReceipt(&receipt); err != nil {
		return 0, true, err
	}
	// Correlation is evaluated after the bounded, indexed selector page. A
	// large prefix of other senders cannot turn a page into a full-table scan.
	rows, err := conn.QueryContext(ctx, `SELECT payload,available_at,lease_owner,lease_expires_at FROM run_event_waits
		WHERE scope_kind=? AND scope_id=? AND source=? AND event_type=? AND subject=? AND status='pending'
		AND (run_id,wait_key)>(?,?) ORDER BY run_id,wait_key LIMIT ?`, scope.Kind, scope.ID,
		receipt.Event.Source, receipt.Event.Type, receipt.Event.Subject, notification.AfterRunID, notification.AfterWaitKey, candidateLimit)
	if err != nil {
		return 0, true, err
	}
	waits := make([]*RunEventWait, 0, candidateLimit)
	for rows.Next() {
		wait, err := scanSQLiteRunEventWait(rows)
		if err != nil {
			_ = rows.Close()
			return 0, true, err
		}
		waits = append(waits, wait)
	}
	err = rows.Err()
	if closeErr := rows.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return 0, true, err
	}
	for _, wait := range waits {
		if !runEventWaitMatches(wait, &receipt) {
			continue
		}
		var consumed bool
		if err := conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM run_event_consumptions
			WHERE scope_kind=? AND scope_id=? AND run_id=? AND source=? AND event_id=?)`, scope.Kind, scope.ID,
			wait.RunID, receipt.Event.Source, receipt.Event.ID).Scan(&consumed); err != nil {
			return 0, true, err
		}
		if consumed {
			continue
		}
		// Existing leases are retained, including when a ready timestamp is
		// lowered. Only their owner can finish the authoritative Run update.
		if _, err := conn.ExecContext(ctx, `UPDATE run_event_waits SET available_at=CASE
			WHEN available_at IS NULL OR available_at>? THEN ? ELSE available_at END
			WHERE scope_kind=? AND scope_id=? AND run_id=? AND wait_key=? AND status='pending'`,
			now.UTC(), now.UTC(), scope.Kind, scope.ID, wait.RunID, wait.Spec.Key); err != nil {
			return 0, true, err
		}
	}
	if len(waits) < candidateLimit {
		return len(waits), true, deleteSQLiteRunEventNotificationConn(ctx, conn, notification)
	}
	last := waits[len(waits)-1]
	_, err = conn.ExecContext(ctx, `UPDATE run_event_notifications SET after_run_id=?,after_wait_key=?,available_at=?
		WHERE scope_kind=? AND scope_id=? AND source=? AND event_id=?`, last.RunID, last.Spec.Key, now.UTC(),
		scope.Kind, scope.ID, notification.Source, notification.EventID)
	return len(waits), true, err
}

func deleteSQLiteRunEventNotificationConn(ctx context.Context, conn *sql.Conn, notification runEventNotification) error {
	_, err := conn.ExecContext(ctx, `DELETE FROM run_event_notifications WHERE scope_kind=? AND scope_id=? AND source=? AND event_id=?`,
		notification.Scope.Kind, notification.Scope.ID, notification.Source, notification.EventID)
	return err
}
