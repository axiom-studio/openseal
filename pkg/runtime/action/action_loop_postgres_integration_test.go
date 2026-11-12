//go:build integration

package action_test

import (
	"context"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action/actiontest"
	"github.com/axiom-studio/openseal/pkg/runtime/store/postgres"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

// The store scenarios of action_loop_store_test.go run on Postgres through
// actiontest.Stores when OPENSEAL_TEST_POSTGRES_DSN is set. This file holds
// the checks only the Postgres store has.

func openLoopActionPostgresStore(t *testing.T) *postgres.Store {
	t.Helper()
	pgSchema := storetest.NewPostgresSchema(t, "openseal_loop_actions_")
	dsn := pgSchema.DSN
	store, err := postgres.New(context.Background(), dsn, postgres.WithSchema(pgSchema.Name))
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
	f := actiontest.NewLoopStore(t, store)
	later := f.LoopCall("later-loop-call")
	later.AvailableAt = f.Now.Add(time.Hour)
	f.Propose(later)
	f.Propose(f.LoopCall("running-loop-call"))
	if claimed := f.OwnClaim("running-loop-call", f.Now); claimed == nil {
		t.Fatal("own claim failed")
	}
	deadline, err := store.NextActionDeadline(ctx, f.Scope, f.Now)
	if err != nil || deadline != nil {
		t.Fatalf("deadline with only loop calls = %v, %v", deadline, err)
	}

	// A worker-dispatched vm call still sets the deadline. It is admitted
	// through its step fence, as in production.
	worker := f.LoopCall("worker-call")
	worker.Origin, worker.Execution, worker.AvailableAt = models.ActionOriginVM, "", f.Now.Add(2*time.Hour)
	worker.InvocationDigest = models.ComputeActionInvocationDigest(worker)
	worker.SemanticDigest = models.ComputeActionSemanticDigest(worker)
	storetest.SeedActionCall(t, store, worker)
	deadline, err = store.NextActionDeadline(ctx, f.Scope, f.Now)
	if err != nil || deadline == nil || !deadline.Equal(worker.AvailableAt) {
		t.Fatalf("deadline = %v, %v; want the worker call's %v", deadline, err, worker.AvailableAt)
	}
}
