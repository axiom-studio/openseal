package runtime

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/lib/pq"
)

const workSignalMigrationVersion int64 = 63

// workSignalPingInterval bounds how long a silently dead listener connection
// can look healthy. It costs one empty query per process, not per scope.
const workSignalPingInterval = 30 * time.Second

var _ WorkSignalSource = (*PostgresStore)(nil)
var _ ActionDeadlineStore = (*PostgresStore)(nil)
var _ AgentRunDeadlineStore = (*PostgresStore)(nil)
var _ ExternalConversationDeadlineStore = (*PostgresStore)(nil)

// workSignalTrigger notifies one family for rows of one table. Updates only
// notify when a claimability column changes, so ordinary payload writes and
// lease renewals stay silent; NOTIFY takes a commit-time global lock.
type workSignalTrigger struct {
	table  string
	name   string
	family WorkFamily
	event  string
	when   string
}

// leaseClaimabilityChanged fires when a leased work row becomes claimable
// sooner: a new status, a moved availability, a released or shortened lease.
const leaseClaimabilityChanged = `OLD.status IS DISTINCT FROM NEW.status OR OLD.available_at IS DISTINCT FROM NEW.available_at
	OR OLD.lease_owner IS DISTINCT FROM NEW.lease_owner
	OR (OLD.lease_expires_at IS NOT NULL AND (NEW.lease_expires_at IS NULL OR NEW.lease_expires_at < OLD.lease_expires_at))`

const statusChanged = `OLD.status IS DISTINCT FROM NEW.status`

func workSignalTriggers() []workSignalTrigger {
	triggers := make([]workSignalTrigger, 0, 32)
	add := func(table string, family WorkFamily, insertWhen, updateWhen string) {
		triggers = append(triggers, workSignalTrigger{table: table, name: "work_signal_insert", family: family, event: "INSERT", when: insertWhen})
		if updateWhen != "" {
			triggers = append(triggers, workSignalTrigger{table: table, name: "work_signal_update", family: family, event: "UPDATE", when: updateWhen})
		}
	}
	add("action_calls", WorkFamilyAction, "", leaseClaimabilityChanged)
	add("agent_runs", WorkFamilyRun, "", leaseClaimabilityChanged)
	// A paused Run hides the ActionCall it waits on from action claims.
	triggers = append(triggers, workSignalTrigger{table: "agent_runs", name: "work_signal_unpause", family: WorkFamilyAction, event: "UPDATE",
		when: `OLD.status = 'paused' AND NEW.status IS DISTINCT FROM OLD.status`})
	add("agent_requests", WorkFamilyRun, "", statusChanged)
	add("skill_runtime_maintenance", WorkFamilyMaintenance, "", `OLD.active IS DISTINCT FROM NEW.active`)
	add("approval_checkpoints", WorkFamilyApproval, "", statusChanged+` OR OLD.expires_at IS DISTINCT FROM NEW.expires_at`)
	add("conversations", WorkFamilyConversation, "", statusChanged)
	add("channel_messages", WorkFamilyConversation, "", "")
	add("skill_setup_requests", WorkFamilyConversation, "", statusChanged)
	add("credential_requests", WorkFamilyConversation, "", statusChanged)
	add("external_conversation_inbox", WorkFamilyExternal, "", leaseClaimabilityChanged)
	add("external_conversation_deliveries", WorkFamilyExternal, "", leaseClaimabilityChanged)
	add("external_conversation_endpoints", WorkFamilyExternal, "", statusChanged+` OR OLD.revision IS DISTINCT FROM NEW.revision`)
	add("callback_events", WorkFamilyExternal, "", leaseClaimabilityChanged)
	add("run_activity", WorkFamilyActivity, `NEW.event_type = 'turn.commentary'`, "")
	return triggers
}

// WorkSignalChannel is the LISTEN channel for this store's schema. Distinct
// schemas in one database never wake each other's workers.
func (s *PostgresStore) WorkSignalChannel() string {
	channel := "openseal_work:" + s.schema
	// PostgreSQL channel names are identifiers limited to 63 bytes.
	if len(channel) > 63 {
		sum := sha256.Sum256([]byte(s.schema))
		channel = "openseal_work:" + hex.EncodeToString(sum[:])[:40]
	}
	return channel
}

