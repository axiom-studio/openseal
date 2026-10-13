package runtime

import (
	"container/heap"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// WorkItem asks for one pass over a family's durable work in a scope at or
// after DueAt. Items are hints: claims, leases and fairness are always decided
// by the store, so a duplicate or early item costs one empty pass.
type WorkItem struct {
	Family WorkFamily
	Scope  Scope
	DueAt  time.Time
}

// WorkQueue is the shared, process-independent schedule of scope passes.
// Every replica publishes into it and consumes from it, so idle scopes cost
// nothing: no loop, no timer and no query exists for a scope until an item
// for it becomes due.
//
// PublishWork records items; for the same family and scope the earliest
// DueAt wins, so bursts collapse into one delivery. ConsumeWork blocks until
// ctx ends, delivering each due item to exactly one consumer through handle,
// serially. It reports WorkSignalConnected once it is consuming, with Rebuild
// set when the queue's schedule was lost and must be rebuilt from the store.
type WorkQueue interface {
	PublishWork(ctx context.Context, items []WorkItem) error
	ConsumeWork(ctx context.Context, handle func(WorkSignal)) error
}

// WorkScopeChecker answers whether a single scope may run workers, without
// listing every scope. Queue-driven supervisors use it to admit a scope the
// first time it has work.
type WorkScopeChecker interface {
	WorkerScopeActive(ctx context.Context, scope Scope) (bool, error)
}

// workItemKey identifies the family and scope an item schedules.
type workItemKey struct {
	family WorkFamily
	scope  Scope
}

// workPublisher coalesces scheduled items in process and publishes them in
// batches, so many pools finishing at once cost one queue round trip.
type workPublisher struct {
	queue  WorkQueue
	logger *zap.SugaredLogger
	mu     sync.Mutex
	items  map[workItemKey]time.Time
	ready  chan struct{}
}

func newWorkPublisher(queue WorkQueue, logger *zap.SugaredLogger) *workPublisher {
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	return &workPublisher{queue: queue, logger: logger, items: make(map[workItemKey]time.Time), ready: make(chan struct{}, 1)}
}

func (p *workPublisher) schedule(item WorkItem) {
	if p == nil || item.Family == "" || item.Scope.Validate() != nil {
		return
	}
	key := workItemKey{family: item.Family, scope: item.Scope}
	p.mu.Lock()
	if current, ok := p.items[key]; !ok || item.DueAt.Before(current) {
		p.items[key] = item.DueAt
	}
	p.mu.Unlock()
	select {
	case p.ready <- struct{}{}:
	default:
	}
}

func (p *workPublisher) take() []WorkItem {
	p.mu.Lock()
	defer p.mu.Unlock()
	items := make([]WorkItem, 0, len(p.items))
	for key, due := range p.items {
		items = append(items, WorkItem{Family: key.family, Scope: key.scope, DueAt: due})
	}
	clear(p.items)
	return items
}

// run publishes until ctx ends. A failed batch is retried with backoff; the
// items stay scheduled, merged with anything scheduled meanwhile.
func (p *workPublisher) run(ctx context.Context) {
	failures := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.ready:
		}
		items := p.take()
		if len(items) == 0 {
			continue
		}
		publishCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := p.queue.PublishWork(publishCtx, items)
		cancel()
		if err == nil {
			failures = 0
			continue
		}
		if ctx.Err() != nil {
			return
		}
		failures++
		p.logger.Warnw("failed to publish scheduled work", "items", len(items), "error", err)
		for _, item := range items {
			p.schedule(item)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(workerPollDelay(100*time.Millisecond, failures, "work-publisher")):
		}
	}
}

// MemoryWorkQueue is a process-local WorkQueue. It suits a single replica, and
// tests. Every replica that uses one handles the work its own commits create.
type MemoryWorkQueue struct {
	mu      sync.Mutex
	due     workDueHeap
	index   map[workItemKey]*workDueEntry
	changed chan struct{}
	now     func() time.Time
}

func NewMemoryWorkQueue() *MemoryWorkQueue {
	return &MemoryWorkQueue{index: make(map[workItemKey]*workDueEntry), changed: make(chan struct{}, 1), now: time.Now}
}

func (q *MemoryWorkQueue) PublishWork(_ context.Context, items []WorkItem) error {
	q.mu.Lock()
	for _, item := range items {
		if item.Family == "" || item.Scope.Validate() != nil {
			continue
		}
		key := workItemKey{family: item.Family, scope: item.Scope}
		if entry, ok := q.index[key]; ok {
			if item.DueAt.Before(entry.due) {
				entry.due = item.DueAt
				heap.Fix(&q.due, entry.index)
			}
			continue
		}
		entry := &workDueEntry{key: key, due: item.DueAt}
		q.index[key] = entry
		heap.Push(&q.due, entry)
	}
	q.mu.Unlock()
	select {
	case q.changed <- struct{}{}:
	default:
	}
	return nil
}

