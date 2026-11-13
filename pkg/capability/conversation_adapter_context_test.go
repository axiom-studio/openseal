package capability

import "testing"

func TestConversationAdapterContextHistoryIsAnOptionalDeclaredFeature(t *testing.T) {
	features, err := normalizeConversationFeatures([]ConversationAdapterFeature{
		ConversationFeatureThreads, ConversationFeatureContextHistory, ConversationFeatureContextHistory,
	})
	if err != nil || len(features) != 2 || features[0] != ConversationFeatureContextHistory {
		t.Fatalf("history feature = %#v, %v", features, err)
	}
}

func TestConversationAdapterVisualsIsAnOptionalDeclaredFeature(t *testing.T) {
	features, err := normalizeConversationFeatures([]ConversationAdapterFeature{
		ConversationFeatureVisuals, ConversationFeatureAttachments, ConversationFeatureVisuals,
	})
	if err != nil || len(features) != 2 || features[0] != ConversationFeatureAttachments || features[1] != ConversationFeatureVisuals {
		t.Fatalf("visuals feature = %#v, %v", features, err)
	}
}

func TestConversationAdapterTakesCardAnswersOnlyWithVisuals(t *testing.T) {
	adapter := ConversationAdapter{
		ProtocolVersion: ConversationAdapterProtocolV1,
		Name:            "Chat", Description: "Receive chat.", Provider: "chat",
		EndpointModes:     []ConversationEndpointMode{ConversationEndpointChannel},
		InboundEventTypes: []string{ConversationEventMessageReceived, ConversationEventCardInteracted},
		Delivery: ConversationDeliveryCapabilities{
			Operations: []ConversationDeliveryOperation{ConversationDeliveryMessageSend},
			Ordering:   ConversationDeliveryOrderConversation, Idempotency: IdempotencyRequired,
		},
		Transport: ConversationAdapterTransport{Kind: "plugin", IngressEndpoint: "chat.ingress", DeliveryEndpoint: "chat.deliver"},
	}
	if _, err := NormalizeConversationAdapter(adapter); err == nil {
		t.Fatal("an adapter without visuals declared card answers")
	}
	adapter.Features = []ConversationAdapterFeature{ConversationFeatureVisuals}
	normalized, err := NormalizeConversationAdapter(adapter)
	if err != nil || len(normalized.InboundEventTypes) != 2 || normalized.InboundEventTypes[0] != ConversationEventCardInteracted {
		t.Fatalf("adapter = %#v, %v", normalized, err)
	}
}
