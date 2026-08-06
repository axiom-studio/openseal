package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
)

const (
	externalConversationEventType = "conversation.message.received"
	externalConversationActorID   = "openseal.external-conversation"
)

// ExternalConversationDispatchRequest is the provider-neutral wake contract
// between durable conversation ingress and a cognitive handler. Implementations
// must use IdempotencyKey when creating work so a crash after dispatch but
// before inbox completion cannot create a second Run.
type ExternalConversationDispatchRequest struct {
	Endpoint       *ExternalConversationEndpoint `json:"endpoint"`
	Conversation   *Conversation                 `json:"conversation"`
	Message        *ChannelMessage               `json:"message"`
	Event          EventEnvelope                 `json:"event"`
	IdempotencyKey string                        `json:"idempotencyKey"`
}

type ExternalConversationDispatchResult struct {
	RunID string `json:"runId,omitempty"`
}

type ExternalConversationDispatcher interface {
	DispatchExternalConversation(context.Context, ExternalConversationDispatchRequest) (*ExternalConversationDispatchResult, error)
}

// ExternalConversationRunbookDispatcher starts one exact immutable Runbook
// entrypoint. The implementation owns Runbook lookup and invocation; provider
// adapters and the transport worker never interpret Runbook definitions.
type ExternalConversationRunbookDispatcher interface {
	DispatchExternalConversationRunbook(
		context.Context,
		ExternalConversationHandler,
		ExternalConversationDispatchRequest,
	) (*ExternalConversationDispatchResult, error)
}

// CanonicalExternalConversationDispatcher uses the existing canonical
// Conversation scheduler for direct Agent and Team handlers and a separate,
// typed boundary for exact Runbook handlers.
type CanonicalExternalConversationDispatcher struct {
	scheduler *ConversationRunScheduler
	runbooks  ExternalConversationRunbookDispatcher
}

func NewCanonicalExternalConversationDispatcher(
	scheduler *ConversationRunScheduler,
	runbooks ExternalConversationRunbookDispatcher,
) *CanonicalExternalConversationDispatcher {
	return &CanonicalExternalConversationDispatcher{scheduler: scheduler, runbooks: runbooks}
}

func (d *CanonicalExternalConversationDispatcher) DispatchExternalConversation(
	ctx context.Context,
	req ExternalConversationDispatchRequest,
) (*ExternalConversationDispatchResult, error) {
	if req.Endpoint == nil || req.Conversation == nil || req.Message == nil || strings.TrimSpace(req.IdempotencyKey) == "" {
		return nil, fmt.Errorf("%w: complete dispatch context is required", ErrInvalidExternalConversation)
	}
	if err := req.Event.Validate(); err != nil {
		return nil, fmt.Errorf("%w: dispatch event: %v", ErrInvalidExternalConversation, err)
	}
	switch req.Endpoint.Handler.Kind {
	case ExternalConversationHandlerAgent, ExternalConversationHandlerTeam:
		if d == nil || d.scheduler == nil {
			return nil, errors.New("conversation run scheduler is not configured")
		}
		result, _, err := d.scheduler.ScheduleMessage(ctx, req.Endpoint.Scope, req.Conversation.ID, req.Message.ID)
		if err != nil {
			return nil, err
		}
		if result == nil || result.Run == nil {
			return nil, errors.New("conversation message did not create a handler Run")
		}
		return &ExternalConversationDispatchResult{RunID: result.Run.ID}, nil
	case ExternalConversationHandlerRunbook:
		if d == nil || d.runbooks == nil {
			return nil, errors.New("external conversation Runbook dispatcher is not configured")
		}
		return d.runbooks.DispatchExternalConversationRunbook(ctx, req.Endpoint.Handler, req)
	default:
		return nil, fmt.Errorf("%w: unsupported handler kind", ErrInvalidExternalConversation)
	}
}

type ExternalConversationInboxWorkerConfig struct {
	WorkerID      string
	LeaseDuration time.Duration
	BaseRetry     time.Duration
	MaximumRetry  time.Duration
}

func (c ExternalConversationInboxWorkerConfig) normalize() (ExternalConversationInboxWorkerConfig, error) {
	c.WorkerID = strings.TrimSpace(c.WorkerID)
	if !validOpaqueIdentifier(c.WorkerID, 256) {
		return ExternalConversationInboxWorkerConfig{}, fmt.Errorf("%w: worker id is invalid", ErrInvalidExternalConversation)
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
		return ExternalConversationInboxWorkerConfig{}, fmt.Errorf("%w: worker durations are invalid", ErrInvalidExternalConversation)
	}
	return c, nil
}

