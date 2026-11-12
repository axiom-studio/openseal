package externalconversation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
)

const externalConversationActorID = "openseal.external-conversation"

// DispatchRequest is the provider-neutral wake contract
// between durable conversation ingress and a cognitive handler. Implementations
// must use IdempotencyKey when creating work so a crash after dispatch but
// before inbox completion cannot create a second Run.
type DispatchRequest struct {
	Endpoint       *models.ExternalConversationEndpoint `json:"endpoint"`
	Conversation   *models.Conversation                 `json:"conversation"`
	Message        *models.ChannelMessage               `json:"message"`
	IdempotencyKey string                               `json:"idempotencyKey"`
}

type DispatchResult struct {
	RunID string `json:"runId,omitempty"`
}

type Dispatcher interface {
	DispatchExternalConversation(context.Context, DispatchRequest) (*DispatchResult, error)
}

// CanonicalExternalConversationDispatcher hands Agent and Team handlers to
// the canonical Conversation scheduler.
type CanonicalExternalConversationDispatcher struct {
	scheduler *conversationrun.Scheduler
}

func NewCanonicalExternalConversationDispatcher(scheduler *conversationrun.Scheduler) *CanonicalExternalConversationDispatcher {
	return &CanonicalExternalConversationDispatcher{scheduler: scheduler}
}

func (d *CanonicalExternalConversationDispatcher) DispatchExternalConversation(
	ctx context.Context,
	req DispatchRequest,
) (*DispatchResult, error) {
	if req.Endpoint == nil || req.Conversation == nil || req.Message == nil || strings.TrimSpace(req.IdempotencyKey) == "" {
		return nil, fmt.Errorf("%w: complete dispatch context is required", models.ErrInvalidExternalConversation)
	}
	switch req.Endpoint.Handler.Kind {
	case models.ExternalConversationHandlerAgent, models.ExternalConversationHandlerTeam:
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
		return &DispatchResult{RunID: result.Run.ID}, nil
	default:
		return nil, fmt.Errorf("%w: unsupported handler kind", models.ErrInvalidExternalConversation)
	}
}

type InboxWorkerConfig struct {
	AttachmentHost    AttachmentHost
	AttachmentContent models.ArtifactContentStore
	WorkerID          string
	LeaseDuration     time.Duration
	BaseRetry         time.Duration
	MaximumRetry      time.Duration
	ContextHost       ContextHost
	ContextCatalog    AdapterResolver
}

func (c InboxWorkerConfig) normalize() (InboxWorkerConfig, error) {
	c.WorkerID = strings.TrimSpace(c.WorkerID)
	if !models.IsValidOpaqueIdentifier(c.WorkerID, 256) {
		return InboxWorkerConfig{}, fmt.Errorf("%w: worker id is invalid", models.ErrInvalidExternalConversation)
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
		return InboxWorkerConfig{}, fmt.Errorf("%w: worker durations are invalid", models.ErrInvalidExternalConversation)
	}
	return c, nil
}

// InboxWorker turns normalized provider events into
// canonical Conversation facts and dispatches exactly one replay-safe handler
// Run. Every intermediate identity is deterministic and durably mapped, so a
// worker may restart after any write without duplicating the visible message.
type InboxWorker struct {
	store         models.ExternalConversationStore
	conversations *runtimeconversation.Service
	dispatcher    Dispatcher
	config        InboxWorkerConfig
	now           func() time.Time
}

func NewInboxWorker(
	store models.ExternalConversationStore,
	dispatcher Dispatcher,
	config InboxWorkerConfig,
	opts ...models.ServiceOption,
) (*InboxWorker, error) {
	if store == nil || dispatcher == nil {
		return nil, errors.New("external conversation store and dispatcher are required")
	}
	normalized, err := config.normalize()
	if err != nil {
		return nil, err
	}
	return &InboxWorker{
		store: store, conversations: runtimeconversation.NewService(store, opts...), dispatcher: dispatcher, config: normalized, now: models.ClockFrom(opts),
	}, nil
}

