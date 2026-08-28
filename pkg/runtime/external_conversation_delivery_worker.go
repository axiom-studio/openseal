package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
)

type ExternalConversationDeliveryHostRequest struct {
	Endpoint *ExternalConversationEndpoint   `json:"endpoint"`
	Adapter  *skill.BoundConversationAdapter `json:"adapter"`
	Delivery *ExternalConversationDelivery   `json:"delivery"`
	Message  *ChannelMessage                 `json:"message"`
}

type ExternalConversationAcknowledgementStatus string

const (
	ExternalConversationAcknowledgementFound    ExternalConversationAcknowledgementStatus = "found"
	ExternalConversationAcknowledgementNotFound ExternalConversationAcknowledgementStatus = "not_found"
	ExternalConversationAcknowledgementUnknown  ExternalConversationAcknowledgementStatus = "unknown"
)

type ExternalConversationDeliveryAcknowledgement struct {
	Status            ExternalConversationAcknowledgementStatus `json:"status"`
	ProviderMessageID string                                    `json:"providerMessageId,omitempty"`
}

func (a *ExternalConversationDeliveryAcknowledgement) Validate() error {
	if a == nil {
		return fmt.Errorf("%w: acknowledgement is required", ErrInvalidExternalConversation)
	}
	switch a.Status {
	case ExternalConversationAcknowledgementFound:
		if !validExternalConversationReference(a.ProviderMessageID, 1024) {
			return fmt.Errorf("%w: found acknowledgement requires a provider message id", ErrInvalidExternalConversation)
		}
	case ExternalConversationAcknowledgementNotFound, ExternalConversationAcknowledgementUnknown:
		if a.ProviderMessageID != "" {
			return fmt.Errorf("%w: unresolved acknowledgement cannot include a provider message id", ErrInvalidExternalConversation)
		}
	default:
		return fmt.Errorf("%w: acknowledgement status is invalid", ErrInvalidExternalConversation)
	}
	return nil
}

type ExternalConversationDeliveryOutcome string

const (
	ExternalConversationDeliveryOutcomeDelivered ExternalConversationDeliveryOutcome = "delivered"
	ExternalConversationDeliveryOutcomeRetry     ExternalConversationDeliveryOutcome = "retry"
	ExternalConversationDeliveryOutcomeFailed    ExternalConversationDeliveryOutcome = "failed"
)

type ExternalConversationDeliveryHostResult struct {
	Outcome           ExternalConversationDeliveryOutcome `json:"outcome"`
	ProviderMessageID string                              `json:"providerMessageId,omitempty"`
	RetryAfter        time.Duration                       `json:"retryAfter,omitempty"`
	ErrorCode         string                              `json:"errorCode,omitempty"`
	Summary           string                              `json:"summary,omitempty"`
}

func (r *ExternalConversationDeliveryHostResult) Validate() error {
	if r == nil || r.RetryAfter < 0 || len(r.ErrorCode) > 128 || len(r.Summary) > 1024 ||
		strings.ContainsAny(r.ErrorCode, "\r\n") || strings.ContainsAny(r.Summary, "\r\n") {
		return fmt.Errorf("%w: invalid delivery host result", ErrInvalidExternalConversation)
	}
	switch r.Outcome {
	case ExternalConversationDeliveryOutcomeDelivered:
		if !validExternalConversationReference(r.ProviderMessageID, 1024) || r.ErrorCode != "" || r.RetryAfter != 0 {
			return fmt.Errorf("%w: delivered result is incomplete", ErrInvalidExternalConversation)
		}
	case ExternalConversationDeliveryOutcomeRetry:
		if r.ProviderMessageID != "" || r.ErrorCode == "" {
			return fmt.Errorf("%w: retry result requires a safe error code", ErrInvalidExternalConversation)
		}
	case ExternalConversationDeliveryOutcomeFailed:
		if r.ProviderMessageID != "" || r.ErrorCode == "" || r.RetryAfter != 0 {
			return fmt.Errorf("%w: failed result requires a safe error code", ErrInvalidExternalConversation)
		}
	default:
		return fmt.Errorf("%w: invalid delivery outcome", ErrInvalidExternalConversation)
	}
	return nil
}

