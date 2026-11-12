package workqueue

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"go.uber.org/zap"
)

// scopePool is one scope's worker pool under a dynamic supervisor.
type scopePool interface {
	Start(context.Context)
	Stop()
	Wake()
	SetWorkSignals(*WorkSignalStatus)
	IdleFor(time.Time) time.Duration
}

// ScopePools keeps one pool per scope that has work.
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
type ScopePools[P scopePool] struct {
	name      string
	source    WorkerScopeSource
	gate      *WorkScopeGate
	interval  time.Duration
	retire    time.Duration
	dueFamily models.WorkFamily
	logger    *zap.SugaredLogger
	create    func(models.Scope) (P, error)

	mu       sync.RWMutex
	pools    map[models.Scope]P
	signals  *WorkSignalStatus
	pending  map[models.Scope]struct{}
	requests chan struct{}
	cancel   context.CancelFunc
	ctx      context.Context
	wg       sync.WaitGroup
}

// retire is how long a queue-driven pool stays idle before it stops;
// supervisors pass PoolRetireAfter.
func NewScopePools[P scopePool](name string, source WorkerScopeSource, interval, retire time.Duration, dueFamily models.WorkFamily, logger *zap.SugaredLogger, create func(models.Scope) (P, error)) *ScopePools[P] {
	return &ScopePools[P]{
		name: name, source: source, gate: NewWorkScopeGate(source, interval), interval: interval, retire: retire, dueFamily: dueFamily,
		logger: logger, create: create, pools: make(map[models.Scope]P), pending: make(map[models.Scope]struct{}), requests: make(chan struct{}, 1),
	}
}

func (s *ScopePools[P]) Start(parent context.Context) {
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

func (s *ScopePools[P]) Stop() {
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

func (s *ScopePools[P]) Each(fn func(P)) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, pool := range s.pools {
		fn(pool)
	}
}

func (s *ScopePools[P]) Wake() { s.Each(func(pool P) { pool.Wake() }) }

// WakeScope wakes the scope's pool, or asks for one while the queue is
// healthy. The read lock orders the wake before any retirement check, so a
// woken pool is never retired by a concurrent pass.
func (s *ScopePools[P]) WakeScope(scope models.Scope) {
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
	s.Request()
}

func (s *ScopePools[P]) Request() {
	select {
	case s.requests <- struct{}{}:
	default:
	}
}

func (s *ScopePools[P]) SetSignals(status *WorkSignalStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.signals = status
	for _, pool := range s.pools {
		pool.SetWorkSignals(status)
	}
}

func (s *ScopePools[P]) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.pools)
}

func (s *ScopePools[P]) currentSignals() *WorkSignalStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.signals
}

func (s *ScopePools[P]) run(ctx context.Context) {
	defer s.wg.Done()
	signals := s.currentSignals()
	// With a queue bound, wait for it rather than listing every scope at
	// startup; listing starts only if the queue is still unhealthy at the
	// first tick.
	if !signals.QueueBound() {
		s.reconcile(ctx)
	}
	ticker := time.NewTicker(min(s.interval, s.retire/2))
	defer ticker.Stop()
	lastReconcile := time.Now()
	for {
		healthy, healthChanged := signals.Watch()
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
func (s *ScopePools[P]) admit(ctx context.Context, signals *WorkSignalStatus) {
	s.mu.Lock()
	scopes := make([]models.Scope, 0, len(s.pending))
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
	allowed, err := s.gate.Filter(ctx, scopes)
	if err != nil && ctx.Err() == nil {
		s.logger.Warnw("worker scope admission failed", "workers", s.name, "error", err)
		// The scopes' work is durable; ask the queue to deliver it again.
		admitted := make(map[models.Scope]struct{}, len(allowed))
		for _, scope := range allowed {
			admitted[scope] = struct{}{}
		}
		for _, scope := range scopes {
			if _, ok := admitted[scope]; !ok {
				signals.ScheduleIn(s.dueFamily, scope, s.interval)
			}
		}
	}
	for _, scope := range allowed {
		s.startPool(ctx, scope, true)
	}
}

// startPool starts a scope pool. Queue admissions log quietly: scopes come
// and go with their work.
func (s *ScopePools[P]) startPool(ctx context.Context, scope models.Scope, quiet bool) {
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
func (s *ScopePools[P]) retireIdle(now time.Time) {
	s.mu.Lock()
	retired := make([]P, 0)
	for scope, pool := range s.pools {
		if pool.IdleFor(now) >= s.retire {
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
func (s *ScopePools[P]) reconcile(ctx context.Context) {
	scopes, err := s.source.ListWorkerScopes(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.logger.Warnw("worker scope reconciliation failed", "workers", s.name, "error", err)
		}
		return
	}
	desired := make(map[models.Scope]struct{}, len(scopes))
	for _, scope := range scopes {
		if err := scope.Validate(); err != nil {
			s.logger.Warnw("ignoring invalid worker scope", "workers", s.name, "scopeKind", scope.Kind, "scopeId", scope.ID, "error", err)
			continue
		}
		desired[scope] = struct{}{}
	}
	ordered := make([]models.Scope, 0, len(desired))
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