// ExternalConversationInboxWorker turns normalized provider events into
// canonical Conversation facts and dispatches exactly one replay-safe handler
// Run. Every intermediate identity is deterministic and durably mapped, so a
// worker may restart after any write without duplicating the visible message.
type ExternalConversationInboxWorker struct {
	store         ExternalConversationStore
	conversations *ConversationService
	dispatcher    ExternalConversationDispatcher
	config        ExternalConversationInboxWorkerConfig
	now           func() time.Time
}

func NewExternalConversationInboxWorker(
	store ExternalConversationStore,
	dispatcher ExternalConversationDispatcher,
	config ExternalConversationInboxWorkerConfig,
) (*ExternalConversationInboxWorker, error) {
	if store == nil || dispatcher == nil {
		return nil, errors.New("external conversation store and dispatcher are required")
	}
	normalized, err := config.normalize()
	if err != nil {
		return nil, err
	}
	return &ExternalConversationInboxWorker{
		store: store, conversations: NewConversationService(store), dispatcher: dispatcher, config: normalized, now: time.Now,
	}, nil
}

// ProcessOne leases and applies at most one due inbox item in a scope. A nil
// result means no work was due.
func (w *ExternalConversationInboxWorker) ProcessOne(ctx context.Context, scope Scope) (*ExternalConversationInboxItem, error) {
	if w == nil || w.store == nil || w.conversations == nil || w.dispatcher == nil {
		return nil, errors.New("external conversation inbox worker is not configured")
	}
	now := w.now().UTC()
	item, err := w.store.ClaimExternalConversationInbox(ctx, scope, w.config.WorkerID, now, w.config.LeaseDuration)
	if err != nil || item == nil {
		return item, err
	}
	if err := w.apply(ctx, item); err != nil {
		if saveErr := w.recordFailure(ctx, item, err); saveErr != nil {
			return nil, saveErr
		}
		return item, err
	}
	return item, nil
}

func (w *ExternalConversationInboxWorker) apply(ctx context.Context, item *ExternalConversationInboxItem) error {
	if item.Status != ExternalConversationInboxLeased || item.LeaseOwner != w.config.WorkerID {
		return ErrExternalConversationLeaseLost
	}
	if item.Event.Type != capability.ConversationEventMessageReceived {
		return fmt.Errorf("%w: event type %q is not yet a canonical message", ErrInvalidExternalConversation, item.Event.Type)
	}
	endpoint, err := w.store.GetExternalConversationEndpoint(ctx, item.Scope, item.EndpointID)
	if err != nil {
		return err
	}
	if endpoint == nil || endpoint.Status != ExternalConversationEndpointActive ||
		endpoint.Revision != item.EndpointRevision ||
		!externalConversationAdapterBelongsToEndpoint(endpoint.Adapter, item.Adapter) {
		return ErrExternalConversationConflict
	}

	conversation, mapping, err := w.ensureConversation(ctx, endpoint, item.Event)
	if err != nil {
		return err
	}
	participant, err := w.ensureParticipant(ctx, endpoint, item.Event)
	if err != nil {
		return err
	}
	message, err := w.ensureInboundMessage(ctx, endpoint, conversation, mapping, participant, item.Event)
	if err != nil {
		return err
	}
	if err := w.ensureInboundMessageMapping(ctx, endpoint, item.Event.ExternalMessageID, message); err != nil {
		return err
	}
	if item.Event.ExternalThreadID != "" && mapping.ThreadRootMessageID == "" {
		mapping, err = w.ensureThreadRoot(ctx, mapping, message.ID)
		if err != nil {
			return err
		}
	}
	event := externalConversationEventEnvelope(item, endpoint, conversation, message)
	dispatch, err := w.dispatcher.DispatchExternalConversation(ctx, ExternalConversationDispatchRequest{
		Endpoint: endpoint, Conversation: conversation, Message: message, Event: event,
		IdempotencyKey: "external-conversation-dispatch:" + item.ID,
	})
	if err != nil {
		return err
	}
	if dispatch == nil || !validOpaqueIdentifier(dispatch.RunID, 256) {
		return errors.New("external conversation dispatcher did not return a canonical Run")
	}
	return w.complete(ctx, item, conversation.ID, message.ID, dispatch.RunID)
}

