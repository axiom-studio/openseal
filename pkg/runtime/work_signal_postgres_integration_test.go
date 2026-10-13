package runtime

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func newWorkSignalPostgresStore(t *testing.T, schema string) (context.Context, *PostgresStore) {
	t.Helper()
	dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OPENSEAL_TEST_POSTGRES_DSN to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	store, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.db.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+store.quotedSchema()+` CASCADE`)
		_ = store.Close()
	})
	return ctx, store
}

func TestPostgresWorkSignalTriggersNotifyOnlyClaimabilityChanges(t *testing.T) {
	ctx, store := newWorkSignalPostgresStore(t, "openseal_work_signal_"+uuid.NewString()[:8])
	// Another schema in the same database must never wake this listener.
	_, foreign := newWorkSignalPostgresStore(t, "openseal_work_signal_foreign_"+uuid.NewString()[:8])
	if store.WorkSignalChannel() == foreign.WorkSignalChannel() {
		t.Fatal("schemas share a work signal channel")
	}
	if long := (&PostgresStore{schema: strings.Repeat("s", 63)}).WorkSignalChannel(); len(long) > 63 {
		t.Fatalf("channel %q exceeds the identifier limit", long)
	}
	signals := make(chan WorkSignal, 64)
	listenCtx, stop := context.WithCancel(ctx)
	defer stop()
	go func() { _ = store.ListenForWork(listenCtx, func(signal WorkSignal) { signals <- signal }) }()
	expect := func(kind WorkSignalKind, family WorkFamily, scope Scope) {
		t.Helper()
		select {
		case signal := <-signals:
			if signal.Kind != kind || signal.Family != family || signal.Scope != scope {
				t.Fatalf("signal = %#v, want %v %q %#v", signal, kind, family, scope)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no %q signal", family)
		}
	}
	expectNone := func(reason string) {
		t.Helper()
		select {
		case signal := <-signals:
			t.Fatalf("%s notified %#v", reason, signal)
		case <-time.After(300 * time.Millisecond):
		}
	}
	expect(WorkSignalConnected, "", Scope{})

	scope := Scope{Kind: "tenant", ID: "work-signals"}
	run, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent", Goal: "notify", Source: RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	expect(WorkSignalChanged, WorkFamilyRun, scope)
	if _, err := NewPortfolioService(foreign).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent", Goal: "elsewhere", Source: RunSourceManual,
	}); err != nil {
		t.Fatal(err)
	}
	expectNone("a foreign schema's Run")

	claimed, err := store.ClaimNextAgentRun(ctx, AgentRunClaim{Scope: scope, WorkerID: "worker", Now: time.Now(), LeaseDuration: time.Minute, AgingInterval: time.Minute})
	if err != nil || claimed == nil || claimed.ID != run.ID {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	expect(WorkSignalChanged, WorkFamilyRun, scope)
	if _, err := store.RenewAgentRunLease(ctx, scope, run.ID, "worker", time.Now(), 2*time.Minute); err != nil {
		t.Fatal(err)
	}
	expectNone("a lease renewal")
	if _, err := store.db.ExecContext(ctx, `UPDATE `+store.table("agent_runs")+` SET payload = payload || '{"note":"x"}'::jsonb WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	expectNone("a payload-only update")

	// Deadlines: a running lease, then a deferred retry.
	now := time.Now().UTC()
	deadline, err := store.NextAgentRunDeadline(ctx, scope, "", "", now)
	if err != nil || deadline == nil || deadline.Before(now.Add(time.Minute)) {
		t.Fatalf("lease deadline = %v, %v", deadline, err)
	}
	if other, err := store.NextAgentRunDeadline(ctx, scope, RunKindConversation, "", now); err != nil || other != nil {
		t.Fatalf("another kind's deadline = %v, %v", other, err)
	}
	retryAt := now.Add(30 * time.Second).Truncate(time.Microsecond)
	if _, err := store.db.ExecContext(ctx, `UPDATE `+store.table("agent_runs")+` SET status='queued', lease_owner='', lease_expires_at=NULL, available_at=$2 WHERE id=$1`, run.ID, retryAt); err != nil {
		t.Fatal(err)
	}
	expect(WorkSignalChanged, WorkFamilyRun, scope)
	if deadline, err := store.NextAgentRunDeadline(ctx, scope, RunKindAgentWork, "agent", now); err != nil || deadline == nil || !deadline.Equal(retryAt) {
		t.Fatalf("retry deadline = %v, %v, want %v", deadline, err, retryAt)
	}
	if deadline, err := store.NextActionDeadline(ctx, scope, now); err != nil || deadline != nil {
		t.Fatalf("action deadline = %v, %v", deadline, err)
	}
	if deadline, err := store.NextExternalConversationDeadline(ctx, scope, now); err != nil || deadline != nil {
		t.Fatalf("external deadline = %v, %v", deadline, err)
	}

	// Commentary activity notifies; other activity does not.
	if _, err := store.db.ExecContext(ctx, `INSERT INTO `+store.table("run_activity")+` (scope_kind,scope_id,run_id,sequence,id,event_type,created_at,payload)
		VALUES ($1,$2,$3,1000,'activity-plain','run.note',now(),'{}'::jsonb), ($1,$2,$3,1001,'activity-commentary','turn.commentary',now(),'{}'::jsonb)`,
		scope.Kind, scope.ID, run.ID); err != nil {
		t.Fatal(err)
	}
	expect(WorkSignalChanged, WorkFamilyActivity, scope)
	expectNone("plain activity")

	// Leaving a pause also wakes action claimers.
	if _, err := store.db.ExecContext(ctx, `UPDATE `+store.table("agent_runs")+` SET status='paused' WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	expect(WorkSignalChanged, WorkFamilyRun, scope)
	if _, err := store.db.ExecContext(ctx, `UPDATE `+store.table("agent_runs")+` SET status='queued' WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	families := map[WorkFamily]bool{}
	for range 2 {
		select {
		case signal := <-signals:
			families[signal.Family] = true
		case <-time.After(5 * time.Second):
			t.Fatal("unpause did not notify both families")
		}
	}
	if !families[WorkFamilyRun] || !families[WorkFamilyAction] {
		t.Fatalf("unpause families = %v", families)
	}

	// Terminating the listener's backend reconnects and reports it.
	if _, err := foreign.db.ExecContext(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE query LIKE 'LISTEN %' AND position($1 in query) > 0`, store.WorkSignalChannel()); err != nil {
		t.Fatal(err)
	}
	expect(WorkSignalDisconnected, "", Scope{})
	expect(WorkSignalConnected, "", Scope{})
	if _, err := store.db.ExecContext(ctx, `UPDATE `+store.table("agent_runs")+` SET status='completed' WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	expect(WorkSignalChanged, WorkFamilyRun, scope)
}

func TestPostgresWorkSignalMigrationRollsBack(t *testing.T) {
	ctx, store := newWorkSignalPostgresStore(t, "openseal_work_signal_rollback_"+uuid.NewString()[:8])
	countTriggers := func() int {
		var count int
		if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pg_trigger t JOIN pg_class c ON c.oid=t.tgrelid
			JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND t.tgname LIKE 'work_signal_%'`, store.schema).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	if got, want := countTriggers(), len(workSignalTriggers()); got != want {
		t.Fatalf("triggers = %d, want %d", got, want)
	}
	if err := store.RollbackPostgresMigrations(ctx, workSignalMigrationVersion-1); err != nil {
		t.Fatal(err)
	}
	if got := countTriggers(); got != 0 {
		t.Fatalf("triggers after rollback = %d", got)
	}
	if err := store.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if got, want := countTriggers(), len(workSignalTriggers()); got != want {
		t.Fatalf("triggers after reapply = %d, want %d", got, want)
	}
}
