package openseal

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime"
)

// outboxStore is an in-memory store with a work outbox the test fills.
type outboxStore struct {
	*runtime.MemoryStore
	claims    atomic.Int64
	deadline  atomic.Pointer[time.Time]
	committed chan struct{}

	mu       sync.Mutex
	outbox   []runtime.WorkItem
	pending  []runtime.WorkItem
	rebuilds int
	sweeps   int
}

func (s *outboxStore) ClaimNextAction(ctx context.Context, claim runtime.ActionClaim) (*runtime.ActionCall, error) {
	s.claims.Add(1)
	return s.MemoryStore.ClaimNextAction(ctx, claim)
}

func (s *outboxStore) NextActionDeadline(context.Context, runtime.Scope, time.Time) (*time.Time, error) {
	return s.deadline.Load(), nil
}

func (s *outboxStore) WorkCommitted() <-chan struct{} { return s.committed }

func (s *outboxStore) commit(items ...runtime.WorkItem) {
	s.mu.Lock()
	s.outbox = append(s.outbox, items...)
	s.mu.Unlock()
	s.committed <- struct{}{}
}

func (s *outboxStore) RelayWorkOutbox(ctx context.Context, _ int, publish func(context.Context, []runtime.WorkItem) error) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.outbox) == 0 {
		return 0, nil
	}
	if err := publish(ctx, s.outbox); err != nil {
		return 0, err
	}
	relayed := len(s.outbox)
	s.outbox = nil
	return relayed, nil
}

func (s *outboxStore) ListPendingWork(_ context.Context, rebuild bool) ([]runtime.WorkItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweeps++
	if rebuild {
		s.rebuilds++
	}
	return append([]runtime.WorkItem(nil), s.pending...), nil
}

func (s *outboxStore) counts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sweeps, s.rebuilds
}

// scriptedQueue records published items and delivers the signals the test
// sends.
type scriptedQueue struct {
	signals chan runtime.WorkSignal
	mu      sync.Mutex
	items   []runtime.WorkItem
}

func (q *scriptedQueue) PublishWork(_ context.Context, items []runtime.WorkItem) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.items = append(q.items, items...)
	return nil
}

func (q *scriptedQueue) ConsumeWork(ctx context.Context, handle func(runtime.WorkSignal)) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case signal := <-q.signals:
			handle(signal)
		}
	}
}

func (q *scriptedQueue) published(family runtime.WorkFamily, scope runtime.Scope) []runtime.WorkItem {
	q.mu.Lock()
	defer q.mu.Unlock()
	var result []runtime.WorkItem
	for _, item := range q.items {
		if item.Family == family && item.Scope == scope {
			result = append(result, item)
		}
	}
	return result
}

func waitFor(t *testing.T, condition func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal(message)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestEngineWorkQueueRelaysOutboxSchedulesDeadlinesAndRoutesDeliveries(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	scope := Scope{Kind: "local", ID: "queue"}
	other := Scope{Kind: "local", ID: "elsewhere"}
	store := &outboxStore{MemoryStore: runtime.NewMemoryStore(), committed: make(chan struct{}, 1)}
	store.pending = []runtime.WorkItem{{Family: runtime.WorkFamilyAction, Scope: other, DueAt: time.Now()}}
	queue := &scriptedQueue{signals: make(chan runtime.WorkSignal)}
	engine, err := New(
		WithStore(store),
		WithWorkQueue(queue),
		WithActionWorkers(ActionWorkerConfig{Scope: scope, Concurrency: 1, PollInterval: time.Hour, LeaseDuration: time.Second}, nil,
			ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
				return map[string]interface{}{}, nil
			})),
	)
	if err != nil {
		t.Fatal(err)
	}
	engine.Start(ctx)
	defer engine.Stop()
	if engine.WorkSignalHealth().Healthy {
		t.Fatal("queue reported healthy before connecting")
	}

	// A queue that lost its schedule is rebuilt from the store's pending work.
	queue.signals <- runtime.WorkSignal{Kind: runtime.WorkSignalConnected, Rebuild: true}
	waitFor(t, func() bool { return engine.WorkSignalHealth().Healthy }, "connected queue was not reported healthy")
	waitFor(t, func() bool { return len(queue.published(runtime.WorkFamilyAction, other)) == 1 }, "pending work was not republished")
	if sweeps, rebuilds := store.counts(); sweeps != 1 || rebuilds != 1 {
		t.Fatalf("sweeps = %d, rebuilds = %d", sweeps, rebuilds)
	}

	// Committed outbox rows reach the queue without any polling.
	committed := runtime.WorkItem{Family: runtime.WorkFamilyRun, Scope: scope, DueAt: time.Now()}
	store.commit(committed)
	waitFor(t, func() bool { return len(queue.published(runtime.WorkFamilyRun, scope)) == 1 }, "committed outbox rows were not relayed")

	// Deliveries wake only the delivered family's workers in that scope, and
	// the woken pool schedules its next deadline into the queue.
	waitFor(t, func() bool { return store.claims.Load() >= 1 }, "initial claim was not observed")
	time.Sleep(20 * time.Millisecond)
	claims := store.claims.Load()
	queue.signals <- runtime.WorkSignal{Kind: runtime.WorkSignalChanged, Family: runtime.WorkFamilyAction, Scope: other}
	queue.signals <- runtime.WorkSignal{Kind: runtime.WorkSignalChanged, Family: runtime.WorkFamilyRun, Scope: scope}
	time.Sleep(50 * time.Millisecond)
	if got := store.claims.Load(); got != claims {
		t.Fatalf("unrelated deliveries caused %d action claims", got-claims)
	}
	retryAt := time.Now().Add(time.Minute).UTC()
	store.deadline.Store(&retryAt)
	queue.signals <- runtime.WorkSignal{Kind: runtime.WorkSignalChanged, Family: runtime.WorkFamilyAction, Scope: scope}
	waitFor(t, func() bool { return store.claims.Load() > claims }, "action delivery did not wake the scope's pool")
	waitFor(t, func() bool {
		for _, item := range queue.published(runtime.WorkFamilyAction, scope) {
			if item.DueAt.Equal(retryAt) {
				return true
			}
		}
		return false
	}, "the pool did not schedule its next deadline")

	queue.signals <- runtime.WorkSignal{Kind: runtime.WorkSignalDisconnected}
	waitFor(t, func() bool { return !engine.WorkSignalHealth().Healthy }, "disconnect was not reported")
	// Reconnecting a queue that kept its schedule needs no rebuild.
	queue.signals <- runtime.WorkSignal{Kind: runtime.WorkSignalConnected}
	waitFor(t, func() bool { return engine.WorkSignalHealth().Healthy }, "reconnect was not reported")
	time.Sleep(20 * time.Millisecond)
	if _, rebuilds := store.counts(); rebuilds != 1 {
		t.Fatalf("reconnect rebuilt the schedule: %d rebuilds", rebuilds)
	}
	if health := engine.WorkSignalHealth(); health.Connects != 2 || health.Disconnects != 1 || health.Signals != 3 {
		t.Fatalf("health = %#v", health)
	}
}
