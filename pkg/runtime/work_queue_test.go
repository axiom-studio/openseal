package runtime

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// queueHarness binds a status to a memory work queue, like the Engine does,
// and hands each due item to the delivery function under test.
type queueHarness struct {
	status  *WorkSignalStatus
	queue   *MemoryWorkQueue
	deliver atomic.Pointer[func(WorkSignal)]
}

func newQueueHarness(t *testing.T) *queueHarness {
	t.Helper()
	h := &queueHarness{status: NewWorkSignalStatus(), queue: NewMemoryWorkQueue()}
	publish := h.status.BindQueue(h.queue, nil)
	go publish(t.Context())
	go func() {
		_ = h.queue.ConsumeWork(t.Context(), func(signal WorkSignal) {
			h.status.Observe(signal)
			if deliver := h.deliver.Load(); deliver != nil && signal.Kind == WorkSignalChanged {
				(*deliver)(signal)
			}
		})
	}()
	waitForCondition(t, time.Second, h.status.Healthy, "queue did not connect")
	return h
}

func (h *queueHarness) onDelivery(deliver func(WorkSignal)) { h.deliver.Store(&deliver) }

// deadlineActionStore reports ActionCall deadlines with the PostgreSQL
// store's semantics over the in-memory store, and counts every read.
type deadlineActionStore struct {
	*MemoryStore
	claims        atomic.Int64
	claimed       atomic.Int64
	deadlineReads atomic.Int64
}

func (s *deadlineActionStore) ClaimNextAction(ctx context.Context, claim ActionClaim) (*ActionCall, error) {
	s.claims.Add(1)
	call, err := s.MemoryStore.ClaimNextAction(ctx, claim)
	if call != nil {
		s.claimed.Add(1)
	}
	return call, err
}

func (s *deadlineActionStore) NextActionDeadline(_ context.Context, scope Scope, now time.Time) (*time.Time, error) {
	s.deadlineReads.Add(1)
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result *time.Time
	for _, call := range s.actions {
		if call.Scope != scope {
			continue
		}
		if call.Status == ActionCallStatusReady && call.AvailableAt.After(now) {
			result = earliestDeadline(result, &call.AvailableAt)
		}
		if call.Status == ActionCallStatusRunning && call.LeaseExpiresAt != nil && call.LeaseExpiresAt.After(now) {
			result = earliestDeadline(result, call.LeaseExpiresAt)
		}
	}
	return result, nil
}

func (s *deadlineActionStore) reads() int64 { return s.claims.Load() + s.deadlineReads.Load() }

func waitForCondition(t *testing.T, timeout time.Duration, condition func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal(message)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestQueueDrivenActionPoolIssuesNoReadsWhileIdle(t *testing.T) {
	store := &deadlineActionStore{MemoryStore: NewMemoryStore()}
	catalog, scope := governedActionCatalog(t)
	pool := newTestActionPool(t, store, catalog, scope, 5*time.Millisecond, 4, ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
		return nil, errors.New("unexpected idle dispatch")
	}))
	pool.SetWorkSignals(newQueueHarness(t).status)
	pool.Start(t.Context())
	defer pool.Stop()
	waitForCondition(t, time.Second, func() bool { return store.claims.Load() > 0 && store.deadlineReads.Load() > 0 }, "initial probe was not observed")
	time.Sleep(20 * time.Millisecond)
	settled := store.reads()
	time.Sleep(250 * time.Millisecond)
	if reads := store.reads() - settled; reads != 0 {
		t.Fatalf("idle queue-driven pool issued %d reads with a 5ms poll interval", reads)
	}
}

func TestQueueDrivenActionPoolClaimsPromptlyOnScopeWake(t *testing.T) {
	store := &deadlineActionStore{MemoryStore: NewMemoryStore()}
	catalog, scope := governedActionCatalog(t)
	dispatched := make(chan struct{}, 1)
	pool := newTestActionPool(t, store, catalog, scope, time.Hour, 2, ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
		dispatched <- struct{}{}
		return map[string]interface{}{"ok": true}, nil
	}))
	pool.SetWorkSignals(newQueueHarness(t).status)
	pool.Start(t.Context())
	defer pool.Stop()
	waitForCondition(t, time.Second, func() bool { return store.deadlineReads.Load() > 0 }, "initial probe was not observed")
	createActionPoolWork(t, store.MemoryStore, catalog, scope)
	pool.WakeScope(Scope{Kind: scope.Kind, ID: "another"})
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

