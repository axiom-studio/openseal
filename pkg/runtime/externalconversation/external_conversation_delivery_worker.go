package externalconversation

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/skill"
)

type ExternalConversationDeliveryHostRequest struct {
	Attachments []ExternalConversationAttachmentContent `json:"attachments,omitempty"`
	Endpoint    *models.ExternalConversationEndpoint    `json:"endpoint"`
	Adapter     *skill.BoundConversationAdapter         `json:"adapter"`
	Delivery    *models.ExternalConversationDelivery    `json:"delivery"`
	Message     *models.ChannelMessage                  `json:"message"`
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
		return fmt.Errorf("%w: acknowledgement is required", models.ErrInvalidExternalConversation)
	}
	switch a.Status {
	case ExternalConversationAcknowledgementFound:
		if !models.IsValidExternalConversationReference(a.ProviderMessageID, 1024) {
			return fmt.Errorf("%w: found acknowledgement requires a provider message id", models.ErrInvalidExternalConversation)
		}
	case ExternalConversationAcknowledgementNotFound, ExternalConversationAcknowledgementUnknown:
		if a.ProviderMessageID != "" {
			return fmt.Errorf("%w: unresolved acknowledgement cannot include a provider message id", models.ErrInvalidExternalConversation)
		}
	default:
		return fmt.Errorf("%w: acknowledgement status is invalid", models.ErrInvalidExternalConversation)
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
	Progress          map[string]interface{}              `json:"progress,omitempty"`
	Outcome           ExternalConversationDeliveryOutcome `json:"outcome"`
	ProviderMessageID string                              `json:"providerMessageId,omitempty"`
	RetryAfter        time.Duration                       `json:"retryAfter,omitempty"`
	ErrorCode         string                              `json:"errorCode,omitempty"`
	Summary           string                              `json:"summary,omitempty"`
}

