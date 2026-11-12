package runtime

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
)

// externalReactionDeliveryScanPages bounds the outbox pages read to find the
// provider message of a canonical answer (newest first, 100 per page).
const externalReactionDeliveryScanPages = 10

// applyReaction mirrors a provider reaction onto the mapped canonical
// message. The actor is the mapped external participant; the reaction is
// marked external so it is never delivered back out. A reaction on a message
// OpenSeal does not know, or cannot show to that participant, is applied as a
// no-op. Reactions never start a Run.
func (w *ExternalConversationInboxWorker) applyReaction(ctx context.Context, item *ExternalConversationInboxItem) error {
	endpoint, err := w.store.GetExternalConversationEndpoint(ctx, item.Scope, item.EndpointID)
	if err != nil {
		return err
	}
	if endpoint == nil || endpoint.Status != ExternalConversationEndpointActive ||
		endpoint.Revision != item.EndpointRevision ||
		!externalConversationAdapterBelongsToEndpoint(endpoint.Adapter, item.Adapter) {
		return ErrExternalConversationConflict
	}
	event := item.Event
	var mapping *ExternalMessageMapping
	for _, direction := range []ExternalMessageDirection{ExternalMessageInbound, ExternalMessageOutbound} {
		candidate, err := w.store.GetExternalMessageMapping(ctx, endpoint.Scope, endpoint.ID, direction, event.ExternalMessageID)
		if err != nil {
			return err
		}
		if candidate != nil && candidate.Scope == endpoint.Scope && candidate.EndpointID == endpoint.ID {
			mapping = candidate
			break
		}
	}
	if mapping == nil {
		return w.complete(ctx, item, "", "", "")
	}
	participant, err := w.ensureParticipant(ctx, endpoint, event)
	if err != nil {
		return err
	}
	request := conversation.MessageReactionRequest{
		Scope: endpoint.Scope, ConversationID: mapping.ConversationID, MessageID: mapping.ChannelMessageID,
		Emoji: event.Emoji, Actor: participant.Participant, FromExternal: true,
	}
	if event.Type == capability.ConversationEventReactionAdded {
		_, err = w.conversations.AddMessageReaction(ctx, request)
	} else {
		_, err = w.conversations.RemoveMessageReaction(ctx, request)
	}
	if errors.Is(err, ErrChannelMessageNotFound) {
		return w.complete(ctx, item, mapping.ConversationID, "", "")
	}
	if err != nil {
		return err
	}
	return w.complete(ctx, item, mapping.ConversationID, mapping.ChannelMessageID, "")
}

// externalReactionTarget is where a canonical message lives in an external
// conversation.
type externalReactionTarget struct {
	endpointID             string
	externalConversationID string
	externalThreadID       string
	externalMessageID      string
}

// ProjectMessageReaction enqueues reaction.add or reaction.remove for a
// reaction made in OpenSeal on a message that came from, or was delivered
// to, an external conversation whose adapter declares reactions. The
// delivery parameters carry the emoji and the provider message id.
func (s *ExternalConversationTransportService) ProjectMessageReaction(ctx context.Context, event *ConversationReactionEvent) error {
	if s == nil || s.store == nil || s.resolver == nil || event == nil {
		return nil
	}
	operation := capability.ConversationDeliveryReactionAdd
	if event.Type == ConversationReactionRemoved {
		operation = capability.ConversationDeliveryReactionRemove
	}
	message, err := s.store.GetChannelMessage(ctx, event.Scope, event.ConversationID, event.MessageID)
	if err != nil || message == nil || message.Historical || message.Audience.Kind != ConversationAudienceChannel {
		return err
	}
	target, err := s.externalReactionTarget(ctx, message)
	if err != nil || target == nil {
		return err
	}
	_, resolved, err := s.resolveActiveEndpoint(ctx, event.Scope, target.endpointID)
	if err != nil {
		return err
	}
	if !containsConversationFeature(resolved.Adapter.Features, capability.ConversationFeatureReactions) ||
		!containsConversationDeliveryOperation(resolved.Adapter.Delivery.Operations, operation) {
		return nil
	}
	_, err = s.Enqueue(ctx, EnqueueExternalConversationDeliveryRequest{
		Scope: event.Scope, EndpointID: target.endpointID, Operation: operation,
		ConversationID: message.ConversationID, ChannelMessageID: message.ID,
		ExternalConversationID: target.externalConversationID, ExternalThreadID: target.externalThreadID,
		Parameters: map[string]interface{}{
			"emoji": event.Emoji, "externalMessageId": target.externalMessageID,
			"actorType": string(event.Actor.Type), "actorId": event.Actor.ID,
		},
		IdempotencyKey: "conversation-reaction:" + target.endpointID + ":" + message.ConversationID + ":" + strconv.FormatInt(event.Sequence, 10),
	})
	return err
}

// externalReactionTarget finds the external conversation of an imported
// message from its source, and of an OpenSeal answer from its delivered send.
func (s *ExternalConversationTransportService) externalReactionTarget(ctx context.Context, message *ChannelMessage) (*externalReactionTarget, error) {
	if source := message.ExternalSource; source != nil {
		for _, reference := range message.References {
			if reference.Kind != ConversationReferenceExternalSource {
				continue
			}
			endpoint, err := s.store.GetExternalConversationEndpoint(ctx, message.Scope, reference.ID)
			if err != nil {
				return nil, err
			}
			if endpoint == nil || strings.TrimSpace(source.MessageID) == "" || strings.TrimSpace(source.ChannelID) == "" {
				continue
			}
			return &externalReactionTarget{
				endpointID: endpoint.ID, externalConversationID: source.ChannelID,
				externalThreadID: source.ThreadID, externalMessageID: source.MessageID,
			}, nil
		}
		return nil, nil
	}
	for page := 0; page < externalReactionDeliveryScanPages; page++ {
		deliveries, err := s.store.ListExternalConversationDeliveries(ctx, ExternalConversationDeliveryFilter{
			Scope: message.Scope, ConversationID: message.ConversationID,
			Statuses: []ExternalConversationDeliveryStatus{ExternalConversationDeliveryDelivered},
			Limit:    100, Offset: page * 100,
		})
		if err != nil {
			return nil, err
		}
		for _, delivery := range deliveries {
			if delivery != nil && delivery.ChannelMessageID == message.ID &&
				delivery.Operation == capability.ConversationDeliveryMessageSend && strings.TrimSpace(delivery.ProviderMessageID) != "" {
				return &externalReactionTarget{
					endpointID: delivery.EndpointID, externalConversationID: delivery.ExternalConversationID,
					externalThreadID: delivery.ExternalThreadID, externalMessageID: delivery.ProviderMessageID,
				}, nil
			}
		}
		if len(deliveries) < 100 {
			break
		}
	}
	return nil, nil
}