func TestQueueDrivenActionPoolWakesAtScheduledRetryAndLeaseDeadlines(t *testing.T) {
	for _, scenario := range []string{"retry", "expired_lease"} {
		t.Run(scenario, func(t *testing.T) {
			store := &deadlineActionStore{MemoryStore: NewMemoryStore()}
			catalog, scope := governedActionCatalog(t)
			proposal := createActionPoolWork(t, store.MemoryStore, catalog, scope)
			due := time.Now().Add(150 * time.Millisecond)
			if scenario == "retry" {
				store.mu.Lock()
				store.actions[portfolioKey(scope, proposal.Call.ID)].AvailableAt = due
				store.mu.Unlock()
			} else {
				// A crashed worker holds the lease until it expires.
				claimed, err := store.MemoryStore.ClaimNextAction(t.Context(), ActionClaim{Scope: scope, WorkerID: "crashed", Now: time.Now(), LeaseDuration: time.Until(due)})
				if err != nil || claimed == nil {
					t.Fatalf("crashed claim = %#v, %v", claimed, err)
				}
			}
			pool := newTestActionPool(t, store, catalog, scope, time.Hour, 1, ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
				return map[string]interface{}{"ok": true}, nil
			}))
			// The pool holds no timer: its deadline goes into the queue,
			// which delivers it back when due.
			harness := newQueueHarness(t)
			delivered := atomic.Int64{}
			harness.onDelivery(func(signal WorkSignal) {
				if signal.Family == WorkFamilyAction {
					delivered.Add(1)
					pool.WakeScope(signal.Scope)
				}
			})
			pool.SetWorkSignals(harness.status)
			pool.Start(t.Context())
			defer pool.Stop()
			waitForCondition(t, 2*time.Second, func() bool { return store.claimed.Load() > 0 }, "deadline did not wake the pool")
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
	store := &deadlineActionStore{MemoryStore: NewMemoryStore()}
	catalog, scope := governedActionCatalog(t)
	status := newQueueHarness(t).status
	pool := newTestActionPool(t, store, catalog, scope, 10*time.Millisecond, 1, ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
		return map[string]interface{}{"ok": true}, nil
	}))
	pool.SetWorkSignals(status)
	pool.Start(t.Context())
	defer pool.Stop()
	waitForCondition(t, time.Second, func() bool { return store.deadlineReads.Load() > 0 }, "initial probe was not observed")
	status.Observe(WorkSignal{Kind: WorkSignalDisconnected})
	before := store.claims.Load()
	waitForCondition(t, time.Second, func() bool { return store.claims.Load() >= before+3 }, "unhealthy queue did not restore polling")
	// Reconnecting stops polling again.
	status.Observe(WorkSignal{Kind: WorkSignalConnected})
	time.Sleep(30 * time.Millisecond)
	settled := store.claims.Load()
	time.Sleep(150 * time.Millisecond)
	if claims := store.claims.Load() - settled; claims != 0 {
		t.Fatalf("reconnected pool kept polling: %d claims", claims)
	}
}

// deadlineAgentRunStore counts every read an AgentRunWorkerPool makes.
type deadlineAgentRunStore struct {
	*MemoryStore
	claims        atomic.Int64
	lists         atomic.Int64
	deadlineReads atomic.Int64
	deadline      atomic.Pointer[time.Time]
}

func (s *deadlineAgentRunStore) ClaimNextAgentRunWithDecision(ctx context.Context, claim AgentRunClaim) (*AgentRunAdmissionDecision, error) {
	s.claims.Add(1)
	return s.MemoryStore.ClaimNextAgentRunWithDecision(ctx, claim)
}

func (s *deadlineAgentRunStore) ClaimNextAgentRun(ctx context.Context, claim AgentRunClaim) (*AgentRun, error) {
	s.claims.Add(1)
	return s.MemoryStore.ClaimNextAgentRun(ctx, claim)
}

func (s *deadlineAgentRunStore) ListAgentRuns(ctx context.Context, filter AgentRunFilter) ([]*AgentRun, error) {
	s.lists.Add(1)
	return s.MemoryStore.ListAgentRuns(ctx, filter)
}

