package runtime

import (
	"context"
	"encoding/json"
	"testing"
)

// The put* fixtures write kernel records straight into a SQLiteStore's tables,
// skipping every store rule, for tests that need a state no store method
// produces (a record left by a crashed worker, a tampered row) or that only
// need records to read back. Each replaces the row with the same key. The
// columns mirror the store's own inserts; triggers still project them.

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
	execStoreSQL(t, store, `INSERT INTO agent_turns
		(id, scope_kind, scope_id, run_id, sequence, status, revision, lease_owner, lease_expires_at, created_at, payload)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, turn.ID, turn.Scope.Kind, turn.Scope.ID,
		turn.RunID, turn.Sequence, turn.Status, turn.Revision, turn.LeaseOwner,
		turn.LeaseExpiresAt, turn.CreatedAt, mustJSON(t, turn))
}

func putStoredAction(t testing.TB, store *SQLiteStore, call *ActionCall) {
	t.Helper()
	deleteStoredRow(t, store, "action_calls", call.Scope, call.ID)
	execStoreSQL(t, store, `INSERT INTO action_calls
		(id, scope_kind, scope_id, run_id, turn_id, status, idempotency_key, external_operation_digest, approval_id, available_at,
		 lease_owner, lease_expires_at, revision, created_at, payload, deployment_id, binding_id, binding_revision, skill_id, skill_version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		call.ID, call.Scope.Kind, call.Scope.ID, call.RunID, call.TurnID, call.Status, call.IdempotencyKey,
		externalOperationClaimDigest(call), call.ApprovalID, call.AvailableAt, call.LeaseOwner, call.LeaseExpiresAt, call.Revision, call.CreatedAt, mustJSON(t, call),
		call.DeploymentID, call.BindingID, call.BindingRevision, call.SkillID, call.SkillVersion)
}

func putStoredApproval(t testing.TB, store *SQLiteStore, approval *ApprovalCheckpoint) {
	t.Helper()
	deleteStoredRow(t, store, "approval_checkpoints", approval.Scope, approval.ID)
	execStoreSQL(t, store, `INSERT INTO approval_checkpoints
		(id, scope_kind, scope_id, run_id, action_call_id, status, expires_at, revision, created_at, payload)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, approval.ID, approval.Scope.Kind, approval.Scope.ID,
		approval.RunID, approval.ActionCallID, approval.Status, approval.ExpiresAt, approval.Revision,
		approval.CreatedAt, mustJSON(t, approval))
}

// putStoredConversationTask stores task with workRun as the state of its work
// Run, which the store keeps beside the task.
func putStoredConversationTask(t testing.TB, store *SQLiteStore, task *ConversationTask, workRun *AgentRun) {
	t.Helper()
	deleteStoredRow(t, store, "conversation_tasks", task.Scope, task.ID)
	active := 1
	if isTerminalAgentRunStatus(workRun.Status) {
		active = 0
	}
	execStoreSQL(t, store, `INSERT INTO conversation_tasks
		(scope_kind, scope_id, id, source_run_id, task_key, request_digest, owner_type, owner_id, actor_type, actor_id,
		 conversation_id, thread_root_id, work_run_id, work_status, is_active, created_at, payload)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, task.Scope.Kind, task.Scope.ID,
		task.ID, task.SourceRunID, task.TaskKey, task.RequestDigest,
		task.Owner.Type, task.Owner.ID, task.AuthenticatedActor.Type, task.AuthenticatedActor.ID,
		task.ConversationID, task.ThreadRootID, task.WorkRunID, workRun.Status, active, task.CreatedAt.UTC(), mustJSON(t, task))
}

func putStoredExternalEndpoint(t testing.TB, store *SQLiteStore, endpoint *ExternalConversationEndpoint) {
	t.Helper()
	deleteStoredRow(t, store, "external_conversation_endpoints", endpoint.Scope, endpoint.ID)
	execStoreSQL(t, store, `INSERT INTO external_conversation_endpoints
		(scope_kind,scope_id,id,ingress_route,owner_type,owner_id,provider,status,revision,updated_at,payload)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		endpoint.Scope.Kind, endpoint.Scope.ID, endpoint.ID, endpoint.IngressRoute,
		endpoint.Owner.Type, endpoint.Owner.ID, endpoint.Provider, endpoint.Status,
		endpoint.Revision, endpoint.UpdatedAt, mustJSON(t, endpoint))
}

func putStoredExternalDelivery(t testing.TB, store *SQLiteStore, delivery *ExternalConversationDelivery) {
	t.Helper()
	deleteStoredRow(t, store, "external_conversation_deliveries", delivery.Scope, delivery.ID)
	execStoreSQL(t, store, `INSERT INTO external_conversation_deliveries
		(scope_kind,scope_id,id,endpoint_id,endpoint_revision,conversation_id,channel_message_id,idempotency_key,status,
		 available_at,lease_owner,lease_expires_at,created_at,revision,payload)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, delivery.Scope.Kind, delivery.Scope.ID, delivery.ID, delivery.EndpointID,
		delivery.EndpointRevision, delivery.ConversationID, delivery.ChannelMessageID, delivery.IdempotencyKey, delivery.Status,
		delivery.AvailableAt, delivery.LeaseOwner, nullableSQLiteTime(delivery.LeaseExpiresAt), delivery.CreatedAt, delivery.Revision, mustJSON(t, delivery))
}

