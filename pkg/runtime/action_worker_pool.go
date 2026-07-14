package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
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
	limiter *WorkerLimiter
}

func NewActionWorkerPool(store KernelStore, catalog ActionExecutionCatalog, credentials CredentialResolver, dispatcher ActionDispatcher, logger *zap.SugaredLogger, config ActionWorkerConfig) (*ActionWorkerPool, error) {
	if store == nil || catalog == nil || dispatcher == nil {
		return nil, errors.New("action store, catalog, and dispatcher are required")
	}
	if err := config.applyDefaults(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	return &ActionWorkerPool{worker: NewActionWorker(store, catalog, credentials, dispatcher), config: config, logger: logger, wake: make(chan struct{}, 1)}, nil
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
}

func (p *ActionWorkerPool) Stop() {
	p.mu.Lock()
	cancel := p.cancel
	p.cancel = nil
	p.mu.Unlock()
	if cancel != nil {
		cancel()
		p.wg.Wait()
	}
}

func (p *ActionWorkerPool) Wake() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *ActionWorkerPool) SetWorkerLimiter(limiter *WorkerLimiter) {
	p.limiter = limiter
}

func (p *ActionWorkerPool) run(ctx context.Context, workerID string) {
	defer p.wg.Done()
	consecutiveFailures := 0
	for {
		release, acquireErr := p.limiter.acquire(ctx)
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
			continue
		}
		if !waitForWorkerPoll(ctx, p.wake, workerPollDelay(p.config.PollInterval, consecutiveFailures, workerID)) {
			return
		}
	}
}
