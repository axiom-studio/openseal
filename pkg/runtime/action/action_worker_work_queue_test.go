package action_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/action/actiontest"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/runtime/workqueue"
	"github.com/axiom-studio/openseal/pkg/runtime/workqueue/workqueuetest"
)

// deadlineActionStore reports ActionCall deadlines with the PostgreSQL
// store's semantics over the SQLite store, and counts every read.
type deadlineActionStore struct {
	*sqlite.Store
	claims        atomic.Int64
	claimsDone    atomic.Int64
	claimed       atomic.Int64
	deadlineReads atomic.Int64
}

func (s *deadlineActionStore) ClaimNextAction(ctx context.Context, claim models.ActionClaim) (*models.ActionCall, error) {
	s.claims.Add(1)
	call, err := s.Store.ClaimNextAction(ctx, claim)
	s.claimsDone.Add(1)
	if call != nil {
		s.claimed.Add(1)
	}
	return call, err
}

func (s *deadlineActionStore) NextActionDeadline(ctx context.Context, scope models.Scope, now time.Time) (*time.Time, error) {
	s.deadlineReads.Add(1)
	calls, err := s.Store.ListActionCalls(ctx, models.ActionFilter{Scope: scope, Status: []models.ActionCallStatus{models.ActionCallStatusReady, models.ActionCallStatusRunning}, Limit: 500})
	if err != nil {
		return nil, err
	}
	var result *time.Time
	for _, call := range calls {
		if call.Status == models.ActionCallStatusReady && call.AvailableAt.After(now) {
			result = workqueue.EarliestDeadline(result, &call.AvailableAt)
		}
		if call.Status == models.ActionCallStatusRunning && call.LeaseExpiresAt != nil && call.LeaseExpiresAt.After(now) {
			result = workqueue.EarliestDeadline(result, call.LeaseExpiresAt)
		}
	}
	return result, nil
}

func (s *deadlineActionStore) reads() int64 { return s.claims.Load() + s.deadlineReads.Load() }

func TestQueueDrivenActionPoolIssuesNoReadsWhileIdle(t *testing.T) {
	store := &deadlineActionStore{Store: kerneltest.NewStore(t)}
	catalog, scope := actiontest.GovernedCatalog(t)
	pool := actiontest.NewPool(t, store, catalog, scope, 5*time.Millisecond, 4, action.DispatcherFunc(func(context.Context, agentrun.ActionDispatchInput) (map[string]interface{}, error) {
		return nil, errors.New("unexpected idle dispatch")
	}))
	pool.SetWorkSignals(workqueuetest.NewHarness(t).Status)
	pool.Start(t.Context())
	defer pool.Stop()
	workqueuetest.WaitFor(t, time.Second, func() bool { return store.claims.Load() > 0 && store.deadlineReads.Load() > 0 }, "initial probe was not observed")
	time.Sleep(20 * time.Millisecond)
	settled := store.reads()
	time.Sleep(250 * time.Millisecond)
	if reads := store.reads() - settled; reads != 0 {
		t.Fatalf("idle queue-driven pool issued %d reads with a 5ms poll interval", reads)
	}
}

func TestQueueDrivenActionPoolClaimsPromptlyOnScopeWake(t *testing.T) {
	store := &deadlineActionStore{Store: kerneltest.NewStore(t)}
	catalog, scope := actiontest.GovernedCatalog(t)
	dispatched := make(chan struct{}, 1)
	pool := actiontest.NewPool(t, store, catalog, scope, time.Hour, 2, action.DispatcherFunc(func(context.Context, agentrun.ActionDispatchInput) (map[string]interface{}, error) {
		dispatched <- struct{}{}
		return map[string]interface{}{"ok": true}, nil
	}))
	pool.SetWorkSignals(workqueuetest.NewHarness(t).Status)
	pool.Start(t.Context())
	defer pool.Stop()
	// The start-up claim must finish before work exists, or it claims the
	// work itself and hides whether the other scope's wake did.
	workqueuetest.WaitFor(t, time.Second, func() bool { return store.claimsDone.Load() > 0 && store.deadlineReads.Load() > 0 }, "initial probe was not observed")
	actiontest.CreatePoolWork(t, store.Store, catalog, scope)
	pool.WakeScope(models.Scope{Kind: scope.Kind, ID: "another"})
	select {
	case <-dispatched:
		t.Fatal("a signal for another scope woke this pool")
	case <-time.After(50 * time.Millisecond):
	}
	pool.WakeScope(scope)
	select {
	case <-dispatched:
	case <-time.After(time.Second):
		t.Fatal("scope wake did not claim promptly")
	}
}

