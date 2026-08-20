package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

type CallbackEventWorkerConfig struct {
	WorkerID      string
	LeaseDuration time.Duration
	BaseRetry     time.Duration
	MaximumRetry  time.Duration
	MaxAttempts   int
}

func (c CallbackEventWorkerConfig) normalize() (CallbackEventWorkerConfig, error) {
	c.WorkerID = strings.TrimSpace(c.WorkerID)
	if !validOpaqueIdentifier(c.WorkerID, 256) {
		return CallbackEventWorkerConfig{}, fmt.Errorf("%w: callback worker id is invalid", ErrInvalidCallbackRegistration)
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
		return CallbackEventWorkerConfig{}, fmt.Errorf("%w: callback worker policy is invalid", ErrInvalidCallbackRegistration)
	}
	return c, nil
}

// CallbackEventWorker dispatches provider-verified durable receipts outside
// the provider acknowledgement request. Claims are leased and CAS-updated so
// expired work is recovered after a process or pod restart.
type CallbackEventWorker struct {
	store interface {
		CallbackRegistrationStore
		CallbackEventStore
	}
	service *CallbackIngressService
	config  CallbackEventWorkerConfig
	now     func() time.Time
}

func NewCallbackEventWorker(store interface {
	CallbackRegistrationStore
	CallbackEventStore
}, service *CallbackIngressService, config CallbackEventWorkerConfig) (*CallbackEventWorker, error) {
	if store == nil || service == nil {
		return nil, errors.New("callback event store and ingress service are required")
	}
	normalized, err := config.normalize()
	if err != nil {
		return nil, err
	}
	return &CallbackEventWorker{store: store, service: service, config: normalized, now: time.Now}, nil
}

func (w *CallbackEventWorker) ProcessOne(ctx context.Context, scope Scope) (*CallbackEventReceipt, error) {
	if w == nil || w.store == nil || w.service == nil {
		return nil, errors.New("callback event worker is not configured")
	}
	now := w.now().UTC()
	receipt, err := w.store.ClaimCallbackEvent(ctx, scope, w.config.WorkerID, now, w.config.LeaseDuration)
	if err != nil || receipt == nil {
		return receipt, err
	}
	registration, err := w.store.GetCallbackRegistration(ctx, receipt.Scope, receipt.RegistrationID)
	if err == nil && (registration == nil || registration.Status != CallbackRegistrationActive || registration.Revision != receipt.RegistrationRevision) {
		err = fmt.Errorf("%w: callback registration is unavailable or changed", ErrCallbackRegistrationConflict)
	}
	if err == nil {
		err = w.service.dispatch(ctx, registration, receipt.Event)
	}
	finishedAt := w.now().UTC()
	previousRevision := receipt.Revision
	receipt.Revision++
	receipt.UpdatedAt = finishedAt
	receipt.LeaseOwner, receipt.LeaseExpiresAt = "", time.Time{}
	if err == nil {
		receipt.Status = CallbackEventApplied
		receipt.LastError = ""
		receipt.AppliedAt = &finishedAt
	} else {
		receipt.LastError = boundedCallbackError(err)
		if receipt.Attempts >= w.config.MaxAttempts || errors.Is(err, ErrInvalidCallbackRegistration) {
			receipt.Status = CallbackEventFailed
		} else {
			receipt.Status = CallbackEventPending
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
