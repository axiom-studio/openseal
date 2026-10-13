package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
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

// ActionWorkerSupervisor runs an isolated worker pool per scope with
// ActionCall work. See scopePools for when a scope has a pool.
type ActionWorkerSupervisor struct {
	store       KernelStore
	catalog     ActionExecutionCatalog
	credentials CredentialResolver
	leaseIssuer ActionCredentialLeaseIssuer
	dispatcher  ActionDispatcher
	config      DynamicActionWorkerConfig
	logger      *zap.SugaredLogger
	limiter     atomic.Pointer[WorkerLimiter]
	pools       *scopePools[*ActionWorkerPool]
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
	return newActionWorkerSupervisor(store, catalog, credentials, nil, dispatcher, source, logger, config)
}

func NewActionCredentialLeaseWorkerSupervisor(
	store KernelStore,
	catalog ActionExecutionCatalog,
	issuer ActionCredentialLeaseIssuer,
	dispatcher ActionDispatcher,
	source WorkerScopeSource,
	logger *zap.SugaredLogger,
	config DynamicActionWorkerConfig,
) (*ActionWorkerSupervisor, error) {
	if issuer == nil {
		return nil, errors.New("action credential lease issuer is required")
	}
	return newActionWorkerSupervisor(store, catalog, nil, issuer, dispatcher, source, logger, config)
}

func newActionWorkerSupervisor(
	store KernelStore,
	catalog ActionExecutionCatalog,
	credentials CredentialResolver,
	issuer ActionCredentialLeaseIssuer,
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
	supervisor := &ActionWorkerSupervisor{
		store: store, catalog: catalog, credentials: credentials, leaseIssuer: issuer, dispatcher: dispatcher,
		config: config, logger: logger,
	}
	supervisor.pools = newScopePools("action", source, config.ReconcileInterval, WorkFamilyAction, logger, supervisor.newPool)
	return supervisor, nil
}

func (s *ActionWorkerSupervisor) newPool(scope Scope) (*ActionWorkerPool, error) {
	pool, err := newActionWorkerPool(s.store, s.catalog, s.credentials, s.leaseIssuer, s.dispatcher, s.logger, ActionWorkerConfig{
		Scope: scope, Concurrency: s.config.Concurrency, PollInterval: s.config.PollInterval,
		LeaseDuration:  s.config.LeaseDuration,
		WorkerIDPrefix: fmt.Sprintf("%s-%s-%s", s.config.WorkerIDPrefix, scope.Kind, scope.ID),
	})
	if err != nil {
		return nil, err
	}
	pool.SetWorkerLimiter(s.limiter.Load())
	return pool, nil
}

func (s *ActionWorkerSupervisor) Start(parent context.Context) { s.pools.start(parent) }

func (s *ActionWorkerSupervisor) Stop() { s.pools.stop() }

func (s *ActionWorkerSupervisor) Wake() { s.pools.wake() }

func (s *ActionWorkerSupervisor) WakeScope(scope Scope) { s.pools.wakeScope(scope) }

// RefreshScopes lists worker scopes now while polling.
func (s *ActionWorkerSupervisor) RefreshScopes() { s.pools.request() }

// SetWorkSignals binds the work queue status. Call before Start.
func (s *ActionWorkerSupervisor) SetWorkSignals(status *WorkSignalStatus) { s.pools.setSignals(status) }

func (s *ActionWorkerSupervisor) SetWorkerLimiter(limiter *WorkerLimiter) {
	s.limiter.Store(limiter)
	s.pools.each(func(pool *ActionWorkerPool) { pool.SetWorkerLimiter(limiter) })
}

// ScopeCount exposes bounded operational state without revealing queued work.
func (s *ActionWorkerSupervisor) ScopeCount() int { return s.pools.count() }
