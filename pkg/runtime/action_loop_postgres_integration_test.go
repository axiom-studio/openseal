//go:build integration

package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

// The store scenarios of action_loop_store_test.go run on Postgres through
// nestedActionStores when OPENSEAL_TEST_POSTGRES_DSN is set. This file holds
// the checks only the Postgres store has.

func openLoopActionPostgresStore(t *testing.T) *PostgresStore {
	t.Helper()
	pgSchema := storetest.NewPostgresSchema(t, "openseal_loop_actions_")
	dsn := pgSchema.DSN
	store, err := NewPostgresStore(context.Background(), dsn, WithPostgresSchema(pgSchema.Name))
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
	// through its step fence, as in production.
	worker := f.loopCall("worker-call")
	worker.Origin, worker.Execution, worker.AvailableAt = ActionOriginVM, "", f.now.Add(2*time.Hour)
	worker.InvocationDigest = ComputeActionInvocationDigest(worker)
	worker.SemanticDigest = ComputeActionSemanticDigest(worker)
	seedActionCall(t, store, worker)
	deadline, err = store.NextActionDeadline(ctx, f.scope, f.now)
	if err != nil || deadline == nil || !deadline.Equal(worker.AvailableAt) {
		t.Fatalf("deadline = %v, %v; want the worker call's %v", deadline, err, worker.AvailableAt)
	}
}
