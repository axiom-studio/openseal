package runtime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

type ExternalConversationSupervisorConfig struct {
	Interval         time.Duration
	BatchSize        int
	Inbox            ExternalConversationInboxWorkerConfig
	Delivery         ExternalConversationDeliveryWorkerConfig
	Callback         CallbackEventWorkerConfig
	Acknowledgements RunProgressAcknowledgementWorkerConfig
}

func (c *ExternalConversationSupervisorConfig) applyDefaults() error {
	if c.Interval == 0 {
		c.Interval = time.Second
	}
	if c.BatchSize == 0 {
		c.BatchSize = 100
	}
	if c.Interval < 100*time.Millisecond || c.Interval > time.Hour || c.BatchSize < 1 || c.BatchSize > 1000 {
		return errors.New("external conversation interval or batch size is invalid")
	}
	return nil
}

// ExternalConversationSupervisor runs the provider-neutral durable transport
// loop. Provider verification, normalization, and delivery remain in the
// Skill-owned adapter host; this supervisor only advances canonical state.
type ExternalConversationSupervisor struct {
	inbox            *ExternalConversationInboxWorker
	replies          *ExternalConversationReplyWorker
	delivery         *ExternalConversationDeliveryWorker
	approvals        *ApprovalNotificationWorker
	callbacks        *CallbackEventWorker
	acknowledgements *RunProgressAcknowledgementWorker
	scopes           WorkerScopeSource
	config           ExternalConversationSupervisorConfig
	logger           *zap.SugaredLogger
	wake             chan struct{}
	signals          atomic.Pointer[WorkSignalStatus]
	queue            *scopeWakeQueue
	deadlines        ExternalConversationDeadlineStore
	cancel           context.CancelFunc
	wg               sync.WaitGroup
	startOnce        sync.Once
	stopOnce         sync.Once
}

func (s *ExternalConversationSupervisor) SetCallbackWorker(worker *CallbackEventWorker) {
	if s != nil {
		s.callbacks = worker
	}
}

func (s *ExternalConversationSupervisor) SetAcknowledgementWorker(worker *RunProgressAcknowledgementWorker) {
	if s != nil {
		s.acknowledgements = worker
	}
}

func NewExternalConversationSupervisor(
	inbox *ExternalConversationInboxWorker,
	replies *ExternalConversationReplyWorker,
	delivery *ExternalConversationDeliveryWorker,
	approvals *ApprovalNotificationWorker,
	scopes WorkerScopeSource,
	logger *zap.SugaredLogger,
	config ExternalConversationSupervisorConfig,
) (*ExternalConversationSupervisor, error) {
	if inbox == nil || replies == nil || delivery == nil || approvals == nil || scopes == nil {
		return nil, errors.New("external conversation workers and scope source are required")
	}
	if err := config.applyDefaults(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	supervisor := &ExternalConversationSupervisor{
		inbox: inbox, replies: replies, delivery: delivery, approvals: approvals, scopes: scopes,
		config: config, logger: logger, wake: make(chan struct{}, 1), queue: newScopeWakeQueue(),
	}
	supervisor.deadlines, _ = inbox.store.(ExternalConversationDeadlineStore)
	return supervisor, nil
}

func (s *ExternalConversationSupervisor) Start(ctx context.Context) {
	s.startOnce.Do(func() {
		workerCtx, cancel := context.WithCancel(ctx)
		s.cancel = cancel
		s.wg.Add(1)
		go s.loop(workerCtx)
		s.Wake()
	})
}

func (s *ExternalConversationSupervisor) Stop() {
	s.stopOnce.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
		s.wg.Wait()
	})
}

func (s *ExternalConversationSupervisor) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// WakeScope reconciles one scope promptly instead of every listed scope.
func (s *ExternalConversationSupervisor) WakeScope(scope Scope) {
	s.queue.wakeScope(scope)
}

// SetWorkSignals stops the fixed tick while the work queue is healthy: the
// supervisor then runs for scopes the queue delivers, and schedules each
// scope's next retry, lease or approval deadline into the queue. Call before
// Start.
func (s *ExternalConversationSupervisor) SetWorkSignals(status *WorkSignalStatus) {
	s.signals.Store(status)
}

func (s *ExternalConversationSupervisor) Reconcile(ctx context.Context) error {
	scopes, err := s.scopes.ListWorkerScopes(ctx)
	if err != nil {
		return err
	}
	return s.reconcileScopes(ctx, normalizedConversationRunScopes(scopes))
}

func (s *ExternalConversationSupervisor) reconcileScopes(ctx context.Context, scopes []Scope) error {
	var reconcileErrors []error
	for _, scope := range scopes {
		if err := s.reconcileScope(ctx, scope); err != nil {
			reconcileErrors = append(reconcileErrors, err)
		}
	}
	return errors.Join(reconcileErrors...)
}