// ProcessOne leases and applies at most one due inbox item in a scope. A nil
// result means no work was due.
func (w *InboxWorker) ProcessOne(ctx context.Context, scope models.Scope) (*models.ExternalConversationInboxItem, error) {
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

func (w *InboxWorker) apply(ctx context.Context, item *models.ExternalConversationInboxItem) error {
	if item.Status != models.ExternalConversationInboxLeased || item.LeaseOwner != w.config.WorkerID {
		return models.ErrExternalConversationLeaseLost
	}
	if item.Event.Type == capability.ConversationEventReactionAdded || item.Event.Type == capability.ConversationEventReactionRemoved {
		return w.applyReaction(ctx, item)
	}
	if item.Event.Type != capability.ConversationEventMessageReceived {
		return fmt.Errorf("%w: event type %q is not yet a canonical message", models.ErrInvalidExternalConversation, item.Event.Type)
	}
	endpoint, err := w.store.GetExternalConversationEndpoint(ctx, item.Scope, item.EndpointID)
	if err != nil {
		return err
	}
	if endpoint == nil || endpoint.Status != models.ExternalConversationEndpointActive ||
		endpoint.Revision != item.EndpointRevision ||
		!models.ExternalConversationAdapterBelongsToEndpoint(endpoint.Adapter, item.Adapter) {
		return models.ErrExternalConversationConflict
	}

	conversation, mapping, err := w.ensureConversation(ctx, endpoint, item.Event)
	if err != nil {
		return err
	}
	// Keep the durable, verified ingress event immutable for lease validation.
	// Provider labels enrich the applied message, not the original event.
	contextualItem := *item
	mapping, contextState, err := w.hydrateContext(ctx, endpoint, conversation, mapping, &contextualItem)
	if err != nil {
		return err
	}
	participant, err := w.ensureParticipant(ctx, endpoint, contextualItem.Event)
	if err != nil {
		return err
	}
	message, err := w.ensureInboundMessage(ctx, endpoint, conversation, mapping, participant, contextualItem.Event, contextState)
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
	// A message already imported as provider history is context, not a new
	// instruction. A delayed provider event must not wake obsolete work.
	if message.Historical {
		return w.complete(ctx, item, conversation.ID, message.ID, "")
	}
	dispatch, err := w.dispatcher.DispatchExternalConversation(ctx, DispatchRequest{
		Endpoint: endpoint, Conversation: conversation, Message: message,
		IdempotencyKey: "external-conversation-dispatch:" + item.ID,
	})
	if err != nil {
		return err
	}
	if dispatch == nil || !models.IsValidOpaqueIdentifier(dispatch.RunID, 256) {
		return errors.New("external conversation dispatcher did not return a canonical Run")
	}
	return w.complete(ctx, item, conversation.ID, message.ID, dispatch.RunID)
}

func (w *InboxWorker) ensureConversation(
	ctx context.Context,
	endpoint *models.ExternalConversationEndpoint,
	event models.NormalizedExternalConversationEvent,
) (*models.Conversation, *models.ExternalConversationMapping, error) {
	base, err := w.store.GetExternalConversationMapping(ctx, endpoint.Scope, endpoint.ID, event.ExternalConversationID, "")
	if err != nil {
		return nil, nil, err
	}
	if base == nil {
		key := stableExternalConversationID(endpoint.Scope, endpoint.ID, "conversation", event.ExternalConversationID)
		origin := &models.ConversationReference{Kind: models.ConversationReferenceExternalSource, ID: endpoint.ID, Version: endpoint.Revision}
		conversation, _, createErr := w.conversations.CreateConversation(ctx, runtimeconversation.CreateConversationRequest{
			Scope: endpoint.Scope, Owner: endpoint.Owner, Title: endpoint.Name, Origin: origin, CreatedBy: endpoint.CreatedBy, IdempotencyKey: key,
		})
		if createErr != nil {
			return nil, nil, createErr
		}
		now := w.now().UTC()
		candidate := &models.ExternalConversationMapping{
			Scope: endpoint.Scope, EndpointID: endpoint.ID, ExternalConversationID: event.ExternalConversationID,
			ConversationID: conversation.ID, Revision: 1, CreatedAt: now, UpdatedAt: now,
		}
		if saveErr := w.store.SaveExternalConversationMapping(ctx, candidate, 0); saveErr != nil {
			if !errors.Is(saveErr, models.ErrExternalConversationConflict) {
				return nil, nil, saveErr
			}
			candidate, saveErr = w.store.GetExternalConversationMapping(ctx, endpoint.Scope, endpoint.ID, event.ExternalConversationID, "")
			if saveErr != nil || candidate == nil || candidate.ConversationID != conversation.ID {
				return nil, nil, models.ErrExternalConversationConflict
			}
		}
		base = candidate
	}
	conversation, err := w.conversations.GetConversation(ctx, endpoint.Scope, base.ConversationID)
	if err != nil {
		return nil, nil, err
	}
	if conversation.Owner != endpoint.Owner || conversation.Origin == nil ||
		conversation.Origin.Kind != models.ConversationReferenceExternalSource || conversation.Origin.ID != endpoint.ID {
		return nil, nil, models.ErrExternalConversationConflict
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
		rootID, err := w.existingReplyThreadRoot(ctx, endpoint, conversation, event.ReplyToExternalMessageID)
		if err != nil {
			return nil, nil, err
		}
		now := w.now().UTC()
		candidate := &models.ExternalConversationMapping{
			Scope: endpoint.Scope, EndpointID: endpoint.ID, ExternalConversationID: event.ExternalConversationID,
			ExternalThreadID: event.ExternalThreadID, ConversationID: conversation.ID,
			ThreadRootMessageID: rootID,
			Revision:            1, CreatedAt: now, UpdatedAt: now,
		}
		if saveErr := w.store.SaveExternalConversationMapping(ctx, candidate, 0); saveErr != nil {
			if !errors.Is(saveErr, models.ErrExternalConversationConflict) {
				return nil, nil, saveErr
			}
			candidate, saveErr = w.store.GetExternalConversationMapping(
				ctx, endpoint.Scope, endpoint.ID, event.ExternalConversationID, event.ExternalThreadID,
			)
			if saveErr != nil || candidate == nil || candidate.ConversationID != conversation.ID {
				return nil, nil, models.ErrExternalConversationConflict
			}
		}
		thread = candidate
	}
	if thread.ConversationID != conversation.ID {
		return nil, nil, models.ErrExternalConversationConflict
	}
	return conversation, thread, nil
}

// A provider may identify each reply chain by its immediate parent instead of
// a stable thread ID. Reuse the canonical root only through the authorized
// endpoint's message mapping, and never import a parent from another chat.
func (w *InboxWorker) existingReplyThreadRoot(
	ctx context.Context,
	endpoint *models.ExternalConversationEndpoint,
	conversation *models.Conversation,
	externalParentID string,
) (string, error) {
	if externalParentID == "" {
		return "", nil
	}
	for _, direction := range []models.ExternalMessageDirection{models.ExternalMessageInbound, models.ExternalMessageOutbound} {
		mapping, err := w.store.GetExternalMessageMapping(ctx, endpoint.Scope, endpoint.ID, direction, externalParentID)
		if err != nil {
			return "", err
		}
		if mapping == nil || mapping.Scope != endpoint.Scope || mapping.EndpointID != endpoint.ID ||
			mapping.ConversationID != conversation.ID {
			continue
		}
		parent, err := w.store.GetChannelMessage(ctx, endpoint.Scope, conversation.ID, mapping.ChannelMessageID)
		if err != nil {
			return "", err
		}
		if parent == nil || parent.Scope != endpoint.Scope || parent.ConversationID != conversation.ID {
			continue
		}
		rootID := parent.ID
		if parent.ThreadRootID != "" {
			rootID = parent.ThreadRootID
			root, err := w.store.GetChannelMessage(ctx, endpoint.Scope, conversation.ID, rootID)
			if err != nil {
				return "", err
			}
			if root == nil || root.Scope != endpoint.Scope || root.ConversationID != conversation.ID {
				continue
			}
		}
		return rootID, nil
	}
	return "", nil
}

func (w *InboxWorker) ensureParticipant(
	ctx context.Context,
	endpoint *models.ExternalConversationEndpoint,
	event models.NormalizedExternalConversationEvent,
) (*models.ExternalParticipantMapping, error) {
	current, err := w.store.GetExternalParticipantMapping(ctx, endpoint.Scope, endpoint.ID, event.ExternalParticipantID)
	if err != nil {
		return nil, err
	}
	if current == nil {
		now := w.now().UTC()
		candidate := &models.ExternalParticipantMapping{
			Scope: endpoint.Scope, EndpointID: endpoint.ID, ExternalParticipantID: event.ExternalParticipantID,
			Participant: models.ConversationParticipant{
				Type: models.ConversationParticipantUser,
				ID:   stableExternalConversationID(endpoint.Scope, endpoint.ID, "participant", event.ExternalParticipantID),
			},
			DisplayName: strings.TrimSpace(event.ParticipantDisplayName),
			Revision:    1, CreatedAt: now, UpdatedAt: now,
		}
		if saveErr := w.store.SaveExternalParticipantMapping(ctx, candidate, 0); saveErr != nil {
			if !errors.Is(saveErr, models.ErrExternalConversationConflict) {
				return nil, saveErr
			}
			candidate, saveErr = w.store.GetExternalParticipantMapping(ctx, endpoint.Scope, endpoint.ID, event.ExternalParticipantID)
			if saveErr != nil || candidate == nil {
				return nil, models.ErrExternalConversationConflict
			}
		}
		return candidate, nil
	}
	if name := strings.TrimSpace(event.ParticipantDisplayName); name != "" && current.DisplayName != name {
		next := models.CloneExternalParticipantMapping(current)
		next.DisplayName = strings.TrimSpace(event.ParticipantDisplayName)
		next.Revision++
		next.UpdatedAt = w.now().UTC()
		if err := w.store.SaveExternalParticipantMapping(ctx, next, current.Revision); err != nil {
			if !errors.Is(err, models.ErrExternalConversationConflict) {
				return nil, err
			}
			return w.store.GetExternalParticipantMapping(ctx, endpoint.Scope, endpoint.ID, event.ExternalParticipantID)
		}
		return next, nil
	}
	return current, nil
}

func (w *InboxWorker) ensureInboundMessage(
	ctx context.Context,
	endpoint *models.ExternalConversationEndpoint,
	conversation *models.Conversation,
	thread *models.ExternalConversationMapping,
	participant *models.ExternalParticipantMapping,
	event models.NormalizedExternalConversationEvent,
	contextState *runtimeconversation.ExternalConversationContextState,
) (*models.ChannelMessage, error) {
	if existing, err := w.store.GetExternalMessageMapping(
		ctx, endpoint.Scope, endpoint.ID, models.ExternalMessageInbound, event.ExternalMessageID,
	); err != nil {
		return nil, err
	} else if existing != nil {
		if existing.ConversationID != conversation.ID {
			return nil, models.ErrExternalConversationConflict
		}
		return w.conversations.GetChannelMessage(ctx, endpoint.Scope, conversation.ID, existing.ChannelMessageID)
	}
	key := stableExternalConversationID(endpoint.Scope, endpoint.ID, "message", event.ExternalMessageID)
	// The previous attempt may have committed the message and crashed before
	// writing its provider mapping. Keep the original context snapshot intact.
	if existing, err := w.store.FindChannelMessageByIdempotencyKey(ctx, endpoint.Scope, conversation.ID, key); err != nil {
		return nil, err
	} else if existing != nil {
		return existing, nil
	}
	references := []models.ConversationReference{{Kind: models.ConversationReferenceExternalSource, ID: endpoint.ID, Version: endpoint.Revision}}
	if contextState != nil {
		references = append(references, externalConversationContextReference(endpoint.ID, contextState))
		if origin := externalConversationContextOriginReference(endpoint.ID, contextState); origin != nil {
			references = append(references, *origin)
		}
		if code := runtimeconversation.NormalizeExternalConversationContextErrorCode(contextState.ErrorCode); code != "" {
			references = append(references, models.ConversationReference{Kind: models.ConversationReferenceExternalSource,
				ID: runtimeconversation.ExternalConversationContextErrorPrefix(endpoint.ID) + code})
		}
	}
	attachmentRefs, notices, err := w.importAttachments(ctx, endpoint, conversation, event)
	if err != nil {
		return nil, err
	}
	references = append(references, attachmentRefs...)
	text := strings.TrimSpace(event.Text)
	if text == "" {
		text = "Please look at the attached files."
	}
	if len(notices) > 0 {
		text += "\n\nAttachment access: " + strings.Join(notices, "; ")
	}
	for attempts := 0; attempts < 32; attempts++ {
		current, err := w.conversations.GetConversation(ctx, endpoint.Scope, conversation.ID)
		if err != nil {
			return nil, err
		}
		replyTo := ""
		if thread != nil {
			replyTo = thread.ThreadRootMessageID
		}
		if event.ReplyToExternalMessageID != "" {
			for _, direction := range []models.ExternalMessageDirection{models.ExternalMessageInbound, models.ExternalMessageOutbound} {
				parent, err := w.store.GetExternalMessageMapping(ctx, endpoint.Scope, endpoint.ID, direction, event.ReplyToExternalMessageID)
				if err != nil {
					return nil, err
				}
				if parent == nil || parent.Scope != endpoint.Scope || parent.EndpointID != endpoint.ID || parent.ConversationID != conversation.ID {
					continue
				}
				message, err := w.conversations.GetChannelMessage(ctx, endpoint.Scope, conversation.ID, parent.ChannelMessageID)
				if err != nil {
					return nil, err
				}
				if message != nil && message.Scope == endpoint.Scope && message.ConversationID == conversation.ID &&
					(replyTo == "" || message.ID == replyTo || message.ThreadRootID == replyTo) {
					replyTo = message.ID
					break
				}
			}
		}
		source := activity.NewExternalMessageSource(endpoint, event)
		source.ParticipantDisplayName = models.ExternalSourceLabel(participant.DisplayName, 160)
		result, postErr := w.conversations.PostChannelMessage(ctx, runtimeconversation.PostChannelMessageRequest{
			Scope: endpoint.Scope, ConversationID: conversation.ID, ExpectedRevision: current.Revision,
			Sender: participant.Participant, SenderDisplayName: participant.DisplayName,
			ExternalSource: source,
			Intent:         models.MessageIntentQuestion, Content: text, Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel},
			ReplyToMessageID: replyTo, References: references,
			RequiresResponse: true, IdempotencyKey: key,
		})
		if errors.Is(postErr, models.ErrRevisionConflict) {
			continue
		}
		if postErr != nil {
			return nil, postErr
		}
		return result.Message, nil
	}
	return nil, models.ErrRevisionConflict
}