func putStoredArtifact(t testing.TB, store *SQLiteStore, artifact *Artifact) {
	t.Helper()
	execStoreSQL(t, store, `DELETE FROM artifacts WHERE scope_kind = ? AND scope_id = ? AND id = ? AND version = ?`,
		artifact.Scope.Kind, artifact.Scope.ID, artifact.ID, artifact.Version)
	execStoreSQL(t, store, `INSERT INTO artifacts
		(scope_kind, scope_id, id, version, type, media_type, classification, digest, content_ref,
		 producer_run_id, producer_request_id, created_at, payload)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		artifact.Scope.Kind, artifact.Scope.ID, artifact.ID, artifact.Version, artifact.Type, artifact.MediaType,
		artifact.Classification, artifact.Digest, artifact.ContentRef, artifact.Provenance.RunID,
		artifact.Provenance.RequestID, artifact.CreatedAt, mustJSON(t, artifact))
}

func putStoredAgentRequest(t testing.TB, store *SQLiteStore, request *AgentRequest) {
	t.Helper()
	deleteStoredRow(t, store, "agent_requests", request.Scope, request.ID)
	execStoreSQL(t, store, `INSERT INTO agent_requests
		(scope_kind, scope_id, id, kind, status, requester_type, requester_id, recipient_type, recipient_id,
		 source_run_id, child_run_id, idempotency_key, revision, created_at, updated_at, payload)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		request.Scope.Kind, request.Scope.ID, request.ID, request.Kind, request.Status,
		request.Requester.Type, request.Requester.ID, request.Recipient.Type, request.Recipient.ID,
		request.SourceRunID, request.ChildRunID, request.IdempotencyKey, request.Revision,
		request.CreatedAt, request.UpdatedAt, mustJSON(t, request))
}

// rewriteStoredPayload decodes the JSON payload of one scoped row of table,
// applies edit and writes it back. It bypasses every store rule, so a test can
// model a tampered or inconsistent row that no store method would produce.
// Indexed columns are not touched; edit only payload-only fields with it.
func rewriteStoredPayload[T any](t testing.TB, store *SQLiteStore, table string, scope Scope, id string, edit func(*T)) {
	t.Helper()
	ctx := context.Background()
	var payload []byte
	if err := store.db.QueryRowContext(ctx, `SELECT payload FROM `+table+` WHERE scope_kind = ? AND scope_id = ? AND id = ?`, scope.Kind, scope.ID, id).Scan(&payload); err != nil {
		t.Fatalf("read %s %s: %v", table, id, err)
	}
	var value T
	if err := json.Unmarshal(payload, &value); err != nil {
		t.Fatalf("decode %s %s: %v", table, id, err)
	}
	edit(&value)
	encoded, err := json.Marshal(&value)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE `+table+` SET payload = ? WHERE scope_kind = ? AND scope_id = ? AND id = ?`, string(encoded), scope.Kind, scope.ID, id); err != nil {
		t.Fatalf("write %s %s: %v", table, id, err)
	}
}

// execStoreSQL runs a statement against the store's database.
func execStoreSQL(t testing.TB, store *SQLiteStore, query string, args ...any) {
	t.Helper()
	if _, err := store.db.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

// countStoreRows counts the rows of table.
func countStoreRows(t testing.TB, store *SQLiteStore, table string) int {
	t.Helper()
	var count int
	if err := store.db.QueryRowContext(context.Background(), `SELECT count(*) FROM `+table).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func deleteStoredRow(t testing.TB, store *SQLiteStore, table string, scope Scope, id string) {
	t.Helper()
	execStoreSQL(t, store, `DELETE FROM `+table+` WHERE scope_kind = ? AND scope_id = ? AND id = ?`, scope.Kind, scope.ID, id)
}

// getStoredRun reads a Run that the test knows exists.
func getStoredRun(t testing.TB, store *SQLiteStore, scope Scope, id string) *AgentRun {
	t.Helper()
	run, err := store.GetAgentRun(context.Background(), scope, id)
	if err != nil {
		t.Fatalf("get run %s: %v", id, err)
	}
	return run
}

// storedRoutineEvent is one row of routine_events. KeepsData reports whether
// the row still holds the event's payload, summary and fields.
type storedRoutineEvent struct {
	ActivationID, EventID string
	State                 RoutineEventState
	DroppedReason         string
	KeepsData             bool
}

func listStoredRoutineEvents(t testing.TB, store *SQLiteStore, scope Scope) []storedRoutineEvent {
	t.Helper()
	rows, err := store.db.QueryContext(context.Background(), `SELECT activation_id, event_id, state, COALESCE(dropped_reason, ''), payload IS NOT NULL
		FROM routine_events WHERE scope_kind = ? AND scope_id = ? ORDER BY activation_id, event_id`, scope.Kind, scope.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var events []storedRoutineEvent
	for rows.Next() {
		var event storedRoutineEvent
		if err := rows.Scan(&event.ActivationID, &event.EventID, &event.State, &event.DroppedReason, &event.KeepsData); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return events
}
