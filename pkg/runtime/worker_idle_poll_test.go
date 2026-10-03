package runtime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
)

type countingActionPoolStore struct {
	*MemoryStore
	claims    atomic.Int64
	empty     atomic.Int64
	hydration atomic.Int64
	failRead  bool
	entered   chan struct{}
	release   chan struct{}
}

func (s *countingActionPoolStore) ClaimNextAction(ctx context.Context, claim ActionClaim) (*ActionCall, error) {
	n := s.claims.Add(1)
	call, err := s.MemoryStore.ClaimNextAction(ctx, claim)
	if n == 1 && s.entered != nil {
		close(s.entered)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-s.release:
		}
	}
	if call == nil && err == nil {
		s.empty.Add(1)
	}
	return call, err
}

func (s *countingActionPoolStore) GetAgentRun(ctx context.Context, scope Scope, id string) (*AgentRun, error) {
	s.hydration.Add(1)
	if s.failRead {
		return nil, errors.New("temporary Run hydration failure")
	}
	return s.MemoryStore.GetAgentRun(ctx, scope, id)
}

func TestActionWorkerPoolCoalescesIdleClaimsBeforeHydration(t *testing.T) {
	store := &countingActionPoolStore{MemoryStore: NewMemoryStore()}
	pool, err := NewActionWorkerPool(store, skill.NewCatalog(), nil,
		ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
			return nil, errors.New("unexpected idle dispatch")
		}), nil, ActionWorkerConfig{Scope: Scope{Kind: "tenant", ID: "idle"}, Concurrency: 8, PollInterval: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	pool.Start(t.Context())
	time.Sleep(160 * time.Millisecond)
	pool.Stop()
	if claims := store.claims.Load(); claims < 2 || claims > 16 {
		t.Fatalf("idle claims scaled with concurrency: %d", claims)
	}
	if got := store.hydration.Load(); got != 0 {
		t.Fatalf("empty claims hydrated %d Runs", got)
	}
}

func TestActionWorkerPoolWakeSurvivesEmptyClaimRace(t *testing.T) {
	store := &countingActionPoolStore{MemoryStore: NewMemoryStore(), entered: make(chan struct{}), release: make(chan struct{})}
	catalog, scope := governedActionCatalog(t)
	dispatched := make(chan struct{}, 1)
	pool := newTestActionPool(t, store, catalog, scope, time.Hour, 1, ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
		dispatched <- struct{}{}
		return map[string]interface{}{"ok": true}, nil
	}))
	pool.Start(t.Context())
	defer pool.Stop()
	select {
	case <-store.entered:
	case <-time.After(time.Second):
		t.Fatal("initial empty claim was not reached")
	}
	createActionPoolWork(t, store.MemoryStore, catalog, scope)
	pool.Wake()
	close(store.release)
	select {
	case <-dispatched:
	case <-time.After(time.Second):
		t.Fatal("enqueue between empty claim and wait lost its wake")
	}
}

func TestActionWorkerPoolRecoversMissedWakeAndRestart(t *testing.T) {
	for _, scenario := range []string{"missed_wake", "restart", "wake_after_idle"} {
		t.Run(scenario, func(t *testing.T) {
			store := &countingActionPoolStore{MemoryStore: NewMemoryStore()}
			catalog, scope := governedActionCatalog(t)
			dispatched := make(chan struct{}, 1)
			interval := 25 * time.Millisecond
			if scenario != "missed_wake" {
				interval = time.Hour
			}
			pool := newTestActionPool(t, store, catalog, scope, interval, 2, ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
				dispatched <- struct{}{}
				return map[string]interface{}{"ok": true}, nil
			}))
			pool.Start(t.Context())
			deadline := time.Now().Add(time.Second)
			for store.empty.Load() == 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if store.empty.Load() == 0 {
				pool.Stop()
				t.Fatal("initial empty claim was not observed")
			}
			if scenario == "restart" {
				pool.Stop()
			}
			createActionPoolWork(t, store.MemoryStore, catalog, scope)
			started := time.Now()
			if scenario == "restart" {
				pool.Start(t.Context())
			} else if scenario == "wake_after_idle" {
				pool.Wake()
			}
			defer pool.Stop()
			select {
			case <-dispatched:
				if scenario == "missed_wake" && time.Since(started) > 500*time.Millisecond {
					t.Fatal("missed notification changed the configured polling latency")
				}
			case <-time.After(time.Second):
				t.Fatal("durable work was not recovered")
			}
		})
	}
}

