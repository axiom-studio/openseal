package externalconversationtest

import (
	"context"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runtime/externalconversation"
)

// StreamChatHost is a provider chat: sends post messages, updates edit them
// and deletes remove them.
type StreamChatHost struct {
	Messages  map[string]string
	Requests  []externalconversation.DeliveryHostRequest
	rejectAll capability.ConversationDeliveryOperation
	next      int
}

func (h *StreamChatHost) LookupExternalConversationDelivery(context.Context, externalconversation.DeliveryHostRequest) (*externalconversation.DeliveryAcknowledgement, error) {
	return &externalconversation.DeliveryAcknowledgement{Status: externalconversation.AcknowledgementUnknown}, nil
}

func (h *StreamChatHost) DeliverExternalConversation(_ context.Context, request externalconversation.DeliveryHostRequest) (*externalconversation.DeliveryHostResult, error) {
	h.Requests = append(h.Requests, request)
	if request.Delivery.Operation == h.rejectAll {
		return &externalconversation.DeliveryHostResult{Outcome: externalconversation.DeliveryOutcomeFailed, ErrorCode: "message_not_found"}, nil
	}
	switch request.Delivery.Operation {
	case capability.ConversationDeliveryMessageSend:
		h.next++
		id := "chat-" + string(rune('0'+h.next))
		h.Messages[id] = request.Message.Content
		return &externalconversation.DeliveryHostResult{Outcome: externalconversation.DeliveryOutcomeDelivered, ProviderMessageID: id}, nil
	case capability.ConversationDeliveryMessageUpdate:
		id, _ := request.Delivery.Parameters["providerMessageId"].(string)
		h.Messages[id] = request.Message.Content
		return &externalconversation.DeliveryHostResult{Outcome: externalconversation.DeliveryOutcomeDelivered, ProviderMessageID: id}, nil
	case capability.ConversationDeliveryMessageDelete:
		id, _ := request.Delivery.Parameters["providerMessageId"].(string)
		delete(h.Messages, id)
		return &externalconversation.DeliveryHostResult{Outcome: externalconversation.DeliveryOutcomeDelivered, ProviderMessageID: id}, nil
	}
	return &externalconversation.DeliveryHostResult{Outcome: externalconversation.DeliveryOutcomeDelivered, ProviderMessageID: "status"}, nil
}
