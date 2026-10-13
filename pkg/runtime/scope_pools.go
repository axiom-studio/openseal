package runtime

import (
	"context"
	"sort"
	"sync"
	"time"

	"go.uber.org/zap"
)

// scopePool is one scope's worker pool under a dynamic supervisor.
type scopePool interface {
	Start(context.Context)
	Stop()
	Wake()
	SetWorkSignals(*WorkSignalStatus)
	idleFor(time.Time) time.Duration
}

// scopePools keeps one pool per scope that has work.
//
// While the work queue is healthy a scope gets a pool the first time the
// queue delivers work for it, after a point check that the scope may run
// workers, and loses it once the pool has been idle for its retire interval. The
// pool's next deadline is already in the queue, so an idle scope costs no
// goroutine, timer or query. Scopes are never listed in this mode.
//
// Without a healthy queue every listed scope keeps a polling pool, refreshed
// each reconcile interval. A source failure leaves existing pools running so
// a transient control-plane read cannot interrupt already leased work.
type scopePools[P scopePool] struct {
	name      string
	source    WorkerScopeSource
	gate      *workScopeGate
	interval  time.Duration
	retire    time.Duration
	dueFamily WorkFamily
	logger    *zap.SugaredLogger
	create    func(Scope) (P, error)

	mu       sync.RWMutex
	pools    map[Scope]P
	signals  *WorkSignalStatus
	pending  map[Scope]struct{}
	requests chan struct{}
	cancel   context.CancelFunc
	ctx      context.Context
	wg       sync.WaitGroup
}

func newScopePools[P scopePool](name string, source WorkerScopeSource, interval time.Duration, dueFamily WorkFamily, logger *zap.SugaredLogger, create func(Scope) (P, error)) *scopePools[P] {
	return &scopePools[P]{
		name: name, source: source, gate: newWorkScopeGate(source, interval), interval: interval, retire: poolRetireAfter, dueFamily: dueFamily,
		logger: logger, create: create, pools: make(map[Scope]P), pending: make(map[Scope]struct{}), requests: make(chan struct{}, 1),
	}
}

func (s *scopePools[P]) start(parent context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		return
	}
	if parent == nil {
		parent = context.Background()
	}
	s.ctx, s.cancel = context.WithCancel(parent)
	s.wg.Add(1)
	go s.run(s.ctx)
}

func (s *scopePools[P]) stop() {
	s.mu.Lock()
	cancel := s.cancel
	s.cancel = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
		s.wg.Wait()
	}
	s.mu.Lock()
	pools := make([]P, 0, len(s.pools))
	for scope, pool := range s.pools {
		pools = append(pools, pool)
		delete(s.pools, scope)
	}
	s.mu.Unlock()
	for _, pool := range pools {
		pool.Stop()
	}
}

func (s *scopePools[P]) each(fn func(P)) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, pool := range s.pools {
		fn(pool)
	}
}

func (s *scopePools[P]) wake() { s.each(func(pool P) { pool.Wake() }) }

// wakeScope wakes the scope's pool, or asks for one while the queue is
// healthy. The read lock orders the wake before any retirement check, so a
// woken pool is never retired by a concurrent pass.
func (s *scopePools[P]) wakeScope(scope Scope) {
	s.mu.RLock()
	pool, ok := s.pools[scope]
	signals := s.signals
	s.mu.RUnlock()
	if ok {
		pool.Wake()
		return
	}
	if !signals.Healthy() || scope.Validate() != nil {
		return
	}
	s.mu.Lock()
	s.pending[scope] = struct{}{}
	s.mu.Unlock()
	s.request()
}

func (s *scopePools[P]) request() {
	select {
	case s.requests <- struct{}{}:
	default:
	}
}

func (s *scopePools[P]) setSignals(status *WorkSignalStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.signals = status
	for _, pool := range s.pools {
		pool.SetWorkSignals(status)
	}
}

func (s *scopePools[P]) count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.pools)
}

func (s *scopePools[P]) currentSignals() *WorkSignalStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.signals
}

