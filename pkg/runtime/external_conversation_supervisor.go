package runtime

import (
	"context"
	"errors"
	"sync"
	"time"

	"go.uber.org/zap"
)

type ExternalConversationSupervisorConfig struct {
	Interval  time.Duration
	BatchSize int
	Inbox     ExternalConversationInboxWorkerConfig
	Delivery  ExternalConversationDeliveryWorkerConfig
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
	inbox     *ExternalConversationInboxWorker
	replies   *ExternalConversationReplyWorker
	delivery  *ExternalConversationDeliveryWorker
	scopes    WorkerScopeSource
	config    ExternalConversationSupervisorConfig
	logger    *zap.SugaredLogger
	wake      chan struct{}
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	startOnce sync.Once
	stopOnce  sync.Once
}

func NewExternalConversationSupervisor(
	inbox *ExternalConversationInboxWorker,
	replies *ExternalConversationReplyWorker,
	delivery *ExternalConversationDeliveryWorker,
	scopes WorkerScopeSource,
	logger *zap.SugaredLogger,
	config ExternalConversationSupervisorConfig,
) (*ExternalConversationSupervisor, error) {
	if inbox == nil || replies == nil || delivery == nil || scopes == nil {
		return nil, errors.New("external conversation workers and scope source are required")
	}
	if err := config.applyDefaults(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	return &ExternalConversationSupervisor{
		inbox: inbox, replies: replies, delivery: delivery, scopes: scopes,
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
	for _, scope := range normalizedConversationRunScopes(scopes) {
		for range s.config.BatchSize {
			item, processErr := s.inbox.ProcessOne(ctx, scope)
			if processErr != nil {
				return processErr
			}
			if item == nil {
				break
			}
		}
		if _, err := s.replies.ProcessScope(ctx, scope, s.config.BatchSize); err != nil {
			return err
		}
		for range s.config.BatchSize {
			delivery, processErr := s.delivery.ProcessOne(ctx, scope)
			if processErr != nil {
				return processErr
			}
			if delivery == nil {
				break
			}
		}
	}
	return nil
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
