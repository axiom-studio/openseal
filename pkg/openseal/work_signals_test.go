package openseal

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/runtime/workqueue"
)

// outboxStore is an in-memory store with a work outbox the test fills.
type outboxStore struct {
	*sqlite.Store
	claims    atomic.Int64
	deadline  atomic.Pointer[time.Time]
	committed chan struct{}

	mu       sync.Mutex
	outbox   []models.WorkItem
	pending  []models.WorkItem
	rebuilds int
	sweeps   int
}

func (s *outboxStore) ClaimNextAction(ctx context.Context, claim models.ActionClaim) (*models.ActionCall, error) {
	s.claims.Add(1)
	return s.Store.ClaimNextAction(ctx, claim)
}

func (s *outboxStore) NextActionDeadline(context.Context, models.Scope, time.Time) (*time.Time, error) {
	return s.deadline.Load(), nil
}

func (s *outboxStore) WorkCommitted() <-chan struct{} { return s.committed }

func (s *outboxStore) commit(items ...models.WorkItem) {
	s.mu.Lock()
	s.outbox = append(s.outbox, items...)
	s.mu.Unlock()
	s.committed <- struct{}{}
}

func (s *outboxStore) RelayWorkOutbox(ctx context.Context, _ int, publish func(context.Context, []models.WorkItem) error) (int, error) {
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

func (s *outboxStore) ListPendingWork(_ context.Context, rebuild bool) ([]models.WorkItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweeps++
	if rebuild {
		s.rebuilds++
	}
	return append([]models.WorkItem(nil), s.pending...), nil
}

func (s *outboxStore) counts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sweeps, s.rebuilds
}

// scriptedQueue records published items and delivers the signals the test
// sends.
type scriptedQueue struct {
	signals chan workqueue.WorkSignal
	mu      sync.Mutex
	items   []models.WorkItem
}

func (q *scriptedQueue) PublishWork(_ context.Context, items []models.WorkItem) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.items = append(q.items, items...)
	return nil
}

func (q *scriptedQueue) ConsumeWork(ctx context.Context, handle func(workqueue.WorkSignal)) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case signal := <-q.signals:
			handle(signal)
		}
	}
}

func (q *scriptedQueue) published(family models.WorkFamily, scope models.Scope) []models.WorkItem {
	q.mu.Lock()
	defer q.mu.Unlock()
	var result []models.WorkItem
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
	store := &outboxStore{Store: kerneltest.NewStore(t), committed: make(chan struct{}, 1)}
	store.pending = []models.WorkItem{{Family: models.WorkFamilyAction, Scope: other, DueAt: time.Now()}}
	queue := &scriptedQueue{signals: make(chan workqueue.WorkSignal)}
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
	t.Cleanup(func() { engine.Close() })
	if err := engine.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer engine.Stop()
	if engine.WorkSignalHealth().Healthy {
		t.Fatal("queue reported healthy before connecting")
	}

	// A queue that lost its schedule is rebuilt from the store's pending work.
	queue.signals <- workqueue.WorkSignal{Kind: workqueue.WorkSignalConnected, Rebuild: true}
	waitFor(t, func() bool { return engine.WorkSignalHealth().Healthy }, "connected queue was not reported healthy")
	waitFor(t, func() bool { return len(queue.published(models.WorkFamilyAction, other)) == 1 }, "pending work was not republished")
	if sweeps, rebuilds := store.counts(); sweeps != 1 || rebuilds != 1 {
		t.Fatalf("sweeps = %d, rebuilds = %d", sweeps, rebuilds)
	}

	// Committed outbox rows reach the queue without any polling.
	committed := models.WorkItem{Family: models.WorkFamilyRun, Scope: scope, DueAt: time.Now()}
	store.commit(committed)
	waitFor(t, func() bool { return len(queue.published(models.WorkFamilyRun, scope)) == 1 }, "committed outbox rows were not relayed")

	// Deliveries wake only the delivered family's workers in that scope, and
	// the woken pool schedules its next deadline into the queue.
	waitFor(t, func() bool { return store.claims.Load() >= 1 }, "initial claim was not observed")
	time.Sleep(20 * time.Millisecond)
	claims := store.claims.Load()
	queue.signals <- workqueue.WorkSignal{Kind: workqueue.WorkSignalChanged, Family: models.WorkFamilyAction, Scope: other}
	queue.signals <- workqueue.WorkSignal{Kind: workqueue.WorkSignalChanged, Family: models.WorkFamilyRun, Scope: scope}
	time.Sleep(50 * time.Millisecond)
	if got := store.claims.Load(); got != claims {
		t.Fatalf("unrelated deliveries caused %d action claims", got-claims)
	}
	retryAt := time.Now().Add(time.Minute).UTC()
	store.deadline.Store(&retryAt)
	queue.signals <- workqueue.WorkSignal{Kind: workqueue.WorkSignalChanged, Family: models.WorkFamilyAction, Scope: scope}
	waitFor(t, func() bool { return store.claims.Load() > claims }, "action delivery did not wake the scope's pool")
	waitFor(t, func() bool {
		for _, item := range queue.published(models.WorkFamilyAction, scope) {
			if item.DueAt.Equal(retryAt) {
				return true
			}
		}
		return false
	}, "the pool did not schedule its next deadline")

	queue.signals <- workqueue.WorkSignal{Kind: workqueue.WorkSignalDisconnected}
	waitFor(t, func() bool { return !engine.WorkSignalHealth().Healthy }, "disconnect was not reported")
	// Reconnecting a queue that kept its schedule needs no rebuild.
	queue.signals <- workqueue.WorkSignal{Kind: workqueue.WorkSignalConnected}
	waitFor(t, func() bool { return engine.WorkSignalHealth().Healthy }, "reconnect was not reported")
	time.Sleep(20 * time.Millisecond)
	if _, rebuilds := store.counts(); rebuilds != 1 {
		t.Fatalf("reconnect rebuilt the schedule: %d rebuilds", rebuilds)
	}
	if health := engine.WorkSignalHealth(); health.Connects != 2 || health.Disconnects != 1 || health.Signals != 3 {
		t.Fatalf("health = %#v", health)
	}
}