func (s *deadlineAgentRunStore) NextAgentRunDeadline(context.Context, Scope, RunKind, string, time.Time) (*time.Time, error) {
	s.deadlineReads.Add(1)
	return s.deadline.Load(), nil
}

func (s *deadlineAgentRunStore) reads() int64 {
	return s.claims.Load() + s.lists.Load() + s.deadlineReads.Load()
}

func TestQueueDrivenAgentRunPoolIssuesNoReadsWhileIdleAndWakesOnDeadline(t *testing.T) {
	store := &deadlineAgentRunStore{MemoryStore: NewMemoryStore()}
	scope := Scope{Kind: "tenant", ID: "idle-runs"}
	pool, err := NewAgentRunWorkerPool(store, TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*TurnRunnerBinding, error) {
		return nil, errors.New("unexpected idle turn")
	}), nil, AgentRunWorkerConfig{Scope: scope, Concurrency: 4, PollInterval: 5 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	harness := newQueueHarness(t)
	harness.onDelivery(func(signal WorkSignal) {
		if signal.Family == WorkFamilyRunDue {
			pool.WakeScope(signal.Scope)
		}
	})
	pool.SetWorkSignals(harness.status)
	pool.Start(t.Context())
	defer pool.Stop()
	waitForCondition(t, time.Second, func() bool { return store.deadlineReads.Load() > 0 }, "initial probe was not observed")
	time.Sleep(20 * time.Millisecond)
	settled := store.reads()
	time.Sleep(250 * time.Millisecond)
	if reads := store.reads() - settled; reads != 0 {
		t.Fatalf("idle queue-driven Run pool issued %d reads with a 5ms poll interval", reads)
	}

	// A wake probes once and schedules the next lease expiry into the
	// queue, which wakes the pool at exactly that deadline.
	due := time.Now().Add(120 * time.Millisecond)
	store.deadline.Store(&due)
	pool.WakeScope(scope)
	waitForCondition(t, time.Second, func() bool { return store.deadlineReads.Load() >= 2 }, "wake did not probe")
	store.deadline.Store(nil)
	afterWake := store.deadlineReads.Load()
	waitForCondition(t, time.Second, func() bool { return store.deadlineReads.Load() > afterWake }, "deadline did not wake the pool")
	if time.Now().Before(due) {
		t.Fatal("pool woke before its deadline")
	}
	time.Sleep(20 * time.Millisecond)
	settled = store.reads()
	time.Sleep(150 * time.Millisecond)
	if reads := store.reads() - settled; reads != 0 {
		t.Fatalf("pool kept reading after its deadline passed: %d reads", reads)
	}
}

type countingScopeSource struct {
	scopes atomic.Pointer[[]Scope]
	lists  atomic.Int64
}

func (s *countingScopeSource) ListWorkerScopes(context.Context) ([]Scope, error) {
	s.lists.Add(1)
	if scopes := s.scopes.Load(); scopes != nil {
		return append([]Scope(nil), (*scopes)...), nil
	}
	return nil, nil
}

// checkedScopeSource answers point lookups and counts both kinds of read.
type checkedScopeSource struct {
	countingScopeSource
	checks atomic.Int64
	active atomic.Bool
}

func (s *checkedScopeSource) WorkerScopeActive(context.Context, Scope) (bool, error) {
	s.checks.Add(1)
	return s.active.Load(), nil
}

