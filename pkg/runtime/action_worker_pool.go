package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

type ActionWorkerConfig struct {
	Scope          Scope
	Concurrency    int
	PollInterval   time.Duration
	LeaseDuration  time.Duration
	WorkerIDPrefix string
}

func (c *ActionWorkerConfig) applyDefaults() error {
	if err := c.Scope.Validate(); err != nil {
		return err
	}
	if c.Concurrency <= 0 {
		c.Concurrency = 2
	}
	if c.PollInterval <= 0 {
		c.PollInterval = time.Second
	}
	if c.LeaseDuration <= 0 {
		c.LeaseDuration = time.Minute
	}
	if strings.TrimSpace(c.WorkerIDPrefix) == "" {
		c.WorkerIDPrefix = "action-worker"
	}
	if c.Concurrency > 256 {
		return errors.New("action worker concurrency cannot exceed 256")
	}
	return nil
}

type ActionWorkerPool struct {
	worker  *ActionWorker
	config  ActionWorkerConfig
	logger  *zap.SugaredLogger
	wake    chan struct{}
	mu      sync.Mutex
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	limiter atomic.Pointer[WorkerLimiter]
	poll    *workerPollSignal
}

func NewActionWorkerPool(store KernelStore, catalog ActionExecutionCatalog, credentials CredentialResolver, dispatcher ActionDispatcher, logger *zap.SugaredLogger, config ActionWorkerConfig) (*ActionWorkerPool, error) {
	return newActionWorkerPool(store, catalog, credentials, nil, dispatcher, logger, config)
}

func NewActionCredentialLeaseWorkerPool(store KernelStore, catalog ActionExecutionCatalog, issuer ActionCredentialLeaseIssuer, dispatcher ActionDispatcher, logger *zap.SugaredLogger, config ActionWorkerConfig) (*ActionWorkerPool, error) {
	if issuer == nil {
		return nil, errors.New("action credential lease issuer is required")
	}
	return newActionWorkerPool(store, catalog, nil, issuer, dispatcher, logger, config)
}

func newActionWorkerPool(store KernelStore, catalog ActionExecutionCatalog, credentials CredentialResolver, issuer ActionCredentialLeaseIssuer, dispatcher ActionDispatcher, logger *zap.SugaredLogger, config ActionWorkerConfig) (*ActionWorkerPool, error) {
	if store == nil || catalog == nil || dispatcher == nil {
		return nil, errors.New("action store, catalog, and dispatcher are required")
	}
	if err := config.applyDefaults(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	worker := NewActionWorker(store, catalog, credentials, dispatcher)
	if issuer != nil {
		worker = NewActionWorkerWithCredentialLeaseIssuer(store, catalog, issuer, dispatcher)
	}
	pool := &ActionWorkerPool{worker: worker, config: config, logger: logger, wake: make(chan struct{}, 1), poll: newWorkerPollSignal()}
	worker.onClaim = pool.requestClaim
	return pool, nil
}

func (p *ActionWorkerPool) Start(parent context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	p.cancel = cancel
	for index := 0; index < p.config.Concurrency; index++ {
		p.wg.Add(1)
		go p.run(ctx, fmt.Sprintf("%s-%d", p.config.WorkerIDPrefix, index+1))
	}
	p.wg.Add(1)
	go p.pollClaims(ctx)
	p.requestClaim()
}

func (p *ActionWorkerPool) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel != nil {
		p.cancel()
		p.wg.Wait()
		p.cancel = nil
	}
}

func (p *ActionWorkerPool) Wake() {
	p.poll.notify()
	p.requestClaim()
}

func (p *ActionWorkerPool) WakeScope(scope Scope) {
	if scope == p.config.Scope {
		p.Wake()
	}
}

func (p *ActionWorkerPool) requestClaim() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// One timer per scope recovers missed or cross-process notifications. Empty
// claims retain the configured latency without multiplying reads by the
// worker concurrency. Successful claims hand off before provider execution.
func (p *ActionWorkerPool) pollClaims(ctx context.Context) {
	defer p.wg.Done()
	for waitForWorkerPoll(ctx, nil, workerPollDelay(p.config.PollInterval, 0, p.config.WorkerIDPrefix+"-poll")) {
		p.requestClaim()
	}
}

func (p *ActionWorkerPool) SetWorkerLimiter(limiter *WorkerLimiter) {
	p.limiter.Store(limiter)
}

func (p *ActionWorkerPool) run(ctx context.Context, workerID string) {
	defer p.wg.Done()
	consecutiveFailures := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.wake:
		}
		_, observedWake := p.poll.snapshot()
		release, acquireErr := p.limiter.Load().acquire(ctx)
		if acquireErr != nil {
			return
		}
		result, err := p.worker.RunOnce(ctx, p.config.Scope, workerID, p.config.LeaseDuration)
		release()
		if err != nil && ctx.Err() == nil {
			consecutiveFailures++
			p.logger.Warnw("action worker iteration failed", "worker", workerID, "scopeKind", p.config.Scope.Kind, "scopeId", p.config.Scope.ID, "error", err)
		} else if err == nil {
			consecutiveFailures = 0
		}
		if ctx.Err() != nil {
			return
		}
		if result != nil {
			p.requestClaim()
			continue
		}
		if err == nil {
			continue
		}
		if !waitForWorkerPoll(ctx, observedWake, workerPollDelay(p.config.PollInterval, consecutiveFailures, workerID)) {
			return
		}
	}
}