func (r *ExternalConversationDeliveryHostResult) Validate() error {
	if r == nil || r.RetryAfter < 0 || len(r.ErrorCode) > 128 || len(r.Summary) > 1024 ||
		strings.ContainsAny(r.ErrorCode, "\r\n") || strings.ContainsAny(r.Summary, "\r\n") {
		return fmt.Errorf("%w: invalid delivery host result", models.ErrInvalidExternalConversation)
	}
	if err := models.ValidateExternalConversationConfiguration(r.Progress); err != nil {
		return err
	}
	switch r.Outcome {
	case ExternalConversationDeliveryOutcomeDelivered:
		if !models.IsValidExternalConversationReference(r.ProviderMessageID, 1024) || r.ErrorCode != "" || r.RetryAfter != 0 {
			return fmt.Errorf("%w: delivered result is incomplete", models.ErrInvalidExternalConversation)
		}
	case ExternalConversationDeliveryOutcomeRetry:
		if r.ProviderMessageID != "" || r.ErrorCode == "" {
			return fmt.Errorf("%w: retry result requires a safe error code", models.ErrInvalidExternalConversation)
		}
	case ExternalConversationDeliveryOutcomeFailed:
		if r.ProviderMessageID != "" || r.ErrorCode == "" || r.RetryAfter != 0 {
			return fmt.Errorf("%w: failed result requires a safe error code", models.ErrInvalidExternalConversation)
		}
	default:
		return fmt.Errorf("%w: invalid delivery outcome", models.ErrInvalidExternalConversation)
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
	AttachmentContent models.ArtifactContentStore
	WorkerID          string
	LeaseDuration     time.Duration
	BaseRetry         time.Duration
	MaximumRetry      time.Duration
}

func (c ExternalConversationDeliveryWorkerConfig) normalize() (ExternalConversationDeliveryWorkerConfig, error) {
	c.WorkerID = strings.TrimSpace(c.WorkerID)
	if !models.IsValidOpaqueIdentifier(c.WorkerID, 256) {
		return ExternalConversationDeliveryWorkerConfig{}, fmt.Errorf("%w: worker id is invalid", models.ErrInvalidExternalConversation)
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
		return ExternalConversationDeliveryWorkerConfig{}, fmt.Errorf("%w: worker durations are invalid", models.ErrInvalidExternalConversation)
	}
	return c, nil
}

type ExternalConversationDeliveryWorker struct {
	store    models.ExternalConversationStore
	resolver ExternalConversationAdapterResolver
	host     ExternalConversationAdapterHost
	config   ExternalConversationDeliveryWorkerConfig
	now      func() time.Time
}

func NewExternalConversationDeliveryWorker(
	store models.ExternalConversationStore,
	resolver ExternalConversationAdapterResolver,
	host ExternalConversationAdapterHost,
	config ExternalConversationDeliveryWorkerConfig,
	opts ...models.ServiceOption,
) (*ExternalConversationDeliveryWorker, error) {
	if store == nil || resolver == nil || host == nil {
		return nil, errors.New("external conversation store, adapter resolver, and host are required")
	}
	normalized, err := config.normalize()
	if err != nil {
		return nil, err
	}
	return &ExternalConversationDeliveryWorker{
		store: store, resolver: resolver, host: host, config: normalized, now: models.ClockFrom(opts),
	}, nil
}

func (w *ExternalConversationDeliveryWorker) ProcessOne(ctx context.Context, scope models.Scope) (*models.ExternalConversationDelivery, error) {
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

func (w *ExternalConversationDeliveryWorker) deliver(ctx context.Context, delivery *models.ExternalConversationDelivery) error {
	if delivery.Status != models.ExternalConversationDeliveryLeased || delivery.LeaseOwner != w.config.WorkerID {
		return models.ErrExternalConversationLeaseLost
	}
	endpoint, adapter, err := w.resolve(ctx, delivery)
	if err != nil {
		return err
	}
	message, err := w.store.GetChannelMessage(ctx, delivery.Scope, delivery.ConversationID, delivery.ChannelMessageID)
	if err != nil {
		return err
	}
	if message == nil || message.Historical || message.Audience.Kind != models.ConversationAudienceChannel {
		return fmt.Errorf("%w: canonical delivery message is unavailable", models.ErrInvalidExternalConversation)
	}
	if preview, ok := externalAnswerStreamPreview(delivery); ok {
		// Once the committed reply is queued it owns the streamed message, so a
		// late provisional edit must not overwrite it.
		final, err := w.store.GetExternalConversationDelivery(ctx, delivery.Scope,
			stableExternalConversationID(delivery.Scope, delivery.EndpointID, "delivery", externalConversationReplyKey(delivery.Correlation.ID)))
		if err != nil {
			return err
		}
		if final != nil {
			return w.markFailed(ctx, delivery, "answer_superseded", "The committed reply replaced this preview.")
		}
		// The trigger message only anchors a preview; its provisional text is
		// the body, with no references or files. A Team member's preview
		// speaks as that member.
		sender := endpoint.DeploymentID
		if agent, _ := delivery.Parameters[externalAnswerStreamAgentParameter].(string); agent != "" {
			sender = agent
		}
		message = &models.ChannelMessage{
			ID: message.ID, Scope: message.Scope, ConversationID: message.ConversationID, Sequence: message.Sequence,
			Sender: models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: sender},
			Intent: models.MessageIntentAnswer, Content: preview, Audience: message.Audience,
			ThreadRootID: message.ThreadRootID, ReplyToMessageID: message.ID, CreatedAt: message.CreatedAt,
		}
	}
	message = withRoutineReportHeader(delivery, message)
	deliveryEndpoint := models.CloneExternalConversationEndpoint(endpoint)
	if origin := strings.TrimSpace(delivery.ExternalConversationID); origin != "" {
		deliveryEndpoint.Address = origin
	} else if strings.TrimSpace(deliveryEndpoint.Address) == "" {
		return fmt.Errorf("%w: installation-wide endpoint delivery requires the originating conversation", models.ErrInvalidExternalConversation)
	}
	effectiveDelivery := models.CloneExternalConversationDelivery(delivery)
	// Keep the durable intent and receipt intact; execution uses the currently
	// reviewed connection, including when setup changed while this was queued.
	effectiveDelivery.Adapter = externalConversationResolvedAdapterReference(adapter)
	effectiveDelivery.EndpointRevision = endpoint.Revision
	deliveryEndpoint.Adapter = effectiveDelivery.Adapter
	request := ExternalConversationDeliveryHostRequest{
		Endpoint: deliveryEndpoint, Adapter: adapter, Delivery: effectiveDelivery, Message: message,
	}
	if containsConversationFeature(adapter.Adapter.Features, capability.ConversationFeatureAttachments) && delivery.Operation == capability.ConversationDeliveryMessageSend {
		request.Attachments, err = externalDeliveryAttachments(ctx, w.store, w.config.AttachmentContent, delivery.Scope, message)
		if err != nil {
			return err
		}
	}
	// Typing/progress indicators are ephemeral state, not durable messages.
	// Provider message lookup cannot prove their delivery and can turn a harmless
	// retry into a permanent endpoint conflict, so replay them directly with the
	// same idempotency key.
	if delivery.Attempt > 1 && adapter.Adapter.Delivery.SupportsAcknowledgementLookup &&
		delivery.Operation != capability.ConversationDeliveryTypingIndicator {
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
	if result.Progress != nil {
		delivery.Progress = maps.Clone(result.Progress)
	}
	switch result.Outcome {
	case ExternalConversationDeliveryOutcomeDelivered:
		return w.markDelivered(ctx, delivery, result.ProviderMessageID)
	case ExternalConversationDeliveryOutcomeRetry:
		if result.RetryAfter > 0 && !adapter.Adapter.Delivery.SupportsRetryAfter {
			return fmt.Errorf("%w: adapter returned undeclared retry-after", models.ErrInvalidExternalConversation)
		}
		return w.recordFailure(ctx, delivery, errors.New("adapter requested retry"), result.ErrorCode, result.Summary, result.RetryAfter)
	case ExternalConversationDeliveryOutcomeFailed:
		return w.markFailed(ctx, delivery, result.ErrorCode, result.Summary)
	default:
		return models.ErrInvalidExternalConversation
	}
}

func (w *ExternalConversationDeliveryWorker) resolve(
	ctx context.Context,
	delivery *models.ExternalConversationDelivery,
) (*models.ExternalConversationEndpoint, *skill.BoundConversationAdapter, error) {
	endpoint, err := w.store.GetExternalConversationEndpoint(ctx, delivery.Scope, delivery.EndpointID)
	if err != nil {
		return nil, nil, err
	}
	if !externalConversationSnapshotMatchesEndpoint(endpoint, delivery.Scope, delivery.EndpointID, delivery.EndpointRevision,
		delivery.Adapter, delivery.ExternalConversationID) {
		return nil, nil, models.ErrExternalConversationConflict
	}
	ref := delivery.Adapter
	adapter, err := w.resolver.ResolveConversationAdapterBinding(
		ctx, skill.ScopeReference{Kind: delivery.Scope.Kind, ID: delivery.Scope.ID}, endpoint.DeploymentID,
		ref.BindingID, ref.AdapterID,
	)
	if err != nil || !externalConversationSnapshotMatchesBinding(endpoint, ref, adapter) {
		return nil, nil, models.ErrExternalConversationConflict
	}
	if !containsConversationDeliveryOperation(adapter.Adapter.Delivery.Operations, delivery.Operation) {
		return nil, nil, fmt.Errorf("%w: delivery operation is no longer available", models.ErrExternalConversationConflict)
	}
	return endpoint, adapter, nil
}

func (w *ExternalConversationDeliveryWorker) markDelivered(
	ctx context.Context,
	delivery *models.ExternalConversationDelivery,
	providerMessageID string,
) error {
	now := w.now().UTC()
	// Ephemeral status acknowledgements identify a thread, not a message.
	// Recording them as outbound messages collides with earlier progress or
	// reply mappings that share the thread timestamp. A streamed preview's
	// message is mapped by the committed reply that finishes it, and a
	// deleted message needs no mapping.
	_, preview := externalAnswerStreamPreview(delivery)
	// A reaction belongs to an already mapped message and maps nothing new.
	reaction := delivery.Operation == capability.ConversationDeliveryReactionAdd || delivery.Operation == capability.ConversationDeliveryReactionRemove
	if delivery.Operation != capability.ConversationDeliveryTypingIndicator && delivery.Operation != capability.ConversationDeliveryMessageDelete && !preview && !reaction {
		mapping := &models.ExternalMessageMapping{
			Scope: delivery.Scope, EndpointID: delivery.EndpointID, Direction: models.ExternalMessageOutbound,
			ExternalMessageID: providerMessageID, ConversationID: delivery.ConversationID, ChannelMessageID: delivery.ChannelMessageID,
			Revision: 1, CreatedAt: now, UpdatedAt: now,
		}
		if err := w.store.SaveExternalMessageMapping(ctx, mapping, 0); err != nil {
			if !errors.Is(err, models.ErrExternalConversationConflict) {
				return err
			}
			current, getErr := w.store.GetExternalMessageMapping(
				ctx, delivery.Scope, delivery.EndpointID, models.ExternalMessageOutbound, providerMessageID,
			)
			if getErr != nil || current == nil || current.ConversationID != delivery.ConversationID ||
				current.ChannelMessageID != delivery.ChannelMessageID {
				return models.ErrExternalConversationConflict
			}
		}
	}
	next := models.CloneExternalConversationDelivery(delivery)
	next.Status = models.ExternalConversationDeliveryDelivered
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
	delivery *models.ExternalConversationDelivery,
	code, summary string,
) error {
	now := w.now().UTC()
	next := models.CloneExternalConversationDelivery(delivery)
	next.Status = models.ExternalConversationDeliveryFailed
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
	delivery *models.ExternalConversationDelivery,
	processingErr error,
	code, summary string,
	retryAfter time.Duration,
) error {
	now := w.now().UTC()
	next := models.CloneExternalConversationDelivery(delivery)
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
		next.Status = models.ExternalConversationDeliveryFailed
	} else {
		next.Status = models.ExternalConversationDeliveryRetry
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
	case errors.Is(err, models.ErrExternalConversationConflict):
		return "endpoint_or_adapter_conflict"
	case errors.Is(err, models.ErrInvalidExternalConversation):
		return "invalid_delivery"
	default:
		return "adapter_delivery_unconfirmed"
	}
}