func TestQueueDrivenSupervisorAdmitsScopesWithoutListingAndRetiresIdlePools(t *testing.T) {
	store := &deadlineActionStore{MemoryStore: NewMemoryStore()}
	catalog, scope := governedActionCatalog(t)
	source := &checkedScopeSource{}
	source.active.Store(true)
	supervisor, err := NewActionWorkerSupervisor(store, catalog, nil, ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
		return map[string]interface{}{"ok": true}, nil
	}), source, nil, DynamicActionWorkerConfig{Concurrency: 1, PollInterval: 5 * time.Millisecond, ReconcileInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	supervisor.pools.retire = 60 * time.Millisecond
	supervisor.SetWorkSignals(newQueueHarness(t).status)
	supervisor.Start(t.Context())
	defer supervisor.Stop()
	time.Sleep(50 * time.Millisecond)
	if lists, pools := source.lists.Load(), supervisor.ScopeCount(); lists != 0 || pools != 0 {
		t.Fatalf("idle queue-driven supervisor listed scopes %d times and runs %d pools", lists, pools)
	}
	supervisor.WakeScope(scope)
	waitForCondition(t, time.Second, func() bool { return supervisor.ScopeCount() == 1 }, "queued work did not start its scope pool")
	waitForCondition(t, time.Second, func() bool { return supervisor.ScopeCount() == 0 }, "idle pool was not retired")
	if lists, checks := source.lists.Load(), source.checks.Load(); lists != 0 || checks != 1 {
		t.Fatalf("admission listed scopes %d times and checked %d times", lists, checks)
	}
	// An inactive scope is never admitted; the cached answer is reused.
	inactive := Scope{Kind: "tenant", ID: "suspended"}
	source.active.Store(false)
	supervisor.WakeScope(inactive)
	waitForCondition(t, time.Second, func() bool { return source.checks.Load() == 2 }, "inactive scope was not checked")
	time.Sleep(30 * time.Millisecond)
	if pools := supervisor.ScopeCount(); pools != 0 {
		t.Fatalf("inactive scope got %d pools", pools)
	}
}

func TestScopeWakeQueueDefersBusyScopes(t *testing.T) {
	queue := newScopeWakeQueue()
	now := time.Now()
	a, b := Scope{Kind: "tenant", ID: "a"}, Scope{Kind: "tenant", ID: "b"}
	queue.wakeScope(b)
	queue.wakeScope(a)
	ready, deferred := queue.take(now, 0)
	if len(ready) != 2 || ready[0] != a || ready[1] != b || len(deferred) != 0 {
		t.Fatalf("take = %#v %#v", ready, deferred)
	}
	// A scope that just ran is deferred to the end of its spacing.
	queue.wakeScope(b)
	ready, deferred = queue.take(now.Add(time.Second), 5*time.Second)
	if len(ready) != 0 || !deferred[b].Equal(now.Add(5*time.Second)) {
		t.Fatalf("busy scope = %#v %#v", ready, deferred)
	}
	queue.wakeScope(b)
	if ready, _ := queue.take(now.Add(5*time.Second), 5*time.Second); len(ready) != 1 || ready[0] != b {
		t.Fatalf("deferred scope did not run at the end of its spacing: %#v", ready)
	}
	if retry := queue.outcome(a, errors.New("down"), time.Second, now); retry == nil || !retry.After(now) {
		t.Fatalf("failure retry = %v", retry)
	}
	if retry := queue.outcome(a, nil, time.Second, now); retry != nil {
		t.Fatalf("success scheduled a retry at %v", retry)
	}
}

