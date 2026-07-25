package capability

import (
	"reflect"
	"testing"
)

func TestNormalizeConversationAdapterProducesStableProviderNeutralContract(t *testing.T) {
	adapter, err := NormalizeConversationAdapter(ConversationAdapter{
		ProtocolVersion: ConversationAdapterProtocolV1,
		Name:            "Slack conversations", Description: "Receive and deliver Slack conversations.", Provider: "slack",
		EndpointModes:     []ConversationEndpointMode{ConversationEndpointDirect, ConversationEndpointChannel},
		InboundEventTypes: []string{ConversationEventReactionAdded, ConversationEventMessageReceived},
		Features:          []ConversationAdapterFeature{ConversationFeatureThreads, ConversationFeatureMentions},
		Credentials: []CredentialRequirement{{
			Name: "SLACK_CONNECTION", Kind: "slack-oauth",
			OAuth2: &OAuth2Requirement{
				Provider: "slack", Subject: OAuth2SubjectInstallation,
				Scopes: []string{"chat:write", "channels:history"},
			},
		}},
		Delivery: ConversationDeliveryCapabilities{
			Operations: []ConversationDeliveryOperation{ConversationDeliveryMessageUpdate, ConversationDeliveryMessageSend},
			Ordering:   ConversationDeliveryOrderThread, Idempotency: IdempotencyRequired,
			SupportsAcknowledgementLookup: true, SupportsRetryAfter: true,
		},
		Transport: ConversationAdapterTransport{
			Kind: "plugin", IngressEndpoint: "slack.conversation.ingress", DeliveryEndpoint: "slack.conversation.deliver",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(adapter.EndpointModes, []ConversationEndpointMode{ConversationEndpointChannel, ConversationEndpointDirect}) ||
		!reflect.DeepEqual(adapter.InboundEventTypes, []string{ConversationEventMessageReceived, ConversationEventReactionAdded}) ||
		!reflect.DeepEqual(adapter.Credentials[0].OAuth2.Scopes, []string{"channels:history", "chat:write"}) {
		t.Fatalf("normalized adapter = %#v", adapter)
	}
}

func TestNormalizeConversationAdapterRejectsUnknownPortableSemantics(t *testing.T) {
	valid := ConversationAdapter{
		ProtocolVersion: ConversationAdapterProtocolV1,
		Name:            "Chat", Description: "Receive chat.", Provider: "chat",
		EndpointModes:     []ConversationEndpointMode{ConversationEndpointChannel},
		InboundEventTypes: []string{ConversationEventMessageReceived},
		Delivery: ConversationDeliveryCapabilities{
			Operations: []ConversationDeliveryOperation{ConversationDeliveryMessageSend},
			Ordering:   ConversationDeliveryOrderConversation, Idempotency: IdempotencySupported,
		},
		Transport: ConversationAdapterTransport{
			Kind: "plugin", IngressEndpoint: "chat.ingress", DeliveryEndpoint: "chat.deliver",
		},
	}
	for name, mutate := range map[string]func(*ConversationAdapter){
		"mode":      func(value *ConversationAdapter) { value.EndpointModes = []ConversationEndpointMode{"workspace"} },
		"event":     func(value *ConversationAdapter) { value.InboundEventTypes = []string{"slack.message"} },
		"feature":   func(value *ConversationAdapter) { value.Features = []ConversationAdapterFeature{"provider_magic"} },
		"transport": func(value *ConversationAdapter) { value.Transport.DeliveryEndpoint = "" },
		"protocol":  func(value *ConversationAdapter) { value.ProtocolVersion = "v0" },
		"delivery":  func(value *ConversationAdapter) { value.Delivery.Idempotency = IdempotencyNone },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := valid
			mutate(&candidate)
			if _, err := NormalizeConversationAdapter(candidate); err == nil {
				t.Fatalf("invalid %s was accepted", name)
			}
		})
	}
}