func (s *ExternalConversationSupervisor) reconcileScope(ctx context.Context, scope Scope) error {
	var reconcileErrors []error
	if s.callbacks != nil {
		for range s.config.BatchSize {
			receipt, processErr := s.callbacks.ProcessOne(ctx, scope)
			if processErr != nil {
				reconcileErrors = append(reconcileErrors, processErr)
				break
			}
			if receipt == nil {
				break
			}
		}
	}
	if _, err := s.approvals.ProcessScope(ctx, scope, s.config.BatchSize); err != nil {
		reconcileErrors = append(reconcileErrors, err)
	}
	for range s.config.BatchSize {
		item, processErr := s.inbox.ProcessOne(ctx, scope)
		if processErr != nil {
			reconcileErrors = append(reconcileErrors, processErr)
			break
		}
		if item == nil {
			break
		}
	}
	// Project progress immediately after ingress has created the run. Reply
	// reconciliation can include model work and may complete a short run before
	// a later acknowledgement pass ever observes it as active.
	if s.acknowledgements != nil {
		if _, err := s.acknowledgements.ProcessCommentaryScope(ctx, scope); err != nil {
			reconcileErrors = append(reconcileErrors, err)
		}
	}
	if _, err := s.replies.ProcessScope(ctx, scope, s.config.BatchSize); err != nil {
		reconcileErrors = append(reconcileErrors, err)
	}
	for range s.config.BatchSize {
		delivery, processErr := s.delivery.ProcessOne(ctx, scope)
		if processErr != nil {
			reconcileErrors = append(reconcileErrors, processErr)
			break
		}
		if delivery == nil {
			break
		}
	}
	return errors.Join(reconcileErrors...)
}

// scheduleScope schedules a queue-driven scope's next pass: its earliest
// durable deadline, or a backed-off retry after a failed pass.
func (s *ExternalConversationSupervisor) scheduleScope(ctx context.Context, signals *WorkSignalStatus, scope Scope, failed error) {
	now := time.Now().UTC()
	next, err := s.deadlines.NextExternalConversationDeadline(ctx, scope, now)
	if err != nil && ctx.Err() == nil {
		s.logger.Warnw("external conversation deadline lookup failed", "scopeKind", scope.Kind, "scopeId", scope.ID, "error", err)
	}
	retry := s.queue.outcome(scope, errors.Join(failed, err), s.config.Interval, now)
	signals.schedule(WorkFamilyExternalDue, scope, earliestDeadline(next, retry))
}

func (s *ExternalConversationSupervisor) loop(ctx context.Context) {
	defer s.wg.Done()
	ticker := time.NewTicker(s.config.Interval)
	defer ticker.Stop()
	signals := s.signals.Load()
	known := newKnownWorkerScopes(s.scopes, 10*s.config.Interval)
	gate := newWorkScopeGate(s.scopes, 10*s.config.Interval)
	for {
		healthy, healthChanged := signals.watch()
		queueDriven := healthy && s.deadlines != nil
		full := false
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
			// A bound queue rebuilds its schedule from the store, so a full
			// pass is only the fallback while there is none.
			full = !queueDriven && !(signals.queueBound() && s.deadlines != nil)
		case <-ticker.C:
			// While the queue drives this loop each scope runs when changed or
			// due, so the fixed tick is only the fallback.
			full = !queueDriven
		case <-s.queue.signal:
		case <-healthChanged:
			continue
		}
		spacing := time.Duration(0)
		if queueDriven {
			spacing = s.config.Interval
		}
		now := time.Now()
		scopes, deferred := s.queue.take(now, spacing)
		for scope, at := range deferred {
			signals.schedule(WorkFamilyExternalDue, scope, &at)
		}
		if full {
			listed, err := known.list(ctx)
			if err != nil {
				if ctx.Err() == nil {
					s.logger.Warnw("failed to reconcile external conversations", "error", err)
				}
				continue
			}
			scopes = listed
			s.queue.ran(listed, now)
		} else {
			allowed, err := gate.filter(ctx, scopes)
			if err != nil && ctx.Err() == nil {
				s.logger.Warnw("failed to check external conversation scopes", "error", err)
				if queueDriven {
					for _, scope := range scopes {
						signals.scheduleIn(WorkFamilyExternalDue, scope, 10*s.config.Interval)
					}
				}
			}
			scopes = allowed
		}
		var reconcileErrors []error
		for _, scope := range scopes {
			err := s.reconcileScope(ctx, scope)
			if err != nil {
				reconcileErrors = append(reconcileErrors, err)
			}
			if queueDriven && ctx.Err() == nil {
				s.scheduleScope(ctx, signals, scope, err)
			}
		}
		if err := errors.Join(reconcileErrors...); err != nil && ctx.Err() == nil {
			s.logger.Warnw("failed to reconcile external conversations", "error", err)
		}
	}
}
