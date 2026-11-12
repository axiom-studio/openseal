package sqlite

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
)

// The put* fixtures write kernel records straight into the store's tables,
// skipping every store rule, for tests that need a state no store method
// produces (a record left by a crashed worker, a tampered row) or that only
// need records to read back. Each replaces the row with the same key through
// the store's own row insert, so triggers still project it.

func putStoredRun(t testing.TB, store *Store, run *models.AgentRun) {
	t.Helper()
	ctx := context.Background()
	encoded, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	payload := string(encoded)
	result, err := store.db.ExecContext(ctx, `UPDATE agent_runs SET objective_id = ?, parent_run_id = ?, root_run_id = ?,
		assigned_agent_id = ?, status = ?, priority = ?, revision = ?, deadline = ?, available_at = ?, queue_entered_at = ?,
		lease_owner = ?, lease_expires_at = ?, last_claimed_at = ?, attempt = ?, created_at = ?, payload = ?
		WHERE scope_kind = ? AND scope_id = ? AND id = ?`,
		run.ObjectiveID, run.ParentRunID, run.RootRunID, run.AssignedAgentID, run.Status, run.Priority, run.Revision,
		run.Deadline, run.AvailableAt, run.QueueEnteredAt, run.LeaseOwner, run.LeaseExpiresAt, run.LastClaimedAt,
		run.Attempt, run.CreatedAt, payload, run.Scope.Kind, run.Scope.ID, run.ID)
	if err != nil {
		t.Fatalf("put run %s: %v", run.ID, err)
	}
	if updated, _ := result.RowsAffected(); updated == 1 {
		return
	}
	conn, err := store.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := insertSQLiteAgentRunConn(ctx, conn, run); err != nil {
		t.Fatalf("put run %s: %v", run.ID, err)
	}
}

func putStoredAction(t testing.TB, store *Store, call *models.ActionCall) {
	t.Helper()
	deleteStoredRow(t, store, "action_calls", call.Scope, call.ID)
	if err := insertSQLiteActionCall(context.Background(), store.db, call); err != nil {
		t.Fatalf("put action %s: %v", call.ID, err)
	}
}

func putStoredApproval(t testing.TB, store *Store, approval *models.ApprovalCheckpoint) {
	t.Helper()
	deleteStoredRow(t, store, "approval_checkpoints", approval.Scope, approval.ID)
	if err := insertSQLiteApprovalCheckpoint(context.Background(), store.db, approval); err != nil {
		t.Fatalf("put approval %s: %v", approval.ID, err)
	}
}

// execStoreSQL runs a statement against the store's database.
func execStoreSQL(t testing.TB, store *Store, query string, args ...any) {
	t.Helper()
	if _, err := store.db.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

func deleteStoredRow(t testing.TB, store *Store, table string, scope models.Scope, id string) {
	t.Helper()
	execStoreSQL(t, store, `DELETE FROM `+table+` WHERE scope_kind = ? AND scope_id = ? AND id = ?`, scope.Kind, scope.ID, id)
}