func (w *ExternalConversationInboxWorker) ensureConversation(
	ctx context.Context,
	endpoint *ExternalConversationEndpoint,
	event NormalizedExternalConversationEvent,
) (*Conversation, *ExternalConversationMapping, error) {
	base, err := w.store.GetExternalConversationMapping(ctx, endpoint.Scope, endpoint.ID, event.ExternalConversationID, "")
	if err != nil {
		return nil, nil, err
	}
	if base == nil {
		key := stableExternalConversationID(endpoint.Scope, endpoint.ID, "conversation", event.ExternalConversationID)
		origin := &ConversationReference{Kind: ConversationReferenceExternalSource, ID: endpoint.ID, Version: endpoint.Revision}
		conversation, _, createErr := w.conversations.CreateConversation(ctx, CreateConversationRequest{
			Scope: endpoint.Scope, Owner: endpoint.Owner, Title: endpoint.Name, Origin: origin, IdempotencyKey: key,
		})
		if createErr != nil {
			return nil, nil, createErr
		}
		now := w.now().UTC()
		candidate := &ExternalConversationMapping{
			Scope: endpoint.Scope, EndpointID: endpoint.ID, ExternalConversationID: event.ExternalConversationID,
			ConversationID: conversation.ID, Revision: 1, CreatedAt: now, UpdatedAt: now,
		}
		if saveErr := w.store.SaveExternalConversationMapping(ctx, candidate, 0); saveErr != nil {
			if !errors.Is(saveErr, ErrExternalConversationConflict) {
				return nil, nil, saveErr
			}
			candidate, saveErr = w.store.GetExternalConversationMapping(ctx, endpoint.Scope, endpoint.ID, event.ExternalConversationID, "")
			if saveErr != nil || candidate == nil || candidate.ConversationID != conversation.ID {
				return nil, nil, ErrExternalConversationConflict
			}
		}
		base = candidate
	}
	conversation, err := w.conversations.GetConversation(ctx, endpoint.Scope, base.ConversationID)
	if err != nil {
		return nil, nil, err
	}
	if conversation.Owner != endpoint.Owner || conversation.Origin == nil ||
		conversation.Origin.Kind != ConversationReferenceExternalSource || conversation.Origin.ID != endpoint.ID {
		return nil, nil, ErrExternalConversationConflict
	}
	if event.ExternalThreadID == "" {
		return conversation, base, nil
	}
	thread, err := w.store.GetExternalConversationMapping(
		ctx, endpoint.Scope, endpoint.ID, event.ExternalConversationID, event.ExternalThreadID,
	)
	if err != nil {
		return nil, nil, err
	}
	if thread == nil {
		now := w.now().UTC()
		candidate := &ExternalConversationMapping{
			Scope: endpoint.Scope, EndpointID: endpoint.ID, ExternalConversationID: event.ExternalConversationID,
			ExternalThreadID: event.ExternalThreadID, ConversationID: conversation.ID,
			Revision: 1, CreatedAt: now, UpdatedAt: now,
		}
		if saveErr := w.store.SaveExternalConversationMapping(ctx, candidate, 0); saveErr != nil {
			if !errors.Is(saveErr, ErrExternalConversationConflict) {
				return nil, nil, saveErr
			}
			candidate, saveErr = w.store.GetExternalConversationMapping(
				ctx, endpoint.Scope, endpoint.ID, event.ExternalConversationID, event.ExternalThreadID,
			)
			if saveErr != nil || candidate == nil || candidate.ConversationID != conversation.ID {
				return nil, nil, ErrExternalConversationConflict
			}
		}
		thread = candidate
	}
	if thread.ConversationID != conversation.ID {
		return nil, nil, ErrExternalConversationConflict
	}
	return conversation, thread, nil
}

