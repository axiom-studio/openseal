package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestSkillReferenceUpgradeEstablishesLifecycleForWorkforceBinding(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	catalog := skill.NewCatalogWithStore(store)
	for _, version := range []string{"1.0.0", "1.1.0"} {
		if err := catalog.Register(ctx, &skill.Definition{
			ID: "browser", Version: version, Name: "Browser",
			Requirements: skill.Requirements{AlwaysAvailable: true}, Transport: skill.TransportReference{Kind: "tool", Endpoint: "browser"},
			Actions: map[string]skill.Action{"open": {
				Name: "open", Description: "Open a page", InputSchema: map[string]interface{}{"type": "object"},
				Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead, Idempotency: skill.IdempotencySupported,
			}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	scope := skill.ScopeReference{Kind: "tenant", ID: "1"}
	// Workforce apply bindings intentionally have no management lifecycle yet.
	if err := store.SaveSkillBinding(ctx, &skill.Binding{
		ID: "browser", Scope: scope, DeploymentID: "researcher", SkillID: "browser", SkillVersion: "1.0.0",
		AllowedActions: []string{"open"}, MaximumRisk: skill.RiskLevelRead, Revision: 1,
	}, 0); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC)
	service := NewSkillReferenceUpgradeService(store, catalog)
	service.now = func() time.Time { return now }
	plan, err := service.Plan(ctx, PlanSkillReferenceUpgradeRequest{
		Scope: Scope{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "researcher", BindingID: "browser", ToVersion: "1.1.0",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.Apply(ctx, ApplySkillReferenceUpgradeRequest{
		Plan: plan, Actor: ActivityActor{Type: "user", ID: "operator"}, Reason: "Adopt managed Browser runtime",
	}); err != nil {
		t.Fatal(err)
	}
	bindings, err := catalog.ListBindings(ctx, scope, "researcher")
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 1 || bindings[0].SkillVersion != "1.1.0" || !bindings[0].CreatedAt.Equal(now) ||
		!bindings[0].UpdatedAt.Equal(now) || len(bindings[0].Lifecycle) != 1 || bindings[0].Lifecycle[0].Revision != 2 {
		t.Fatalf("upgraded workforce binding = %#v", bindings)
	}
}

func TestSkillReferenceUpgradeMovesConversationAndCallbackAdaptersAtomically(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	catalog := skill.NewCatalogWithStore(store)
	for _, version := range []string{"1.0.0", "1.1.0"} {
		conversationAdapter, err := capability.NormalizeConversationAdapter(skill.ConversationAdapter{
			ProtocolVersion: capability.ConversationAdapterProtocolV1, Name: "Slack conversations", Description: "Deliver governed Slack messages.", Provider: "slack",
			EndpointModes: []capability.ConversationEndpointMode{capability.ConversationEndpointChannel}, InboundEventTypes: []string{capability.ConversationEventMessageReceived},
			Features:    []capability.ConversationAdapterFeature{capability.ConversationFeatureThreads},
			Credentials: []capability.CredentialRequirement{{Name: "token", Kind: "api_key"}},
			Delivery:    capability.ConversationDeliveryCapabilities{Operations: []capability.ConversationDeliveryOperation{capability.ConversationDeliveryMessageSend}, Ordering: capability.ConversationDeliveryOrderEndpoint, Idempotency: capability.IdempotencyRequired},
			Transport:   capability.ConversationAdapterTransport{Kind: "http", IngressEndpoint: "/ingress", DeliveryEndpoint: "/deliver", IngressCredentials: []string{"token"}, DeliveryCredentials: []string{"token"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		callbackAdapter, err := capability.NormalizeCallbackAdapter(skill.CallbackAdapter{
			ProtocolVersion: capability.CallbackAdapterProtocolV1, Name: "Slack interactions", Description: "Verify governed Slack interactions.", Provider: "slack",
			EventTypes: []string{capability.CallbackEventApprovalDecided}, Credentials: []capability.CredentialRequirement{{Name: "token", Kind: "api_key"}},
			Transport: capability.CallbackAdapterTransport{Kind: "http", IngressEndpoint: "/interactions", IngressCredentials: []string{"token"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := catalog.Register(ctx, &skill.Definition{
			ID: "slack", Version: version, Name: "Slack", Requirements: skill.Requirements{AlwaysAvailable: true},
			ConversationAdapters: map[string]skill.ConversationAdapter{"conversations": conversationAdapter},
			CallbackAdapters:     map[string]skill.CallbackAdapter{"interactions": callbackAdapter},
		}); err != nil {
			t.Fatal(err)
		}
	}
	scope := Scope{Kind: "tenant", ID: "1"}
	skillScope := skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}
	if err := store.SaveSkillBinding(ctx, &skill.Binding{
		ID: "slack", Scope: skillScope, DeploymentID: "agent:researcher", SkillID: "slack", SkillVersion: "1.0.0",
		EnabledConversationAdapters: []string{"conversations"}, EnabledCallbackAdapters: []string{"interactions"},
		Credentials: map[string]skill.CredentialReference{"token": {Kind: "api_key", ID: "vault://slack.token"}},
		MaximumRisk: skill.RiskLevelExternal, Revision: 3,
	}, 0); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC)
	owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent:researcher"}
	if err := store.CreateExternalConversationEndpoint(ctx, &ExternalConversationEndpoint{
		ID: "approval-channel", IngressRoute: "approval-channel-route", Scope: scope, Owner: owner, DeploymentID: owner.ID,
		Name: "Approval channel", Provider: "slack", Mode: capability.ConversationEndpointChannel, Address: "C123",
		Adapter: ExternalConversationAdapterReference{SkillID: "slack", SkillVersion: "1.0.0", BindingID: "slack", BindingRevision: 3, AdapterID: "conversations"},
		Handler: ExternalConversationHandler{Kind: ExternalConversationHandlerAgent, ID: owner.ID},
		Policy:  ExternalConversationPolicy{MessageSelection: ExternalConversationSelectAllMessages, ReplyMode: ExternalConversationReplyThread, IgnoreBots: true},
		Status:  ExternalConversationEndpointActive, Revision: 2, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateCallbackRegistration(ctx, &CallbackRegistration{
		ID: "approval-callback", IngressRoute: "approval-callback-route", Scope: scope, Owner: owner, DeploymentID: owner.ID,
		Name: "Approval callback", Provider: "slack",
		Adapter:       CallbackAdapterReference{SkillID: "slack", SkillVersion: "1.0.0", BindingID: "slack", BindingRevision: 3, AdapterID: "interactions"},
		Subscriptions: []CallbackSubscription{{EventType: capability.CallbackEventApprovalDecided, Consumer: "approvals"}},
		Status:        CallbackRegistrationActive, Revision: 2, CreatedAt: now, UpdatedAt: now,
		Lifecycle: []CallbackRegistrationLifecycleEntry{{Revision: 2, Action: CallbackRegistrationActivated, Actor: ActivityActor{Type: "user", ID: "operator"}, Reason: "activate", At: now}},
	}); err != nil {
		t.Fatal(err)
	}
	service := NewSkillReferenceUpgradeService(store, catalog)
	service.now = func() time.Time { return now.Add(time.Hour) }
	plan, err := service.Plan(ctx, PlanSkillReferenceUpgradeRequest{Scope: scope, DeploymentID: owner.ID, BindingID: "slack", ToVersion: "1.1.0"})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.ConversationEndpoints) != 1 || len(plan.CallbackRegistrations) != 1 {
		t.Fatalf("adapter impacts = %#v %#v", plan.ConversationEndpoints, plan.CallbackRegistrations)
	}
	if _, err = service.Apply(ctx, ApplySkillReferenceUpgradeRequest{Plan: plan, Actor: ActivityActor{Type: "user", ID: "operator"}, Reason: "upgrade Slack"}); err != nil {
		t.Fatal(err)
	}
	endpoint, _ := store.GetExternalConversationEndpoint(ctx, scope, "approval-channel")
	registration, _ := store.GetCallbackRegistration(ctx, scope, "approval-callback")
	if endpoint.Adapter.SkillVersion != "1.1.0" || endpoint.Adapter.BindingRevision != 4 || endpoint.Revision != 3 ||
		registration.Adapter.SkillVersion != "1.1.0" || registration.Adapter.BindingRevision != 4 || registration.Revision != 3 {
		t.Fatalf("upgraded adapters = endpoint %#v callback %#v", endpoint.Adapter, registration.Adapter)
	}
}
