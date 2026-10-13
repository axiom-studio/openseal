package runtime

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func healthyWorkSignals() *WorkSignalStatus {
	status := NewWorkSignalStatus()
	status.Observe(WorkSignal{Kind: WorkSignalConnected})
	return status
}

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

func TestSignalDrivenActionPoolIssuesNoReadsWhileIdle(t *testing.T) {
	store := &deadlineActionStore{MemoryStore: NewMemoryStore()}
	catalog, scope := governedActionCatalog(t)
	pool := newTestActionPool(t, store, catalog, scope, 5*time.Millisecond, 4, ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
		return nil, errors.New("unexpected idle dispatch")
	}))
	pool.SetWorkSignals(healthyWorkSignals())
	pool.Start(t.Context())
	defer pool.Stop()
	waitForCondition(t, time.Second, func() bool { return store.claims.Load() > 0 && store.deadlineReads.Load() > 0 }, "initial probe was not observed")
	time.Sleep(20 * time.Millisecond)
	settled := store.reads()
	time.Sleep(250 * time.Millisecond)
	if reads := store.reads() - settled; reads != 0 {
		t.Fatalf("idle signal-driven pool issued %d reads with a 5ms poll interval", reads)
	}
}

func TestSignalDrivenActionPoolClaimsPromptlyOnScopeWake(t *testing.T) {
	store := &deadlineActionStore{MemoryStore: NewMemoryStore()}
	catalog, scope := governedActionCatalog(t)
	dispatched := make(chan struct{}, 1)
	pool := newTestActionPool(t, store, catalog, scope, time.Hour, 2, ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
		dispatched <- struct{}{}
		return map[string]interface{}{"ok": true}, nil
	}))
	pool.SetWorkSignals(healthyWorkSignals())
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

func TestSignalDrivenActionPoolSleepsUntilRetryAndLeaseDeadlines(t *testing.T) {
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
			pool.SetWorkSignals(healthyWorkSignals())
			pool.Start(t.Context())
			defer pool.Stop()
			waitForCondition(t, 2*time.Second, func() bool { return store.claimed.Load() > 0 }, "deadline did not wake the pool")
			if time.Now().Before(due) {
				t.Fatal("work was claimed before its deadline")
			}
			if late := time.Since(due); late > 500*time.Millisecond {
				t.Fatalf("deadline claim was %s late", late)
			}
		})
	}
}

func TestSignalDrivenActionPoolPollsWhileListenerIsUnhealthy(t *testing.T) {
	store := &deadlineActionStore{MemoryStore: NewMemoryStore()}
	catalog, scope := governedActionCatalog(t)
	status := healthyWorkSignals()
	pool := newTestActionPool(t, store, catalog, scope, 10*time.Millisecond, 1, ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
		return map[string]interface{}{"ok": true}, nil
	}))
	pool.SetWorkSignals(status)
	pool.Start(t.Context())
	defer pool.Stop()
	waitForCondition(t, time.Second, func() bool { return store.deadlineReads.Load() > 0 }, "initial probe was not observed")
	status.Observe(WorkSignal{Kind: WorkSignalDisconnected})
	before := store.claims.Load()
	waitForCondition(t, time.Second, func() bool { return store.claims.Load() >= before+3 }, "unhealthy listener did not restore polling")
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