// ConsumeWork delivers due items until ctx ends. A memory queue starts empty,
// so its first connect always asks for a rebuild.
func (q *MemoryWorkQueue) ConsumeWork(ctx context.Context, handle func(WorkSignal)) error {
	if handle == nil {
		return errors.New("work handler is required")
	}
	handle(WorkSignal{Kind: WorkSignalConnected, Rebuild: true})
	for {
		now := q.now()
		var wait <-chan time.Time
		var timer *time.Timer
		q.mu.Lock()
		var ready []workItemKey
		for q.due.Len() > 0 && !q.due[0].due.After(now) {
			entry := heap.Pop(&q.due).(*workDueEntry)
			delete(q.index, entry.key)
			ready = append(ready, entry.key)
		}
		if len(ready) == 0 && q.due.Len() > 0 {
			timer = time.NewTimer(q.due[0].due.Sub(now))
			wait = timer.C
		}
		q.mu.Unlock()
		for _, key := range ready {
			handle(WorkSignal{Kind: WorkSignalChanged, Family: key.family, Scope: key.scope})
		}
		if len(ready) > 0 {
			continue
		}
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return ctx.Err()
		case <-q.changed:
		case <-wait:
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

type workDueEntry struct {
	key   workItemKey
	due   time.Time
	index int
}

type workDueHeap []*workDueEntry

func (h workDueHeap) Len() int           { return len(h) }
func (h workDueHeap) Less(i, j int) bool { return h[i].due.Before(h[j].due) }
func (h workDueHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index, h[j].index = i, j
}
func (h *workDueHeap) Push(value any) {
	entry := value.(*workDueEntry)
	entry.index = len(*h)
	*h = append(*h, entry)
}
func (h *workDueHeap) Pop() any {
	old := *h
	entry := old[len(old)-1]
	*h = old[:len(old)-1]
	return entry
}

// workScopeGate admits a queued scope to worker supervision. It prefers a
// point lookup and caches answers, so admitting a scope never lists every
// scope; sources without a point lookup fall back to a throttled listing.
type workScopeGate struct {
	source  WorkerScopeSource
	checker WorkScopeChecker
	known   *knownWorkerScopes
	mu      sync.Mutex
	cache   map[Scope]workScopeAnswer
	now     func() time.Time
}

type workScopeAnswer struct {
	active  bool
	expires time.Time
}

const (
	workScopeActiveTTL   = 5 * time.Minute
	workScopeInactiveTTL = time.Minute
	workScopeCacheLimit  = 65536
)

func newWorkScopeGate(source WorkerScopeSource, refresh time.Duration) *workScopeGate {
	gate := &workScopeGate{source: source, cache: make(map[Scope]workScopeAnswer), now: time.Now}
	gate.checker, _ = source.(WorkScopeChecker)
	if gate.checker == nil {
		gate.known = newKnownWorkerScopes(source, refresh)
	}
	return gate
}

// filter keeps the scopes that may run workers. Lookup failures drop the
// scope for this pass; its work stays durable and is retried when the
// scope is next scheduled.
func (g *workScopeGate) filter(ctx context.Context, scopes []Scope) ([]Scope, error) {
	if g.checker == nil {
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.known.filter(ctx, g.now(), scopes)
	}
	result := make([]Scope, 0, len(scopes))
	var errs []error
	for _, scope := range scopes {
		active, err := g.active(ctx, scope)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if active {
			result = append(result, scope)
		}
	}
	return result, errors.Join(errs...)
}

func (g *workScopeGate) active(ctx context.Context, scope Scope) (bool, error) {
	now := g.now()
	g.mu.Lock()
	answer, ok := g.cache[scope]
	g.mu.Unlock()
	if ok && now.Before(answer.expires) {
		return answer.active, nil
	}
	active, err := g.checker.WorkerScopeActive(ctx, scope)
	if err != nil {
		return false, err
	}
	ttl := workScopeInactiveTTL
	if active {
		ttl = workScopeActiveTTL
	}
	g.mu.Lock()
	if len(g.cache) >= workScopeCacheLimit {
		clear(g.cache)
	}
	g.cache[scope] = workScopeAnswer{active: active, expires: now.Add(ttl)}
	g.mu.Unlock()
	return active, nil
}

// poolActivity tracks whether a scope pool is doing anything, so a
// queue-driven supervisor can retire pools of idle scopes. A retired scope
// costs nothing until the queue delivers its next item.
type poolActivity struct {
	busy atomic.Int32
	last atomic.Int64
}

func (a *poolActivity) touch() { a.last.Store(time.Now().UnixNano()) }

func (a *poolActivity) begin() {
	a.busy.Add(1)
	a.touch()
}

func (a *poolActivity) end() {
	a.touch()
	a.busy.Add(-1)
}

// idleFor returns how long the pool has done nothing, or zero while busy.
func (a *poolActivity) idleFor(now time.Time) time.Duration {
	if a.busy.Load() > 0 {
		return 0
	}
	return max(now.Sub(time.Unix(0, a.last.Load())), 0)
}

// poolRetireAfter is how long a queue-driven scope pool stays idle before its
// supervisor stops it. Its next deadline is already in the queue.
const poolRetireAfter = time.Minute

// BindQueue makes status schedule loop passes into queue. Call before the
// loops start. The returned function publishes scheduled passes until its
// context ends.
func (s *WorkSignalStatus) BindQueue(queue WorkQueue, logger *zap.SugaredLogger) func(context.Context) {
	publisher := newWorkPublisher(queue, logger)
	s.bindPublisher(publisher)
	return publisher.run
}
