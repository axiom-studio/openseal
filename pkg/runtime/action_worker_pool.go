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
	signals atomic.Pointer[WorkSignalStatus]
	// deadlines lets a queue-driven pool schedule its next retry or lease
	// expiry instead of polling. Stores without it always poll.
	deadlines ActionDeadlineStore
	activity  poolActivity
	now       func() time.Time
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
	pool := &ActionWorkerPool{worker: worker, config: config, logger: logger, wake: make(chan struct{}, 1), poll: newWorkerPollSignal(), now: time.Now}
	pool.deadlines, _ = store.(ActionDeadlineStore)
	pool.activity.touch()
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
	p.activity.touch()
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

// SetWorkSignals lets the pool schedule its next durable deadline into the
// work queue and sleep until woken while status is healthy. Without it the
// pool polls.
func (p *ActionWorkerPool) SetWorkSignals(status *WorkSignalStatus) {
	p.signals.Store(status)
}

// queueDriven reports whether idle waits may rely on the work queue.
func (p *ActionWorkerPool) queueDriven() bool {
	return p.deadlines != nil && p.signals.Load().Healthy()
}

// idleFor reports how long the pool has neither claimed nor been woken.
func (p *ActionWorkerPool) idleFor(now time.Time) time.Duration {
	return p.activity.idleFor(now)
}

// Without a work queue, one timer per scope recovers missed or cross-process
// work. Empty claims retain the configured latency without multiplying reads
// by the worker concurrency. Successful claims hand off before provider
// execution. While the queue is healthy the loop instead schedules the next
// retry or lease expiry into the queue after each wake and holds no timer, so
// an idle scope issues no reads at all.
func (p *ActionWorkerPool) pollClaims(ctx context.Context) {
	defer p.wg.Done()
	seed := p.config.WorkerIDPrefix + "-poll"
	failures := 0
	for {
		// Capture before reading the deadline so a commit during the read
		// still ends the following wait.
		_, observedWake := p.poll.snapshot()
		signals := p.signals.Load()
		healthy, healthChanged := signals.watch()
		wait := pollWait(workerPollDelay(p.config.PollInterval, 0, seed))
		if healthy && p.deadlines != nil {
			p.activity.begin()
			deadline, err := p.deadlines.NextActionDeadline(ctx, p.config.Scope, p.now().UTC())
			p.activity.end()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				failures++
				p.logger.Warnw("action deadline lookup failed", "scopeKind", p.config.Scope.Kind, "scopeId", p.config.Scope.ID, "error", err)
				signals.scheduleIn(WorkFamilyAction, p.config.Scope, workerPollDelay(p.config.PollInterval, failures, seed))
			} else {
				failures = 0
				signals.schedule(WorkFamilyAction, p.config.Scope, deadline)
			}
			wait = waitForever()
		} else {
			observedWake = nil
		}
		if !waitIdle(ctx, observedWake, healthChanged, wait) {
			return
		}
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
		p.activity.begin()
		release, acquireErr := p.limiter.Load().acquire(ctx)
		if acquireErr != nil {
			p.activity.end()
			return
		}
		result, err := p.worker.RunOnce(ctx, p.config.Scope, workerID, p.config.LeaseDuration)
		release()
		p.activity.end()
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
		if p.queueDriven() {
			// No poll tick follows a failed claim while queue-driven, so retry
			// after the backoff instead of waiting for a wake that may never come.
			p.requestClaim()
		}
	}
}