func (s *scopePools[P]) run(ctx context.Context) {
	defer s.wg.Done()
	signals := s.currentSignals()
	// With a queue bound, wait for it rather than listing every scope at
	// startup; listing starts only if the queue is still unhealthy at the
	// first tick.
	if !signals.queueBound() {
		s.reconcile(ctx)
	}
	ticker := time.NewTicker(min(s.interval, s.retire/2))
	defer ticker.Stop()
	lastReconcile := time.Now()
	for {
		healthy, healthChanged := signals.watch()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if healthy {
				s.retireIdle(time.Now())
				continue
			}
			if time.Since(lastReconcile) < s.interval {
				continue
			}
		case <-s.requests:
			if healthy {
				s.admit(ctx, signals)
				continue
			}
		case <-healthChanged:
			if signals.Healthy() {
				continue
			}
			// The queue went away: list now so every scope polls again.
		}
		lastReconcile = time.Now()
		s.reconcile(ctx)
	}
}

// admit starts pools for scopes the queue delivered work for.
func (s *scopePools[P]) admit(ctx context.Context, signals *WorkSignalStatus) {
	s.mu.Lock()
	scopes := make([]Scope, 0, len(s.pending))
	for scope := range s.pending {
		if _, running := s.pools[scope]; !running {
			scopes = append(scopes, scope)
		}
	}
	clear(s.pending)
	s.mu.Unlock()
	if len(scopes) == 0 {
		return
	}
	allowed, err := s.gate.filter(ctx, scopes)
	if err != nil && ctx.Err() == nil {
		s.logger.Warnw("worker scope admission failed", "workers", s.name, "error", err)
		// The scopes' work is durable; ask the queue to deliver it again.
		admitted := make(map[Scope]struct{}, len(allowed))
		for _, scope := range allowed {
			admitted[scope] = struct{}{}
		}
		for _, scope := range scopes {
			if _, ok := admitted[scope]; !ok {
				signals.scheduleIn(s.dueFamily, scope, s.interval)
			}
		}
	}
	for _, scope := range allowed {
		s.startPool(ctx, scope, true)
	}
}

// startPool starts a scope pool. Queue admissions log quietly: scopes come
// and go with their work.
func (s *scopePools[P]) startPool(ctx context.Context, scope Scope, quiet bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, running := s.pools[scope]; running || ctx.Err() != nil {
		return
	}
	pool, err := s.create(scope)
	if err != nil {
		s.logger.Warnw("failed to create worker pool", "workers", s.name, "scopeKind", scope.Kind, "scopeId", scope.ID, "error", err)
		return
	}
	pool.SetWorkSignals(s.signals)
	s.pools[scope] = pool
	pool.Start(ctx)
	if quiet {
		s.logger.Debugw("started worker scope", "workers", s.name, "scopeKind", scope.Kind, "scopeId", scope.ID)
	} else {
		s.logger.Infow("started worker scope", "workers", s.name, "scopeKind", scope.Kind, "scopeId", scope.ID)
	}
}

// retireIdle stops pools that have been idle long enough. Their next
// deadlines are in the queue, which brings them back when due.
func (s *scopePools[P]) retireIdle(now time.Time) {
	s.mu.Lock()
	retired := make([]P, 0)
	for scope, pool := range s.pools {
		if pool.idleFor(now) >= s.retire {
			retired = append(retired, pool)
			delete(s.pools, scope)
		}
	}
	s.mu.Unlock()
	for _, pool := range retired {
		pool.Stop()
	}
}

// reconcile matches pools to the listed scopes while polling.
func (s *scopePools[P]) reconcile(ctx context.Context) {
	scopes, err := s.source.ListWorkerScopes(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.logger.Warnw("worker scope reconciliation failed", "workers", s.name, "error", err)
		}
		return
	}
	desired := make(map[Scope]struct{}, len(scopes))
	for _, scope := range scopes {
		if err := scope.Validate(); err != nil {
			s.logger.Warnw("ignoring invalid worker scope", "workers", s.name, "scopeKind", scope.Kind, "scopeId", scope.ID, "error", err)
			continue
		}
		desired[scope] = struct{}{}
	}
	ordered := make([]Scope, 0, len(desired))
	for scope := range desired {
		ordered = append(ordered, scope)
	}
	sort.Slice(ordered, func(i, j int) bool { return workerScopeKey(ordered[i]) < workerScopeKey(ordered[j]) })
	for _, scope := range ordered {
		s.startPool(ctx, scope, false)
	}
	s.mu.Lock()
	removed := make([]P, 0)
	for scope, pool := range s.pools {
		if _, ok := desired[scope]; !ok {
			removed = append(removed, pool)
			delete(s.pools, scope)
		}
	}
	s.mu.Unlock()
	for _, pool := range removed {
		pool.Stop()
	}
}
