package runworker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/workqueue"
	"go.uber.org/zap"
)

// DynamicAgentRunWorkerConfig controls one kind-scoped autonomous Run pool per
// active isolation scope. Kind is an execution boundary, not an ownership
// shortcut: Team-owned conversation Runs and ordinary Agent work never share a
// claim stream.
type DynamicAgentRunWorkerConfig struct {
	Kind                              models.RunKind
	AssignedAgentID                   string
	Concurrency                       int
	MaxActiveForAgent                 int
	MaxActiveForOwner                 int
	MaxActiveForObjective             int
	MaxActiveForConcurrencyKey        int
	ConversationTaskForegroundReserve int
	LeaseDuration                     time.Duration
	TurnLeaseDuration                 time.Duration
	AgingInterval                     time.Duration
	PollInterval                      time.Duration
	ReconcileInterval                 time.Duration
	WorkerIDPrefix                    string
}

func (c *DynamicAgentRunWorkerConfig) applyDefaults() error {
	c.Kind = models.NormalizeRunKind(c.Kind)
	if !models.IsValidRunKind(c.Kind) {
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
	if err := models.ValidateConversationTaskForegroundReserve(c.ConversationTaskForegroundReserve, c.MaxActiveForAgent); err != nil {
		return err
	}
	if c.MaxActiveForOwner < 0 || c.MaxActiveForObjective < 0 || c.MaxActiveForConcurrencyKey < 0 {
		return errors.New("agent run portfolio concurrency limits cannot be negative")
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

// AgentRunWorkerSupervisor runs an isolated, kind-aware Run pool per scope
// with Run work. See workqueue.ScopePools for when a scope has a pool.
type AgentRunWorkerSupervisor struct {
	store    models.KernelStore
	resolver conversation.TurnRunnerResolver
	config   DynamicAgentRunWorkerConfig
	logger   *zap.SugaredLogger
	pools    *workqueue.ScopePools[*AgentRunWorkerPool]

	mu                sync.RWMutex
	actions           *action.Coordinator
	runFinalizer      agentrun.RunTerminalFinalizer
	limiter           *workqueue.WorkerLimiter
	executionPreparer models.AcceptedRunExecutionPreparer
}

func (s *AgentRunWorkerSupervisor) SetRunTerminalFinalizer(finalizer agentrun.RunTerminalFinalizer) {
	s.mu.Lock()
	s.runFinalizer = finalizer
	s.mu.Unlock()
	s.pools.Each(func(pool *AgentRunWorkerPool) { pool.SetRunTerminalFinalizer(finalizer) })
}

// SetActionCoordinator supplies the shared governed action lifecycle to every
// scope pool created by this supervisor.
func (s *AgentRunWorkerSupervisor) SetActionCoordinator(actions *action.Coordinator) {
	s.mu.Lock()
	s.actions = actions
	s.mu.Unlock()
	s.pools.Each(func(pool *AgentRunWorkerPool) { pool.SetActionCoordinator(actions) })
}

func (s *AgentRunWorkerSupervisor) SetWorkerLimiter(limiter *workqueue.WorkerLimiter) {
	s.mu.Lock()
	s.limiter = limiter
	s.mu.Unlock()
	s.pools.Each(func(pool *AgentRunWorkerPool) { pool.SetWorkerLimiter(limiter) })
}

func NewAgentRunWorkerSupervisor(
	store models.KernelStore,
	resolver conversation.TurnRunnerResolver,
	source workqueue.WorkerScopeSource,
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
	supervisor := &AgentRunWorkerSupervisor{store: store, resolver: resolver, config: config, logger: logger}
	supervisor.pools = workqueue.NewScopePools("run:"+string(config.Kind), source, config.ReconcileInterval, workqueue.PoolRetireAfter, models.WorkFamilyRunDue, logger, supervisor.newPool)
	return supervisor, nil
}

func (s *AgentRunWorkerSupervisor) newPool(scope models.Scope) (*AgentRunWorkerPool, error) {
	pool, err := NewAgentRunWorkerPool(s.store, s.resolver, s.logger, AgentRunWorkerConfig{
		Scope: scope, Kind: s.config.Kind, AssignedAgentID: s.config.AssignedAgentID,
		Concurrency: s.config.Concurrency, MaxActiveForAgent: s.config.MaxActiveForAgent,
		MaxActiveForOwner: s.config.MaxActiveForOwner, MaxActiveForObjective: s.config.MaxActiveForObjective,
		MaxActiveForConcurrencyKey:        s.config.MaxActiveForConcurrencyKey,
		ConversationTaskForegroundReserve: s.config.ConversationTaskForegroundReserve,
		LeaseDuration:                     s.config.LeaseDuration,
		TurnLeaseDuration:                 s.config.TurnLeaseDuration, AgingInterval: s.config.AgingInterval,
		PollInterval:   s.config.PollInterval,
		WorkerIDPrefix: fmt.Sprintf("%s-%s-%s-%s", s.config.WorkerIDPrefix, s.config.Kind, scope.Kind, scope.ID),
	})
	if err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	pool.SetActionCoordinator(s.actions)
	pool.SetRunTerminalFinalizer(s.runFinalizer)
	pool.SetWorkerLimiter(s.limiter)
	pool.SetAcceptedRunExecutionPreparer(s.executionPreparer)
	return pool, nil
}

func (s *AgentRunWorkerSupervisor) Start(parent context.Context) { s.pools.Start(parent) }

func (s *AgentRunWorkerSupervisor) Stop() { s.pools.Stop() }

func (s *AgentRunWorkerSupervisor) Wake() { s.pools.Wake() }

func (s *AgentRunWorkerSupervisor) WakeScope(scope models.Scope) { s.pools.WakeScope(scope) }

// RefreshScopes lists worker scopes now while polling.
func (s *AgentRunWorkerSupervisor) RefreshScopes() { s.pools.Request() }

// SetWorkSignals binds the work queue status. Call before Start.
func (s *AgentRunWorkerSupervisor) SetWorkSignals(status *workqueue.WorkSignalStatus) {
	s.pools.SetSignals(status)
}

func (s *AgentRunWorkerSupervisor) ScopeCount() int { return s.pools.Count() }

func (s *AgentRunWorkerSupervisor) SetAcceptedRunExecutionPreparer(preparer models.AcceptedRunExecutionPreparer) {
	s.mu.Lock()
	s.executionPreparer = preparer
	s.mu.Unlock()
	s.pools.Each(func(pool *AgentRunWorkerPool) { pool.SetAcceptedRunExecutionPreparer(preparer) })
}
