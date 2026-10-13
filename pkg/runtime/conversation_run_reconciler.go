package runtime

import (
	"context"
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

type ConversationRunReconcilerConfig struct {
	Interval time.Duration
}

func (c *ConversationRunReconcilerConfig) applyDefaults() error {
	if c.Interval == 0 {
		c.Interval = 5 * time.Second
	}
	if c.Interval < 100*time.Millisecond || c.Interval > time.Hour {
		return errors.New("conversation Run reconciliation interval must be between 100ms and one hour")
	}
	return nil
}

// ConversationRunReconciler periodically scans active host scopes so a
// committed message is never dependent on the process surviving the immediate
// scheduling call. One loop owns reconciliation to avoid local overlap;
// idempotent Run creation and service cursors make multiple replicas safe.
type ConversationRunReconciler struct {
	scheduler *ConversationRunScheduler
	scopes    WorkerScopeSource
	config    ConversationRunReconcilerConfig
	logger    *zap.SugaredLogger
	wake      chan struct{}
	signals   atomic.Pointer[WorkSignalStatus]
	queue     *scopeWakeQueue
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	startOnce sync.Once
	stopOnce  sync.Once
}

func NewConversationRunReconciler(
	scheduler *ConversationRunScheduler,
	scopes WorkerScopeSource,
	logger *zap.SugaredLogger,
	config ConversationRunReconcilerConfig,
) (*ConversationRunReconciler, error) {
	if scheduler == nil || scopes == nil {
		return nil, errors.New("conversation Run scheduler and scope source are required")
	}
	if err := config.applyDefaults(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	reconciler := &ConversationRunReconciler{
		scheduler: scheduler, scopes: scopes, config: config, logger: logger, wake: make(chan struct{}, 1),
		queue: newScopeWakeQueue(),
	}
	return reconciler, nil
}

func (r *ConversationRunReconciler) Start(ctx context.Context) {
	r.startOnce.Do(func() {
		workerCtx, cancel := context.WithCancel(ctx)
		r.cancel = cancel
		r.wg.Add(1)
		go r.loop(workerCtx)
		r.Wake()
	})
}

func (r *ConversationRunReconciler) Stop() {
	r.stopOnce.Do(func() {
		if r.cancel != nil {
			r.cancel()
		}
		r.wg.Wait()
	})
}

func (r *ConversationRunReconciler) Wake() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// WakeScope reconciles one scope promptly instead of every listed scope.
func (r *ConversationRunReconciler) WakeScope(scope Scope) {
	r.queue.wakeScope(scope)
}

// SetWorkSignals stops the fixed reconciliation tick while status is healthy:
// the reconciler then runs only for signalled scopes. Call before Start.
func (r *ConversationRunReconciler) SetWorkSignals(status *WorkSignalStatus) {
	r.signals.Store(status)
}

func (r *ConversationRunReconciler) Reconcile(ctx context.Context) error {
	scopes, err := r.scopes.ListWorkerScopes(ctx)
	if err != nil {
		return err
	}
	return r.reconcileScopes(ctx, normalizedConversationRunScopes(scopes))
}

func (r *ConversationRunReconciler) loop(ctx context.Context) {
	defer r.wg.Done()
	ticker := time.NewTicker(r.config.Interval)
	defer ticker.Stop()
	known := newKnownWorkerScopes(r.scopes, r.config.Interval)
	for {
		healthy, healthChanged := r.signals.Load().watch()
		retry := newDeadlineTimer(healthy, r.queue.nextDeadline())
		full, skip := false, false
		select {
		case <-ctx.Done():
			retry.stop()
			return
		case <-r.wake:
			full = true
		case <-ticker.C:
			// While signals are healthy every change is signalled per scope,
			// so the fixed tick is only the fallback for an unhealthy listener.
			full, skip = true, healthy
		case <-r.queue.signal:
		case <-retry.channel():
		case <-healthChanged:
			skip = true
		}
		retry.stop()
		if skip {
			continue
		}
		spacing := time.Duration(0)
		if healthy {
			spacing = r.config.Interval
		}
		now := time.Now()
		queuedFull, scopes := r.queue.take(now, spacing)
		if full || queuedFull {
			r.queue.clearDeadlines()
			listed, err := known.list(ctx)
			if err == nil {
				r.queue.ran(listed, now)
				err = r.reconcileScopes(ctx, listed)
			}
			if err != nil && ctx.Err() == nil {
				r.logger.Warnw("failed to reconcile conversation Runs", "error", err)
			}
			continue
		}
		scopes, err := known.filter(ctx, time.Now(), scopes)
		if err != nil && ctx.Err() == nil {
			r.logger.Warnw("failed to list conversation Run scopes", "error", err)
		}
		for _, scope := range scopes {
			_, err := r.scheduler.ReconcileScope(ctx, scope)
			if ctx.Err() != nil {
				return
			}
			// A signal-driven scope has no tick to retry it; back off instead.
			if !r.queue.outcome(scope, err, r.config.Interval, time.Now()) {
				r.logger.Warnw("failed to reconcile conversation Runs", "scopeKind", scope.Kind, "scopeId", scope.ID, "error", err)
			}
		}
	}
}

func (r *ConversationRunReconciler) reconcileScopes(ctx context.Context, scopes []Scope) error {
	for _, scope := range scopes {
		if _, err := r.scheduler.ReconcileScope(ctx, scope); err != nil {
			return err
		}
	}
	return nil
}

func normalizedConversationRunScopes(scopes []Scope) []Scope {
	seen := make(map[Scope]struct{}, len(scopes))
	result := make([]Scope, 0, len(scopes))
	for _, scope := range scopes {
		if scope.Validate() != nil {
			continue
		}
		if _, exists := seen[scope]; exists {
			continue
		}
		seen[scope] = struct{}{}
		result = append(result, scope)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Kind != result[j].Kind {
			return result[i].Kind < result[j].Kind
		}
		return result[i].ID < result[j].ID
	})
	return result
}
