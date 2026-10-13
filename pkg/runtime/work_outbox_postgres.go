package runtime

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"strings"
	"time"

	"github.com/lib/pq"
)

const (
	// workSignalMigrationVersion added the reply deadline index; its NOTIFY
	// triggers are superseded by the work outbox.
	workSignalMigrationVersion int64 = 63
	workOutboxMigrationVersion int64 = 64
)

var _ WorkOutboxStore = (*PostgresStore)(nil)
var _ ActionDeadlineStore = (*PostgresStore)(nil)
var _ AgentRunDeadlineStore = (*PostgresStore)(nil)
var _ ExternalConversationDeadlineStore = (*PostgresStore)(nil)

// WorkOutboxStore is a store whose committed work changes are recorded in a
// transactional outbox for relay into a WorkQueue.
type WorkOutboxStore interface {
	// WorkCommitted receives after this process commits a write that may
	// have added outbox rows. It is a coalescing hint, never closed.
	WorkCommitted() <-chan struct{}
	// RelayWorkOutbox removes up to limit outbox rows, from every process,
	// and hands them to publish in the same transaction: rows are only
	// removed once publish succeeds. It returns how many rows it removed.
	RelayWorkOutbox(ctx context.Context, limit int, publish func(context.Context, []WorkItem) error) (int, error)
	// ListPendingWork returns one item per family and scope with durable
	// work that may need a pass, at its earliest deadline. Recent marks a
	// rebuild after the queue lost its schedule: it adds scopes with recent
	// conversation activity, which the claimable tables do not cover.
	ListPendingWork(ctx context.Context, rebuild bool) ([]WorkItem, error)
}

// workOutboxTrigger records one family for rows of one table. Updates only
// record when a claimability column changes, so ordinary payload writes and
// lease renewals add nothing.
type workOutboxTrigger struct {
	table string
	name  string
	event string
	when  string
	// rows is the VALUES list of (family, due, lease) rows to record. A
	// lease row only records while its due time is known.
	rows []string
}

// leaseClaimabilityChanged fires when a leased work row becomes claimable
// sooner: a new status, a moved availability, a released or shortened lease.
const leaseClaimabilityChanged = `OLD.status IS DISTINCT FROM NEW.status OR OLD.available_at IS DISTINCT FROM NEW.available_at
	OR OLD.lease_owner IS DISTINCT FROM NEW.lease_owner
	OR (OLD.lease_expires_at IS NOT NULL AND (NEW.lease_expires_at IS NULL OR NEW.lease_expires_at < OLD.lease_expires_at))`

const statusChanged = `OLD.status IS DISTINCT FROM NEW.status`

// leasedDue is when a leased row is next claimable: its lease expiry while
// leased, its availability otherwise.
func leasedDue(leased string) string {
	return `CASE WHEN NEW.status = '` + leased + `' THEN NEW.lease_expires_at ELSE NEW.available_at END`
}