func (w *ExternalConversationInboxWorker) ensureParticipant(
	ctx context.Context,
	endpoint *ExternalConversationEndpoint,
	event NormalizedExternalConversationEvent,
) (*ExternalParticipantMapping, error) {
	current, err := w.store.GetExternalParticipantMapping(ctx, endpoint.Scope, endpoint.ID, event.ExternalParticipantID)
	if err != nil {
		return nil, err
	}
	if current == nil {
		now := w.now().UTC()
		candidate := &ExternalParticipantMapping{
			Scope: endpoint.Scope, EndpointID: endpoint.ID, ExternalParticipantID: event.ExternalParticipantID,
			Participant: ConversationParticipant{
				Type: ConversationParticipantUser,
				ID:   stableExternalConversationID(endpoint.Scope, endpoint.ID, "participant", event.ExternalParticipantID),
			},
			DisplayName: strings.TrimSpace(event.ParticipantDisplayName),
			Revision:    1, CreatedAt: now, UpdatedAt: now,
		}
		if saveErr := w.store.SaveExternalParticipantMapping(ctx, candidate, 0); saveErr != nil {
			if !errors.Is(saveErr, ErrExternalConversationConflict) {
				return nil, saveErr
			}
			candidate, saveErr = w.store.GetExternalParticipantMapping(ctx, endpoint.Scope, endpoint.ID, event.ExternalParticipantID)
			if saveErr != nil || candidate == nil {
				return nil, ErrExternalConversationConflict
			}
		}
		return candidate, nil
	}
	if current.DisplayName != strings.TrimSpace(event.ParticipantDisplayName) {
		next := cloneExternalParticipantMapping(current)
		next.DisplayName = strings.TrimSpace(event.ParticipantDisplayName)
		next.Revision++
		next.UpdatedAt = w.now().UTC()
		if err := w.store.SaveExternalParticipantMapping(ctx, next, current.Revision); err != nil {
			if !errors.Is(err, ErrExternalConversationConflict) {
				return nil, err
			}
			return w.store.GetExternalParticipantMapping(ctx, endpoint.Scope, endpoint.ID, event.ExternalParticipantID)
		}
		return next, nil
	}
	return current, nil
}

func (w *ExternalConversationInboxWorker) ensureInboundMessage(
	ctx context.Context,
	endpoint *ExternalConversationEndpoint,
	conversation *Conversation,
	thread *ExternalConversationMapping,
	participant *ExternalParticipantMapping,
	event NormalizedExternalConversationEvent,
) (*ChannelMessage, error) {
	if existing, err := w.store.GetExternalMessageMapping(
		ctx, endpoint.Scope, endpoint.ID, ExternalMessageInbound, event.ExternalMessageID,
	); err != nil {
		return nil, err
	} else if existing != nil {
		if existing.ConversationID != conversation.ID {
			return nil, ErrExternalConversationConflict
		}
		return w.conversations.GetChannelMessage(ctx, endpoint.Scope, conversation.ID, existing.ChannelMessageID)
	}
	key := stableExternalConversationID(endpoint.Scope, endpoint.ID, "message", event.ExternalMessageID)
	for attempts := 0; attempts < 32; attempts++ {
		current, err := w.conversations.GetConversation(ctx, endpoint.Scope, conversation.ID)
		if err != nil {
			return nil, err
		}
		replyTo := ""
		if thread != nil {
			replyTo = thread.ThreadRootMessageID
		}
		result, postErr := w.conversations.PostChannelMessage(ctx, PostChannelMessageRequest{
			Scope: endpoint.Scope, ConversationID: conversation.ID, ExpectedRevision: current.Revision,
			Sender: participant.Participant, SenderDisplayName: participant.DisplayName,
			Intent: MessageIntentQuestion, Content: event.Text, Audience: ConversationAudience{Kind: ConversationAudienceChannel},
			ReplyToMessageID: replyTo, References: []ConversationReference{{
				Kind: ConversationReferenceExternalSource, ID: endpoint.ID, Version: endpoint.Revision,
			}},
			RequiresResponse: true, IdempotencyKey: key,
		})
		if errors.Is(postErr, ErrRevisionConflict) {
			continue
		}
		if postErr != nil {
			return nil, postErr
		}
		return result.Message, nil
	}
	return nil, ErrRevisionConflict
}

