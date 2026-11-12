package callback

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
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
	store := kerneltest.NewStore(t)
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
	store := kerneltest.NewStore(t)
	catalog := newCallbackCatalog(t, ctx, store)
	registration := createActiveCallbackRegistration(t, ctx, store, catalog)
	consumed := 0
	var received models.EventEnvelope
	service := NewCallbackIngressService(store, catalog, map[string]CallbackEventConsumer{
		"approvals": CallbackEventConsumerFunc(func(_ context.Context, got *models.CallbackRegistration, subscription models.CallbackSubscription, event models.EventEnvelope) error {
			consumed++
			received = event
			if got.ID != registration.ID || subscription.TargetID != "slack-destination" {
				t.Fatalf("dispatch registration/subscription = %#v / %#v", got, subscription)
			}
			return nil
		}),
	}, models.WithClock(func() time.Time { return time.Date(2026, 7, 30, 2, 0, 0, 0, time.UTC) }))
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
	if consumed != 1 || len(result.Receipts) != 1 || result.Receipts[0].Status != models.CallbackEventApplied {
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

func TestCallbackIngressAcknowledgesDurableReceiptBeforeSlowConsumer(t *testing.T) {
	ctx := context.Background()
	store := kerneltest.NewStore(t)
	catalog := newCallbackCatalog(t, ctx, store)
	registration := createActiveCallbackRegistration(t, ctx, store, catalog)
	consumed := 0
	now := time.Date(2026, 8, 20, 20, 19, 36, 0, time.UTC)
	service := NewCallbackIngressService(store, catalog, map[string]CallbackEventConsumer{
		"approvals": CallbackEventConsumerFunc(func(context.Context, *models.CallbackRegistration, models.CallbackSubscription, models.EventEnvelope) error {
			consumed++
			time.Sleep(50 * time.Millisecond)
			return nil
		}),
	}, models.WithClock(func() time.Time { return now }))
	wakes := 0
	service.SetDurableDispatcher(func() { wakes++ })
	host := &callbackHostStub{result: &CallbackHostResult{
		StatusCode: http.StatusOK,
		Events: []NormalizedCallbackEvent{{
			ID: "slack-action-async", Type: "approval.decided", Source: "slack", OccurredAt: now,
		}},
	}}
	request := CallbackPublicRequest{Route: registration.IngressRoute, Method: http.MethodPost, Body: []byte("signed-body")}

	started := time.Now()
	result, err := service.Receive(ctx, request, host)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed >= 25*time.Millisecond {
		t.Fatalf("durable acknowledgement waited for consumer: %s", elapsed)
	}
	if consumed != 0 || wakes != 1 || len(result.Receipts) != 1 || result.Receipts[0].Status != models.CallbackEventPending {
		t.Fatalf("ingress result consumed=%d wakes=%d result=%#v", consumed, wakes, result)
	}
	replayed, err := service.Receive(ctx, request, host)
	if err != nil || replayed.Replayed != 1 || consumed != 0 {
		t.Fatalf("pending duplicate was not acknowledged: consumed=%d result=%#v err=%v", consumed, replayed, err)
	}

	worker, err := NewCallbackEventWorker(store, service, CallbackEventWorkerConfig{
		WorkerID: "callback-worker", LeaseDuration: time.Minute, BaseRetry: time.Second, MaximumRetry: time.Minute,
	}, models.WithClock(func() time.Time { return now.Add(time.Second) }))
	if err != nil {
		t.Fatal(err)
	}
	applied, err := worker.ProcessOne(ctx, registration.Scope)
	if err != nil {
		t.Fatal(err)
	}
	if consumed != 1 || applied == nil || applied.Status != models.CallbackEventApplied || applied.Attempts != 1 {
		t.Fatalf("worker result consumed=%d receipt=%#v", consumed, applied)
	}
	if next, err := worker.ProcessOne(ctx, registration.Scope); err != nil || next != nil {
		t.Fatalf("applied callback was reclaimed: receipt=%#v err=%v", next, err)
	}
}

func TestCallbackEventWorkerReclaimsExpiredLease(t *testing.T) {
	ctx := context.Background()
	store := kerneltest.NewStore(t)
	catalog := newCallbackCatalog(t, ctx, store)
	registration := createActiveCallbackRegistration(t, ctx, store, catalog)
	consumed := 0
	now := time.Date(2026, 8, 20, 20, 19, 36, 0, time.UTC)
	service := NewCallbackIngressService(store, catalog, map[string]CallbackEventConsumer{
		"approvals": CallbackEventConsumerFunc(func(context.Context, *models.CallbackRegistration, models.CallbackSubscription, models.EventEnvelope) error {
			consumed++
			return nil
		}),
	}, models.WithClock(func() time.Time { return now }))
	service.SetDurableDispatcher(nil)
	host := &callbackHostStub{result: &CallbackHostResult{StatusCode: http.StatusOK, Events: []NormalizedCallbackEvent{{
		ID: "restart-event", Type: "approval.decided", Source: "slack", OccurredAt: now,
	}}}}
	if _, err := service.Receive(ctx, CallbackPublicRequest{Route: registration.IngressRoute, Method: http.MethodPost, Body: []byte("signed")}, host); err != nil {
		t.Fatal(err)
	}
	if claimed, err := store.ClaimCallbackEvent(ctx, registration.Scope, "stopped-worker", now, time.Second); err != nil || claimed == nil {
		t.Fatalf("initial claim = %#v, %v", claimed, err)
	}
	worker, err := NewCallbackEventWorker(store, service, CallbackEventWorkerConfig{WorkerID: "restarted-worker", LeaseDuration: time.Minute}, models.WithClock(func() time.Time { return now.Add(2 * time.Second) }))
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := worker.ProcessOne(ctx, registration.Scope)
	if err != nil || recovered == nil || recovered.Status != models.CallbackEventApplied || recovered.Attempts != 2 || consumed != 1 {
		t.Fatalf("recovered callback = %#v consumed=%d err=%v", recovered, consumed, err)
	}
}

func TestCallbackIngressRejectsBeforeDispatchWhenVerificationFails(t *testing.T) {
	ctx := context.Background()
	store, db := newCallbackTestSQLiteStore(t)
	catalog := newCallbackCatalog(t, ctx, store)
	registration := createActiveCallbackRegistration(t, ctx, store, catalog)
	consumed := 0
	service := NewCallbackIngressService(store, catalog, map[string]CallbackEventConsumer{
		"approvals": CallbackEventConsumerFunc(func(context.Context, *models.CallbackRegistration, models.CallbackSubscription, models.EventEnvelope) error {
			consumed++
			return nil
		}),
	})
	verificationErr := errors.New("provider signature is invalid")
	_, err := service.Receive(ctx, CallbackPublicRequest{
		Route: registration.IngressRoute, Method: http.MethodPost, Body: []byte("forged"),
	}, &callbackHostStub{err: verificationErr})
	if receipts := storetest.CountRows(t, db, "callback_events"); !errors.Is(err, verificationErr) || consumed != 0 || receipts != 0 {
		t.Fatalf("verification failure = %v, consumed %d, receipts %d", err, consumed, receipts)
	}
}

func TestCallbackRegistryAndReceiptsSurviveSQLiteRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "callbacks.db")
	store, err := sqlite.New(path)
	if err != nil {
		t.Fatal(err)
	}
	catalog := skill.NewCatalogWithStore(store)
	registerCallbackFixture(t, ctx, catalog)
	registration := createActiveCallbackRegistration(t, ctx, store, catalog)
	consumed := 0
	service := NewCallbackIngressService(store, catalog, map[string]CallbackEventConsumer{
		"approvals": CallbackEventConsumerFunc(func(context.Context, *models.CallbackRegistration, models.CallbackSubscription, models.EventEnvelope) error {
			consumed++
			return nil
		}),
	})
	service.SetDurableDispatcher(nil)
	host := &callbackHostStub{result: &CallbackHostResult{StatusCode: http.StatusOK, Events: []NormalizedCallbackEvent{{
		ID: "event-after-restart", Type: "approval.decided", Source: "slack", OccurredAt: time.Now().UTC(),
	}}}}
	request := CallbackPublicRequest{Route: registration.IngressRoute, Method: http.MethodPost, Body: []byte("signed")}
	if _, err := service.Receive(ctx, request, host); err != nil {
		t.Fatal(err)
	}
	if consumed != 0 {
		t.Fatalf("durable ingress dispatched before restart: %d", consumed)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := sqlite.New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restartedCatalog := skill.NewCatalogWithStore(reopened)
	restarted := NewCallbackIngressService(reopened, restartedCatalog, map[string]CallbackEventConsumer{
		"approvals": CallbackEventConsumerFunc(func(context.Context, *models.CallbackRegistration, models.CallbackSubscription, models.EventEnvelope) error {
			consumed++
			return nil
		}),
	})
	restarted.SetDurableDispatcher(nil)
	worker, err := NewCallbackEventWorker(reopened, restarted, CallbackEventWorkerConfig{WorkerID: "sqlite-restart-worker"})
	if err != nil {
		t.Fatal(err)
	}
	if receipt, err := worker.ProcessOne(ctx, registration.Scope); err != nil || receipt == nil || receipt.Status != models.CallbackEventApplied {
		t.Fatalf("restart dispatch = %#v, %v", receipt, err)
	}
	result, err := restarted.Receive(ctx, request, host)
	if err != nil {
		t.Fatal(err)
	}
	if consumed != 1 || result.Replayed != 1 || len(result.Receipts) != 1 || result.Receipts[0].Status != models.CallbackEventApplied {
		t.Fatalf("restart replay = consumed %d, result %#v", consumed, result)
	}
}

