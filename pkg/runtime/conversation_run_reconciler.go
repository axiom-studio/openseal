package runtime

import (
	"context"
	"errors"
	"sort"
	"sync"
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
	return &ConversationRunReconciler{
		scheduler: scheduler, scopes: scopes, config: config, logger: logger, wake: make(chan struct{}, 1),
	}, nil
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

func (r *ConversationRunReconciler) Reconcile(ctx context.Context) error {
	scopes, err := r.scopes.ListWorkerScopes(ctx)
	if err != nil {
		return err
	}
	scopes = normalizedConversationRunScopes(scopes)
	for _, scope := range scopes {
		if _, err := r.scheduler.ReconcileScope(ctx, scope); err != nil {
			return err
		}
	}
	return nil
}

func (r *ConversationRunReconciler) loop(ctx context.Context) {
	defer r.wg.Done()
	ticker := time.NewTicker(r.config.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.wake:
		case <-ticker.C:
		}
		if err := r.Reconcile(ctx); err != nil && ctx.Err() == nil {
			r.logger.Warnw("failed to reconcile conversation Runs", "error", err)
		}
	}
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
