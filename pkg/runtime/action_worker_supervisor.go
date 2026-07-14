package runtime

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// DynamicActionWorkerConfig controls a set of scope-bound worker pools. Each
// active scope receives an independent pool and lease namespace.
type DynamicActionWorkerConfig struct {
	Concurrency       int
	PollInterval      time.Duration
	LeaseDuration     time.Duration
	ReconcileInterval time.Duration
	WorkerIDPrefix    string
}

func (c *DynamicActionWorkerConfig) applyDefaults() error {
	if c.Concurrency <= 0 {
		c.Concurrency = 2
	}
	if c.PollInterval <= 0 {
		c.PollInterval = time.Second
	}
	if c.LeaseDuration <= 0 {
		c.LeaseDuration = time.Minute
	}
	if c.ReconcileInterval <= 0 {
		c.ReconcileInterval = 30 * time.Second
	}
	if strings.TrimSpace(c.WorkerIDPrefix) == "" {
		c.WorkerIDPrefix = "action-worker"
	}
	if c.Concurrency > 256 {
		return errors.New("action worker concurrency cannot exceed 256")
	}
	return nil
}

// ActionWorkerSupervisor reconciles active scopes into isolated worker pools.
// A source failure leaves existing pools running so a transient control-plane
// read cannot interrupt already leased work.
type ActionWorkerSupervisor struct {
	store       KernelStore
	catalog     ActionExecutionCatalog
	credentials CredentialResolver
	dispatcher  ActionDispatcher
	source      WorkerScopeSource
	config      DynamicActionWorkerConfig
	logger      *zap.SugaredLogger
	limiter     *WorkerLimiter

	mu     sync.RWMutex
	pools  map[string]*ActionWorkerPool
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func NewActionWorkerSupervisor(
	store KernelStore,
	catalog ActionExecutionCatalog,
	credentials CredentialResolver,
	dispatcher ActionDispatcher,
	source WorkerScopeSource,
	logger *zap.SugaredLogger,
	config DynamicActionWorkerConfig,
) (*ActionWorkerSupervisor, error) {
	if store == nil || catalog == nil || dispatcher == nil || source == nil {
		return nil, errors.New("action store, catalog, dispatcher, and scope source are required")
	}
	if err := config.applyDefaults(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	return &ActionWorkerSupervisor{
		store: store, catalog: catalog, credentials: credentials, dispatcher: dispatcher,
		source: source, config: config, logger: logger, pools: make(map[string]*ActionWorkerPool),
	}, nil
}

func (s *ActionWorkerSupervisor) Start(parent context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		return
	}
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel
	s.wg.Add(1)
	go s.run(ctx)
}

func (s *ActionWorkerSupervisor) Stop() {
	s.mu.Lock()
	cancel := s.cancel
	s.cancel = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
		s.wg.Wait()
	}

	s.mu.Lock()
	pools := make([]*ActionWorkerPool, 0, len(s.pools))
	for key, pool := range s.pools {
		pools = append(pools, pool)
		delete(s.pools, key)
	}
	s.mu.Unlock()
	for _, pool := range pools {
		pool.Stop()
	}
}

func (s *ActionWorkerSupervisor) Wake() {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, pool := range s.pools {
		pool.Wake()
	}
}

func (s *ActionWorkerSupervisor) SetWorkerLimiter(limiter *WorkerLimiter) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.limiter = limiter
	for _, pool := range s.pools {
		pool.SetWorkerLimiter(limiter)
	}
}

// ScopeCount exposes bounded operational state without revealing queued work.
func (s *ActionWorkerSupervisor) ScopeCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.pools)
}

func (s *ActionWorkerSupervisor) run(ctx context.Context) {
	defer s.wg.Done()
	s.reconcile(ctx)
	ticker := time.NewTicker(s.config.ReconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.reconcile(ctx)
		}
	}
}

func (s *ActionWorkerSupervisor) reconcile(ctx context.Context) {
	scopes, err := s.source.ListWorkerScopes(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.logger.Warnw("action worker scope reconciliation failed", "error", err)
		}
		return
	}
	desired := make(map[string]Scope, len(scopes))
	for _, scope := range scopes {
		if err := scope.Validate(); err != nil {
			s.logger.Warnw("ignoring invalid action worker scope", "scopeKind", scope.Kind, "scopeId", scope.ID, "error", err)
			continue
		}
		desired[workerScopeKey(scope)] = scope
	}

	keys := make([]string, 0, len(desired))
	for key := range desired {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	s.mu.Lock()
	for _, key := range keys {
		if s.pools[key] != nil {
			continue
		}
		scope := desired[key]
		pool, err := NewActionWorkerPool(s.store, s.catalog, s.credentials, s.dispatcher, s.logger, ActionWorkerConfig{
			Scope: scope, Concurrency: s.config.Concurrency, PollInterval: s.config.PollInterval,
			LeaseDuration:  s.config.LeaseDuration,
			WorkerIDPrefix: fmt.Sprintf("%s-%s-%s", s.config.WorkerIDPrefix, scope.Kind, scope.ID),
		})
		if err != nil {
			s.logger.Warnw("failed to create action worker pool", "scopeKind", scope.Kind, "scopeId", scope.ID, "error", err)
			continue
		}
		pool.SetWorkerLimiter(s.limiter)
		s.pools[key] = pool
		pool.Start(ctx)
		s.logger.Infow("started action worker scope", "scopeKind", scope.Kind, "scopeId", scope.ID, "concurrency", s.config.Concurrency)
	}
	removed := make([]*ActionWorkerPool, 0)
	for key, pool := range s.pools {
		if _, ok := desired[key]; ok {
			continue
		}
		removed = append(removed, pool)
		delete(s.pools, key)
	}
	s.mu.Unlock()
	for _, pool := range removed {
		pool.Stop()
	}
}
