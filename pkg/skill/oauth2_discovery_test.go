package skill

import "testing"

func TestDiscoveryNormalizesOAuth2RequirementsWithoutConnectionIdentity(t *testing.T) {
	page, err := NormalizeDiscoveryPage(DiscoveryRequest{Limit: 1}, &DiscoveryPage{Items: []DiscoveryCandidate{{
		ID: "slack", Version: "1.0.0", Name: "Slack", Readiness: DiscoveryReadinessBindable,
		Credentials: []DiscoveryCredential{{
			Name: "SLACK_CONNECTION", Kind: "slack-oauth", Configured: true,
			OAuth2: &OAuth2Requirement{
				Provider: "slack", Subject: OAuth2SubjectInstallation,
				Scopes: []string{"chat:write", "channels:history"},
			},
		}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	scopes := page.Items[0].Credentials[0].OAuth2.Scopes
	if len(scopes) != 2 || scopes[0] != "channels:history" || scopes[1] != "chat:write" {
		t.Fatalf("normalized discovery scopes = %#v", scopes)
	}
}

func TestDiscoveryProjectsConversationAdapterGuaranteesWithoutEntrypointsOrConnections(t *testing.T) {
	page, err := NormalizeDiscoveryPage(DiscoveryRequest{Limit: 1}, &DiscoveryPage{Items: []DiscoveryCandidate{{
		ID: "slack", Version: "1.0.0", Name: "Slack", Readiness: DiscoveryReadinessBindable,
		ConversationAdapters: []DiscoveryConversationAdapter{{
			ID: "conversations", ProtocolVersion: ConversationAdapterProtocolV1, Provider: "slack",
			EndpointModes:                []ConversationEndpointMode{ConversationEndpointDirect, ConversationEndpointChannel},
			DiscoverableDestinationModes: []ConversationEndpointMode{ConversationEndpointChannel},
			InboundEventTypes:            []string{ConversationEventReactionAdded, ConversationEventMessageReceived},
			Features:                     []ConversationAdapterFeature{ConversationFeatureThreads, ConversationFeatureMentions},
			Delivery: ConversationDeliveryCapabilities{
				Operations: []ConversationDeliveryOperation{ConversationDeliveryMessageUpdate, ConversationDeliveryMessageSend},
				Ordering:   ConversationDeliveryOrderThread, Idempotency: IdempotencyRequired,
				SupportsAcknowledgementLookup: true, SupportsRetryAfter: true,
			},
			Credentials: []DiscoveryCredential{{
				Name: "SLACK_CONNECTION", Kind: "slack-oauth", Configured: true,
				OAuth2: &OAuth2Requirement{
					Provider: "slack", Subject: OAuth2SubjectInstallation,
					Scopes: []string{"chat:write", "channels:history"},
				},
			}},
		}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	adapter := page.Items[0].ConversationAdapters[0]
	if adapter.ProtocolVersion != ConversationAdapterProtocolV1 || adapter.Provider != "slack" ||
		len(adapter.Delivery.Operations) != 2 || adapter.Delivery.Operations[0] != ConversationDeliveryMessageSend ||
		adapter.Credentials[0].OAuth2.Scopes[0] != "channels:history" || len(adapter.DiscoverableDestinationModes) != 1 || adapter.DiscoverableDestinationModes[0] != ConversationEndpointChannel {
		t.Fatalf("normalized discovery adapter = %#v", adapter)
	}
}
