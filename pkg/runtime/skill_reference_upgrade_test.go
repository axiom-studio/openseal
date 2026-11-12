package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestSkillReferenceUpgradeEstablishesLifecycleForWorkforceBinding(t *testing.T) {
	ctx := context.Background()
	store := newTestKernelStore(t)
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
	service := NewSkillReferenceUpgradeService(store, catalog, models.WithClock(func() time.Time { return now }))
	plan, err := service.Plan(ctx, PlanSkillReferenceUpgradeRequest{
		Scope: models.Scope{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "researcher", BindingID: "browser", ToVersion: "1.1.0",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.Apply(ctx, ApplySkillReferenceUpgradeRequest{
		Plan: plan, Actor: models.ActivityActor{Type: "user", ID: "operator"}, Reason: "Adopt managed Browser runtime",
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

// saveSkillBindingAtRevision stores binding through its earlier revisions, as
// edits would, because a store accepts only the next revision on each save.
func saveSkillBindingAtRevision(t *testing.T, store *sqlite.Store, binding *skill.Binding) {
	t.Helper()
	target := binding.Revision
	for revision := int64(1); revision <= target; revision++ {
		step := *binding
		step.Revision = revision
		if err := store.SaveSkillBinding(t.Context(), &step, revision-1); err != nil {
			t.Fatalf("save skill binding revision %d: %v", revision, err)
		}
	}
}

func TestSkillReferenceUpgradeMovesConversationAndCallbackAdaptersAtomically(t *testing.T) {
	ctx := context.Background()
	store := newTestKernelStore(t)
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
	scope := models.Scope{Kind: "tenant", ID: "1"}
	skillScope := skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}
	saveSkillBindingAtRevision(t, store, &skill.Binding{
		ID: "slack", Scope: skillScope, DeploymentID: "agent:researcher", SkillID: "slack", SkillVersion: "1.0.0",
		EnabledConversationAdapters: []string{"conversations"}, EnabledCallbackAdapters: []string{"interactions"},
		Credentials: map[string]skill.CredentialReference{"token": {Kind: "api_key", ID: "credential://slack.token"}},
		MaximumRisk: skill.RiskLevelExternal, Revision: 3,
	})
	now := time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC)
	owner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent:researcher"}
	if err := store.CreateExternalConversationEndpoint(ctx, &models.ExternalConversationEndpoint{
		ID: "approval-channel", IngressRoute: "approval-channel-route", Scope: scope, Owner: owner, DeploymentID: owner.ID,
		Name: "Approval channel", Provider: "slack", Mode: capability.ConversationEndpointChannel, Address: "C123",
		Adapter: models.ExternalConversationAdapterReference{SkillID: "slack", SkillVersion: "1.0.0", BindingID: "slack", BindingRevision: 3, AdapterID: "conversations"},
		Handler: models.ExternalConversationHandler{Kind: models.ExternalConversationHandlerAgent, ID: owner.ID},
		Policy:  models.ExternalConversationPolicy{MessageSelection: models.ExternalConversationSelectAllMessages, ReplyMode: models.ExternalConversationReplyThread, IgnoreBots: true},
		Status:  models.ExternalConversationEndpointActive, Revision: 2, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateCallbackRegistration(ctx, &models.CallbackRegistration{
		ID: "approval-callback", IngressRoute: "approval-callback-route", Scope: scope, Owner: owner, DeploymentID: owner.ID,
		Name: "Approval callback", Provider: "slack",
		Adapter:       models.CallbackAdapterReference{SkillID: "slack", SkillVersion: "1.0.0", BindingID: "slack", BindingRevision: 3, AdapterID: "interactions"},
		Subscriptions: []models.CallbackSubscription{{EventType: capability.CallbackEventApprovalDecided, Consumer: "approvals"}},
		Status:        models.CallbackRegistrationActive, Revision: 2, CreatedAt: now, UpdatedAt: now,
		Lifecycle: []models.CallbackRegistrationLifecycleEntry{{Revision: 2, Action: models.CallbackRegistrationActivated, Actor: models.ActivityActor{Type: "user", ID: "operator"}, Reason: "activate", At: now}},
	}); err != nil {
		t.Fatal(err)
	}
	service := NewSkillReferenceUpgradeService(store, catalog, models.WithClock(func() time.Time { return now.Add(time.Hour) }))
	plan, err := service.Plan(ctx, PlanSkillReferenceUpgradeRequest{Scope: scope, DeploymentID: owner.ID, BindingID: "slack", ToVersion: "1.1.0"})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.ConversationEndpoints) != 1 || len(plan.CallbackRegistrations) != 1 {
		t.Fatalf("adapter impacts = %#v %#v", plan.ConversationEndpoints, plan.CallbackRegistrations)
	}
	if _, err = service.Apply(ctx, ApplySkillReferenceUpgradeRequest{Plan: plan, Actor: models.ActivityActor{Type: "user", ID: "operator"}, Reason: "upgrade Slack"}); err != nil {
		t.Fatal(err)
	}
	endpoint, _ := store.GetExternalConversationEndpoint(ctx, scope, "approval-channel")
	registration, _ := store.GetCallbackRegistration(ctx, scope, "approval-callback")
	if endpoint.Adapter.SkillVersion != "1.1.0" || endpoint.Adapter.BindingRevision != 4 || endpoint.Revision != 3 ||
		registration.Adapter.SkillVersion != "1.1.0" || registration.Adapter.BindingRevision != 4 || registration.Revision != 3 {
		t.Fatalf("upgraded adapters = endpoint %#v callback %#v", endpoint.Adapter, registration.Adapter)
	}
}

func TestCompatibleAdapterFeatureAdditionPreservesAuthority(t *testing.T) {
	before := &skill.Definition{ConversationAdapters: map[string]skill.ConversationAdapter{"chat": {Features: []capability.ConversationAdapterFeature{"threading"}}}}
	after := &skill.Definition{ConversationAdapters: map[string]skill.ConversationAdapter{"chat": {Features: []capability.ConversationAdapterFeature{"threading", "attachments"}}}}
	binding := &skill.Binding{EnabledConversationAdapters: []string{"chat"}}
	if findings := compareUpgradeAdapterContracts(before, after, binding); len(findings) != 0 {
		t.Fatalf("additive interface blocked: %#v", findings)
	}
	adapter := after.ConversationAdapters["chat"]
	adapter.Features = []capability.ConversationAdapterFeature{"attachments"}
	after.ConversationAdapters["chat"] = adapter
	if len(compareUpgradeAdapterContracts(before, after, binding)) == 0 {
		t.Fatal("removed feature accepted")
	}
	adapter = after.ConversationAdapters["chat"]
	adapter.Features = before.ConversationAdapters["chat"].Features
	adapter.Provider = "foreign"
	after.ConversationAdapters["chat"] = adapter
	if len(compareUpgradeAdapterContracts(before, after, binding)) == 0 {
		t.Fatal("provider authority change accepted")
	}
}

func TestSkillReferenceUpgradeDropsActionsRemovedFromTarget(t *testing.T) {
	ctx := context.Background()
	store := newTestKernelStore(t)
	catalog := skill.NewCatalogWithStore(store)
	action := func(name string) skill.Action {
		return skill.Action{
			Name: name, Description: name, InputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"url": map[string]interface{}{"type": "string"}}},
			Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead, Idempotency: skill.IdempotencySupported,
		}
	}
	for version, actions := range map[string][]string{"1.0.0": {"open", "capture"}, "2.0.0": {"open"}} {
		definition := &skill.Definition{
			ID: "browser", Version: version, Name: "Browser",
			Requirements: skill.Requirements{AlwaysAvailable: true}, Transport: skill.TransportReference{Kind: "tool", Endpoint: "browser"},
			Actions: map[string]skill.Action{},
		}
		for _, name := range actions {
			definition.Actions[name] = action(name)
		}
		if err := catalog.Register(ctx, definition); err != nil {
			t.Fatal(err)
		}
	}
	scope := skill.ScopeReference{Kind: "tenant", ID: "1"}
	if err := store.SaveSkillBinding(ctx, &skill.Binding{
		ID: "browser", Scope: scope, DeploymentID: "researcher", SkillID: "browser", SkillVersion: "1.0.0",
		AllowedActions: []string{"open", "capture"}, MaximumRisk: skill.RiskLevelRead, Revision: 1,
		ArgumentRestrictions: map[string]map[string]skill.ArgumentRule{"capture": {"url": {Enum: []interface{}{"https://example.com"}}}},
	}, 0); err != nil {
		t.Fatal(err)
	}
	service := NewSkillReferenceUpgradeService(store, catalog)
	plan, err := service.Plan(ctx, PlanSkillReferenceUpgradeRequest{
		Scope: models.Scope{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "researcher", BindingID: "browser", ToVersion: "2.0.0",
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.ApprovalRequired || len(plan.Findings) != 0 || len(plan.RemovedActions) != 1 || plan.RemovedActions[0] != "capture" {
		t.Fatalf("plan approval=%v findings=%#v removed=%#v", plan.ApprovalRequired, plan.Findings, plan.RemovedActions)
	}
	if _, err = service.Apply(ctx, ApplySkillReferenceUpgradeRequest{
		Plan: plan, Actor: models.ActivityActor{Type: "user", ID: "operator"}, Reason: "Follow the newest Browser",
	}); err != nil {
		t.Fatal(err)
	}
	bindings, err := catalog.ListBindings(ctx, scope, "researcher")
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 1 || bindings[0].SkillVersion != "2.0.0" || len(bindings[0].AllowedActions) != 1 ||
		bindings[0].AllowedActions[0] != "open" || len(bindings[0].ArgumentRestrictions) != 0 {
		t.Fatalf("upgraded binding = %#v", bindings)
	}
}

func TestSkillReferenceUpgradeGrantActions(t *testing.T) {
	ctx := context.Background()
	store := newTestKernelStore(t)
	catalog := skill.NewCatalogWithStore(store)
	action := func(name string) skill.Action {
		return skill.Action{
			Name: name, Description: name, InputSchema: map[string]interface{}{"type": "object"},
			Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead, Idempotency: skill.IdempotencySupported,
		}
	}
	register := func(id, kind, version string, actions ...string) {
		t.Helper()
		definition := &skill.Definition{
			ID: id, Version: version, Name: id, Requirements: skill.Requirements{AlwaysAvailable: true},
			Transport: skill.TransportReference{Kind: kind, Endpoint: id}, Actions: map[string]skill.Action{},
		}
		for _, name := range actions {
			definition.Actions[name] = action(name)
		}
		if err := catalog.Register(ctx, definition); err != nil {
			t.Fatal(err)
		}
	}
	register("routines", "kernel", "1.0.0", "list", "start", "create")
	register("routines", "kernel", "1.1.0", "list", "start", "create", "event_sources", "send_test_event")
	register("browser", "tool", "1.0.0", "open")
	register("browser", "tool", "1.1.0", "open", "capture")
	scope := skill.ScopeReference{Kind: "tenant", ID: "1"}
	save := func(id, skillID string, allowed ...string) {
		t.Helper()
		if err := store.SaveSkillBinding(ctx, &skill.Binding{
			ID: id, Scope: scope, DeploymentID: "finn", SkillID: skillID, SkillVersion: "1.0.0",
			AllowedActions: allowed, MaximumRisk: skill.RiskLevelRead, Revision: 1,
		}, 0); err != nil {
			t.Fatal(err)
		}
	}
	// The owner removed start from the primary binding; the sibling never had create.
	save("routines", "routines", "list", "create")
	save("routines-team", "routines", "list", "start")
	save("browser", "browser", "open")
	service := NewSkillReferenceUpgradeService(store, catalog)
	request := PlanSkillReferenceUpgradeRequest{
		Scope: models.Scope{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "finn", BindingID: "routines", ToVersion: "1.1.0",
	}

	plain, err := service.Plan(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(plain.AddedActions) != 0 {
		t.Fatalf("plan without grants added %#v", plain.AddedActions)
	}
	request.GrantActions = []string{"send_test_event", "list", "start", "create", "event_sources", "unknown", " event_sources "}
	plan, err := service.Plan(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.AddedActions; len(got) != 2 || got[0] != "event_sources" || got[1] != "send_test_event" {
		t.Fatalf("added actions = %#v", got)
	}
	if plan.ApprovalRequired || len(plan.Findings) != 0 {
		t.Fatalf("granted actions alone required approval: %#v", plan.Findings)
	}
	if plan.Digest == plain.Digest {
		t.Fatal("plan digest does not cover added actions")
	}
	if len(plan.Siblings) != 1 || plan.Siblings[0].ID != "routines-team" {
		t.Fatalf("siblings = %#v", plan.Siblings)
	}

	if _, err = service.Apply(ctx, ApplySkillReferenceUpgradeRequest{
		Plan: plan, Actor: models.ActivityActor{Type: "system", ID: "sentinel"}, Reason: "Grant new routine actions",
	}); err != nil {
		t.Fatal(err)
	}
	bindings, err := catalog.ListBindings(ctx, scope, "finn")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"routines":      {"list", "create", "event_sources", "send_test_event"},
		"routines-team": {"list", "start", "event_sources", "send_test_event"},
		"browser":       {"open"},
	}
	for _, binding := range bindings {
		expected, ok := want[binding.ID]
		if !ok {
			t.Fatalf("unexpected binding %s", binding.ID)
		}
		delete(want, binding.ID)
		if binding.SkillID == "routines" && binding.SkillVersion != "1.1.0" {
			t.Fatalf("%s version = %s", binding.ID, binding.SkillVersion)
		}
		if strings.Join(binding.AllowedActions, ",") != strings.Join(expected, ",") {
			t.Fatalf("%s allowed actions = %#v, want %#v", binding.ID, binding.AllowedActions, expected)
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing bindings %#v", want)
	}

	// A tampered plan that claims extra added actions no longer matches.
	tampered := *plan
	tampered.AddedActions = []string{"event_sources", "send_test_event", "start"}
	if _, err = service.Apply(ctx, ApplySkillReferenceUpgradeRequest{
		Plan: &tampered, Actor: models.ActivityActor{Type: "system", ID: "sentinel"}, Reason: "replay",
	}); err == nil {
		t.Fatal("tampered plan applied")
	}

	_, err = service.Plan(ctx, PlanSkillReferenceUpgradeRequest{
		Scope: models.Scope{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "finn", BindingID: "browser", ToVersion: "1.1.0",
		GrantActions: []string{"capture"},
	})
	if !errors.Is(err, models.ErrSkillReferenceUpgradeInvalid) {
		t.Fatalf("grant on a tool Skill = %v", err)
	}
}
