package runtime

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// processPostgresRunEventNotificationTx advances a bounded descriptor batch
// with one shared wait-candidate budget, so empty traffic and large fanouts
// both yield without making a claim transaction grow with tenant workload.
// Only the notification row may be skipped when another replica owns it. Wait
// rows must be locked before advancing the cursor, otherwise a busy processor's
// no-match snapshot could lose the concurrent publication notification.
func (s *PostgresStore) processPostgresRunEventNotificationTx(ctx context.Context, tx *sql.Tx, scope Scope, now time.Time) error {
	remaining := runEventNotificationPageLimit
	for descriptors := 0; descriptors < runEventNotificationBatchLimit && remaining > 0; descriptors++ {
		inspected, found, err := s.processPostgresRunEventNotificationPageTx(ctx, tx, scope, now, remaining)
		if err != nil || !found {
			return err
		}
		remaining -= inspected
	}
	return nil
}

func (s *PostgresStore) processPostgresRunEventNotificationPageTx(ctx context.Context, tx *sql.Tx, scope Scope, now time.Time, limit int) (int, bool, error) {
	var notification runEventNotification
	var payload []byte
	notification.Scope = scope
	err := tx.QueryRowContext(ctx, `SELECT notices.source,notices.event_id,notices.after_run_id,notices.after_wait_key,
		notices.available_at,inbox.payload FROM `+s.table("run_event_notifications")+` AS notices
		JOIN `+s.table("run_event_inbox")+` AS inbox ON inbox.scope_kind=notices.scope_kind AND inbox.scope_id=notices.scope_id
		AND inbox.source=notices.source AND inbox.event_id=notices.event_id
		WHERE notices.scope_kind=$1 AND notices.scope_id=$2 AND notices.available_at <= $3
		ORDER BY notices.available_at,notices.source,notices.event_id LIMIT 1 FOR UPDATE OF notices SKIP LOCKED`,
		scope.Kind, scope.ID, now).Scan(&notification.Source, &notification.EventID, &notification.AfterRunID,
		&notification.AfterWaitKey, &notification.AvailableAt, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	var receipt RunEventReceipt
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	if err := decoder.Decode(&receipt); err != nil {
		return 0, false, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT payload,status,available_at,lease_owner,lease_expires_at
		FROM `+s.table("run_event_waits")+` WHERE scope_kind=$1 AND scope_id=$2 AND source=$3 AND event_type=$4 AND subject=$5
		AND status='pending' AND (run_id,wait_key)>($6,$7) ORDER BY run_id,wait_key LIMIT $8 FOR UPDATE`,
		scope.Kind, scope.ID, receipt.Event.Source, receipt.Event.Type, receipt.Event.Subject,
		notification.AfterRunID, notification.AfterWaitKey, limit)
	if err != nil {
		return 0, false, err
	}
	page := make([]*RunEventWait, 0, limit)
	for rows.Next() {
		wait, err := scanPostgresRunEventWait(rows)
		if err != nil {
			_ = rows.Close()
			return 0, false, err
		}
		page = append(page, wait)
	}
	if err := rows.Close(); err != nil {
		return 0, false, err
	}
	if err := rows.Err(); err != nil {
		return 0, false, err
	}
	matching := make([]map[string]string, 0, len(page))
	for _, wait := range page {
		if !runEventWaitMatches(wait, &receipt) {
			continue
		}
		matching = append(matching, map[string]string{"run_id": wait.RunID, "wait_key": wait.Spec.Key})
	}
	if len(matching) > 0 {
		keys, err := json.Marshal(matching)
		if err != nil {
			return 0, false, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE `+s.table("run_event_waits")+` AS waits
			SET available_at=LEAST(COALESCE(waits.available_at,$1),$1)
			FROM jsonb_to_recordset($4::jsonb) AS matching(run_id TEXT,wait_key TEXT)
			WHERE waits.scope_kind=$2 AND waits.scope_id=$3 AND waits.run_id=matching.run_id AND waits.wait_key=matching.wait_key
			AND waits.status='pending'`, receipt.ReceivedAt, scope.Kind, scope.ID, string(keys)); err != nil {
			return 0, false, err
		}
	}
	if len(page) < limit {
		_, err := tx.ExecContext(ctx, `DELETE FROM `+s.table("run_event_notifications")+`
			WHERE scope_kind=$1 AND scope_id=$2 AND source=$3 AND event_id=$4`, scope.Kind, scope.ID,
			notification.Source, notification.EventID)
		return len(page), true, err
	}
	last := page[len(page)-1]
	_, err = tx.ExecContext(ctx, `UPDATE `+s.table("run_event_notifications")+`
		SET after_run_id=$1,after_wait_key=$2,available_at=$3 WHERE scope_kind=$4 AND scope_id=$5 AND source=$6 AND event_id=$7`,
		last.RunID, last.Spec.Key, now, scope.Kind, scope.ID, notification.Source, notification.EventID)
	return len(page), true, err
}
