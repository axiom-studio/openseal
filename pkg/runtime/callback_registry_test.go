package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestCallbackRegistryPinsExactAdapterAndLifecycle(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	catalog := skill.NewCatalogWithStore(store)
	definition, binding := registerCallbackFixture(t, ctx, catalog)
	registry := NewCallbackRegistry(store, catalog)
	clock := time.Date(2026, 7, 30, 1, 2, 3, 0, time.UTC)
	registry.now = func() time.Time { return clock }
	registry.newID = func() string { return "opaque-route" }
	created, err := registry.Create(ctx, CreateCallbackRegistrationRequest{
		ID: "slack-approval", Scope: Scope{Kind: "tenant", ID: "one"},
		Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent-one"}, DeploymentID: "agent-one",
		Name: "Slack approvals", Provider: "slack",
		Adapter: CallbackAdapterReference{
			SkillID: definition.ID, SkillVersion: definition.Version, BindingID: binding.ID,
			BindingRevision: binding.Revision, AdapterID: "interactions",
		},
		Subscriptions: []CallbackSubscription{{EventType: "approval.decided", Consumer: "approvals", TargetID: "slack-destination"}},
		Configuration: map[string]interface{}{"channel": "C123"},
		Actor:         ActivityActor{Type: "user", ID: "operator"}, Reason: "receive signed approval decisions",
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Status != CallbackRegistrationPaused || created.IngressRoute != "opaque-route" || created.Revision != 1 {
		t.Fatalf("created callback = %#v", created)
	}
	active := CallbackRegistrationActive
	clock = clock.Add(time.Minute)
	updated, err := registry.Update(ctx, created.Scope, created.ID, UpdateCallbackRegistrationRequest{
		ExpectedRevision: 1, Status: &active,
		Actor: ActivityActor{Type: "user", ID: "operator"}, Reason: "provider endpoint configured",
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != CallbackRegistrationActive || updated.Revision != 2 || updated.Lifecycle[1].Action != CallbackRegistrationActivated {
		t.Fatalf("active callback = %#v", updated)
	}
	byRoute, err := store.GetCallbackRegistrationByIngressRoute(ctx, "opaque-route")
	if err != nil || byRoute.ID != created.ID {
		t.Fatalf("route lookup = %#v, %v", byRoute, err)
	}
	if _, err := registry.Update(ctx, created.Scope, created.ID, UpdateCallbackRegistrationRequest{
		ExpectedRevision: 1, Actor: ActivityActor{Type: "user", ID: "operator"}, Reason: "stale update",
	}); !errors.Is(err, ErrCallbackRegistrationConflict) {
		t.Fatalf("stale update error = %v", err)
	}
}

func TestCallbackRegistryRejectsUndeclaredEvent(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	catalog := skill.NewCatalogWithStore(store)
	definition, binding := registerCallbackFixture(t, ctx, catalog)
	_, err := NewCallbackRegistry(store, catalog).Create(ctx, CreateCallbackRegistrationRequest{
		Scope: Scope{Kind: "tenant", ID: "one"}, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent-one"},
		DeploymentID: "agent-one", Name: "Invalid callback", Provider: "slack",
		Adapter: CallbackAdapterReference{
			SkillID: definition.ID, SkillVersion: definition.Version, BindingID: binding.ID,
			BindingRevision: binding.Revision, AdapterID: "interactions",
		},
		Subscriptions: []CallbackSubscription{{EventType: "source.changed", Consumer: "runbooks"}},
		Actor:         ActivityActor{Type: "user", ID: "operator"}, Reason: "invalid event acceptance",
	})
	if !errors.Is(err, ErrInvalidCallbackRegistration) {
		t.Fatalf("undeclared event error = %v", err)
	}
}

func registerCallbackFixture(t *testing.T, ctx context.Context, catalog *skill.Catalog) (*skill.Definition, *skill.Binding) {
	t.Helper()
	definition := &skill.Definition{
		ID: "skill-slack", Version: "2.2.0", Name: "Slack", Actions: map[string]skill.Action{},
		CallbackAdapters: map[string]skill.CallbackAdapter{
			"interactions": {
				ProtocolVersion: skill.CallbackAdapterProtocolV1, Name: "Slack interactions",
				Description: "Verify signed interactive callbacks.", Provider: "slack",
				EventTypes:  []string{"approval.decided"},
				Credentials: []capability.CredentialRequirement{{Name: "signing_secret", Kind: "slack_signing_secret"}},
				Transport: skill.CallbackAdapterTransport{
					Kind: "http", IngressEndpoint: "/v1/callbacks/slack",
					IngressCredentials: []string{"signing_secret"},
				},
			},
		},
	}
	if err := catalog.Register(ctx, definition); err != nil {
		t.Fatal(err)
	}
	binding := &skill.Binding{
		ID: "slack", Scope: skill.ScopeReference{Kind: "tenant", ID: "one"}, DeploymentID: "agent-one",
		SkillID: definition.ID, SkillVersion: definition.Version,
		EnabledCallbackAdapters: []string{"interactions"}, MaximumRisk: skill.RiskLevelRead, Revision: 1,
		Credentials: map[string]skill.CredentialReference{
			"signing_secret": {Kind: "slack_signing_secret", ID: "vault-signing-secret"},
		},
	}
	if err := catalog.Bind(ctx, binding); err != nil {
		t.Fatal(err)
	}
	return definition, binding
}