func TestMemoryWorkQueueKeepsEarliestDueAndAsksForRebuild(t *testing.T) {
	queue := NewMemoryWorkQueue()
	scope := Scope{Kind: "tenant", ID: "a"}
	now := time.Now()
	if err := queue.PublishWork(t.Context(), []WorkItem{
		{Family: WorkFamilyAction, Scope: scope, DueAt: now.Add(time.Hour)},
		{Family: WorkFamilyAction, Scope: scope, DueAt: now.Add(40 * time.Millisecond)},
		{Family: WorkFamilyAction, Scope: scope, DueAt: now.Add(time.Minute)},
		{Family: WorkFamilyRun, Scope: scope, DueAt: now.Add(-time.Second)},
		{Family: WorkFamilyRun, Scope: Scope{}, DueAt: now},
	}); err != nil {
		t.Fatal(err)
	}
	signals := make(chan WorkSignal, 8)
	go func() { _ = queue.ConsumeWork(t.Context(), func(signal WorkSignal) { signals <- signal }) }()
	if connected := <-signals; connected.Kind != WorkSignalConnected || !connected.Rebuild {
		t.Fatalf("first signal = %#v", connected)
	}
	if run := <-signals; run.Family != WorkFamilyRun || run.Scope != scope {
		t.Fatalf("overdue item = %#v", run)
	}
	select {
	case action := <-signals:
		if action.Family != WorkFamilyAction || time.Now().Before(now.Add(40*time.Millisecond)) {
			t.Fatalf("action item = %#v", action)
		}
	case <-time.After(time.Second):
		t.Fatal("earliest action deadline was not delivered")
	}
	select {
	case extra := <-signals:
		t.Fatalf("later duplicates were delivered: %#v", extra)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestWritesRows(t *testing.T) {
	for query, writes := range map[string]bool{
		"SELECT 1": false, "\n  select * from x": false, "INSERT INTO x": true, "update x": true,
		"DELETE FROM x": true, "WITH moved AS (DELETE FROM x RETURNING *) SELECT 1": true, "(SELECT 1)": false,
	} {
		if got := writesRows(query); got != writes {
			t.Fatalf("writesRows(%q) = %t", query, got)
		}
	}
}

func TestTerminalRunSweepProgressDoesNotRepeatCompletedSweeps(t *testing.T) {
	progress := newTerminalRunSweepProgress()
	now := time.Now()
	if offset, due := progress.next("a", now); offset != 0 || !due {
		t.Fatalf("new sweep = %d %t", offset, due)
	}
	progress.advance("a", 20)
	if offset, due := progress.next("a", now); offset != 20 || !due {
		t.Fatalf("resumed sweep = %d %t", offset, due)
	}
	progress.finish("a", now)
	if _, due := progress.next("a", now.Add(time.Hour)); due {
		t.Fatal("completed sweep restarted within its repeat interval")
	}
	if offset, due := progress.next("a", now.Add(terminalRunSweepRepeat)); offset != 0 || !due {
		t.Fatalf("sweep did not restart after its repeat interval: %d %t", offset, due)
	}
}

type signalConversationStore struct {
	*MemoryStore
	conversationLists atomic.Int64
}

func (s *signalConversationStore) ListConversations(ctx context.Context, filter ConversationFilter) ([]*Conversation, error) {
	s.conversationLists.Add(1)
	return s.MemoryStore.ListConversations(ctx, filter)
}

func TestQueueDrivenConversationRunReconcilerRunsOnlyQueuedScopes(t *testing.T) {
	store := &signalConversationStore{MemoryStore: NewMemoryStore()}
	scheduler, err := NewConversationRunScheduler(store, store, ConversationRunSchedulerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	scope, other := Scope{Kind: "tenant", ID: "a"}, Scope{Kind: "tenant", ID: "b"}
	source := &checkedScopeSource{}
	source.active.Store(true)
	source.scopes.Store(&[]Scope{scope, other})
	reconciler, err := NewConversationRunReconciler(scheduler, source, nil, ConversationRunReconcilerConfig{Interval: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	harness := newQueueHarness(t)
	harness.onDelivery(func(signal WorkSignal) {
		if signal.Family == WorkFamilyConversationDue {
			reconciler.WakeScope(signal.Scope)
		}
	})
	reconciler.SetWorkSignals(harness.status)
	reconciler.Start(t.Context())
	defer reconciler.Stop()
	// With a queue there is no full pass, at start or on the tick.
	time.Sleep(350 * time.Millisecond)
	if lists, scopes := store.conversationLists.Load(), source.lists.Load(); lists != 0 || scopes != 0 {
		t.Fatalf("idle queue-driven reconciler read %d conversation pages and %d scope lists", lists, scopes)
	}
	reconciler.WakeScope(scope)
	waitForCondition(t, time.Second, func() bool { return store.conversationLists.Load() == 1 }, "scope wake did not reconcile")
	// A second wake within the spacing is deferred through the queue.
	reconciler.WakeScope(scope)
	time.Sleep(30 * time.Millisecond)
	if lists := store.conversationLists.Load(); lists != 1 {
		t.Fatalf("busy scope reconciled again within its spacing: %d", lists)
	}
	waitForCondition(t, time.Second, func() bool { return store.conversationLists.Load() == 2 }, "deferred pass was not delivered")
	if scopes := source.lists.Load(); scopes != 0 {
		t.Fatalf("queued passes listed scopes %d times", scopes)
	}
	// Without a healthy queue the configured tick reconciles every scope.
	harness.status.Observe(WorkSignal{Kind: WorkSignalDisconnected})
	waitForCondition(t, time.Second, func() bool { return source.lists.Load() >= 2 }, "unhealthy queue did not restore the tick")
}