func (s *PostgresStore) migrateWorkSignals(ctx context.Context, tx *sql.Tx) error {
	applied, err := s.postgresMigrationApplied(ctx, tx, workSignalMigrationVersion)
	if err != nil || applied {
		return err
	}
	statements := []string{
		`CREATE OR REPLACE FUNCTION ` + s.table("notify_work_signal") + `() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
		PERFORM pg_notify(TG_ARGV[0], TG_ARGV[1] || ':' || NEW.scope_kind || ':' || NEW.scope_id);
		RETURN NULL;
		END $$`,
		// Reply retries live in the payload; keep their deadline lookup to the
		// small set of unresolved applied items.
		`CREATE INDEX IF NOT EXISTS external_conversation_inbox_reply_due_idx ON ` + s.table("external_conversation_inbox") +
			`(scope_kind,scope_id) WHERE status='applied' AND payload->>'replyState' IS NULL AND payload->>'replyAvailableAt' IS NOT NULL`,
	}
	channel := pq.QuoteLiteral(s.WorkSignalChannel())
	for _, trigger := range workSignalTriggers() {
		when := ""
		if trigger.when != "" {
			when = ` WHEN (` + trigger.when + `)`
		}
		statements = append(statements, `DROP TRIGGER IF EXISTS `+trigger.name+` ON `+s.table(trigger.table)+`;
			CREATE TRIGGER `+trigger.name+` AFTER `+trigger.event+` ON `+s.table(trigger.table)+` FOR EACH ROW`+when+`
			EXECUTE FUNCTION `+s.table("notify_work_signal")+`(`+channel+`, `+pq.QuoteLiteral(string(trigger.family))+`)`)
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+`(version,name) VALUES($1,'work signal notifications') ON CONFLICT(version) DO NOTHING`, workSignalMigrationVersion)
	return err
}

func (s *PostgresStore) rollbackWorkSignals(ctx context.Context, tx *sql.Tx) error {
	for _, trigger := range workSignalTriggers() {
		if _, err := tx.ExecContext(ctx, `DROP TRIGGER IF EXISTS `+trigger.name+` ON `+s.table(trigger.table)); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `DROP INDEX IF EXISTS `+s.table("external_conversation_inbox_reply_due_idx")+`;
		DROP FUNCTION IF EXISTS `+s.table("notify_work_signal")+`()`)
	return err
}

// parseWorkSignal decodes "<family>:<scope kind>:<scope id>".
func parseWorkSignal(payload string) (WorkSignal, bool) {
	parts := strings.SplitN(payload, ":", 3)
	if len(parts) != 3 {
		return WorkSignal{}, false
	}
	signal := WorkSignal{Kind: WorkSignalChanged, Family: WorkFamily(parts[0]), Scope: Scope{Kind: parts[1], ID: parts[2]}}
	if signal.Family == "" || signal.Scope.Validate() != nil {
		return WorkSignal{}, false
	}
	return signal, true
}

// ListenForWork holds a dedicated LISTEN connection until ctx ends and calls
// handle serially. WorkSignalConnected is reported once LISTEN is active and
// after every reconnect; WorkSignalDisconnected when the connection is lost.
func (s *PostgresStore) ListenForWork(ctx context.Context, handle func(WorkSignal)) error {
	if s == nil || strings.TrimSpace(s.dsn) == "" {
		return errors.New("PostgreSQL work signals require the store DSN")
	}
	if handle == nil {
		return errors.New("work signal handler is required")
	}
	events := make(chan pq.ListenerEventType, 16)
	listener := pq.NewListener(s.dsn, 100*time.Millisecond, 10*time.Second, func(event pq.ListenerEventType, _ error) {
		select {
		case events <- event:
		case <-ctx.Done():
		}
	})
	defer listener.Close()
	listening := make(chan error, 1)
	go func() { listening <- listener.Listen(s.WorkSignalChannel()) }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-listening:
		if err != nil {
			return err
		}
	}
	// Listen returns once LISTEN is active on a live connection. The initial
	// connected event fired before that, so report health from here.
	healthy := true
	handle(WorkSignal{Kind: WorkSignalConnected})
	ping := time.NewTicker(workSignalPingInterval)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event := <-events:
			switch event {
			case pq.ListenerEventDisconnected:
				if healthy {
					healthy = false
					handle(WorkSignal{Kind: WorkSignalDisconnected})
				}
			case pq.ListenerEventReconnected:
				// pq re-issues LISTEN before reporting the reconnect.
				healthy = true
				handle(WorkSignal{Kind: WorkSignalConnected})
			}
		case notification, ok := <-listener.Notify:
			if !ok {
				return errors.New("PostgreSQL work signal listener closed")
			}
			if notification == nil {
				continue // pq's reconnect marker; the reconnect event catches up.
			}
			if signal, valid := parseWorkSignal(notification.Extra); valid {
				handle(signal)
			}
		case <-ping.C:
			if err := listener.Ping(); err != nil && healthy {
				healthy = false
				handle(WorkSignal{Kind: WorkSignalDisconnected})
			}
		}
	}
}