// scheduleActionRetry claims the ready call as an earlier worker would and
// persists that attempt's outcome: ready again at retryAt.
func scheduleActionRetry(t *testing.T, store models.KernelStore, call *models.ActionCall, retryAt time.Time) {
	t.Helper()
	now := time.Now().UTC()
	claimed, err := store.ClaimNextAction(t.Context(), models.ActionClaim{Scope: call.Scope, WorkerID: "earlier-worker", Now: now, LeaseDuration: time.Minute})
	if err != nil || claimed == nil || claimed.ID != call.ID {
		t.Fatalf("earlier claim = %#v, %v", claimed, err)
	}
	run, err := store.GetAgentRun(t.Context(), call.Scope, call.RunID)
	if err != nil || run == nil {
		t.Fatalf("run = %#v, %v", run, err)
	}
	retry := models.CloneActionCall(claimed)
	retry.Status, retry.AvailableAt, retry.LeaseOwner, retry.LeaseExpiresAt = models.ActionCallStatusReady, retryAt, "", nil
	retry.Revision++
	retry.UpdatedAt = now
	if _, err := store.PersistActionExecution(t.Context(), models.ActionExecutionRecord{
		Call: retry, ExpectedCallRevision: claimed.Revision, ExpectedRunRevision: run.Revision, WorkerID: "earlier-worker", Now: now,
		Event: &models.ActivityEvent{ID: "retry-" + call.ID, Scope: call.Scope, RunID: call.RunID, EventType: "action.retry_scheduled", Summary: "Retry scheduled", CreatedAt: now},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestQueueDrivenActionPoolWakesAtScheduledRetryAndLeaseDeadlines(t *testing.T) {
	for _, scenario := range []string{"retry", "expired_lease"} {
		t.Run(scenario, func(t *testing.T) {
			store := &deadlineActionStore{Store: kerneltest.NewStore(t)}
			catalog, scope := actiontest.GovernedCatalog(t)
			proposal := actiontest.CreatePoolWork(t, store.Store, catalog, scope)
			// The SQLite store compares timestamps as text, so like every
			// runtime caller the fixture passes UTC times.
			due := time.Now().UTC().Add(150 * time.Millisecond)
			if scenario == "retry" {
				// An earlier attempt failed and scheduled its retry.
				scheduleActionRetry(t, store.Store, proposal.Call, due)
			} else {
				// A crashed worker holds the lease until it expires.
				claimed, err := store.Store.ClaimNextAction(t.Context(), models.ActionClaim{Scope: scope, WorkerID: "crashed", Now: time.Now().UTC(), LeaseDuration: time.Until(due)})
				if err != nil || claimed == nil {
					t.Fatalf("crashed claim = %#v, %v", claimed, err)
				}
			}
			pool := actiontest.NewPool(t, store, catalog, scope, time.Hour, 1, action.DispatcherFunc(func(context.Context, agentrun.ActionDispatchInput) (map[string]interface{}, error) {
				return map[string]interface{}{"ok": true}, nil
			}))
			// The pool holds no timer: its deadline goes into the queue,
			// which delivers it back when due.
			harness := workqueuetest.NewHarness(t)
			delivered := atomic.Int64{}
			harness.OnDelivery(func(signal workqueue.WorkSignal) {
				if signal.Family == models.WorkFamilyAction {
					delivered.Add(1)
					pool.WakeScope(signal.Scope)
				}
			})
			pool.SetWorkSignals(harness.Status)
			pool.Start(t.Context())
			defer pool.Stop()
			workqueuetest.WaitFor(t, 2*time.Second, func() bool { return store.claimed.Load() > 0 }, "deadline did not wake the pool")
			if delivered.Load() == 0 {
				t.Fatal("the claim did not come from a queue delivery")
			}
			if time.Now().Before(due) {
				t.Fatal("work was claimed before its deadline")
			}
			if late := time.Since(due); late > 500*time.Millisecond {
				t.Fatalf("deadline claim was %s late", late)
			}
		})
	}
}

func TestQueueDrivenActionPoolPollsWhileQueueIsUnhealthy(t *testing.T) {
	store := &deadlineActionStore{Store: kerneltest.NewStore(t)}
	catalog, scope := actiontest.GovernedCatalog(t)
	status := workqueuetest.NewHarness(t).Status
	pool := actiontest.NewPool(t, store, catalog, scope, 10*time.Millisecond, 1, action.DispatcherFunc(func(context.Context, agentrun.ActionDispatchInput) (map[string]interface{}, error) {
		return map[string]interface{}{"ok": true}, nil
	}))
	pool.SetWorkSignals(status)
	pool.Start(t.Context())
	defer pool.Stop()
	workqueuetest.WaitFor(t, time.Second, func() bool { return store.deadlineReads.Load() > 0 }, "initial probe was not observed")
	status.Observe(workqueue.WorkSignal{Kind: workqueue.WorkSignalDisconnected})
	before := store.claims.Load()
	workqueuetest.WaitFor(t, time.Second, func() bool { return store.claims.Load() >= before+3 }, "unhealthy queue did not restore polling")
	// Reconnecting stops polling again.
	status.Observe(workqueue.WorkSignal{Kind: workqueue.WorkSignalConnected})
	time.Sleep(30 * time.Millisecond)
	settled := store.claims.Load()
	time.Sleep(150 * time.Millisecond)
	if claims := store.claims.Load() - settled; claims != 0 {
		t.Fatalf("reconnected pool kept polling: %d claims", claims)
	}
}

func TestQueueDrivenSupervisorAdmitsScopesWithoutListingAndRetiresIdlePools(t *testing.T) {
	store := &deadlineActionStore{Store: kerneltest.NewStore(t)}
	catalog, scope := actiontest.GovernedCatalog(t)
	source := &workqueuetest.ScopeSource{}
	source.Active.Store(true)
	supervisor, err := action.NewActionWorkerSupervisorRetiringAfter(store, catalog, nil, nil, action.DispatcherFunc(func(context.Context, agentrun.ActionDispatchInput) (map[string]interface{}, error) {
		return map[string]interface{}{"ok": true}, nil
	}), source, nil, action.DynamicActionWorkerConfig{Concurrency: 1, PollInterval: 5 * time.Millisecond, ReconcileInterval: 10 * time.Millisecond}, 60*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	supervisor.SetWorkSignals(workqueuetest.NewHarness(t).Status)
	supervisor.Start(t.Context())
	defer supervisor.Stop()
	time.Sleep(50 * time.Millisecond)
	if lists, pools := source.Lists.Load(), supervisor.ScopeCount(); lists != 0 || pools != 0 {
		t.Fatalf("idle queue-driven supervisor listed scopes %d times and runs %d pools", lists, pools)
	}
	supervisor.WakeScope(scope)
	workqueuetest.WaitFor(t, time.Second, func() bool { return supervisor.ScopeCount() == 1 }, "queued work did not start its scope pool")
	workqueuetest.WaitFor(t, time.Second, func() bool { return supervisor.ScopeCount() == 0 }, "idle pool was not retired")
	if lists, checks := source.Lists.Load(), source.Checks.Load(); lists != 0 || checks != 1 {
		t.Fatalf("admission listed scopes %d times and checked %d times", lists, checks)
	}
	// An inactive scope is never admitted; the cached answer is reused.
	inactive := models.Scope{Kind: "tenant", ID: "suspended"}
	source.Active.Store(false)
	supervisor.WakeScope(inactive)
	workqueuetest.WaitFor(t, time.Second, func() bool { return source.Checks.Load() == 2 }, "inactive scope was not checked")
	time.Sleep(30 * time.Millisecond)
	if pools := supervisor.ScopeCount(); pools != 0 {
		t.Fatalf("inactive scope got %d pools", pools)
	}
}
