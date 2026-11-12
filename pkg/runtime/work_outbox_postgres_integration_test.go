package runtime

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
	"github.com/google/uuid"
)

// Postgres store test (outbox triggers and migrations): it moves to
// store/postgres in U2.8. It writes its Runs with the store's CreateAgentRun
// (storetest.QueuedAgentRun), since a store package cannot import runtime.

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

// relayed drains the outbox and returns what it held, keyed by family.
func relayed(t *testing.T, ctx context.Context, store *PostgresStore) map[models.WorkFamily][]models.WorkItem {
	t.Helper()
	result := map[models.WorkFamily][]models.WorkItem{}
	if _, err := store.RelayWorkOutbox(ctx, 500, func(_ context.Context, items []models.WorkItem) error {
		for _, item := range items {
			result[item.Family] = append(result[item.Family], item)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestPostgresWorkOutboxRecordsOnlyClaimabilityChanges(t *testing.T) {
	ctx, store := newWorkSignalPostgresStore(t, "openseal_work_outbox_"+uuid.NewString()[:8])
	_, foreign := newWorkSignalPostgresStore(t, "openseal_work_outbox_foreign_"+uuid.NewString()[:8])
	scope := models.Scope{Kind: "tenant", ID: "work-outbox"}
	expect := func(reason string, want map[models.WorkFamily]int) map[models.WorkFamily][]models.WorkItem {
		t.Helper()
		got := relayed(t, ctx, store)
		for family, count := range want {
			if len(got[family]) != count {
				t.Fatalf("%s: %s items = %#v, want %d", reason, family, got[family], count)
			}
			for _, item := range got[family] {
				if item.Scope != scope {
					t.Fatalf("%s: item scope = %#v", reason, item.Scope)
				}
			}
		}
		for family, items := range got {
			if _, expected := want[family]; !expected {
				t.Fatalf("%s: unexpected %s items %#v", reason, family, items)
			}
		}
		return got
	}
	relayed(t, ctx, store)
	select {
	case <-store.WorkCommitted():
	default:
	}

	before := time.Now().UTC()
	run := storetest.QueuedAgentRun(scope, "agent", "record", time.Now())
	run.Source = models.RunSourceManual
	if err := store.CreateAgentRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	select {
	case <-store.WorkCommitted():
	default:
		t.Fatal("a committed write raised no work hint")
	}
	created := expect("a new Run", map[models.WorkFamily]int{models.WorkFamilyRun: 1})
	if due := created[models.WorkFamilyRun][0].DueAt; due.Before(before.Add(-time.Second)) || due.After(time.Now().Add(time.Second)) {
		t.Fatalf("new Run due at %v", due)
	}
	select {
	case <-store.WorkCommitted():
		t.Fatal("the relay's own commit raised a work hint")
	default:
	}
	elsewhere := storetest.QueuedAgentRun(scope, "agent", "elsewhere", time.Now())
	elsewhere.Source = models.RunSourceManual
	if err := foreign.CreateAgentRun(ctx, elsewhere); err != nil {
		t.Fatal(err)
	}
	expect("a foreign schema's Run", map[models.WorkFamily]int{})

	claimed, err := store.ClaimNextAgentRun(ctx, models.AgentRunClaim{Scope: scope, WorkerID: "worker", Now: time.Now(), LeaseDuration: time.Minute, AgingInterval: time.Minute})
	if err != nil || claimed == nil || claimed.ID != run.ID {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	// A claim schedules its lease expiry, so a crashed worker's Run is reclaimed.
	claim := expect("a claim", map[models.WorkFamily]int{models.WorkFamilyRun: 1, models.WorkFamilyRunDue: 1})
	if due := claim[models.WorkFamilyRunDue][0].DueAt; due.Before(time.Now().Add(50 * time.Second)) {
		t.Fatalf("lease expiry due at %v", due)
	}
	if _, err := store.RenewAgentRunLease(ctx, scope, run.ID, "worker", time.Now(), 2*time.Minute); err != nil {
		t.Fatal(err)
	}
	expect("a lease renewal", map[models.WorkFamily]int{})
	if _, err := store.db.ExecContext(ctx, `UPDATE `+store.table("agent_runs")+` SET payload = payload || '{"note":"x"}'::jsonb WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	expect("a payload-only update", map[models.WorkFamily]int{})

	// A deferred retry is due at its availability.
	now := time.Now().UTC()
	retryAt := now.Add(30 * time.Second).Truncate(time.Microsecond)
	if _, err := store.db.ExecContext(ctx, `UPDATE `+store.table("agent_runs")+` SET status='queued', lease_owner='', lease_expires_at=NULL, available_at=$2 WHERE id=$1`, run.ID, retryAt); err != nil {
		t.Fatal(err)
	}
	if retry := expect("a deferred retry", map[models.WorkFamily]int{models.WorkFamilyRun: 1}); !retry[models.WorkFamilyRun][0].DueAt.Equal(retryAt) {
		t.Fatalf("retry due at %v, want %v", retry[models.WorkFamilyRun][0].DueAt, retryAt)
	}
	if deadline, err := store.NextAgentRunDeadline(ctx, scope, models.RunKindAgentWork, "agent", now); err != nil || deadline == nil || !deadline.Equal(retryAt) {
		t.Fatalf("retry deadline = %v, %v, want %v", deadline, err, retryAt)
	}
	pending, err := store.ListPendingWork(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Family != models.WorkFamilyRunDue || pending[0].Scope != scope || !pending[0].DueAt.Equal(retryAt) {
		t.Fatalf("pending work = %#v", pending)
	}

	// Commentary and answer previews are recorded; other activity is not.
	if _, err := store.db.ExecContext(ctx, `INSERT INTO `+store.table("run_activity")+` (scope_kind,scope_id,run_id,sequence,id,event_type,created_at,payload)
		VALUES ($1,$2,$3,1000,'activity-plain','run.note',now(),'{}'::jsonb), ($1,$2,$3,1001,'activity-commentary','turn.commentary',now(),'{}'::jsonb)`,
		scope.Kind, scope.ID, run.ID); err != nil {
		t.Fatal(err)
	}
	expect("activity", map[models.WorkFamily]int{models.WorkFamilyActivity: 1})
	if _, err := store.db.ExecContext(ctx, `INSERT INTO `+store.table("run_activity")+` (scope_kind,scope_id,run_id,sequence,id,event_type,created_at,payload)
		VALUES ($1,$2,$3,1002,'activity-preview','turn.answer_preview',now(),'{}'::jsonb)`, scope.Kind, scope.ID, run.ID); err != nil {
		t.Fatal(err)
	}
	expect("an answer preview", map[models.WorkFamily]int{models.WorkFamilyActivity: 1})

	// Leaving a pause also wakes action claimers.
	if _, err := store.db.ExecContext(ctx, `UPDATE `+store.table("agent_runs")+` SET status='paused' WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	expect("a pause", map[models.WorkFamily]int{models.WorkFamilyRun: 1})
	if _, err := store.db.ExecContext(ctx, `UPDATE `+store.table("agent_runs")+` SET status='queued', available_at=now() WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	expect("an unpause", map[models.WorkFamily]int{models.WorkFamilyRun: 1, models.WorkFamilyAction: 1})

	// A failed publish keeps the rows for the next relay.
	if _, err := store.db.ExecContext(ctx, `UPDATE `+store.table("agent_runs")+` SET status='completed' WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RelayWorkOutbox(ctx, 500, func(context.Context, []models.WorkItem) error { return errors.New("queue down") }); err == nil {
		t.Fatal("relay ignored a publish failure")
	}
	expect("rows kept after a failed publish", map[models.WorkFamily]int{models.WorkFamilyRun: 1})
	if pending, err := store.ListPendingWork(ctx, true); err != nil || len(pending) != 0 {
		t.Fatalf("pending work after completion = %#v, %v", pending, err)
	}
}

func TestPostgresWorkOutboxMigrationRollsBack(t *testing.T) {
	ctx, store := newWorkSignalPostgresStore(t, "openseal_work_outbox_rollback_"+uuid.NewString()[:8])
	countTriggers := func(pattern string) int {
		var count int
		if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pg_trigger t JOIN pg_class c ON c.oid=t.tgrelid
			JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND t.tgname LIKE $2`, store.schema, pattern).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	outboxExists := func() bool {
		var exists bool
		if err := store.db.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, store.schema+".work_outbox").Scan(&exists); err != nil {
			t.Fatal(err)
		}
		return exists
	}
	if got, want := countTriggers("work_outbox_%"), len(workOutboxTriggers()); got != want || !outboxExists() {
		t.Fatalf("triggers = %d, want %d; outbox %t", got, want, outboxExists())
	}
	if got := countTriggers("work_signal_%"); got != 0 {
		t.Fatalf("NOTIFY triggers remain: %d", got)
	}
	previewTriggered := func() bool {
		var definition string
		if err := store.db.QueryRowContext(ctx, `SELECT pg_get_triggerdef(t.oid) FROM pg_trigger t JOIN pg_class c ON c.oid=t.tgrelid
			JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND c.relname='run_activity' AND t.tgname='work_outbox_insert'`, store.schema).Scan(&definition); err != nil {
			t.Fatal(err)
		}
		return strings.Contains(definition, "turn.answer_preview")
	}
	if !previewTriggered() {
		t.Fatal("answer previews do not wake external conversation workers")
	}
	if err := store.RollbackPostgresMigrations(ctx, answerStreamSignalMigrationVersion-1); err != nil {
		t.Fatal(err)
	}
	if previewTriggered() || countTriggers("work_outbox_%") != len(workOutboxTriggers()) {
		t.Fatal("answer stream signal rollback kept preview wakes")
	}
	if err := store.RollbackPostgresMigrations(ctx, workOutboxMigrationVersion-1); err != nil {
		t.Fatal(err)
	}
	if got := countTriggers("work_outbox_%"); got != 0 || outboxExists() {
		t.Fatalf("after rollback: %d triggers, outbox %t", got, outboxExists())
	}
	// A database that applied the NOTIFY triggers loses them on upgrade.
	if _, err := store.db.ExecContext(ctx, `CREATE OR REPLACE FUNCTION `+store.table("notify_work_signal")+`() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NULL; END $$;
		CREATE TRIGGER work_signal_insert AFTER INSERT ON `+store.table("agent_runs")+` FOR EACH ROW EXECUTE FUNCTION `+store.table("notify_work_signal")+`()`); err != nil {
		t.Fatal(err)
	}
	if err := store.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if got, want := countTriggers("work_outbox_%"), len(workOutboxTriggers()); got != want || countTriggers("work_signal_%") != 0 {
		t.Fatalf("after reapply: %d outbox triggers, want %d; %d NOTIFY triggers", got, want, countTriggers("work_signal_%"))
	}
}
