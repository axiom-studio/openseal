package callback

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
)

type EventWorkerConfig struct {
	WorkerID      string
	LeaseDuration time.Duration
	BaseRetry     time.Duration
	MaximumRetry  time.Duration
	MaxAttempts   int
}

func (c EventWorkerConfig) normalize() (EventWorkerConfig, error) {
	c.WorkerID = strings.TrimSpace(c.WorkerID)
	if !models.IsValidOpaqueIdentifier(c.WorkerID, 256) {
		return EventWorkerConfig{}, fmt.Errorf("%w: callback worker id is invalid", models.ErrInvalidCallbackRegistration)
	}
	if c.LeaseDuration == 0 {
		c.LeaseDuration = time.Minute
	}
	if c.BaseRetry == 0 {
		c.BaseRetry = time.Second
	}
	if c.MaximumRetry == 0 {
		c.MaximumRetry = 5 * time.Minute
	}
	if c.MaxAttempts == 0 {
		c.MaxAttempts = 10
	}
	if c.LeaseDuration <= 0 || c.BaseRetry <= 0 || c.MaximumRetry < c.BaseRetry || c.MaxAttempts < 1 || c.MaxAttempts > 1000 {
		return EventWorkerConfig{}, fmt.Errorf("%w: callback worker policy is invalid", models.ErrInvalidCallbackRegistration)
	}
	return c, nil
}

// EventWorker dispatches provider-verified durable receipts outside
// the provider acknowledgement request. Claims are leased and CAS-updated so
// expired work is recovered after a process or pod restart.
type EventWorker struct {
	store interface {
		models.CallbackRegistrationStore
		models.CallbackEventStore
	}
	service *IngressService
	config  EventWorkerConfig
	now     func() time.Time
}

func NewEventWorker(store interface {
	models.CallbackRegistrationStore
	models.CallbackEventStore
}, service *IngressService, config EventWorkerConfig, opts ...models.ServiceOption) (*EventWorker, error) {
	if store == nil || service == nil {
		return nil, errors.New("callback event store and ingress service are required")
	}
	normalized, err := config.normalize()
	if err != nil {
		return nil, err
	}
	return &EventWorker{store: store, service: service, config: normalized, now: models.ClockFrom(opts)}, nil
}

func (w *EventWorker) ProcessOne(ctx context.Context, scope models.Scope) (*models.CallbackEventReceipt, error) {
	if w == nil || w.store == nil || w.service == nil {
		return nil, errors.New("callback event worker is not configured")
	}
	now := w.now().UTC()
	receipt, err := w.store.ClaimCallbackEvent(ctx, scope, w.config.WorkerID, now, w.config.LeaseDuration)
	if err != nil || receipt == nil {
		return receipt, err
	}
	registration, err := w.store.GetCallbackRegistration(ctx, receipt.Scope, receipt.RegistrationID)
	if err == nil && (registration == nil || registration.Status != models.CallbackRegistrationActive || registration.Revision != receipt.RegistrationRevision) {
		err = fmt.Errorf("%w: callback registration is unavailable or changed", models.ErrCallbackRegistrationConflict)
	}
	if err == nil {
		err = w.service.dispatch(ctx, registration, receipt.Event, receipt.CreatedAt)
	}
	finishedAt := w.now().UTC()
	previousRevision := receipt.Revision
	receipt.Revision++
	receipt.UpdatedAt = finishedAt
	receipt.LeaseOwner, receipt.LeaseExpiresAt = "", time.Time{}
	if err == nil {
		receipt.Status = models.CallbackEventApplied
		receipt.LastError = ""
		receipt.AppliedAt = &finishedAt
	} else {
		receipt.LastError = boundedCallbackError(err)
		if receipt.Attempts >= w.config.MaxAttempts || errors.Is(err, models.ErrInvalidCallbackRegistration) {
			receipt.Status = models.CallbackEventFailed
		} else {
			receipt.Status = models.CallbackEventPending
			receipt.AvailableAt = finishedAt.Add(callbackRetryDelay(w.config.BaseRetry, w.config.MaximumRetry, receipt.Attempts))
		}
	}
	if saveErr := w.store.SaveClaimedCallbackEvent(ctx, receipt, previousRevision, w.config.WorkerID); saveErr != nil {
		return nil, saveErr
	}
	return receipt, err
}

func callbackRetryDelay(base, maximum time.Duration, attempts int) time.Duration {
	delay := base
	for i := 1; i < attempts && delay < maximum; i++ {
		if delay > maximum/2 {
			return maximum
		}
		delay *= 2
	}
	if delay > maximum {
		return maximum
	}
	return delay
}