func scanDeadline(row *sql.Row) (*time.Time, error) {
	var deadline sql.NullTime
	if err := row.Scan(&deadline); err != nil {
		return nil, err
	}
	if !deadline.Valid {
		return nil, nil
	}
	return new(deadline.Time.UTC()), nil
}

// NextActionDeadline returns the earliest future retry or lease expiry among
// the scope's claimable ActionCalls. Work that is claimable now is excluded:
// claims find it, and anything gated on another row is signalled when that
// row changes.
func (s *PostgresStore) NextActionDeadline(ctx context.Context, scope Scope, now time.Time) (*time.Time, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return scanDeadline(s.db.QueryRowContext(ctx, `SELECT LEAST(
		(SELECT MIN(available_at) FROM `+s.table("action_calls")+` WHERE scope_kind=$1 AND scope_id=$2 AND status=$3 AND available_at > $5),
		(SELECT MIN(lease_expires_at) FROM `+s.table("action_calls")+` WHERE scope_kind=$1 AND scope_id=$2 AND status=$4 AND lease_expires_at > $5))`,
		scope.Kind, scope.ID, ActionCallStatusReady, ActionCallStatusRunning, now.UTC()))
}

// NextAgentRunDeadline returns the earliest future availability or lease
// expiry among queued and running Runs that one pool could claim.
func (s *PostgresStore) NextAgentRunDeadline(ctx context.Context, scope Scope, kind RunKind, assignedAgentID string, now time.Time) (*time.Time, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	filter := ` AND ($4::text = '' OR COALESCE(payload->>'kind', 'agent_work') = $4::text) AND ($5::text = '' OR assigned_agent_id = $5::text)`
	return scanDeadline(s.db.QueryRowContext(ctx, `SELECT LEAST(
		(SELECT MIN(available_at) FROM `+s.table("agent_runs")+` WHERE scope_kind=$1 AND scope_id=$2 AND status=$6 AND available_at > $3`+filter+`),
		(SELECT MIN(lease_expires_at) FROM `+s.table("agent_runs")+` WHERE scope_kind=$1 AND scope_id=$2 AND status IN ($6, $7) AND lease_expires_at > $3`+filter+`))`,
		scope.Kind, scope.ID, now.UTC(), string(kind), assignedAgentID, AgentRunStatusQueued, AgentRunStatusRunning))
}

// NextExternalConversationDeadline returns the earliest future time at which
// external conversation work in the scope becomes due without another write.
func (s *PostgresStore) NextExternalConversationDeadline(ctx context.Context, scope Scope, now time.Time) (*time.Time, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	inbox, deliveries, callbacks := s.table("external_conversation_inbox"), s.table("external_conversation_deliveries"), s.table("callback_events")
	return scanDeadline(s.db.QueryRowContext(ctx, `SELECT LEAST(
		(SELECT MIN(available_at) FROM `+inbox+` WHERE scope_kind=$1 AND scope_id=$2 AND status=ANY($4) AND available_at > $3),
		(SELECT MIN(lease_expires_at) FROM `+inbox+` WHERE scope_kind=$1 AND scope_id=$2 AND status=$5 AND lease_expires_at > $3),
		(SELECT MIN((payload->>'replyAvailableAt')::timestamptz) FROM `+inbox+` WHERE scope_kind=$1 AND scope_id=$2 AND status='applied'
			AND payload->>'replyState' IS NULL AND payload->>'replyAvailableAt' IS NOT NULL AND (payload->>'replyAvailableAt')::timestamptz > $3),
		(SELECT MIN(available_at) FROM `+deliveries+` WHERE scope_kind=$1 AND scope_id=$2 AND status=ANY($6) AND available_at > $3),
		(SELECT MIN(lease_expires_at) FROM `+deliveries+` WHERE scope_kind=$1 AND scope_id=$2 AND status=$7 AND lease_expires_at > $3),
		(SELECT MIN(available_at) FROM `+callbacks+` WHERE scope_kind=$1 AND scope_id=$2 AND status=$8 AND available_at > $3),
		(SELECT MIN(lease_expires_at) FROM `+callbacks+` WHERE scope_kind=$1 AND scope_id=$2 AND status=$9 AND lease_expires_at > $3),
		(SELECT MIN(expires_at) FROM `+s.table("approval_checkpoints")+` WHERE scope_kind=$1 AND scope_id=$2 AND status=$10 AND expires_at > $3))`,
		scope.Kind, scope.ID, now.UTC(),
		pq.Array([]string{string(ExternalConversationInboxPending), string(ExternalConversationInboxRetry)}), ExternalConversationInboxLeased,
		pq.Array([]string{string(ExternalConversationDeliveryPending), string(ExternalConversationDeliveryRetry)}), ExternalConversationDeliveryLeased,
		CallbackEventPending, CallbackEventLeased, ApprovalStatusPending))
}
