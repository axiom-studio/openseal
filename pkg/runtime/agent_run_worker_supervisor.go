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

// DynamicAgentRunWorkerConfig controls one kind-scoped autonomous Run pool per
// active isolation scope. Kind is an execution boundary, not an ownership
// shortcut: Team-owned conversation Runs and ordinary Agent work never share a
// claim stream.
type DynamicAgentRunWorkerConfig struct {
	Kind                       RunKind
	AssignedAgentID            string
	Concurrency                int
	MaxActiveForAgent          int
	MaxActiveForConcurrencyKey int
	MaxTurnsPerClaim           int
	LeaseDuration              time.Duration
	TurnLeaseDuration          time.Duration
	AgingInterval              time.Duration
	PollInterval               time.Duration
	ReconcileInterval          time.Duration
	WorkerIDPrefix             string
}

func (c *DynamicAgentRunWorkerConfig) applyDefaults() error {
	c.Kind = normalizeRunKind(c.Kind)
	if !validRunKind(c.Kind) {
		return fmt.Errorf("unsupported run kind %q", c.Kind)
	}
	if c.Concurrency <= 0 {
		c.Concurrency = 1
	}
	if c.Concurrency > 256 {
		return errors.New("agent run worker concurrency cannot exceed 256")
	}
	if c.MaxActiveForAgent <= 0 {
		c.MaxActiveForAgent = c.Concurrency
	}
	if c.MaxTurnsPerClaim <= 0 {
		c.MaxTurnsPerClaim = 1
	}
	if c.LeaseDuration <= 0 {
		c.LeaseDuration = 30 * time.Second
	}
	if c.TurnLeaseDuration <= 0 {
		c.TurnLeaseDuration = c.LeaseDuration
	}
	if c.AgingInterval <= 0 {
		c.AgingInterval = time.Minute
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 500 * time.Millisecond
	}
	if c.ReconcileInterval <= 0 {
		c.ReconcileInterval = 30 * time.Second
	}
	if strings.TrimSpace(c.WorkerIDPrefix) == "" {
		c.WorkerIDPrefix = "agent-run-worker"
	}
	return nil
}

// AgentRunWorkerSupervisor reconciles active scopes into isolated, kind-aware
// Run pools. A transient scope-source failure leaves existing pools running so
// already leased work is not interrupted.
type AgentRunWorkerSupervisor struct {
	store          KernelStore
	resolver       TurnRunnerResolver
	source         WorkerScopeSource
	config         DynamicAgentRunWorkerConfig
	logger         *zap.SugaredLogger
	actions        *ActionCoordinator
	actionObserver ActionProposalObserver
	limiter        *WorkerLimiter

	mu     sync.RWMutex
	pools  map[string]*AgentRunWorkerPool
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func (s *AgentRunWorkerSupervisor) SetActionProposalObserver(observer ActionProposalObserver) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.actionObserver = observer
	for _, pool := range s.pools {
		pool.SetActionProposalObserver(observer)
	}
}

// SetActionCoordinator supplies the shared governed action lifecycle to every
// scope pool created by this supervisor.
func (s *AgentRunWorkerSupervisor) SetActionCoordinator(actions *ActionCoordinator) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.actions = actions
	for _, pool := range s.pools {
		pool.SetActionCoordinator(actions)
	}
}

func (s *AgentRunWorkerSupervisor) SetWorkerLimiter(limiter *WorkerLimiter) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.limiter = limiter
	for _, pool := range s.pools {
		pool.SetWorkerLimiter(limiter)
	}
}

func NewAgentRunWorkerSupervisor(
	store KernelStore,
	resolver TurnRunnerResolver,
	source WorkerScopeSource,
	logger *zap.SugaredLogger,
	config DynamicAgentRunWorkerConfig,
) (*AgentRunWorkerSupervisor, error) {
	if store == nil || resolver == nil || source == nil {
		return nil, errors.New("kernel store, turn runner resolver, and scope source are required")
	}
	if err := config.applyDefaults(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	return &AgentRunWorkerSupervisor{
		store: store, resolver: resolver, source: source, config: config, logger: logger,
		pools: make(map[string]*AgentRunWorkerPool),
	}, nil
}

func (s *AgentRunWorkerSupervisor) Start(parent context.Context) {
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

func (s *AgentRunWorkerSupervisor) Stop() {
	s.mu.Lock()
	cancel := s.cancel
	s.cancel = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
		s.wg.Wait()
	}

	s.mu.Lock()
	pools := make([]*AgentRunWorkerPool, 0, len(s.pools))
	for key, pool := range s.pools {
		pools = append(pools, pool)
		delete(s.pools, key)
	}
	s.mu.Unlock()
	for _, pool := range pools {
		pool.Stop()
	}
}

func (s *AgentRunWorkerSupervisor) Wake() {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, pool := range s.pools {
		pool.Wake()
	}
}

func (s *AgentRunWorkerSupervisor) ScopeCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.pools)
}

func (s *AgentRunWorkerSupervisor) run(ctx context.Context) {
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

func (s *AgentRunWorkerSupervisor) reconcile(ctx context.Context) {
	scopes, err := s.source.ListWorkerScopes(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.logger.Warnw("agent run worker scope reconciliation failed", "runKind", s.config.Kind, "error", err)
		}
		return
	}
	desired := make(map[string]Scope, len(scopes))
	for _, scope := range scopes {
		if err := scope.Validate(); err != nil {
			s.logger.Warnw("ignoring invalid agent run worker scope", "scopeKind", scope.Kind, "scopeId", scope.ID, "error", err)
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
		pool, err := NewAgentRunWorkerPool(s.store, s.resolver, s.logger, AgentRunWorkerConfig{
			Scope: scope, Kind: s.config.Kind, AssignedAgentID: s.config.AssignedAgentID,
			Concurrency: s.config.Concurrency, MaxActiveForAgent: s.config.MaxActiveForAgent,
			MaxActiveForConcurrencyKey: s.config.MaxActiveForConcurrencyKey,
			MaxTurnsPerClaim:           s.config.MaxTurnsPerClaim, LeaseDuration: s.config.LeaseDuration,
			TurnLeaseDuration: s.config.TurnLeaseDuration, AgingInterval: s.config.AgingInterval,
			PollInterval:   s.config.PollInterval,
			WorkerIDPrefix: fmt.Sprintf("%s-%s-%s-%s", s.config.WorkerIDPrefix, s.config.Kind, scope.Kind, scope.ID),
		})
		if err != nil {
			s.logger.Warnw("failed to create agent run worker pool", "runKind", s.config.Kind, "scopeKind", scope.Kind, "scopeId", scope.ID, "error", err)
			continue
		}
		pool.SetActionCoordinator(s.actions)
		pool.SetActionProposalObserver(s.actionObserver)
		pool.SetWorkerLimiter(s.limiter)
		s.pools[key] = pool
		pool.Start(ctx)
		s.logger.Infow("started agent run worker scope", "runKind", s.config.Kind, "scopeKind", scope.Kind, "scopeId", scope.ID, "concurrency", s.config.Concurrency)
	}
	removed := make([]*AgentRunWorkerPool, 0)
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
