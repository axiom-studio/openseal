package runtime

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestCallbackRegistryConnectionOnlyManagedLifecycle(t *testing.T) {
	for _, kind := range []string{"polling", "websocket"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			store := NewMemoryStore()
			catalog, request := managedCallbackRegistrationFixture(t, ctx, store, kind)
			registry := NewCallbackRegistry(store, catalog)
			created, err := registry.Create(ctx, request)
			if err != nil || created.Validate() != nil || len(created.Subscriptions) != 0 {
				t.Fatalf("created connection-only registration = %#v, %v", created, err)
			}
			active := CallbackRegistrationActive
			updated, err := registry.Update(ctx, created.Scope, created.ID, UpdateCallbackRegistrationRequest{
				ExpectedRevision: created.Revision, Status: &active,
				Actor: request.Actor, Reason: "activate managed provider connection",
			})
			if err != nil || updated.Status != active || len(updated.Subscriptions) != 0 {
				t.Fatalf("active connection-only registration = %#v, %v", updated, err)
			}
			byRoute, err := store.GetCallbackRegistrationByIngressRoute(ctx, updated.IngressRoute)
			if err != nil || byRoute.ID != created.ID || byRoute.Validate() != nil {
				t.Fatalf("durable connection-only registration = %#v, %v", byRoute, err)
			}
		})
	}
}

func TestCallbackRegistryRejectsEmptySubscriptionsWithoutManagedConnection(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	catalog := skill.NewCatalogWithStore(store)
	definition, binding := registerCallbackFixture(t, ctx, catalog)
	request := CreateCallbackRegistrationRequest{
		ID: "ordinary-webhook", Scope: Scope{Kind: "tenant", ID: "one"},
		Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent-one"}, DeploymentID: "agent-one",
		Name: "Slack webhook", Provider: "slack",
		Adapter: CallbackAdapterReference{
			SkillID: definition.ID, SkillVersion: definition.Version, BindingID: binding.ID,
			BindingRevision: binding.Revision, AdapterID: "interactions",
		},
		Actor: ActivityActor{Type: "user", ID: "operator"}, Reason: "register a callback",
	}
	registry := NewCallbackRegistry(store, catalog)
	if _, err := registry.Create(ctx, request); !errors.Is(err, ErrInvalidCallbackRegistration) || !strings.Contains(err.Error(), "subscriptions are required") {
		t.Fatalf("empty ordinary webhook error = %v", err)
	}
	request.Subscriptions = []CallbackSubscription{{EventType: "approval.decided", Consumer: "approvals"}}
	created, err := registry.Create(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	// A paused registration must not bypass the authoritative check by removing
	// its subscriptions before activation.
	if _, err := registry.Update(ctx, created.Scope, created.ID, UpdateCallbackRegistrationRequest{
		ExpectedRevision: created.Revision, Subscriptions: []CallbackSubscription{},
		Actor: request.Actor, Reason: "remove all webhook subscriptions",
	}); !errors.Is(err, ErrInvalidCallbackRegistration) {
		t.Fatalf("paused webhook removal error = %v", err)
	}
	unchanged, err := registry.Get(ctx, created.Scope, created.ID)
	if err != nil || unchanged.Revision != created.Revision || len(unchanged.Subscriptions) != 1 {
		t.Fatalf("invalid mutation changed durable registration = %#v, %v", unchanged, err)
	}
}

func TestCallbackRegistryConnectionOnlyRejectsInventedEventSubscription(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	catalog, request := managedCallbackRegistrationFixture(t, ctx, store, "polling")
	request.Subscriptions = []CallbackSubscription{{EventType: "approval.decided", Consumer: "approvals"}}
	if _, err := NewCallbackRegistry(store, catalog).Create(ctx, request); !errors.Is(err, ErrInvalidCallbackRegistration) {
		t.Fatalf("invented polling callback event error = %v", err)
	}
}

func TestCallbackIngressRefusesConnectionOnlyRegistration(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	catalog, request := managedCallbackRegistrationFixture(t, ctx, store, "polling")
	registry := NewCallbackRegistry(store, catalog)
	created, err := registry.Create(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	active := CallbackRegistrationActive
	registration, err := registry.Update(ctx, created.Scope, created.ID, UpdateCallbackRegistrationRequest{
		ExpectedRevision: created.Revision, Status: &active,
		Actor: request.Actor, Reason: "start managed polling connection",
	})
	if err != nil {
		t.Fatal(err)
	}
	host := &callbackHostStub{result: &CallbackHostResult{StatusCode: http.StatusOK}}
	_, err = NewCallbackIngressService(store, catalog, nil).Receive(ctx, CallbackPublicRequest{
		Route: registration.IngressRoute, Method: http.MethodPost, Body: []byte("untrusted provider body"),
	}, host)
	if !errors.Is(err, ErrCallbackRegistrationConflict) || !strings.Contains(err.Error(), "connection-only") || host.calls != 0 {
		t.Fatalf("connection-only callback ingress = host calls %d, error %v", host.calls, err)
	}
}

func managedCallbackRegistrationFixture(t *testing.T, ctx context.Context, store *MemoryStore, kind string) (*skill.Catalog, CreateCallbackRegistrationRequest) {
	t.Helper()
	adapter, err := capability.NormalizeCallbackAdapter(skill.CallbackAdapter{
		ProtocolVersion: skill.CallbackAdapterProtocolV1, Name: "Telegram updates",
		Description: "Forward authenticated Telegram updates through the conversation gateway.", Provider: "telegram",
		EventTypes: []string{}, Credentials: []capability.CredentialRequirement{{Name: "bot_token", Kind: "telegram_bot_token"}},
		Transport: skill.CallbackAdapterTransport{
			Kind: "http", IngressEndpoint: "telegram.callback.ingress",
			Connection: &capability.CallbackAdapterConnectionTransport{
				Kind: kind, Endpoint: "telegram.callback.polling", Credentials: []string{"bot_token"}, SharedByCredential: "bot_token",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	catalog := skill.NewCatalogWithStore(store)
	definition := &skill.Definition{
		ID: "skill-telegram", Version: "1.0.0", Name: "Telegram", Actions: map[string]skill.Action{},
		CallbackAdapters: map[string]skill.CallbackAdapter{"updates": adapter},
	}
	if err := catalog.Register(ctx, definition); err != nil {
		t.Fatal(err)
	}
	binding := &skill.Binding{
		ID: "telegram", Scope: skill.ScopeReference{Kind: "tenant", ID: "one"}, DeploymentID: "agent-one",
		SkillID: definition.ID, SkillVersion: definition.Version, EnabledCallbackAdapters: []string{"updates"},
		MaximumRisk: skill.RiskLevelRead, Revision: 1,
		Credentials: map[string]skill.CredentialReference{"bot_token": {Kind: "telegram_bot_token", ID: "vault://telegram-bot-token"}},
	}
	if err := catalog.Bind(ctx, binding); err != nil {
		t.Fatal(err)
	}
	return catalog, CreateCallbackRegistrationRequest{
		ID: "telegram-updates", Scope: Scope{Kind: "tenant", ID: "one"},
		Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent-one"}, DeploymentID: "agent-one",
		Name: "Telegram managed updates", Provider: "telegram",
		Adapter: CallbackAdapterReference{
			SkillID: definition.ID, SkillVersion: definition.Version, BindingID: binding.ID,
			BindingRevision: binding.Revision, AdapterID: "updates",
		},
		Subscriptions: []CallbackSubscription{}, Actor: ActivityActor{Type: "user", ID: "operator"},
		Reason: "receive Telegram messages through the conversation gateway",
	}
}
