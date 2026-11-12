//go:build integration

package runtime

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// The store scenarios of action_loop_store_test.go run on Postgres through
// nestedActionStores when OPENSEAL_TEST_POSTGRES_DSN is set. This file holds
// the checks only the Postgres store has.

func openLoopActionPostgresStore(t *testing.T) *PostgresStore {
	t.Helper()
	pgSchema := newTestPostgresSchema(t, "openseal_loop_actions_")
	dsn := pgSchema.dsn
	store, err := NewPostgresStore(context.Background(), dsn, WithPostgresSchema(pgSchema.name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})
	return store
}

func TestNextActionDeadlineIgnoresLoopCalls(t *testing.T) {
	ctx := context.Background()
	store := openLoopActionPostgresStore(t)
	f := newLoopStoreFixture(t, store)
	later := f.loopCall("later-loop-call")
	later.AvailableAt = f.now.Add(time.Hour)
	f.propose(later)
	f.propose(f.loopCall("running-loop-call"))
	if claimed := f.ownClaim("running-loop-call", f.now); claimed == nil {
		t.Fatal("own claim failed")
	}
	deadline, err := store.NextActionDeadline(ctx, f.scope, f.now)
	if err != nil || deadline != nil {
		t.Fatalf("deadline with only loop calls = %v, %v", deadline, err)
	}

	// A worker-dispatched vm call still sets the deadline. It is admitted
	// through its step fence in production; the test stores it directly.
	worker := f.loopCall("worker-call")
	worker.Origin, worker.Execution, worker.AvailableAt = ActionOriginVM, "", f.now.Add(2*time.Hour)
	worker.InvocationDigest = ComputeActionInvocationDigest(worker)
	worker.SemanticDigest = ComputeActionSemanticDigest(worker)
	payload, err := json.Marshal(worker)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO `+store.table("action_calls")+`
		(id, scope_kind, scope_id, run_id, turn_id, status, idempotency_key, external_operation_digest, approval_id, available_at,
		 lease_owner, lease_expires_at, revision, created_at, payload, deployment_id, binding_id, binding_revision, skill_id, skill_version)
		VALUES ($1,$2,$3,$4,$5,$6,$7,'','',$8,'',NULL,1,$9,$10::jsonb,$11,'',0,$12,$13)`,
		worker.ID, worker.Scope.Kind, worker.Scope.ID, worker.RunID, worker.TurnID, worker.Status, worker.IdempotencyKey, worker.AvailableAt,
		worker.CreatedAt, string(payload), worker.DeploymentID, worker.SkillID, worker.SkillVersion); err != nil {
		t.Fatal(err)
	}
	deadline, err = store.NextActionDeadline(ctx, f.scope, f.now)
	if err != nil || deadline == nil || !deadline.Equal(worker.AvailableAt) {
		t.Fatalf("deadline = %v, %v; want the worker call's %v", deadline, err, worker.AvailableAt)
	}
}