// ExternalConversationAdapterHost is the only executable provider boundary.
// Adapter code and credential redemption belong to the Skill host. Requests
// carry the exact immutable Skill binding with opaque credential references;
// no secret values enter OpenSeal state or this protocol.
type ExternalConversationAdapterHost interface {
	LookupExternalConversationDelivery(
		context.Context,
		ExternalConversationDeliveryHostRequest,
	) (*ExternalConversationDeliveryAcknowledgement, error)
	DeliverExternalConversation(
		context.Context,
		ExternalConversationDeliveryHostRequest,
	) (*ExternalConversationDeliveryHostResult, error)
}

type ExternalConversationDeliveryWorkerConfig struct {
	WorkerID      string
	LeaseDuration time.Duration
	BaseRetry     time.Duration
	MaximumRetry  time.Duration
}

func (c ExternalConversationDeliveryWorkerConfig) normalize() (ExternalConversationDeliveryWorkerConfig, error) {
	c.WorkerID = strings.TrimSpace(c.WorkerID)
	if !validOpaqueIdentifier(c.WorkerID, 256) {
		return ExternalConversationDeliveryWorkerConfig{}, fmt.Errorf("%w: worker id is invalid", ErrInvalidExternalConversation)
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
	if c.LeaseDuration <= 0 || c.BaseRetry <= 0 || c.MaximumRetry < c.BaseRetry {
		return ExternalConversationDeliveryWorkerConfig{}, fmt.Errorf("%w: worker durations are invalid", ErrInvalidExternalConversation)
	}
	return c, nil
}

type ExternalConversationDeliveryWorker struct {
	store    ExternalConversationStore
	resolver ExternalConversationAdapterResolver
	host     ExternalConversationAdapterHost
	config   ExternalConversationDeliveryWorkerConfig
	now      func() time.Time
}

func NewExternalConversationDeliveryWorker(
	store ExternalConversationStore,
	resolver ExternalConversationAdapterResolver,
	host ExternalConversationAdapterHost,
	config ExternalConversationDeliveryWorkerConfig,
) (*ExternalConversationDeliveryWorker, error) {
	if store == nil || resolver == nil || host == nil {
		return nil, errors.New("external conversation store, adapter resolver, and host are required")
	}
	normalized, err := config.normalize()
	if err != nil {
		return nil, err
	}
	return &ExternalConversationDeliveryWorker{
		store: store, resolver: resolver, host: host, config: normalized, now: time.Now,
	}, nil
}

func (w *ExternalConversationDeliveryWorker) ProcessOne(ctx context.Context, scope Scope) (*ExternalConversationDelivery, error) {
	if w == nil || w.store == nil || w.resolver == nil || w.host == nil {
		return nil, errors.New("external conversation delivery worker is not configured")
	}
	now := w.now().UTC()
	delivery, err := w.store.ClaimExternalConversationDelivery(ctx, scope, w.config.WorkerID, now, w.config.LeaseDuration)
	if err != nil || delivery == nil {
		return delivery, err
	}
	if err := w.deliver(ctx, delivery); err != nil {
		if saveErr := w.recordFailure(ctx, delivery, err, "", "The Skill adapter could not confirm delivery.", 0); saveErr != nil {
			return nil, saveErr
		}
		return delivery, err
	}
	return delivery, nil
}

func (w *ExternalConversationDeliveryWorker) deliver(ctx context.Context, delivery *ExternalConversationDelivery) error {
	if delivery.Status != ExternalConversationDeliveryLeased || delivery.LeaseOwner != w.config.WorkerID {
		return ErrExternalConversationLeaseLost
	}
	endpoint, adapter, err := w.resolve(ctx, delivery)
	if err != nil {
		return err
	}
	message, err := w.store.GetChannelMessage(ctx, delivery.Scope, delivery.ConversationID, delivery.ChannelMessageID)
	if err != nil {
		return err
	}
	if message == nil || message.Historical || message.Audience.Kind != ConversationAudienceChannel {
		return fmt.Errorf("%w: canonical delivery message is unavailable", ErrInvalidExternalConversation)
	}
	deliveryEndpoint := cloneExternalConversationEndpoint(endpoint)
	if strings.TrimSpace(deliveryEndpoint.Address) == "" {
		deliveryEndpoint.Address = strings.TrimSpace(delivery.ExternalConversationID)
		if deliveryEndpoint.Address == "" {
			return fmt.Errorf("%w: installation-wide endpoint delivery requires the originating conversation", ErrInvalidExternalConversation)
		}
	}
	request := ExternalConversationDeliveryHostRequest{
		Endpoint: deliveryEndpoint, Adapter: adapter, Delivery: cloneExternalConversationDelivery(delivery), Message: message,
	}
	if delivery.Attempt > 1 && adapter.Adapter.Delivery.SupportsAcknowledgementLookup {
		acknowledgement, lookupErr := w.host.LookupExternalConversationDelivery(ctx, request)
		if lookupErr == nil {
			if validateErr := acknowledgement.Validate(); validateErr != nil {
				return validateErr
			}
			if acknowledgement.Status == ExternalConversationAcknowledgementFound {
				return w.markDelivered(ctx, delivery, acknowledgement.ProviderMessageID)
			}
		}
		// Lookup failure or an unknown acknowledgement does not abandon the
		// durable item. The declared idempotency key is reused for safe replay.
	}
	result, err := w.host.DeliverExternalConversation(ctx, request)
	if err != nil {
		return err
	}
	if err := result.Validate(); err != nil {
		return err
	}
	switch result.Outcome {
	case ExternalConversationDeliveryOutcomeDelivered:
		return w.markDelivered(ctx, delivery, result.ProviderMessageID)
	case ExternalConversationDeliveryOutcomeRetry:
		if result.RetryAfter > 0 && !adapter.Adapter.Delivery.SupportsRetryAfter {
			return fmt.Errorf("%w: adapter returned undeclared retry-after", ErrInvalidExternalConversation)
		}
		return w.recordFailure(ctx, delivery, errors.New("adapter requested retry"), result.ErrorCode, result.Summary, result.RetryAfter)
	case ExternalConversationDeliveryOutcomeFailed:
		return w.markFailed(ctx, delivery, result.ErrorCode, result.Summary)
	default:
		return ErrInvalidExternalConversation
	}
}

func (w *ExternalConversationDeliveryWorker) resolve(
	ctx context.Context,
	delivery *ExternalConversationDelivery,
) (*ExternalConversationEndpoint, *skill.BoundConversationAdapter, error) {
	endpoint, err := w.store.GetExternalConversationEndpoint(ctx, delivery.Scope, delivery.EndpointID)
	if err != nil {
		return nil, nil, err
	}
	if endpoint == nil || endpoint.Status != ExternalConversationEndpointActive ||
		endpoint.Revision != delivery.EndpointRevision ||
		!externalConversationAdapterBelongsToEndpoint(endpoint.Adapter, delivery.Adapter) {
		return nil, nil, ErrExternalConversationConflict
	}
	ref := delivery.Adapter
	adapter, err := w.resolver.ResolveConversationAdapter(
		ctx, skill.ScopeReference{Kind: delivery.Scope.Kind, ID: delivery.Scope.ID}, endpoint.DeploymentID,
		ref.SkillID, ref.SkillVersion, ref.AdapterID, skill.BindingReference{ID: ref.BindingID, Revision: ref.BindingRevision},
	)
	if err != nil || adapter == nil || adapter.Binding == nil ||
		adapter.Binding.SourceIdentity != ref.SourceIdentity || adapter.Adapter.Provider != endpoint.Provider {
		return nil, nil, ErrExternalConversationConflict
	}
	if !containsConversationDeliveryOperation(adapter.Adapter.Delivery.Operations, delivery.Operation) {
		return nil, nil, fmt.Errorf("%w: delivery operation is no longer available", ErrExternalConversationConflict)
	}
	return endpoint, adapter, nil
}

func (w *ExternalConversationDeliveryWorker) markDelivered(
	ctx context.Context,
	delivery *ExternalConversationDelivery,
	providerMessageID string,
) error {
	now := w.now().UTC()
	mapping := &ExternalMessageMapping{
		Scope: delivery.Scope, EndpointID: delivery.EndpointID, Direction: ExternalMessageOutbound,
		ExternalMessageID: providerMessageID, ConversationID: delivery.ConversationID, ChannelMessageID: delivery.ChannelMessageID,
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := w.store.SaveExternalMessageMapping(ctx, mapping, 0); err != nil {
		if !errors.Is(err, ErrExternalConversationConflict) {
			return err
		}
		current, getErr := w.store.GetExternalMessageMapping(
			ctx, delivery.Scope, delivery.EndpointID, ExternalMessageOutbound, providerMessageID,
		)
		if getErr != nil || current == nil || current.ConversationID != delivery.ConversationID ||
			current.ChannelMessageID != delivery.ChannelMessageID {
			return ErrExternalConversationConflict
		}
	}
	next := cloneExternalConversationDelivery(delivery)
	next.Status = ExternalConversationDeliveryDelivered
	next.ProviderMessageID = providerMessageID
	next.LeaseOwner, next.LeaseExpiresAt = "", time.Time{}
	next.ErrorCode, next.Summary = "", ""
	next.DeliveredAt, next.UpdatedAt = now, now
	next.Revision++
	if err := next.Validate(); err != nil {
		return err
	}
	if err := w.store.SaveExternalConversationDelivery(ctx, next, delivery.Revision, w.config.WorkerID); err != nil {
		return err
	}
	*delivery = *next
	return nil
}

func (w *ExternalConversationDeliveryWorker) markFailed(
	ctx context.Context,
	delivery *ExternalConversationDelivery,
	code, summary string,
) error {
	now := w.now().UTC()
	next := cloneExternalConversationDelivery(delivery)
	next.Status = ExternalConversationDeliveryFailed
	next.LeaseOwner, next.LeaseExpiresAt = "", time.Time{}
	next.ErrorCode, next.Summary = code, summary
	next.UpdatedAt = now
	next.Revision++
	if err := next.Validate(); err != nil {
		return err
	}
	if err := w.store.SaveExternalConversationDelivery(ctx, next, delivery.Revision, w.config.WorkerID); err != nil {
		return err
	}
	*delivery = *next
	return nil
}

func (w *ExternalConversationDeliveryWorker) recordFailure(
	ctx context.Context,
	delivery *ExternalConversationDelivery,
	processingErr error,
	code, summary string,
	retryAfter time.Duration,
) error {
	now := w.now().UTC()
	next := cloneExternalConversationDelivery(delivery)
	next.LeaseOwner, next.LeaseExpiresAt = "", time.Time{}
	if code == "" {
		code = externalConversationDeliveryErrorCode(processingErr)
	}
	if summary == "" {
		summary = "The Skill adapter could not confirm delivery."
	}
	next.ErrorCode, next.Summary = code, summary
	next.UpdatedAt = now
	next.Revision++
	if next.Attempt >= next.MaximumAttempts {
		next.Status = ExternalConversationDeliveryFailed
	} else {
		next.Status = ExternalConversationDeliveryRetry
		delay := exponentialExternalConversationRetry(w.config.BaseRetry, w.config.MaximumRetry, next.Attempt)
		if retryAfter > 0 {
			if retryAfter > w.config.MaximumRetry {
				retryAfter = w.config.MaximumRetry
			}
			delay = retryAfter
		}
		next.AvailableAt = now.Add(delay)
	}
	if err := next.Validate(); err != nil {
		return err
	}
	if err := w.store.SaveExternalConversationDelivery(ctx, next, delivery.Revision, w.config.WorkerID); err != nil {
		return err
	}
	*delivery = *next
	return nil
}

func externalConversationDeliveryErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrExternalConversationConflict):
		return "endpoint_or_adapter_conflict"
	case errors.Is(err, ErrInvalidExternalConversation):
		return "invalid_delivery"
	default:
		return "adapter_delivery_unconfirmed"
	}
}
