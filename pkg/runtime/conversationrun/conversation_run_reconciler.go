package conversationrun

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/workqueue"
	"go.uber.org/zap"
)

type ReconcilerConfig struct {
	Interval time.Duration
}

func (c *ReconcilerConfig) applyDefaults() error {
	if c.Interval == 0 {
		c.Interval = 5 * time.Second
	}
	if c.Interval < 100*time.Millisecond || c.Interval > time.Hour {
		return errors.New("conversation Run reconciliation interval must be between 100ms and one hour")
	}
	return nil
}

// Reconciler periodically scans active host scopes so a
// committed message is never dependent on the process surviving the immediate
// scheduling call. One loop owns reconciliation to avoid local overlap;
// idempotent Run creation and service cursors make multiple replicas safe.
type Reconciler struct {
	scheduler *Scheduler
	scopes    workqueue.WorkerScopeSource
	config    ReconcilerConfig
	logger    *zap.SugaredLogger
	wake      chan struct{}
	signals   atomic.Pointer[workqueue.WorkSignalStatus]
	queue     *workqueue.ScopeWakeQueue
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	startOnce sync.Once
	stopOnce  sync.Once
}

func NewReconciler(
	scheduler *Scheduler,
	scopes workqueue.WorkerScopeSource,
	logger *zap.SugaredLogger,
	config ReconcilerConfig,
) (*Reconciler, error) {
	if scheduler == nil || scopes == nil {
		return nil, errors.New("conversation Run scheduler and scope source are required")
	}
	if err := config.applyDefaults(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	reconciler := &Reconciler{
		scheduler: scheduler, scopes: scopes, config: config, logger: logger, wake: make(chan struct{}, 1),
		queue: workqueue.NewScopeWakeQueue(),
	}
	return reconciler, nil
}

func (r *Reconciler) Start(ctx context.Context) {
	r.startOnce.Do(func() {
		workerCtx, cancel := context.WithCancel(ctx)
		r.cancel = cancel
		r.wg.Add(1)
		go r.loop(workerCtx)
		r.Wake()
	})
}

func (r *Reconciler) Stop() {
	r.stopOnce.Do(func() {
		if r.cancel != nil {
			r.cancel()
		}
		r.wg.Wait()
	})
}

func (r *Reconciler) Wake() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// WakeScope reconciles one scope promptly instead of every listed scope.
func (r *Reconciler) WakeScope(scope models.Scope) {
	r.queue.WakeScope(scope)
}

// SetWorkSignals stops the fixed reconciliation tick while the work queue is
// healthy: the reconciler then runs only for scopes the queue delivers. Call
// before Start.
func (r *Reconciler) SetWorkSignals(status *workqueue.WorkSignalStatus) {
	r.signals.Store(status)
}

func (r *Reconciler) Reconcile(ctx context.Context) error {
	scopes, err := r.scopes.ListWorkerScopes(ctx)
	if err != nil {
		return err
	}
	return r.reconcileScopes(ctx, workqueue.NormalizeScopes(scopes))
}

func (r *Reconciler) loop(ctx context.Context) {
	defer r.wg.Done()
	ticker := time.NewTicker(r.config.Interval)
	defer ticker.Stop()
	signals := r.signals.Load()
	known := workqueue.NewKnownWorkerScopes(r.scopes, r.config.Interval)
	gate := workqueue.NewWorkScopeGate(r.scopes, r.config.Interval)
	for {
		healthy, healthChanged := signals.Watch()
		full := false
		select {
		case <-ctx.Done():
			return
		case <-r.wake:
			// A bound queue rebuilds its schedule from the store, so a full
			// scan is only the fallback while there is none.
			full = !healthy && !signals.QueueBound()
		case <-ticker.C:
			// While the queue is healthy every change and deadline arrives
			// per scope, so the fixed tick is only the fallback.
			full = !healthy
		case <-r.queue.Signals():
		case <-healthChanged:
			continue
		}
		spacing := time.Duration(0)
		if healthy {
			spacing = r.config.Interval
		}
		now := time.Now()
		scopes, deferred := r.queue.Take(now, spacing)
		for scope, at := range deferred {
			signals.Schedule(models.WorkFamilyConversationDue, scope, &at)
		}
		if full {
			listed, err := known.List(ctx)
			if err == nil {
				r.queue.Ran(listed, now)
				err = r.reconcileScopes(ctx, listed)
			}
			if err != nil && ctx.Err() == nil {
				r.logger.Warnw("failed to reconcile conversation Runs", "error", err)
			}
			continue
		}
		allowed, err := gate.Filter(ctx, scopes)
		if err != nil && ctx.Err() == nil {
			r.logger.Warnw("failed to check conversation Run scopes", "error", err)
			if healthy {
				for _, scope := range scopes {
					signals.ScheduleIn(models.WorkFamilyConversationDue, scope, r.config.Interval)
				}
			}
		}
		for _, scope := range allowed {
			_, err := r.scheduler.ReconcileScope(ctx, scope)
			if ctx.Err() != nil {
				return
			}
			// A queue-driven scope has no tick to retry it; back off instead.
			if retry := r.queue.Outcome(scope, err, r.config.Interval, time.Now()); retry != nil {
				r.logger.Warnw("failed to reconcile conversation Runs", "scopeKind", scope.Kind, "scopeId", scope.ID, "error", err)
				if healthy {
					signals.Schedule(models.WorkFamilyConversationDue, scope, retry)
				}
			}
		}
	}
}

func (r *Reconciler) reconcileScopes(ctx context.Context, scopes []models.Scope) error {
	for _, scope := range scopes {
		if _, err := r.scheduler.ReconcileScope(ctx, scope); err != nil {
			return err
		}
	}
	return nil
}
