package runtime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/action/actiontest"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/runtime/workqueue"
	"github.com/axiom-studio/openseal/pkg/skill"
)

type countingActionPoolStore struct {
	*sqlite.Store
	claims    atomic.Int64
	empty     atomic.Int64
	hydration atomic.Int64
	failRead  bool
	entered   chan struct{}
	release   chan struct{}
}

func (s *countingActionPoolStore) ClaimNextAction(ctx context.Context, claim ActionClaim) (*ActionCall, error) {
	n := s.claims.Add(1)
	call, err := s.Store.ClaimNextAction(ctx, claim)
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
	return s.Store.GetAgentRun(ctx, scope, id)
}

func TestActionWorkerPoolCoalescesIdleClaimsBeforeHydration(t *testing.T) {
	store := &countingActionPoolStore{Store: newTestKernelStore(t)}
	pool, err := action.NewActionWorkerPool(store, skill.NewCatalog(), nil,
		action.ActionDispatcherFunc(func(context.Context, agentrun.ActionDispatchInput) (map[string]interface{}, error) {
			return nil, errors.New("unexpected idle dispatch")
		}), nil, action.ActionWorkerConfig{Scope: Scope{Kind: "tenant", ID: "idle"}, Concurrency: 8, PollInterval: 20 * time.Millisecond})
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
	store := &countingActionPoolStore{Store: newTestKernelStore(t), entered: make(chan struct{}), release: make(chan struct{})}
	catalog, scope := actiontest.GovernedCatalog(t)
	dispatched := make(chan struct{}, 1)
	pool := actiontest.NewPool(t, store, catalog, scope, time.Hour, 1, action.ActionDispatcherFunc(func(context.Context, agentrun.ActionDispatchInput) (map[string]interface{}, error) {
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
	actiontest.CreatePoolWork(t, store.Store, catalog, scope)
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
			store := &countingActionPoolStore{Store: newTestKernelStore(t)}
			catalog, scope := actiontest.GovernedCatalog(t)
			dispatched := make(chan struct{}, 1)
			interval := 25 * time.Millisecond
			if scenario != "missed_wake" {
				interval = time.Hour
			}
			pool := actiontest.NewPool(t, store, catalog, scope, interval, 2, action.ActionDispatcherFunc(func(context.Context, agentrun.ActionDispatchInput) (map[string]interface{}, error) {
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
			actiontest.CreatePoolWork(t, store.Store, catalog, scope)
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
	store := newTestKernelStore(t)
	catalog, scope := actiontest.GovernedCatalog(t)
	const calls = 8
	for i := 0; i < calls; i++ {
		actiontest.CreatePoolWork(t, store, catalog, scope)
	}
	started, completed := make(chan struct{}, calls), make(chan struct{}, calls)
	release := make(chan struct{})
	pool := actiontest.NewPool(t, store, catalog, scope, time.Hour, 2, action.ActionDispatcherFunc(func(ctx context.Context, _ agentrun.ActionDispatchInput) (map[string]interface{}, error) {
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

func TestWorkerPollSignalPreservesNotificationBeforeWait(t *testing.T) {
	poll := workqueue.NewWorkerPollSignal()
	generation, wake := poll.Snapshot()
	poll.Notify()
	current, _ := poll.Snapshot()
	if current == generation {
		t.Fatalf("committed enqueue did not advance generation: %d", current)
	}
	if !workqueue.WaitForWorkerPoll(t.Context(), wake, time.Hour) {
		t.Fatal("committed wake did not survive the later wait")
	}
}

func TestActionWorkerPoolBacksOffAfterClaimedRunHydrationFailure(t *testing.T) {
	store := &countingActionPoolStore{Store: newTestKernelStore(t), failRead: true}
	catalog, scope := actiontest.GovernedCatalog(t)
	for range 20 {
		actiontest.CreatePoolWork(t, store.Store, catalog, scope)
	}
	pool := actiontest.NewPool(t, store, catalog, scope, 50*time.Millisecond, 1, action.ActionDispatcherFunc(func(context.Context, agentrun.ActionDispatchInput) (map[string]interface{}, error) {
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
	store := newTestKernelStore(t)
	catalog, scope := actiontest.GovernedCatalog(t)
	actiontest.CreatePoolWork(t, store, catalog, scope)
	entered, stopping, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	pool := actiontest.NewPool(t, store, catalog, scope, time.Hour, 1, action.ActionDispatcherFunc(func(ctx context.Context, _ agentrun.ActionDispatchInput) (map[string]interface{}, error) {
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
	poll := workqueue.NewWorkerPollSignal()
	_, wake := poll.Snapshot()
	const waiters = 8
	var group sync.WaitGroup
	for range waiters {
		group.Add(1)
		go func() {
			defer group.Done()
			if !workqueue.WaitForWorkerPoll(t.Context(), wake, time.Hour) {
				t.Error("pool notification did not wake a waiter")
			}
		}()
	}
	poll.Notify()
	group.Wait()
	_, nextWake := poll.Snapshot()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if workqueue.WaitForWorkerPoll(ctx, nextWake, time.Hour) {
		t.Fatal("canceled worker waited on a pool wake")
	}
}

func TestAgentRunWorkerObservesTimerDeadlineWithoutWaitingForFallback(t *testing.T) {
	store := newTestKernelStore(t)
	scope := Scope{Kind: "tenant", ID: "timer-deadline"}
	run, err := NewPortfolioService(store).CreateAgentRun(t.Context(), CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent", Goal: "sleep then resume", Source: RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	producer := &timerStepProducer{steps: 1, wakeIn: 30 * time.Millisecond}
	pool, err := NewAgentRunWorkerPool(store, conversation.TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*agentrun.TurnRunnerBinding, error) {
		return &agentrun.TurnRunnerBinding{DefinitionID: "agent", DefinitionVersion: "1", Producer: producer}, nil
	}), nil, AgentRunWorkerConfig{Scope: scope, PollInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	pool.Start(t.Context())
	defer pool.Stop()
	deadline := time.Now().Add(time.Second)
	for {
		current, err := store.GetAgentRun(t.Context(), scope, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.Status == AgentRunStatusCompleted && producer.calls.Load() == 2 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("observed timer deadline waited for the hour-long reconciliation fallback: %s after %d steps", current.Status, producer.calls.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
}
