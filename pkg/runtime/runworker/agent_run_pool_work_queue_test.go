package runworker_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/runworker"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/runtime/workqueue"
	"github.com/axiom-studio/openseal/pkg/runtime/workqueue/workqueuetest"
)

// deadlineAgentRunStore counts every read an AgentRunWorkerPool makes.
type deadlineAgentRunStore struct {
	*sqlite.Store
	claims        atomic.Int64
	lists         atomic.Int64
	deadlineReads atomic.Int64
	deadline      atomic.Pointer[time.Time]
}

func (s *deadlineAgentRunStore) ClaimNextAgentRunWithDecision(ctx context.Context, claim models.AgentRunClaim) (*models.AgentRunAdmissionDecision, error) {
	s.claims.Add(1)
	return s.Store.ClaimNextAgentRunWithDecision(ctx, claim)
}

func (s *deadlineAgentRunStore) ClaimNextAgentRun(ctx context.Context, claim models.AgentRunClaim) (*models.AgentRun, error) {
	s.claims.Add(1)
	return s.Store.ClaimNextAgentRun(ctx, claim)
}

func (s *deadlineAgentRunStore) ListAgentRuns(ctx context.Context, filter models.AgentRunFilter) ([]*models.AgentRun, error) {
	s.lists.Add(1)
	return s.Store.ListAgentRuns(ctx, filter)
}

func (s *deadlineAgentRunStore) NextAgentRunDeadline(context.Context, models.Scope, models.RunKind, string, time.Time) (*time.Time, error) {
	s.deadlineReads.Add(1)
	return s.deadline.Load(), nil
}

func (s *deadlineAgentRunStore) reads() int64 {
	return s.claims.Load() + s.lists.Load() + s.deadlineReads.Load()
}

func TestQueueDrivenAgentRunPoolIssuesNoReadsWhileIdleAndWakesOnDeadline(t *testing.T) {
	store := &deadlineAgentRunStore{Store: kerneltest.NewStore(t)}
	scope := models.Scope{Kind: "tenant", ID: "idle-runs"}
	pool, err := runworker.NewAgentRunWorkerPool(store, conversation.TurnRunnerResolverFunc(func(context.Context, *models.AgentRun) (*agentrun.TurnRunnerBinding, error) {
		return nil, errors.New("unexpected idle turn")
	}), nil, runworker.AgentRunWorkerConfig{Scope: scope, Concurrency: 4, PollInterval: 5 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	harness := workqueuetest.NewHarness(t)
	harness.OnDelivery(func(signal workqueue.WorkSignal) {
		if signal.Family == models.WorkFamilyRunDue {
			pool.WakeScope(signal.Scope)
		}
	})
	pool.SetWorkSignals(harness.Status)
	pool.Start(t.Context())
	defer pool.Stop()
	workqueuetest.WaitFor(t, time.Second, func() bool { return store.deadlineReads.Load() > 0 }, "initial probe was not observed")
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
	workqueuetest.WaitFor(t, time.Second, func() bool { return store.deadlineReads.Load() >= 2 }, "wake did not probe")
	store.deadline.Store(nil)
	afterWake := store.deadlineReads.Load()
	workqueuetest.WaitFor(t, time.Second, func() bool { return store.deadlineReads.Load() > afterWake }, "deadline did not wake the pool")
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