func (w *InboxWorker) ensureInboundMessageMapping(
	ctx context.Context,
	endpoint *models.ExternalConversationEndpoint,
	externalMessageID string,
	message *models.ChannelMessage,
) error {
	current, err := w.store.GetExternalMessageMapping(ctx, endpoint.Scope, endpoint.ID, models.ExternalMessageInbound, externalMessageID)
	if err != nil {
		return err
	}
	if current != nil {
		if current.ConversationID != message.ConversationID || current.ChannelMessageID != message.ID {
			return models.ErrExternalConversationConflict
		}
		return nil
	}
	now := w.now().UTC()
	candidate := &models.ExternalMessageMapping{
		Scope: endpoint.Scope, EndpointID: endpoint.ID, Direction: models.ExternalMessageInbound,
		ExternalMessageID: externalMessageID, ConversationID: message.ConversationID, ChannelMessageID: message.ID,
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := w.store.SaveExternalMessageMapping(ctx, candidate, 0); err != nil {
		if !errors.Is(err, models.ErrExternalConversationConflict) {
			return err
		}
		current, err = w.store.GetExternalMessageMapping(ctx, endpoint.Scope, endpoint.ID, models.ExternalMessageInbound, externalMessageID)
		if err != nil || current == nil || current.ConversationID != message.ConversationID || current.ChannelMessageID != message.ID {
			return models.ErrExternalConversationConflict
		}
	}
	return nil
}

func (w *InboxWorker) ensureThreadRoot(
	ctx context.Context,
	current *models.ExternalConversationMapping,
	messageID string,
) (*models.ExternalConversationMapping, error) {
	next := models.CloneExternalConversationMapping(current)
	next.ThreadRootMessageID = messageID
	next.Revision++
	next.UpdatedAt = w.now().UTC()
	if err := w.store.SaveExternalConversationMapping(ctx, next, current.Revision); err != nil {
		if !errors.Is(err, models.ErrExternalConversationConflict) {
			return nil, err
		}
		stored, getErr := w.store.GetExternalConversationMapping(
			ctx, current.Scope, current.EndpointID, current.ExternalConversationID, current.ExternalThreadID,
		)
		if getErr != nil || stored == nil || stored.ThreadRootMessageID != messageID {
			return nil, models.ErrExternalConversationConflict
		}
		return stored, nil
	}
	return next, nil
}

func (w *InboxWorker) complete(
	ctx context.Context,
	item *models.ExternalConversationInboxItem,
	conversationID, messageID, runID string,
) error {
	now := w.now().UTC()
	next := models.CloneExternalConversationInboxItem(item)
	next.Status = models.ExternalConversationInboxApplied
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

func (w *InboxWorker) recordFailure(
	ctx context.Context,
	item *models.ExternalConversationInboxItem,
	processingErr error,
) error {
	now := w.now().UTC()
	next := models.CloneExternalConversationInboxItem(item)
	next.LeaseOwner, next.LeaseExpiresAt = "", time.Time{}
	next.ErrorCode = externalConversationErrorCode(processingErr)
	next.Summary = "The normalized conversation event could not be applied."
	next.UpdatedAt = now
	next.Revision++
	if next.Attempt >= next.MaximumAttempts {
		next.Status = models.ExternalConversationInboxDeadLetter
	} else {
		next.Status = models.ExternalConversationInboxRetry
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
	case errors.Is(err, models.ErrExternalConversationConflict):
		return "endpoint_or_mapping_conflict"
	case errors.Is(err, models.ErrRevisionConflict):
		return "conversation_revision_conflict"
	case errors.Is(err, models.ErrInvalidExternalConversation):
		return "invalid_normalized_event"
	default:
		return "handler_dispatch_failed"
	}
}