func (w *ExternalConversationInboxWorker) ensureInboundMessageMapping(
	ctx context.Context,
	endpoint *ExternalConversationEndpoint,
	externalMessageID string,
	message *ChannelMessage,
) error {
	current, err := w.store.GetExternalMessageMapping(ctx, endpoint.Scope, endpoint.ID, ExternalMessageInbound, externalMessageID)
	if err != nil {
		return err
	}
	if current != nil {
		if current.ConversationID != message.ConversationID || current.ChannelMessageID != message.ID {
			return ErrExternalConversationConflict
		}
		return nil
	}
	now := w.now().UTC()
	candidate := &ExternalMessageMapping{
		Scope: endpoint.Scope, EndpointID: endpoint.ID, Direction: ExternalMessageInbound,
		ExternalMessageID: externalMessageID, ConversationID: message.ConversationID, ChannelMessageID: message.ID,
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := w.store.SaveExternalMessageMapping(ctx, candidate, 0); err != nil {
		if !errors.Is(err, ErrExternalConversationConflict) {
			return err
		}
		current, err = w.store.GetExternalMessageMapping(ctx, endpoint.Scope, endpoint.ID, ExternalMessageInbound, externalMessageID)
		if err != nil || current == nil || current.ConversationID != message.ConversationID || current.ChannelMessageID != message.ID {
			return ErrExternalConversationConflict
		}
	}
	return nil
}

func (w *ExternalConversationInboxWorker) ensureThreadRoot(
	ctx context.Context,
	current *ExternalConversationMapping,
	messageID string,
) (*ExternalConversationMapping, error) {
	next := cloneExternalConversationMapping(current)
	next.ThreadRootMessageID = messageID
	next.Revision++
	next.UpdatedAt = w.now().UTC()
	if err := w.store.SaveExternalConversationMapping(ctx, next, current.Revision); err != nil {
		if !errors.Is(err, ErrExternalConversationConflict) {
			return nil, err
		}
		stored, getErr := w.store.GetExternalConversationMapping(
			ctx, current.Scope, current.EndpointID, current.ExternalConversationID, current.ExternalThreadID,
		)
		if getErr != nil || stored == nil || stored.ThreadRootMessageID != messageID {
			return nil, ErrExternalConversationConflict
		}
		return stored, nil
	}
	return next, nil
}

func externalConversationEventEnvelope(
	item *ExternalConversationInboxItem,
	endpoint *ExternalConversationEndpoint,
	conversation *Conversation,
	message *ChannelMessage,
) EventEnvelope {
	return EventEnvelope{
		ID: item.ID, Scope: item.Scope, Type: externalConversationEventType,
		Source: "conversation-adapter:" + endpoint.Provider, Subject: conversation.ID,
		OccurredAt: item.Event.OccurredAt,
		Attributes: map[string]interface{}{
			"endpointId": endpoint.ID, "handlerKind": string(endpoint.Handler.Kind),
			"conversationId": conversation.ID, "messageId": message.ID,
		},
		Payload: map[string]interface{}{
			"conversationId": conversation.ID, "messageId": message.ID,
		},
		Actor: ActivityActor{Type: "service", ID: externalConversationActorID},
	}
}

func (w *ExternalConversationInboxWorker) complete(
	ctx context.Context,
	item *ExternalConversationInboxItem,
	conversationID, messageID, runID string,
) error {
	now := w.now().UTC()
	next := cloneExternalConversationInboxItem(item)
	next.Status = ExternalConversationInboxApplied
	next.ConversationID, next.ChannelMessageID, next.RunID = conversationID, messageID, runID
	next.LeaseOwner, next.LeaseExpiresAt = "", time.Time{}
	next.ErrorCode, next.Summary = "", ""
	next.AppliedAt, next.UpdatedAt = now, now
	next.Revision++
	if err := next.Validate(); err != nil {
		return err
	}
	if err := w.store.SaveExternalConversationInbox(ctx, next, item.Revision, w.config.WorkerID); err != nil {
		return err
	}
	*item = *next
	return nil
}

func (w *ExternalConversationInboxWorker) recordFailure(
	ctx context.Context,
	item *ExternalConversationInboxItem,
	processingErr error,
) error {
	now := w.now().UTC()
	next := cloneExternalConversationInboxItem(item)
	next.LeaseOwner, next.LeaseExpiresAt = "", time.Time{}
	next.ErrorCode = externalConversationErrorCode(processingErr)
	next.Summary = "The normalized conversation event could not be applied."
	next.UpdatedAt = now
	next.Revision++
	if next.Attempt >= next.MaximumAttempts {
		next.Status = ExternalConversationInboxDeadLetter
	} else {
		next.Status = ExternalConversationInboxRetry
		next.AvailableAt = now.Add(exponentialExternalConversationRetry(w.config.BaseRetry, w.config.MaximumRetry, next.Attempt))
	}
	if err := next.Validate(); err != nil {
		return err
	}
	if err := w.store.SaveExternalConversationInbox(ctx, next, item.Revision, w.config.WorkerID); err != nil {
		return err
	}
	*item = *next
	return nil
}

func exponentialExternalConversationRetry(base, maximum time.Duration, attempt int) time.Duration {
	delay := base
	for count := 1; count < attempt && delay < maximum; count++ {
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

func externalConversationErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrExternalConversationConflict):
		return "endpoint_or_mapping_conflict"
	case errors.Is(err, ErrRevisionConflict):
		return "conversation_revision_conflict"
	case errors.Is(err, ErrInvalidExternalConversation):
		return "invalid_normalized_event"
	default:
		return "handler_dispatch_failed"
	}
}