func newCallbackCatalog(t *testing.T, ctx context.Context, store *sqlite.Store) *skillCatalogAdapter {
	t.Helper()
	catalog := newSkillCatalogAdapter(store)
	registerCallbackFixture(t, ctx, catalog.Catalog)
	return catalog
}

// skillCatalogAdapter keeps the test helper's concrete catalog while making
// the callback resolver/store relationship explicit.
type skillCatalogAdapter struct{ *skill.Catalog }

func newSkillCatalogAdapter(store *sqlite.Store) *skillCatalogAdapter {
	return &skillCatalogAdapter{Catalog: skill.NewCatalogWithStore(store)}
}

func createActiveCallbackRegistration(t *testing.T, ctx context.Context, store interface {
	models.CallbackRegistrationStore
}, catalog CallbackAdapterResolver) *models.CallbackRegistration {
	t.Helper()
	registry := NewCallbackRegistry(store, catalog)
	registry.newID = func() string { return "public-callback-route" }
	created, err := registry.Create(ctx, CreateCallbackRegistrationRequest{
		ID: "slack-approval", Scope: models.Scope{Kind: "tenant", ID: "one"},
		Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent-one"}, DeploymentID: "agent-one",
		Name: "Slack approval callback", Provider: "slack",
		Adapter: models.CallbackAdapterReference{
			SkillID: "skill-slack", SkillVersion: "2.2.0", BindingID: "slack", BindingRevision: 1, AdapterID: "interactions",
		},
		Subscriptions: []models.CallbackSubscription{{EventType: "approval.decided", Consumer: "approvals", TargetID: "slack-destination"}},
		Actor:         models.ActivityActor{Type: "user", ID: "operator"}, Reason: "register Slack approval callback",
	})
	if err != nil {
		t.Fatal(err)
	}
	active := models.CallbackRegistrationActive
	updated, err := registry.Update(ctx, created.Scope, created.ID, UpdateCallbackRegistrationRequest{
		ExpectedRevision: created.Revision, Status: &active,
		Actor: models.ActivityActor{Type: "user", ID: "operator"}, Reason: "activate configured callback",
	})
	if err != nil {
		t.Fatal(err)
	}
	return updated
}
