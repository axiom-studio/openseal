package runtime

import (
	"path/filepath"
	"testing"
	"time"
)

func TestTerminalFailureSQLite58UpgradePreservesClaimedReceiptAndDeliveredHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reporting-upgrade.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	now := eventWaitContractEpoch
	first, _, _ := terminalReportingContractRun(t, store, Scope{Kind: "tenant", ID: "upgrade-pending"}, now)
	first = terminalReportingContractComplete(t, store, first, now)
	second, _, _ := terminalReportingContractRun(t, store, Scope{Kind: "tenant", ID: "upgrade-delivered"}, now)
	second = terminalReportingContractComplete(t, store, second, now)
	claims, err := store.ClaimRunTerminalReports(t.Context(), RunTerminalReportingClaim{Scope: &first.Scope, WorkerID: "v58-in-flight", Now: now, LeaseDuration: time.Minute, Limit: 1})
	if err != nil || len(claims) != 1 {
		t.Fatalf("legacy claim fixture: %#v %v", claims, err)
	}
	if count, err := terminalReportingContractWorker(t, store, now).ProcessScope(t.Context(), second.Scope); err != nil || count != 1 {
		t.Fatalf("legacy delivered fixture: %d %v", count, err)
	}
	// Exact old table shape, including its active lease and released snapshot.
	_, err = store.db.Exec(`DROP TRIGGER agent_runs_terminal_reporting_insert; DROP TRIGGER agent_runs_terminal_reporting_update;
		DROP INDEX idx_run_terminal_reports_pending_due; DROP INDEX idx_run_terminal_reports_scoped_due;
		ALTER TABLE run_terminal_reports RENAME TO reporting_v59_fixture;
		CREATE TABLE run_terminal_reports(scope_kind TEXT NOT NULL,scope_id TEXT NOT NULL,run_id TEXT NOT NULL,
			terminal_status TEXT NOT NULL,queue_state TEXT NOT NULL,available_at BIGINT NOT NULL,attempts INTEGER NOT NULL,
			lease_owner TEXT NOT NULL,lease_expires_at BIGINT,delivered_at BIGINT,payload TEXT,
			PRIMARY KEY(scope_kind,scope_id,run_id,terminal_status));
		INSERT INTO run_terminal_reports SELECT scope_kind,scope_id,run_id,terminal_status,queue_state,available_at,attempts,lease_owner,lease_expires_at,delivered_at,payload FROM reporting_v59_fixture;
		DROP TABLE reporting_v59_fixture;`)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := store.GetRunTerminalReport(t.Context(), first.Scope, first.ID, first.Status)
	if err != nil || pending.TerminalRevision != first.Revision || pending.Run == nil || pending.Run.Revision != first.Revision || pending.LeaseOwner != "v58-in-flight" || pending.Attempts != 1 || pending.LeaseExpiresAt == nil || !pending.LeaseExpiresAt.Equal(*claims[0].LeaseExpiresAt) {
		t.Fatalf("upgrade changed active receipt or fencing: %#v %v", pending, err)
	}
	delivered, err := store.GetRunTerminalReport(t.Context(), second.Scope, second.ID, second.Status)
	if err != nil || delivered.DeliveredAt == nil || delivered.Run != nil || delivered.TerminalRevision != 0 {
		t.Fatalf("acknowledged historical receipt changed: %#v %v", delivered, err)
	}
	if count, err := terminalReportingContractWorker(t, store, now).ProcessBatch(t.Context()); err != nil || count != 0 {
		t.Fatalf("upgrade stole a held lease: %d %v", count, err)
	}
	if count, err := terminalReportingContractWorker(t, store, now.Add(2*time.Minute)).ProcessBatch(t.Context()); err != nil || count != 1 {
		t.Fatalf("upgrade lost pending receipt: %d %v", count, err)
	}
	// Reopening an already-upgraded store must preserve the new primary key.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if count, err := terminalReportingContractWorker(t, store, now.Add(3*time.Minute)).ProcessBatch(t.Context()); err != nil || count != 0 {
		t.Fatalf("repeat upgrade regenerated delivery: %d %v", count, err)
	}
}