func workOutboxTriggers() []workOutboxTrigger {
	triggers := make([]workOutboxTrigger, 0, 32)
	add := func(table, insertWhen, updateWhen string, rows ...string) {
		triggers = append(triggers, workOutboxTrigger{table: table, name: "work_outbox_insert", event: "INSERT", when: insertWhen, rows: rows})
		if updateWhen != "" {
			triggers = append(triggers, workOutboxTrigger{table: table, name: "work_outbox_update", event: "UPDATE", when: updateWhen, rows: rows})
		}
	}
	// A claim schedules only its lease expiry: the claimer already works.
	add("action_calls", `NEW.status IN ('ready','running')`, `NEW.status IN ('ready','running') AND (`+leaseClaimabilityChanged+`)`,
		`('action', `+leasedDue("running")+`, false)`)
	// Workforce authoring Runs are claimed by the authoring workers, not by
	// Run pools.
	runFamily := func(family string) string {
		return `CASE WHEN NEW.payload->>'kind' = 'workforce_authoring' THEN 'authoring' ELSE '` + family + `' END`
	}
	add("agent_runs", "", leaseClaimabilityChanged,
		`(`+runFamily("run")+`, CASE WHEN NEW.status = 'running' THEN NULL ELSE NEW.available_at END, false)`,
		`(`+runFamily("run-due")+`, CASE WHEN NEW.status = 'running' THEN NEW.lease_expires_at END, true)`)
	// A paused Run hides the ActionCall it waits on from action claims.
	triggers = append(triggers, workOutboxTrigger{table: "agent_runs", name: "work_outbox_unpause", event: "UPDATE",
		when: `OLD.status = 'paused' AND NEW.status IS DISTINCT FROM OLD.status`, rows: []string{`('action', NULL, false)`}})
	add("agent_requests", "", statusChanged, `('run', NULL, false)`)
	add("skill_runtime_maintenance", "", `OLD.active IS DISTINCT FROM NEW.active`, `('maintenance', NULL, false)`)
	add("approval_checkpoints", "", statusChanged+` OR OLD.expires_at IS DISTINCT FROM NEW.expires_at`, `('approval', NULL, false)`)
	add("conversations", "", statusChanged, `('conversation', NULL, false)`)
	add("channel_messages", "", "", `('conversation', NULL, false)`)
	add("skill_setup_requests", "", statusChanged, `('conversation', NULL, false)`)
	add("credential_requests", "", statusChanged, `('conversation', NULL, false)`)
	add("external_conversation_inbox", "", leaseClaimabilityChanged, `('external', `+leasedDue("leased")+`, false)`)
	add("external_conversation_deliveries", "", leaseClaimabilityChanged, `('external', `+leasedDue("leased")+`, false)`)
	add("external_conversation_endpoints", "", statusChanged+` OR OLD.revision IS DISTINCT FROM NEW.revision`, `('external', NULL, false)`)
	add("callback_events", "", leaseClaimabilityChanged, `('external', `+leasedDue("leased")+`, false)`)
	add("run_activity", `NEW.event_type = 'turn.commentary'`, "", `('activity', NULL, false)`)
	add("workforce_change_sets", "", statusChanged, `('authoring', NULL, false)`)
	return triggers
}

func (t workOutboxTrigger) function() string {
	return "work_outbox_" + t.table + "_" + strings.TrimPrefix(t.name, "work_outbox_")
}

// workSignalTriggerNames are migration 63's NOTIFY triggers.
var workSignalTriggerNames = []string{"work_signal_insert", "work_signal_update", "work_signal_unpause"}

