package runtime

import (
	"context"
	"database/sql"
	"time"
)

const (
	runEventNotificationPageLimit  = 100
	runEventNotificationBatchLimit = 100
)

// runEventNotification is a durable inbox-to-wait delivery cursor. Publishing
// does not walk outstanding executions: it records one notification in the
// same transaction as the authenticated observation. Claiming work visits at
// most 100 notifications and 100 total candidates from their exact
// source/type/subject indexes before leasing runnable waits. Empty notifications
// do not spend the candidate budget, so normal traffic without a recipient does
// not throttle useful replies to one event per worker tick. Cursor progress and
// readiness changes commit together.
//
// The inbox supplies immutable selector facts. Newly armed or resumed waits
// probe that inbox independently, so insertion behind this cursor cannot lose
// a fast reply. Pending notifications may be removed with expired, unprotected
// inbox payloads; payload retention already protects observations that a live
// or paused execution can still consume.
type runEventNotification struct {
	Scope        Scope
	Source       string
	EventID      string
	AfterRunID   string
	AfterWaitKey string
	AvailableAt  time.Time
}

func runEventNotificationForReceipt(receipt *RunEventReceipt) runEventNotification {
	return runEventNotification{
		Scope: receipt.Event.Scope, Source: receipt.Event.Source,
		EventID: receipt.Event.ID, AvailableAt: receipt.ReceivedAt,
	}
}

func runEventWaitAfterNotificationCursor(wait *RunEventWait, notification runEventNotification) bool {
	return wait.RunID > notification.AfterRunID ||
		(wait.RunID == notification.AfterRunID && wait.Spec.Key > notification.AfterWaitKey)
}

func migrateRunEventNotificationsSQLite(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS run_event_notifications (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, source TEXT NOT NULL, event_id TEXT NOT NULL,
			after_run_id TEXT NOT NULL DEFAULT '', after_wait_key TEXT NOT NULL DEFAULT '', available_at DATETIME NOT NULL,
			PRIMARY KEY (scope_kind,scope_id,source,event_id),
			FOREIGN KEY (scope_kind,scope_id,source,event_id)
				REFERENCES run_event_inbox(scope_kind,scope_id,source,event_id) ON DELETE CASCADE
		);
		CREATE INDEX IF NOT EXISTS idx_run_event_notifications_due
			ON run_event_notifications(scope_kind,scope_id,available_at,source,event_id);
		CREATE INDEX IF NOT EXISTS idx_run_event_waits_notification_page
			ON run_event_waits(scope_kind,scope_id,source,event_type,subject,run_id,wait_key) WHERE status='pending';
	`)
	return err
}

func (s *PostgresStore) migrateRunEventNotificationsPostgres(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS `+s.table("run_event_notifications")+` (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, source TEXT NOT NULL, event_id TEXT NOT NULL,
			after_run_id TEXT NOT NULL DEFAULT '', after_wait_key TEXT NOT NULL DEFAULT '', available_at TIMESTAMPTZ NOT NULL,
			PRIMARY KEY (scope_kind,scope_id,source,event_id),
			FOREIGN KEY (scope_kind,scope_id,source,event_id)
				REFERENCES `+s.table("run_event_inbox")+`(scope_kind,scope_id,source,event_id) ON DELETE CASCADE
		);
		CREATE INDEX IF NOT EXISTS run_event_notifications_due_idx
			ON `+s.table("run_event_notifications")+`(scope_kind,scope_id,available_at,source,event_id);
		CREATE INDEX IF NOT EXISTS run_event_waits_notification_idx
			ON `+s.table("run_event_waits")+`(scope_kind,scope_id,source,event_type,subject,run_id,wait_key) WHERE status='pending';
		INSERT INTO `+s.table("schema_migrations")+`(version,name)
			VALUES(51,'bounded run event notification fanout') ON CONFLICT(version) DO NOTHING;
	`)
	return err
}
