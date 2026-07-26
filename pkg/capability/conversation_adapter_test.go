package capability

import (
	"encoding/json"
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
		DestinationDiscovery: []ConversationDestinationDiscovery{{
			Action: "list-channels", Mode: ConversationEndpointChannel,
			ItemsPath: "channels", IDPath: "id", DisplayNamePath: "name", DescriptionPath: "purpose.text",
			CursorArgument: "cursor", LimitArgument: "limit", QueryArgument: "query", NextCursorPath: "nextCursor",
		}},
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
			DeliveryCredentials: []string{"SLACK_CONNECTION"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(adapter.EndpointModes, []ConversationEndpointMode{ConversationEndpointChannel, ConversationEndpointDirect}) ||
		!reflect.DeepEqual(adapter.InboundEventTypes, []string{ConversationEventMessageReceived, ConversationEventReactionAdded}) ||
		!reflect.DeepEqual(adapter.Credentials[0].OAuth2.Scopes, []string{"channels:history", "chat:write"}) ||
		adapter.DestinationDiscovery[0].DescriptionPath != "purpose.text" {
		t.Fatalf("normalized adapter = %#v", adapter)
	}
}

func TestNormalizeConversationAdapterIsStableAcrossJSONStorageWithoutOptionalFields(t *testing.T) {
	normalized, err := NormalizeConversationAdapter(ConversationAdapter{
		ProtocolVersion: ConversationAdapterProtocolV1,
		Name:            "Synthetic conversations", Description: "Receive and deliver synthetic conversations.", Provider: "synthetic",
		EndpointModes:     []ConversationEndpointMode{ConversationEndpointChannel},
		InboundEventTypes: []string{ConversationEventMessageReceived},
		Features:          []ConversationAdapterFeature{},
		Credentials:       []CredentialRequirement{},
		Delivery: ConversationDeliveryCapabilities{
			Operations: []ConversationDeliveryOperation{ConversationDeliveryMessageSend},
			Ordering:   ConversationDeliveryOrderConversation, Idempotency: IdempotencyRequired,
		},
		Transport: ConversationAdapterTransport{
			Kind: "plugin", IngressEndpoint: "synthetic.conversation.ingress", DeliveryEndpoint: "synthetic.conversation.deliver",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if normalized.Features != nil || normalized.Credentials != nil {
		t.Fatalf("optional fields are not canonical: %#v", normalized)
	}
	payload, err := json.Marshal(normalized)
	if err != nil {
		t.Fatal(err)
	}
	var restored ConversationAdapter
	if err := json.Unmarshal(payload, &restored); err != nil {
		t.Fatal(err)
	}
	restored, err = NormalizeConversationAdapter(restored)
	if err != nil || !reflect.DeepEqual(restored, normalized) {
		t.Fatalf("JSON round trip changed canonical adapter: %#v, %v", restored, err)
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
		"incomplete discovery pagination": func(value *ConversationAdapter) {
			value.DestinationDiscovery = []ConversationDestinationDiscovery{{Action: "list", Mode: ConversationEndpointChannel, ItemsPath: "items", IDPath: "id", DisplayNamePath: "name", CursorArgument: "cursor"}}
		},
		"unknown discovery mode": func(value *ConversationAdapter) {
			value.DestinationDiscovery = []ConversationDestinationDiscovery{{Action: "list", Mode: ConversationEndpointDirect, ItemsPath: "items", IDPath: "id", DisplayNamePath: "name"}}
		},
		"unknown credential use": func(value *ConversationAdapter) {
			value.Transport.IngressCredentials = []string{"UNKNOWN"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := valid
			mutate(&candidate)
			if _, err := NormalizeConversationAdapter(candidate); err == nil {
				t.Fatalf("invalid %s was accepted", name)
			}
		})
	}

	unusedCredential := valid
	unusedCredential.Credentials = []CredentialRequirement{{Name: "TOKEN", Kind: "secret"}}
	if _, err := NormalizeConversationAdapter(unusedCredential); err == nil {
		t.Fatal("credential without an ingress or delivery purpose was accepted")
	}
}