func TestSignalDrivenAgentRunPoolIssuesNoReadsWhileIdleAndWakesOnDeadline(t *testing.T) {
	store := &deadlineAgentRunStore{MemoryStore: NewMemoryStore()}
	scope := Scope{Kind: "tenant", ID: "idle-runs"}
	pool, err := NewAgentRunWorkerPool(store, TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*TurnRunnerBinding, error) {
		return nil, errors.New("unexpected idle turn")
	}), nil, AgentRunWorkerConfig{Scope: scope, Concurrency: 4, PollInterval: 5 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	pool.SetWorkSignals(healthyWorkSignals())
	pool.Start(t.Context())
	defer pool.Stop()
	waitForCondition(t, time.Second, func() bool { return store.deadlineReads.Load() > 0 }, "initial probe was not observed")
	time.Sleep(20 * time.Millisecond)
	settled := store.reads()
	time.Sleep(250 * time.Millisecond)
	if reads := store.reads() - settled; reads != 0 {
		t.Fatalf("idle signal-driven Run pool issued %d reads with a 5ms poll interval", reads)
	}

	// A wake probes once and learns the next lease expiry; the pool then
	// sleeps until exactly that deadline.
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

func TestSignalDrivenSupervisorDiscoversScopesFromSignalsInsteadOfPolling(t *testing.T) {
	store := &deadlineActionStore{MemoryStore: NewMemoryStore()}
	catalog, scope := governedActionCatalog(t)
	source := &countingScopeSource{}
	supervisor, err := NewActionWorkerSupervisor(store, catalog, nil, ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
		return map[string]interface{}{"ok": true}, nil
	}), source, nil, DynamicActionWorkerConfig{Concurrency: 1, PollInterval: 5 * time.Millisecond, ReconcileInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	supervisor.SetWorkSignals(healthyWorkSignals())
	supervisor.Start(t.Context())
	defer supervisor.Stop()
	waitForCondition(t, time.Second, func() bool { return source.lists.Load() == 1 }, "initial scope listing was not observed")
	time.Sleep(100 * time.Millisecond)
	if lists := source.lists.Load(); lists != 1 {
		t.Fatalf("healthy supervisor listed scopes %d times with a 10ms interval", lists)
	}
	source.scopes.Store(&[]Scope{scope})
	supervisor.WakeScope(scope)
	waitForCondition(t, time.Second, func() bool { return supervisor.ScopeCount() == 1 }, "signal for a new scope did not start its pool")
}

func TestScopeWakeQueueMergesSignalsAndDueDeadlines(t *testing.T) {
	queue := newScopeWakeQueue()
	now := time.Now()
	a, b, c := Scope{Kind: "tenant", ID: "a"}, Scope{Kind: "tenant", ID: "b"}, Scope{Kind: "tenant", ID: "c"}
	queue.wakeScope(b)
	queue.setDeadline(a, new(now.Add(-time.Millisecond)))
	queue.setDeadline(c, new(now.Add(time.Minute)))
	full, scopes := queue.take(now, 0)
	if full || len(scopes) != 2 || scopes[0] != a || scopes[1] != b {
		t.Fatalf("take = %t %#v", full, scopes)
	}
	if next := queue.nextDeadline(); next == nil || !next.Equal(now.Add(time.Minute)) {
		t.Fatalf("next deadline = %v", next)
	}
	queue.wakeAll()
	if full, _ := queue.take(now, 0); !full {
		t.Fatal("wakeAll did not request a full pass")
	}
	// A scope that just ran coalesces further signals until its spacing ends.
	queue.wakeScope(b)
	if _, scopes := queue.take(now.Add(time.Second), 5*time.Second); len(scopes) != 0 {
		t.Fatalf("busy scope ran again within its spacing: %#v", scopes)
	}
	if next := queue.nextDeadline(); next == nil || !next.Equal(now.Add(5*time.Second)) {
		t.Fatalf("deferred deadline = %v", next)
	}
	if _, scopes := queue.take(now.Add(5*time.Second), 5*time.Second); len(scopes) != 1 || scopes[0] != b {
		t.Fatalf("deferred scope did not run at the end of its spacing: %#v", scopes)
	}
}

func TestParseWorkSignal(t *testing.T) {
	signal, ok := parseWorkSignal("run:tenant:abc:def")
	if !ok || signal.Family != WorkFamilyRun || signal.Scope != (Scope{Kind: "tenant", ID: "abc:def"}) {
		t.Fatalf("signal = %#v, %t", signal, ok)
	}
	for _, payload := range []string{"", "run", "run:tenant", ":tenant:a"} {
		if _, ok := parseWorkSignal(payload); ok {
			t.Fatalf("parsed invalid payload %q", payload)
		}
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

func TestSignalDrivenConversationRunReconcilerRunsOnlySignalledScopes(t *testing.T) {
	store := &signalConversationStore{MemoryStore: NewMemoryStore()}
	scheduler, err := NewConversationRunScheduler(store, store, ConversationRunSchedulerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	scope, other := Scope{Kind: "tenant", ID: "a"}, Scope{Kind: "tenant", ID: "b"}
	source := &countingScopeSource{}
	source.scopes.Store(&[]Scope{scope, other})
	reconciler, err := NewConversationRunReconciler(scheduler, source, nil, ConversationRunReconcilerConfig{Interval: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	status := healthyWorkSignals()
	reconciler.SetWorkSignals(status)
	reconciler.Start(t.Context())
	defer reconciler.Stop()
	// Start reconciles every scope once.
	waitForCondition(t, time.Second, func() bool { return store.conversationLists.Load() == 2 }, "initial full pass was not observed")
	time.Sleep(350 * time.Millisecond)
	if lists, scopes := store.conversationLists.Load(), source.lists.Load(); lists != 2 || scopes != 1 {
		t.Fatalf("idle healthy reconciler read %d conversation pages and %d scope lists", lists, scopes)
	}
	reconciler.WakeScope(scope)
	waitForCondition(t, time.Second, func() bool { return store.conversationLists.Load() == 3 }, "scope wake did not reconcile")
	time.Sleep(50 * time.Millisecond)
	if lists, scopes := store.conversationLists.Load(), source.lists.Load(); lists != 3 || scopes != 1 {
		t.Fatalf("scope wake read %d conversation pages and %d scope lists", lists, scopes)
	}
	// Without a healthy listener the configured tick reconciles every scope.
	status.Observe(WorkSignal{Kind: WorkSignalDisconnected})
	waitForCondition(t, time.Second, func() bool { return source.lists.Load() >= 3 }, "unhealthy listener did not restore the tick")
}