func TestActionWorkerPoolHandsOffClaimBeforeProviderAndDrainsBacklog(t *testing.T) {
	store := NewMemoryStore()
	catalog, scope := governedActionCatalog(t)
	const calls = 8
	for i := 0; i < calls; i++ {
		createActionPoolWork(t, store, catalog, scope)
	}
	started, completed := make(chan struct{}, calls), make(chan struct{}, calls)
	release := make(chan struct{})
	pool := newTestActionPool(t, store, catalog, scope, time.Hour, 2, ActionDispatcherFunc(func(ctx context.Context, _ ActionDispatchInput) (map[string]interface{}, error) {
		started <- struct{}{}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
		}
		completed <- struct{}{}
		return map[string]interface{}{"ok": true}, nil
	}))
	pool.Start(t.Context())
	defer pool.Stop()
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			close(release)
			t.Fatal("claim handoff serialized provider execution")
		}
	}
	close(release)
	for i := 0; i < calls; i++ {
		select {
		case <-completed:
		case <-time.After(time.Second):
			t.Fatal("sustained backlog waited for the hour-long fallback timer")
		}
	}
}

func newTestActionPool(t *testing.T, store KernelStore, catalog *skill.Catalog, scope Scope, interval time.Duration, concurrency int, dispatcher ActionDispatcher) *ActionWorkerPool {
	t.Helper()
	pool, err := NewActionWorkerPool(store, catalog, CredentialResolverFunc(func(context.Context, CredentialResolutionRequest) (map[string]string, error) {
		return map[string]string{"token": "test-secret"}, nil
	}), dispatcher, nil, ActionWorkerConfig{Scope: scope, Concurrency: concurrency, PollInterval: interval, LeaseDuration: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return pool
}

func createActionPoolWork(t *testing.T, store KernelStore, catalog *skill.Catalog, scope Scope) *ActionProposalResult {
	t.Helper()
	now := time.Now().Add(-2 * time.Second)
	portfolio := NewPortfolioService(store)
	portfolio.now = func() time.Time { return now }
	run, err := portfolio.CreateAgentRun(t.Context(), CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "release-team"}, AssignedAgentID: "release-agent", Goal: "deploy", Source: RunSourceObjective,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimNextAgentRun(t.Context(), AgentRunClaim{Scope: scope, WorkerID: "agent-worker", Now: now, LeaseDuration: time.Minute, AgingInterval: time.Minute})
	if err != nil || claimed == nil || claimed.ID != run.ID {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	coordinator := NewActionCoordinator(store, store, catalog, ActionPolicyEvaluatorFunc(func(context.Context, ActionPolicyInput) (ActionPolicyDecision, error) {
		return ActionPolicyDecision{Disposition: ActionDispositionAllow}, nil
	}))
	coordinator.now = func() time.Time { return now.Add(time.Second) }
	proposal, err := coordinator.Propose(t.Context(), ProposeActionRequest{
		Scope: scope, RunID: claimed.ID, WorkerID: "agent-worker", DeploymentID: "release-agent",
		SkillID: "release", SkillVersion: "1.0.0", Action: "deploy", Arguments: map[string]interface{}{"environment": "production"}, IdempotencyKey: "deploy-" + run.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return proposal
}

func TestWorkerPollSignalPreservesNotificationBeforeWait(t *testing.T) {
	poll := newWorkerPollSignal()
	generation, wake := poll.snapshot()
	poll.notify()
	current, _ := poll.snapshot()
	if current == generation {
		t.Fatalf("committed enqueue did not advance generation: %d", current)
	}
	if !waitForWorkerPoll(t.Context(), wake, time.Hour) {
		t.Fatal("committed wake did not survive the later wait")
	}
}

func TestActionWorkerPoolBacksOffAfterClaimedRunHydrationFailure(t *testing.T) {
	store := &countingActionPoolStore{MemoryStore: NewMemoryStore(), failRead: true}
	catalog, scope := governedActionCatalog(t)
	for range 20 {
		createActionPoolWork(t, store.MemoryStore, catalog, scope)
	}
	pool := newTestActionPool(t, store, catalog, scope, 50*time.Millisecond, 1, ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
		return nil, errors.New("hydration failure must prevent dispatch")
	}))
	pool.Start(t.Context())
	defer pool.Stop()
	deadline := time.Now().Add(time.Second)
	for store.hydration.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if store.hydration.Load() == 0 {
		t.Fatal("claimed Run hydration failure was not reached")
	}
	time.Sleep(40 * time.Millisecond)
	if got := store.claims.Load(); got != 1 {
		t.Fatalf("internal successful-claim handoff bypassed error backoff: claims=%d", got)
	}
}

func TestActionWorkerPoolSerializesStopBeforeConcurrentRestart(t *testing.T) {
	store := NewMemoryStore()
	catalog, scope := governedActionCatalog(t)
	createActionPoolWork(t, store, catalog, scope)
	entered, stopping, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	pool := newTestActionPool(t, store, catalog, scope, time.Hour, 1, ActionDispatcherFunc(func(ctx context.Context, _ ActionDispatchInput) (map[string]interface{}, error) {
		close(entered)
		<-ctx.Done()
		close(stopping)
		<-release
		return nil, ctx.Err()
	}))
	pool.Start(t.Context())
	select {
	case <-entered:
	case <-time.After(time.Second):
		close(release)
		pool.Stop()
		t.Fatal("initial worker did not dispatch")
	}
	stopped := make(chan struct{})
	go func() { pool.Stop(); close(stopped) }()
	select {
	case <-stopping:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("Stop did not cancel the first generation")
	}
	restarted := make(chan struct{})
	go func() { pool.Start(t.Context()); close(restarted) }()
	select {
	case <-restarted:
		close(release)
		pool.Stop()
		t.Fatal("Start overlapped a worker generation still being drained")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Stop did not finish draining its own generation")
	}
	select {
	case <-restarted:
	case <-time.After(time.Second):
		t.Fatal("Start did not proceed after Stop completed")
	}
	pool.Stop()
}

func TestWorkerPollSignalBroadcastsAndCancelsAllWaiters(t *testing.T) {
	poll := newWorkerPollSignal()
	_, wake := poll.snapshot()
	const waiters = 8
	var group sync.WaitGroup
	for range waiters {
		group.Add(1)
		go func() {
			defer group.Done()
			if !waitForWorkerPoll(t.Context(), wake, time.Hour) {
				t.Error("pool notification did not wake a waiter")
			}
		}()
	}
	poll.notify()
	group.Wait()
	_, nextWake := poll.snapshot()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if waitForWorkerPoll(ctx, nextWake, time.Hour) {
		t.Fatal("canceled worker waited on a pool wake")
	}
}

func TestWorkerPoolScopeWakeDoesNotWakeAnotherTenant(t *testing.T) {
	scope := Scope{Kind: "tenant", ID: "mine"}
	pool := newTestActionPool(t, NewMemoryStore(), skill.NewCatalog(), scope, time.Hour, 1,
		ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) { return nil, nil }))
	pool.WakeScope(Scope{Kind: "tenant", ID: "other"})
	select {
	case <-pool.wake:
		t.Fatal("scope-specific wake crossed the tenant boundary")
	default:
	}
	pool.WakeScope(scope)
	select {
	case <-pool.wake:
	default:
		t.Fatal("scope-specific wake missed its own tenant")
	}
}

func TestAgentRunWorkerObservesTimerDeadlineWithoutWaitingForFallback(t *testing.T) {
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "timer-deadline"}
	_, err := NewPortfolioService(store).CreateAgentRun(t.Context(), CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent", Goal: "sleep then resume", Source: RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	var turns atomic.Int64
	completed := make(chan struct{}, 1)
	pool, err := NewAgentRunWorkerPool(store, TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*TurnRunnerBinding, error) {
		return &TurnRunnerBinding{DefinitionID: "agent", DefinitionVersion: "1", Runner: TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
			if turns.Add(1) == 1 {
				due := time.Now().Add(30 * time.Millisecond)
				return &TurnOutcome{NextRunStatus: AgentRunStatusSleeping, WakeCondition: &WakeCondition{Type: "timer", WakeAt: &due}, OutputSummary: "waiting"}, nil
			}
			completed <- struct{}{}
			return &TurnOutcome{NextRunStatus: AgentRunStatusCompleted, OutputSummary: "finished"}, nil
		})}, nil
	}), nil, AgentRunWorkerConfig{Scope: scope, PollInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	pool.Start(t.Context())
	defer pool.Stop()
	select {
	case <-completed:
	case <-time.After(time.Second):
		t.Fatal("observed timer deadline waited for the hour-long reconciliation fallback")
	}
}
