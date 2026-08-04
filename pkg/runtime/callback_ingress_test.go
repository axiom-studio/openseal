package runtime

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
)

type callbackHostStub struct {
	result  *CallbackHostResult
	err     error
	calls   int
	request CallbackHostRequest
}

func (h *callbackHostStub) NormalizeCallback(_ context.Context, request CallbackHostRequest) (*CallbackHostResult, error) {
	h.calls++
	h.request = request
	return h.result, h.err
}

func TestCallbackIngressFollowsCurrentBindingWithoutRegistrationRewrite(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	catalog := newCallbackCatalog(t, ctx, store)
	registration := createActiveCallbackRegistration(t, ctx, store, catalog)

	current, err := catalog.GetDefinition(ctx, "skill-slack", "2.2.0")
	if err != nil {
		t.Fatal(err)
	}
	definition := *current
	definition.Version = "2.2.1"
	if err := catalog.Register(ctx, &definition); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Bind(ctx, &skill.Binding{
		ID: "slack", Scope: skill.ScopeReference{Kind: "tenant", ID: "one"}, DeploymentID: "agent-one",
		SkillID: definition.ID, SkillVersion: definition.Version, EnabledCallbackAdapters: []string{"interactions"},
		MaximumRisk: skill.RiskLevelRead, Revision: 2,
		Credentials: map[string]skill.CredentialReference{
			"signing_secret": {Kind: "slack_signing_secret", ID: "credential-store-signing-secret"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	host := &callbackHostStub{result: &CallbackHostResult{StatusCode: http.StatusOK}}
	service := NewCallbackIngressService(store, catalog, nil)
	if _, err := service.Receive(ctx, CallbackPublicRequest{
		Route: registration.IngressRoute, Method: http.MethodPost, Body: []byte("signed-upgrade"),
	}, host); err != nil {
		t.Fatal(err)
	}
	if host.request.Adapter == nil || host.request.Adapter.Binding == nil || host.request.Adapter.Binding.SkillVersion != "2.2.1" {
		t.Fatalf("resolved callback adapter = %#v", host.request.Adapter)
	}
	if registration.Adapter.SkillVersion != "2.2.0" {
		t.Fatalf("test requires an unchanged durable registration, got %#v", registration.Adapter)
	}
}

func TestCallbackIngressVerifiesPersistsDispatchesAndDeduplicates(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	catalog := newCallbackCatalog(t, ctx, store)
	registration := createActiveCallbackRegistration(t, ctx, store, catalog)
	consumed := 0
	var received EventEnvelope
	service := NewCallbackIngressService(store, catalog, map[string]CallbackEventConsumer{
		"approvals": CallbackEventConsumerFunc(func(_ context.Context, got *CallbackRegistration, subscription CallbackSubscription, event EventEnvelope) error {
			consumed++
			received = event
			if got.ID != registration.ID || subscription.TargetID != "slack-destination" {
				t.Fatalf("dispatch registration/subscription = %#v / %#v", got, subscription)
			}
			return nil
		}),
	})
	service.now = func() time.Time { return time.Date(2026, 7, 30, 2, 0, 0, 0, time.UTC) }
	host := &callbackHostStub{result: &CallbackHostResult{
		StatusCode: http.StatusOK,
		Events: []NormalizedCallbackEvent{{
			ID: "slack-action-1", Type: "approval.decided", Source: "slack",
			OccurredAt: time.Date(2026, 7, 30, 1, 59, 0, 0, time.UTC),
			Attributes: map[string]interface{}{"decision": "approve"},
		}},
	}}
	request := CallbackPublicRequest{Route: registration.IngressRoute, Method: http.MethodPost, Body: []byte("signed-body")}
	result, err := service.Receive(ctx, request, host)
	if err != nil {
		t.Fatal(err)
	}
	if consumed != 1 || len(result.Receipts) != 1 || result.Receipts[0].Status != CallbackEventApplied {
		t.Fatalf("first ingress = consumed %d, result %#v", consumed, result)
	}
	if received.Scope != registration.Scope || received.Actor.ID != registration.ID || received.Attributes["decision"] != "approve" {
		t.Fatalf("stamped event = %#v", received)
	}
	replayed, err := service.Receive(ctx, request, host)
	if err != nil {
		t.Fatal(err)
	}
	if consumed != 1 || replayed.Replayed != 1 || len(replayed.Receipts) != 1 {
		t.Fatalf("replayed ingress = consumed %d, result %#v", consumed, replayed)
	}
}

func TestCallbackIngressRejectsBeforeDispatchWhenVerificationFails(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	catalog := newCallbackCatalog(t, ctx, store)
	registration := createActiveCallbackRegistration(t, ctx, store, catalog)
	consumed := 0
	service := NewCallbackIngressService(store, catalog, map[string]CallbackEventConsumer{
		"approvals": CallbackEventConsumerFunc(func(context.Context, *CallbackRegistration, CallbackSubscription, EventEnvelope) error {
			consumed++
			return nil
		}),
	})
	verificationErr := errors.New("provider signature is invalid")
	_, err := service.Receive(ctx, CallbackPublicRequest{
		Route: registration.IngressRoute, Method: http.MethodPost, Body: []byte("forged"),
	}, &callbackHostStub{err: verificationErr})
	if !errors.Is(err, verificationErr) || consumed != 0 || len(store.callbackEvents) != 0 {
		t.Fatalf("verification failure = %v, consumed %d, receipts %d", err, consumed, len(store.callbackEvents))
	}
}

func TestCallbackRegistryAndReceiptsSurviveSQLiteRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "callbacks.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	catalog := skill.NewCatalogWithStore(store)
	registerCallbackFixture(t, ctx, catalog)
	registration := createActiveCallbackRegistration(t, ctx, store, catalog)
	consumed := 0
	service := NewCallbackIngressService(store, catalog, map[string]CallbackEventConsumer{
		"approvals": CallbackEventConsumerFunc(func(context.Context, *CallbackRegistration, CallbackSubscription, EventEnvelope) error {
			consumed++
			return nil
		}),
	})
	host := &callbackHostStub{result: &CallbackHostResult{StatusCode: http.StatusOK, Events: []NormalizedCallbackEvent{{
		ID: "event-after-restart", Type: "approval.decided", Source: "slack", OccurredAt: time.Now().UTC(),
	}}}}
	request := CallbackPublicRequest{Route: registration.IngressRoute, Method: http.MethodPost, Body: []byte("signed")}
	if _, err := service.Receive(ctx, request, host); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restartedCatalog := skill.NewCatalogWithStore(reopened)
	restarted := NewCallbackIngressService(reopened, restartedCatalog, map[string]CallbackEventConsumer{
		"approvals": CallbackEventConsumerFunc(func(context.Context, *CallbackRegistration, CallbackSubscription, EventEnvelope) error {
			consumed++
			return nil
		}),
	})
	result, err := restarted.Receive(ctx, request, host)
	if err != nil {
		t.Fatal(err)
	}
	if consumed != 1 || result.Replayed != 1 || len(result.Receipts) != 1 || result.Receipts[0].Status != CallbackEventApplied {
		t.Fatalf("restart replay = consumed %d, result %#v", consumed, result)
	}
}

func newCallbackCatalog(t *testing.T, ctx context.Context, store *MemoryStore) *skillCatalogAdapter {
	t.Helper()
	catalog := newSkillCatalogAdapter(store)
	registerCallbackFixture(t, ctx, catalog.Catalog)
	return catalog
}

// skillCatalogAdapter keeps the test helper's concrete catalog while making
// the callback resolver/store relationship explicit.
type skillCatalogAdapter struct{ *skill.Catalog }

func newSkillCatalogAdapter(store *MemoryStore) *skillCatalogAdapter {
	return &skillCatalogAdapter{Catalog: skill.NewCatalogWithStore(store)}
}

func createActiveCallbackRegistration(t *testing.T, ctx context.Context, store interface {
	CallbackRegistrationStore
}, catalog CallbackAdapterResolver) *CallbackRegistration {
	t.Helper()
	registry := NewCallbackRegistry(store, catalog)
	registry.newID = func() string { return "public-callback-route" }
	created, err := registry.Create(ctx, CreateCallbackRegistrationRequest{
		ID: "slack-approval", Scope: Scope{Kind: "tenant", ID: "one"},
		Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent-one"}, DeploymentID: "agent-one",
		Name: "Slack approval callback", Provider: "slack",
		Adapter: CallbackAdapterReference{
			SkillID: "skill-slack", SkillVersion: "2.2.0", BindingID: "slack", BindingRevision: 1, AdapterID: "interactions",
		},
		Subscriptions: []CallbackSubscription{{EventType: "approval.decided", Consumer: "approvals", TargetID: "slack-destination"}},
		Actor:         ActivityActor{Type: "user", ID: "operator"}, Reason: "register Slack approval callback",
	})
	if err != nil {
		t.Fatal(err)
	}
	active := CallbackRegistrationActive
	updated, err := registry.Update(ctx, created.Scope, created.ID, UpdateCallbackRegistrationRequest{
		ExpectedRevision: created.Revision, Status: &active,
		Actor: ActivityActor{Type: "user", ID: "operator"}, Reason: "activate configured callback",
	})
	if err != nil {
		t.Fatal(err)
	}
	return updated
}
