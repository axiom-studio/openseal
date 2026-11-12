//go:build integration

package runtime

import (
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun/conversationruntest"
	"github.com/axiom-studio/openseal/pkg/runtime/store/postgres"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

func terminalFailureMigrationPostgres(t *testing.T) (*postgres.Store, storetest.PostgresSchema) {
	t.Helper()
	return openTestPostgresStore(t, "failure_reporting_")
}

func TestTerminalFailurePostgres59MigrationPreserves58LeaseAndRestoresLegacyTrigger(t *testing.T) {
	store, schema := terminalFailureMigrationPostgres(t)
	now := runStoreContractEpoch
	// The conversation exists before the rollback: conversations need the
	// space schema, which is newer than 58.
	run, conversation, trigger := TerminalReportingRun(t, store, Scope{Kind: "tenant", ID: "pg58-lease"}, now)
	if err := store.RollbackPostgresMigrations(t.Context(), 58); err != nil {
		t.Fatal(err)
	}
	var oldFunction string
	if err := schema.DB.QueryRowContext(t.Context(), `SELECT pg_get_functiondef(($1||'.enqueue_run_terminal_report()')::regprocedure)`, schema.Name).Scan(&oldFunction); err != nil || strings.Contains(oldFunction, "terminal_revision") {
		t.Fatalf("rollback did not restore exact old conflict contract: %v", err)
	}
	run = CompleteTerminalReportingRun(t, store, run, now)
	expires := now.Add(time.Minute)
	if _, err := schema.DB.ExecContext(t.Context(), `UPDATE `+schema.Table("run_terminal_reports")+` SET lease_owner='old-reader',lease_expires_at=$1,available_at=$1,attempts=1 WHERE run_id=$2`, expires, run.ID); err != nil {
		t.Fatal(err)
	}
	// All old readers/writers are stopped before upgrade, as required by the
	// explicit schema59 deployment barrier. A process reopen exercises the
	// canonical migration ledger instead of hidden ad-hoc DDL.
	upgraded := openPostgresStoreOn(t, schema)
	if version, err := upgraded.PostgresSchemaVersion(t.Context()); err != nil || version != postgresSchemaHead(t) {
		t.Fatalf("ledger migration absent: %d %v", version, err)
	}
	intent, err := upgraded.GetRunTerminalReport(t.Context(), run.Scope, run.ID, run.Status)
	if err != nil || intent.Run == nil || intent.TerminalRevision != run.Revision || intent.LeaseOwner != "old-reader" || intent.Attempts != 1 || intent.LeaseExpiresAt == nil || !intent.LeaseExpiresAt.Equal(expires) {
		t.Fatalf("migration lost old receipt/fence: %#v %v", intent, err)
	}
	if count, err := TerminalReportingWorker(t, upgraded, now).ProcessBatch(t.Context()); err != nil || count != 0 {
		t.Fatalf("migration stole old lease: %d %v", count, err)
	}
	if count, err := TerminalReportingWorker(t, upgraded, now.Add(2*time.Minute)).ProcessBatch(t.Context()); err != nil || count != 1 {
		t.Fatalf("migration lost old promised reply: %d %v", count, err)
	}
	AssertTerminalReport(t, upgraded, run, conversation, trigger)
	var function string
	if err := schema.DB.QueryRowContext(t.Context(), `SELECT pg_get_functiondef(($1||'.enqueue_run_terminal_report()')::regprocedure)`, schema.Name).Scan(&function); err != nil || !strings.Contains(function, "terminal_revision") || !strings.Contains(function, "OLD.status IS DISTINCT FROM NEW.status") {
		t.Fatalf("version59 trigger lacks attempt identity/transition fence: %v", err)
	}
}

func TestTerminalFailurePostgres59RollbackRejectsMultipleAttemptReceipts(t *testing.T) {
	store, _ := terminalFailureMigrationPostgres(t)
	now := runStoreContractEpoch
	first, _, _ := conversationruntest.TerminalFailureConversation(t, store, now, false)
	first = conversationruntest.FailTerminalConversation(t, store, first, now, "provider_rate_limited")
	commands := agentrun.NewRunCommandService(store, models.WithClock(func() time.Time { return now.Add(time.Second) }))
	retry, err := commands.RetryConversationRun(t.Context(), agentrun.AgentRunCommandRequest{Scope: first.Scope, RunID: first.ID, ExpectedRevision: first.Revision, Actor: ActivityActor{Type: "user", ID: "kev"}})
	if err != nil {
		t.Fatal(err)
	}
	second := conversationruntest.FailTerminalConversation(t, store, retry.Run, now.Add(2*time.Second), "provider_unavailable")
	if err := store.RollbackPostgresMigrations(t.Context(), 58); err == nil || !strings.Contains(err.Error(), "multiple attempt receipts") {
		t.Fatalf("rollback silently discarded an immutable attempt: %v", err)
	}
	if version, err := store.PostgresSchemaVersion(t.Context()); err != nil || version != postgresSchemaHead(t) {
		t.Fatalf("rejected rollback changed ledger: %d %v", version, err)
	}
	latest, err := store.GetRunTerminalReport(t.Context(), second.Scope, second.ID, second.Status)
	if err != nil || latest.TerminalRevision != second.Revision {
		t.Fatalf("rejected rollback changed queue: %#v %v", latest, err)
	}
}

func TestTerminalFailurePostgres59RollbackRequiresNewFailureDelivery(t *testing.T) {
	store, _ := terminalFailureMigrationPostgres(t)
	now := runStoreContractEpoch
	run, _, _ := conversationruntest.TerminalFailureConversation(t, store, now, false)
	run = conversationruntest.FailTerminalConversation(t, store, run, now, "provider_rate_limited")
	if err := store.RollbackPostgresMigrations(t.Context(), 58); err == nil || !strings.Contains(err.Error(), "before new conversation failure replies are delivered") {
		t.Fatalf("rollback abandoned an unsupported pending failure reply: %v", err)
	}
	if version, err := store.PostgresSchemaVersion(t.Context()); err != nil || version != postgresSchemaHead(t) {
		t.Fatalf("rejected rollback changed ledger: %d %v", version, err)
	}
	if count, err := TerminalReportingWorker(t, store, now).ProcessBatch(t.Context()); err != nil || count != 1 {
		t.Fatalf("rejected rollback lost promised failure: %d %v", count, err)
	}
	if err := store.RollbackPostgresMigrations(t.Context(), 58); err != nil {
		t.Fatalf("delivered single attempt prevented safe rollback: %v", err)
	}
}
