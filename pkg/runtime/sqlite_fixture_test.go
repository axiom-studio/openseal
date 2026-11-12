package runtime

import (
	"context"
	"testing"
)

// The put* fixtures write kernel records straight into a SQLiteStore's tables,
// skipping every store rule, for tests that need a state no store method
// produces (a record left by a crashed worker, a tampered row) or that only
// need records to read back. Each replaces the row with the same key through
// the store's own row insert, so triggers still project it.
//
// Every helper here reaches into the store (its connection, its row inserts),
// so this file moves to the SQLite store package with the store's own tests.
// Runtime tests stop using it: they seed through the public store API, or a
// fake at the feature's interface, and keep raw SQL on a connection they own
// (newTestSQLiteStore with the helpers in raw_sql_test.go). A test that needs
// a state only these fixtures produce is a store test and moves with them.

func putStoredRun(t testing.TB, store *SQLiteStore, run *AgentRun) {
	t.Helper()
	ctx := context.Background()
	payload := mustJSON(t, run)
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

func putStoredTurn(t testing.TB, store *SQLiteStore, turn *AgentTurn) {
	t.Helper()
	deleteStoredRow(t, store, "agent_turns", turn.Scope, turn.ID)
	if err := insertSQLiteAgentTurn(context.Background(), store.db, turn); err != nil {
		t.Fatalf("put turn %s: %v", turn.ID, err)
	}
}

func putStoredAction(t testing.TB, store *SQLiteStore, call *ActionCall) {
	t.Helper()
	deleteStoredRow(t, store, "action_calls", call.Scope, call.ID)
	if err := insertSQLiteActionCall(context.Background(), store.db, call); err != nil {
		t.Fatalf("put action %s: %v", call.ID, err)
	}
}

func putStoredApproval(t testing.TB, store *SQLiteStore, approval *ApprovalCheckpoint) {
	t.Helper()
	deleteStoredRow(t, store, "approval_checkpoints", approval.Scope, approval.ID)
	if err := insertSQLiteApprovalCheckpoint(context.Background(), store.db, approval); err != nil {
		t.Fatalf("put approval %s: %v", approval.ID, err)
	}
}

// putStoredConversationTask stores task with workRun as the state of its work
// Run, which the store keeps beside the task.
func putStoredConversationTask(t testing.TB, store *SQLiteStore, task *ConversationTask, workRun *AgentRun) {
	t.Helper()
	deleteStoredRow(t, store, "conversation_tasks", task.Scope, task.ID)
	if err := insertSQLiteConversationTask(context.Background(), store.db, task, workRun.Status); err != nil {
		t.Fatalf("put conversation task %s: %v", task.ID, err)
	}
}

func putStoredExternalEndpoint(t testing.TB, store *SQLiteStore, endpoint *ExternalConversationEndpoint) {
	t.Helper()
	deleteStoredRow(t, store, "external_conversation_endpoints", endpoint.Scope, endpoint.ID)
	if err := insertSQLiteExternalConversationEndpoint(context.Background(), store.db, endpoint); err != nil {
		t.Fatalf("put endpoint %s: %v", endpoint.ID, err)
	}
}

func putStoredExternalDelivery(t testing.TB, store *SQLiteStore, delivery *ExternalConversationDelivery) {
	t.Helper()
	deleteStoredRow(t, store, "external_conversation_deliveries", delivery.Scope, delivery.ID)
	if err := insertSQLiteExternalConversationDelivery(context.Background(), store.db, delivery); err != nil {
		t.Fatalf("put delivery %s: %v", delivery.ID, err)
	}
}

func putStoredArtifact(t testing.TB, store *SQLiteStore, artifact *Artifact) {
	t.Helper()
	ctx := context.Background()
	execStoreSQL(t, store, `DELETE FROM artifacts WHERE scope_kind = ? AND scope_id = ? AND id = ? AND version = ?`,
		artifact.Scope.Kind, artifact.Scope.ID, artifact.ID, artifact.Version)
	var latest int64
	if err := store.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM artifacts WHERE scope_kind = ? AND scope_id = ? AND id = ?`,
		artifact.Scope.Kind, artifact.Scope.ID, artifact.ID).Scan(&latest); err != nil {
		t.Fatal(err)
	}
	if err := insertSQLiteArtifactVersion(ctx, store.db, artifact, latest); err != nil {
		t.Fatalf("put artifact %s: %v", artifact.ID, err)
	}
}

func putStoredAgentRequest(t testing.TB, store *SQLiteStore, request *AgentRequest) {
	t.Helper()
	deleteStoredRow(t, store, "agent_requests", request.Scope, request.ID)
	if err := insertSQLiteAgentRequest(context.Background(), store.db, request); err != nil {
		t.Fatalf("put agent request %s: %v", request.ID, err)
	}
}

// The store-connection forms of the raw_sql_test.go helpers below are only
// for runtime tests not yet on a connection of their own. Delete each when
// its last caller has moved to the raw_sql_test.go form.

// rewriteStoredPayload is rewriteRowPayload on the store's own connection.
func rewriteStoredPayload[T any](t testing.TB, store *SQLiteStore, table string, scope Scope, id string, edit func(*T)) {
	t.Helper()
	rewriteRowPayload(t, store.db, table, scope, id, edit)
}

// execStoreSQL runs a statement against the store's database. It keeps its
// own body (execSQL's twin) so the put* fixtures depend on nothing else.
func execStoreSQL(t testing.TB, store *SQLiteStore, query string, args ...any) {
	t.Helper()
	if _, err := store.db.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

// countStoreRows is countRows on the store's own connection.
func countStoreRows(t testing.TB, store *SQLiteStore, table string) int {
	t.Helper()
	return countRows(t, store.db, table)
}

func deleteStoredRow(t testing.TB, store *SQLiteStore, table string, scope Scope, id string) {
	t.Helper()
	execStoreSQL(t, store, `DELETE FROM `+table+` WHERE scope_kind = ? AND scope_id = ? AND id = ?`, scope.Kind, scope.ID, id)
}

// listStoredRoutineEvents is listRoutineEventRows on the store's own
// connection.
func listStoredRoutineEvents(t testing.TB, store *SQLiteStore, scope Scope) []storedRoutineEvent {
	t.Helper()
	return listRoutineEventRows(t, store.db, scope)
}
