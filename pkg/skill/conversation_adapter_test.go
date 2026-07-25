package skill

import (
	"context"
	"errors"
	"testing"
)

func TestCatalogBindsAndResolvesSkillOwnedConversationAdapter(t *testing.T) {
	definition := conversationAdapterOnlySkill()
	catalog := NewCatalog()
	if err := catalog.Register(context.Background(), definition); err != nil {
		t.Fatal(err)
	}
	scope := ScopeReference{Kind: "tenant", ID: "one"}
	binding := &Binding{
		ID: "slack-conversation", Scope: scope, DeploymentID: "agent", SkillID: definition.ID, SkillVersion: definition.Version,
		EnabledConversationAdapters: []string{"conversations"}, MaximumRisk: RiskLevelRead,
		Credentials: map[string]CredentialReference{
			"SLACK_CONNECTION": {Kind: "slack-oauth", ID: "connection://slack/one"},
		},
		Revision: 1,
	}
	if err := catalog.Bind(context.Background(), binding); err != nil {
		t.Fatal(err)
	}
	resolved, err := catalog.ResolveConversationAdapter(
		context.Background(), scope, "agent", definition.ID, definition.Version, "conversations",
		BindingReference{ID: binding.ID, Revision: binding.Revision},
	)
	if err != nil || resolved.Adapter.Provider != "slack" || resolved.Binding.Credentials["SLACK_CONNECTION"].ID != "connection://slack/one" {
		t.Fatalf("resolved adapter = %#v, %v", resolved, err)
	}

	missingCredential := cloneBinding(binding)
	missingCredential.ID = "missing"
	missingCredential.Credentials = nil
	if err := catalog.Bind(context.Background(), missingCredential); err == nil {
		t.Fatal("adapter binding without its opaque OAuth connection should fail")
	}
}

func TestConversationAdapterActivationRequiresOnlyGenericHost(t *testing.T) {
	definition := conversationAdapterOnlySkill()
	catalog := NewCatalog()
	if err := catalog.Register(context.Background(), definition); err != nil {
		t.Fatal(err)
	}
	scope := ScopeReference{Kind: "tenant", ID: "one"}
	if err := catalog.Bind(context.Background(), &Binding{
		ID: "slack", Scope: scope, DeploymentID: "agent", SkillID: definition.ID, SkillVersion: definition.Version,
		EnabledConversationAdapters: []string{"conversations"}, MaximumRisk: RiskLevelRead,
		Credentials: map[string]CredentialReference{"SLACK_CONNECTION": {Kind: "slack-oauth", ID: "connection://slack/one"}},
		Revision:    1,
	}); err != nil {
		t.Fatal(err)
	}
	unavailable, err := catalog.Activate(context.Background(), scope, "agent", HostCapabilityState{})
	if err != nil || len(unavailable.Unavailable) != 1 ||
		unavailable.Unavailable[0].Reasons[0].Code != "conversation_adapter_host_unavailable" {
		t.Fatalf("unavailable adapter = %#v, %v", unavailable, err)
	}
	active, err := catalog.Activate(context.Background(), scope, "agent", HostCapabilityState{Adapters: map[string]AdapterCapability{
		AdapterConversation: {State: AdapterStateAvailable, Features: []string{"plugin"}},
	}})
	if err != nil || len(active.Skills) != 1 || len(active.Skills[0].ConversationAdapters) != 1 ||
		active.Skills[0].ConversationAdapters[0].Adapter.Provider != "slack" {
		t.Fatalf("active generic-host adapter = %#v, %v", active, err)
	}
}

func TestConversationAdapterResolutionDetectsAmbiguousBindings(t *testing.T) {
	definition := conversationAdapterOnlySkill()
	catalog := NewCatalog()
	if err := catalog.Register(context.Background(), definition); err != nil {
		t.Fatal(err)
	}
	scope := ScopeReference{Kind: "tenant", ID: "one"}
	for _, id := range []string{"one", "two"} {
		if err := catalog.Bind(context.Background(), &Binding{
			ID: id, Scope: scope, DeploymentID: "agent", SkillID: definition.ID, SkillVersion: definition.Version,
			EnabledConversationAdapters: []string{"conversations"}, MaximumRisk: RiskLevelRead,
			Credentials: map[string]CredentialReference{"SLACK_CONNECTION": {Kind: "slack-oauth", ID: "connection://" + id}},
			Revision:    1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := catalog.ResolveConversationAdapter(context.Background(), scope, "agent", definition.ID, definition.Version, "conversations"); !errors.Is(err, ErrBindingAmbiguous) {
		t.Fatalf("ambiguous adapter resolution = %v", err)
	}
}

func conversationAdapterOnlySkill() *Definition {
	return &Definition{
		ID: "slack", Version: "1.0.0", Name: "Slack",
		Actions: map[string]Action{},
		ConversationAdapters: map[string]ConversationAdapter{"conversations": {
			ProtocolVersion: ConversationAdapterProtocolV1,
			Name:            "Slack conversations", Description: "Receive and deliver Slack conversations.", Provider: "slack",
			EndpointModes:     []ConversationEndpointMode{ConversationEndpointChannel},
			InboundEventTypes: []string{ConversationEventMessageReceived},
			Features:          []ConversationAdapterFeature{ConversationFeatureMentions, ConversationFeatureThreads},
			Credentials: []CredentialRequirement{{
				Name: "SLACK_CONNECTION", Kind: "slack-oauth",
				OAuth2: &OAuth2Requirement{
					Provider: "slack", Subject: OAuth2SubjectInstallation,
					Scopes: []string{"channels:history", "chat:write"},
				},
			}},
			Delivery: ConversationDeliveryCapabilities{
				Operations: []ConversationDeliveryOperation{ConversationDeliveryMessageSend},
				Ordering:   ConversationDeliveryOrderThread, Idempotency: IdempotencyRequired,
				SupportsAcknowledgementLookup: true, SupportsRetryAfter: true,
			},
			Transport: ConversationAdapterTransport{
				Kind: "plugin", IngressEndpoint: "slack.conversation.ingress", DeliveryEndpoint: "slack.conversation.deliver",
			},
		}},
	}
}