func (s *PostgresStore) migrateWorkSignals(ctx context.Context, tx *sql.Tx) error {
	applied, err := s.postgresMigrationApplied(ctx, tx, workSignalMigrationVersion)
	if err != nil || applied {
		return err
	}
	// Reply retries live in the payload; keep their deadline lookup to the
	// small set of unresolved applied items.
	if _, err := tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS external_conversation_inbox_reply_due_idx ON `+s.table("external_conversation_inbox")+
		`(scope_kind,scope_id) WHERE status='applied' AND payload->>'replyState' IS NULL AND payload->>'replyAvailableAt' IS NOT NULL`); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+`(version,name) VALUES($1,'work signal deadlines') ON CONFLICT(version) DO NOTHING`, workSignalMigrationVersion)
	return err
}

func (s *PostgresStore) rollbackWorkSignals(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `DROP INDEX IF EXISTS `+s.table("external_conversation_inbox_reply_due_idx"))
	return err
}

// workTables are the tables workOutboxTriggers watch.
func workTables() []string {
	seen := map[string]bool{}
	tables := []string{}
	for _, trigger := range workOutboxTriggers() {
		if !seen[trigger.table] {
			seen[trigger.table] = true
			tables = append(tables, trigger.table)
		}
	}
	return tables
}

func (s *PostgresStore) migrateWorkOutbox(ctx context.Context, tx *sql.Tx) error {
	applied, err := s.postgresMigrationApplied(ctx, tx, workOutboxMigrationVersion)
	if err != nil || applied {
		return err
	}
	outbox := s.table("work_outbox")
	statements := []string{
		// A queue table churns constantly; vacuum it by row count, not size.
		`CREATE TABLE IF NOT EXISTS ` + outbox + ` (id BIGSERIAL PRIMARY KEY, family TEXT NOT NULL,
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, due_at TIMESTAMPTZ NOT NULL)
			WITH (autovacuum_vacuum_scale_factor = 0, autovacuum_vacuum_threshold = 1000, autovacuum_vacuum_cost_delay = 0)`,
		// Small partial indexes keep the pending work sweep to active rows.
		`CREATE INDEX IF NOT EXISTS action_calls_pending_work_idx ON ` + s.table("action_calls") + `(scope_kind,scope_id) WHERE status IN ('ready','running')`,
		`CREATE INDEX IF NOT EXISTS agent_runs_pending_work_idx ON ` + s.table("agent_runs") + `(scope_kind,scope_id) WHERE status IN ('queued','running','sleeping')`,
		`CREATE INDEX IF NOT EXISTS agent_requests_pending_work_idx ON ` + s.table("agent_requests") + `(scope_kind,scope_id) WHERE status IN ('pending','accepted','clarification_requested','completion_review')`,
		`CREATE INDEX IF NOT EXISTS approval_checkpoints_pending_work_idx ON ` + s.table("approval_checkpoints") + `(scope_kind,scope_id) WHERE status = 'pending'`,
		`CREATE INDEX IF NOT EXISTS external_conversation_inbox_pending_work_idx ON ` + s.table("external_conversation_inbox") + `(scope_kind,scope_id) WHERE status IN ('pending','retry','leased')`,
		`CREATE INDEX IF NOT EXISTS external_conversation_deliveries_pending_work_idx ON ` + s.table("external_conversation_deliveries") + `(scope_kind,scope_id) WHERE status IN ('pending','retry','leased')`,
		`CREATE INDEX IF NOT EXISTS callback_events_pending_work_idx ON ` + s.table("callback_events") + `(scope_kind,scope_id) WHERE status IN ('pending','leased')`,
		`CREATE INDEX IF NOT EXISTS workforce_change_sets_pending_work_idx ON ` + s.table("workforce_change_sets") + `(scope_kind,scope_id) WHERE status IN ('evaluating','review')`,
	}
	for _, table := range workTables() {
		for _, name := range workSignalTriggerNames {
			statements = append(statements, `DROP TRIGGER IF EXISTS `+name+` ON `+s.table(table))
		}
	}
	statements = append(statements, `DROP FUNCTION IF EXISTS `+s.table("notify_work_signal")+`()`)
	for _, trigger := range workOutboxTriggers() {
		when := ""
		if trigger.when != "" {
			when = ` WHEN (` + trigger.when + `)`
		}
		// Rows without a deadline are due at commit; past deadlines are due now.
		statements = append(statements, `CREATE OR REPLACE FUNCTION `+s.table(trigger.function())+`() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN
			INSERT INTO `+outbox+`(family, scope_kind, scope_id, due_at)
			SELECT item.family, NEW.scope_kind, NEW.scope_id, GREATEST(now(), COALESCE(item.due::timestamptz, now()))
			FROM (VALUES `+strings.Join(trigger.rows, ", ")+`) AS item(family, due, lease)
			WHERE NOT item.lease OR item.due IS NOT NULL;
			RETURN NULL;
			END $$`,
			`DROP TRIGGER IF EXISTS `+trigger.name+` ON `+s.table(trigger.table)+`;
			CREATE TRIGGER `+trigger.name+` AFTER `+trigger.event+` ON `+s.table(trigger.table)+` FOR EACH ROW`+when+`
			EXECUTE FUNCTION `+s.table(trigger.function())+`()`)
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+`(version,name) VALUES($1,'work outbox') ON CONFLICT(version) DO NOTHING`, workOutboxMigrationVersion)
	return err
}

func (s *PostgresStore) rollbackWorkOutbox(ctx context.Context, tx *sql.Tx) error {
	statements := []string{}
	for _, trigger := range workOutboxTriggers() {
		statements = append(statements, `DROP TRIGGER IF EXISTS `+trigger.name+` ON `+s.table(trigger.table),
			`DROP FUNCTION IF EXISTS `+s.table(trigger.function())+`()`)
	}
	for _, index := range []string{"action_calls", "agent_runs", "agent_requests", "approval_checkpoints",
		"external_conversation_inbox", "external_conversation_deliveries", "callback_events", "workforce_change_sets"} {
		statements = append(statements, `DROP INDEX IF EXISTS `+s.table(index+"_pending_work_idx"))
	}
	statements = append(statements, `DROP TABLE IF EXISTS `+s.table("work_outbox"))
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

// WorkCommitted receives after this process commits a write.
func (s *PostgresStore) WorkCommitted() <-chan struct{} {
	return s.workCommitted
}

func (s *PostgresStore) noteWorkCommitted() {
	select {
	case s.workCommitted <- struct{}{}:
	default:
	}
}

// RelayWorkOutbox moves outbox rows into publish. SKIP LOCKED lets every
// replica relay concurrently without waiting on each other's batches.
func (s *PostgresStore) RelayWorkOutbox(ctx context.Context, limit int, publish func(context.Context, []WorkItem) error) (int, error) {
	if limit <= 0 {
		limit = 500
	}
	// The relay's own commit is not new work; keep it from re-arming itself.
	tx, err := s.db.BeginTx(withoutWorkCommitHint(ctx), nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	outbox := s.table("work_outbox")
	rows, err := tx.QueryContext(ctx, `DELETE FROM `+outbox+` WHERE id IN (
		SELECT id FROM `+outbox+` ORDER BY id LIMIT $1 FOR UPDATE SKIP LOCKED)
		RETURNING family, scope_kind, scope_id, due_at`, limit)
	if err != nil {
		return 0, err
	}
	removed := 0
	earliest := make(map[workItemKey]time.Time)
	for rows.Next() {
		var family, kind, id string
		var due time.Time
		if err := rows.Scan(&family, &kind, &id, &due); err != nil {
			_ = rows.Close()
			return 0, err
		}
		removed++
		key := workItemKey{family: WorkFamily(family), scope: Scope{Kind: kind, ID: id}}
		if current, ok := earliest[key]; !ok || due.Before(current) {
			earliest[key] = due.UTC()
		}
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if removed == 0 {
		return 0, nil
	}
	items := make([]WorkItem, 0, len(earliest))
	for key, due := range earliest {
		items = append(items, WorkItem{Family: key.family, Scope: key.scope, DueAt: due})
	}
	if err := publish(ctx, items); err != nil {
		return 0, err
	}
	return removed, tx.Commit()
}

// ListPendingWork lists scopes with durable work. Each query reads one small
// partial index of active rows, so the sweep costs the active work, not the
// number of scopes or the table sizes.
func (s *PostgresStore) ListPendingWork(ctx context.Context, rebuild bool) ([]WorkItem, error) {
	queries := []string{
		`SELECT 'action', scope_kind, scope_id, MIN(CASE WHEN status='running' THEN COALESCE(lease_expires_at, now()) ELSE available_at END)
			FROM ` + s.table("action_calls") + ` WHERE status IN ('ready','running') GROUP BY scope_kind, scope_id`,
		// Sleeping Runs keep their timers in the payload; a pass reads them.
		`SELECT CASE WHEN payload->>'kind' = 'workforce_authoring' THEN 'authoring' ELSE 'run-due' END, scope_kind, scope_id,
			MIN(CASE WHEN status='running' THEN COALESCE(lease_expires_at, now()) WHEN status='queued' THEN available_at ELSE now() END)
			FROM ` + s.table("agent_runs") + ` WHERE status IN ('queued','running','sleeping') GROUP BY 1, scope_kind, scope_id`,
		`SELECT 'authoring', scope_kind, scope_id, now() FROM ` + s.table("workforce_change_sets") +
			` WHERE status IN ('evaluating','review') GROUP BY scope_kind, scope_id`,
		`SELECT 'run', scope_kind, scope_id, now() FROM ` + s.table("agent_requests") +
			` WHERE status IN ('pending','accepted','clarification_requested','completion_review') GROUP BY scope_kind, scope_id`,
		`SELECT 'maintenance', scope_kind, scope_id, now() FROM ` + s.table("skill_runtime_maintenance") + ` WHERE active GROUP BY scope_kind, scope_id`,
		`SELECT 'external-due', scope_kind, scope_id, MIN(COALESCE(expires_at, now())) FROM ` + s.table("approval_checkpoints") +
			` WHERE status='pending' GROUP BY scope_kind, scope_id`,
		`SELECT 'external-due', scope_kind, scope_id, MIN(CASE WHEN status='leased' THEN COALESCE(lease_expires_at, now()) ELSE available_at END)
			FROM ` + s.table("external_conversation_inbox") + ` WHERE status IN ('pending','retry','leased') GROUP BY scope_kind, scope_id`,
		`SELECT 'external-due', scope_kind, scope_id, now() FROM ` + s.table("external_conversation_inbox") +
			` WHERE status='applied' AND payload->>'replyState' IS NULL AND payload->>'replyAvailableAt' IS NOT NULL GROUP BY scope_kind, scope_id`,
		`SELECT 'external-due', scope_kind, scope_id, MIN(CASE WHEN status='leased' THEN COALESCE(lease_expires_at, now()) ELSE available_at END)
			FROM ` + s.table("external_conversation_deliveries") + ` WHERE status IN ('pending','retry','leased') GROUP BY scope_kind, scope_id`,
		`SELECT 'external-due', scope_kind, scope_id, MIN(CASE WHEN status='leased' THEN COALESCE(lease_expires_at, now()) ELSE available_at END)
			FROM ` + s.table("callback_events") + ` WHERE status IN ('pending','leased') GROUP BY scope_kind, scope_id`,
	}
	if rebuild {
		// Conversation reconciliation recovers messages whose Run was never
		// scheduled. Its queue items may have been lost with the schedule;
		// recently active conversations are where such a gap can be.
		queries = append(queries, `SELECT 'conversation-due', scope_kind, scope_id, now() FROM `+s.table("conversations")+
			` WHERE status='active' AND updated_at > now() - interval '1 hour' GROUP BY scope_kind, scope_id`)
	}
	rows, err := s.db.QueryContext(ctx, strings.Join(queries, "\nUNION ALL\n"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []WorkItem{}
	for rows.Next() {
		var family, kind, id string
		var due sql.NullTime
		if err := rows.Scan(&family, &kind, &id, &due); err != nil {
			return nil, err
		}
		item := WorkItem{Family: WorkFamily(family), Scope: Scope{Kind: kind, ID: id}, DueAt: time.Now().UTC()}
		if due.Valid {
			item.DueAt = due.Time.UTC()
		}
		items = append(items, item)
	}
	return items, rows.Err()
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

// workCommitHintKey marks a context whose writes must not raise the work
// committed hint, such as the outbox relay itself.
type workCommitHintKey struct{}

func withoutWorkCommitHint(ctx context.Context) context.Context {
	return context.WithValue(ctx, workCommitHintKey{}, true)
}

func workCommitHintSuppressed(ctx context.Context) bool {
	suppressed, _ := ctx.Value(workCommitHintKey{}).(bool)
	return suppressed
}

// writeObservingConnector reports committed writes, so the outbox relay runs
// right after a commit instead of polling the outbox. Every transaction that
// is not read-only counts on commit; outside a transaction, statements that
// write count once they succeed.
type writeObservingConnector struct {
	base    driver.Connector
	onWrite func()
}

func (c *writeObservingConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &writeObservingConn{conn: conn, onWrite: c.onWrite}, nil
}

func (c *writeObservingConnector) Driver() driver.Driver { return c.base.Driver() }

type writeObservingConn struct {
	conn    driver.Conn
	onWrite func()
	inTx    bool
}

var (
	_ driver.ConnBeginTx        = (*writeObservingConn)(nil)
	_ driver.ConnPrepareContext = (*writeObservingConn)(nil)
	_ driver.ExecerContext      = (*writeObservingConn)(nil)
	_ driver.QueryerContext     = (*writeObservingConn)(nil)
	_ driver.Pinger             = (*writeObservingConn)(nil)
	_ driver.SessionResetter    = (*writeObservingConn)(nil)
	_ driver.Validator          = (*writeObservingConn)(nil)
)

func (c *writeObservingConn) Prepare(query string) (driver.Stmt, error) { return c.conn.Prepare(query) }

func (c *writeObservingConn) Close() error { return c.conn.Close() }

func (c *writeObservingConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *writeObservingConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	var tx driver.Tx
	var err error
	if begin, ok := c.conn.(driver.ConnBeginTx); ok {
		tx, err = begin.BeginTx(ctx, opts)
	} else {
		tx, err = c.conn.Begin() //nolint:staticcheck // fallback for drivers without BeginTx
	}
	if err != nil {
		return nil, err
	}
	c.inTx = true
	return &writeObservingTx{tx: tx, conn: c, silent: opts.ReadOnly || workCommitHintSuppressed(ctx)}, nil
}

func (c *writeObservingConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if prepare, ok := c.conn.(driver.ConnPrepareContext); ok {
		return prepare.PrepareContext(ctx, query)
	}
	return c.conn.Prepare(query)
}

func (c *writeObservingConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	exec, ok := c.conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	result, err := exec.ExecContext(ctx, query, args)
	if err == nil && !c.inTx && !workCommitHintSuppressed(ctx) && writesRows(query) {
		c.onWrite()
	}
	return result, err
}

func (c *writeObservingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	queryer, ok := c.conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	rows, err := queryer.QueryContext(ctx, query, args)
	if err == nil && !c.inTx && !workCommitHintSuppressed(ctx) && writesRows(query) {
		c.onWrite()
	}
	return rows, err
}

func (c *writeObservingConn) Ping(ctx context.Context) error {
	if pinger, ok := c.conn.(driver.Pinger); ok {
		return pinger.Ping(ctx)
	}
	return nil
}

func (c *writeObservingConn) ResetSession(ctx context.Context) error {
	if resetter, ok := c.conn.(driver.SessionResetter); ok {
		return resetter.ResetSession(ctx)
	}
	return nil
}

func (c *writeObservingConn) IsValid() bool {
	if validator, ok := c.conn.(driver.Validator); ok {
		return validator.IsValid()
	}
	return true
}

type writeObservingTx struct {
	tx     driver.Tx
	conn   *writeObservingConn
	silent bool
}

func (t *writeObservingTx) Commit() error {
	t.conn.inTx = false
	err := t.tx.Commit()
	if err == nil && !t.silent {
		t.conn.onWrite()
	}
	return err
}

func (t *writeObservingTx) Rollback() error {
	t.conn.inTx = false
	return t.tx.Rollback()
}

// writesRows reports whether an autocommit statement may change rows.
func writesRows(query string) bool {
	trimmed := strings.TrimLeft(query, " \t\r\n(")
	if len(trimmed) > 6 {
		trimmed = trimmed[:6]
	}
	switch strings.ToUpper(trimmed) {
	case "INSERT", "UPDATE", "DELETE", "MERGE ":
		return true
	case "SELECT":
		return false
	}
	// WITH ... and anything unusual: assume it writes. A spurious hint costs
	// one empty outbox read.
	return true
}

func openObservedPostgres(dsn string, onWrite func()) (*sql.DB, error) {
	connector, err := pq.NewConnector(dsn)
	if err != nil {
		return nil, err
	}
	return sql.OpenDB(&writeObservingConnector{base: connector, onWrite: onWrite}), nil
}
