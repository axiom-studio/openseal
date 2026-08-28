package runtime

import (
	"context"
	"errors"
	"sync"
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
	return &ExternalConversationSupervisor{
		inbox: inbox, replies: replies, delivery: delivery, approvals: approvals, scopes: scopes,
		config: config, logger: logger, wake: make(chan struct{}, 1),
	}, nil
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

func (s *ExternalConversationSupervisor) Reconcile(ctx context.Context) error {
	scopes, err := s.scopes.ListWorkerScopes(ctx)
	if err != nil {
		return err
	}
	var reconcileErrors []error
	for _, scope := range normalizedConversationRunScopes(scopes) {
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
			if _, err := s.acknowledgements.ProcessScope(ctx, scope); err != nil {
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
	}
	return errors.Join(reconcileErrors...)
}

func (s *ExternalConversationSupervisor) loop(ctx context.Context) {
	defer s.wg.Done()
	ticker := time.NewTicker(s.config.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		case <-ticker.C:
		}
		if err := s.Reconcile(ctx); err != nil && ctx.Err() == nil {
			s.logger.Warnw("failed to reconcile external conversations", "error", err)
		}
	}
}
