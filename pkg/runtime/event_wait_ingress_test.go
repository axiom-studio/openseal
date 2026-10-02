package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// This store records the production Publish contract while embedding the
// existing transport store, so the tests exercise automatic observer wiring.
type ingressRunEventStore struct {
	*MemoryStore
	mu         sync.Mutex
	events     map[string]*RunEventReceipt
	publishErr error
}

func (s *ingressRunEventStore) PublishRunEvent(_ context.Context, receipt *RunEventReceipt) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.publishErr != nil {
		return false, s.publishErr
	}
	if s.events == nil {
		s.events = make(map[string]*RunEventReceipt)
	}
	key := receipt.Event.Scope.Kind + "\x00" + receipt.Event.Scope.ID + "\x00" + receipt.Event.Source + "\x00" + receipt.Event.ID
	if existing := s.events[key]; existing != nil {
		if existing.Digest != receipt.Digest {
			return false, ErrRunEventConflict
		}
		return false, nil
	}
	s.events[key] = receipt
	return true, nil
}

func (*ingressRunEventStore) ClaimRunEventWaits(context.Context, ClaimRunEventWaitsRequest) ([]*RunEventWait, error) {
	return nil, nil
}

func (*ingressRunEventStore) ProcessRunEventWait(context.Context, ProcessRunEventWaitRequest) (*RunEventWaitResult, error) {
	return nil, nil
}

func (*ingressRunEventStore) GetRunEventWait(context.Context, Scope, string, string) (*RunEventWait, error) {
	return nil, nil
}

func (*ingressRunEventStore) PruneRunEvents(context.Context, Scope, time.Time, int) (int, error) {
	return 0, nil
}

func (s *ingressRunEventStore) onlyEvent(t *testing.T) EventEnvelope {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.events) != 1 {
		t.Fatalf("persisted observations = %d, want 1", len(s.events))
	}
	for _, receipt := range s.events {
		return receipt.Event
	}
	return EventEnvelope{}
}

func TestVerifiedGatewayRetainsUnboundReplyForGenericRunWait(t *testing.T) {
	for _, provider := range []string{"slack", "webchat"} {
		t.Run(provider, func(t *testing.T) {
			ctx := context.Background()
			memory, catalog, endpoint := externalConversationDeliveryFixture(t, ctx, provider)
			store := &ingressRunEventStore{MemoryStore: memory}
			service := NewExternalConversationTransportService(store, catalog)
			gateway := ExternalConversationIngressGateway{
				Scope: endpoint.Scope, DeploymentID: endpoint.DeploymentID,
				Adapter: endpoint.Adapter, Provider: provider,
				InstallationID: "installation-one", ApplicationID: "application-one",
			}
			host := &externalConversationGatewayHostStub{result: &ExternalConversationGatewayHostResult{
				StatusCode: http.StatusOK,
				Events: []ExternalConversationGatewayEvent{{
					InstallationID: "installation-one", ApplicationID: "application-one", Address: "unbound-direct-channel",
					Event: NormalizedExternalConversationEvent{
						ID: "reply-one", Type: capability.ConversationEventMessageReceived,
						ExternalConversationID: "unbound-direct-channel", ExternalThreadID: "thread-one",
						ExternalMessageID: "message-one", ExternalParticipantID: "person-one",
						Direct: true, Text: "My priorities are ready", OrderingKey: "message-one",
						OccurredAt: time.Now().UTC(),
						Attributes: map[string]interface{}{
							"bindingId": "forged-binding", "deploymentId": "forged-agent",
							"externalParticipantId": "forged-person", "provider": "forged-provider",
						},
					},
				}},
			}}
			request := ExternalConversationPublicIngressRequest{Route: "gateway-route", Method: http.MethodPost, Body: []byte(`{}`)}
			for attempt := 0; attempt < 2; attempt++ {
				result, err := service.NormalizeExternalConversationGatewayIngress(ctx, gateway, request, host)
				if err != nil || len(result.Received) != 0 {
					t.Fatalf("unbound gateway ingress = %#v, %v", result, err)
				}
			}
			event := store.onlyEvent(t)
			if event.Scope != endpoint.Scope || event.Source != RunEventBindingSource(endpoint.DeploymentID, endpoint.Adapter.BindingID, endpoint.Adapter.AdapterID) ||
				event.Subject != "unbound-direct-channel" || event.Attributes["externalParticipantId"] != "person-one" ||
				event.Attributes["externalThreadId"] != "thread-one" || event.Attributes["bindingId"] != endpoint.Adapter.BindingID ||
				event.Attributes["deploymentId"] != endpoint.DeploymentID || event.Attributes["provider"] != provider ||
				event.Attributes["installationId"] != "installation-one" || event.Attributes["applicationId"] != "application-one" ||
				event.Payload["text"] != "My priorities are ready" {
				t.Fatalf("trusted generic observation = %#v", event)
			}
		})
	}
}

func TestVerifiedGatewayUnboundReplyResumesOriginalRun(t *testing.T) {
	for _, provider := range []string{"slack", "webchat"} {
		t.Run(provider, func(t *testing.T) {
			ctx := t.Context()
			store, catalog, endpoint := externalConversationDeliveryFixture(t, ctx, provider)
			now := time.Now().UTC()
			spec := RunEventWaitSpec{
				Key: "requested-reply", Type: capability.ConversationEventMessageReceived, Subject: "unbound-direct-conversation",
				Source:     RunEventBindingSource(endpoint.DeploymentID, endpoint.Adapter.BindingID, endpoint.Adapter.AdapterID),
				Attributes: map[string]interface{}{"externalParticipantId": "requested-person", "installationId": "reviewed-installation"},
				After:      now.Add(-time.Minute), Deadline: now.Add(time.Hour),
			}
			run := eventWaitContractCreateForAgent(t, store, endpoint.Scope, spec, endpoint.DeploymentID, spec.After)
			gateway := ExternalConversationIngressGateway{Scope: endpoint.Scope, DeploymentID: endpoint.DeploymentID, Adapter: endpoint.Adapter, Provider: provider, InstallationID: "reviewed-installation"}
			host := &externalConversationGatewayHostStub{result: &ExternalConversationGatewayHostResult{StatusCode: http.StatusOK, Events: []ExternalConversationGatewayEvent{{
				InstallationID: gateway.InstallationID, Address: spec.Subject,
				Event: NormalizedExternalConversationEvent{ID: "requested-reply-event", Type: spec.Type, ExternalConversationID: spec.Subject,
					ExternalMessageID: "message-one", ExternalParticipantID: "requested-person", Text: "My priorities are ready", Direct: true, OrderingKey: "message-one", OccurredAt: now},
			}}}}
			service := NewExternalConversationTransportService(store, catalog)
			request := ExternalConversationPublicIngressRequest{Route: "verified-gateway", Method: http.MethodPost, Body: []byte("signed")}
			for attempt := 0; attempt < 2; attempt++ {
				result, err := service.NormalizeExternalConversationGatewayIngress(ctx, gateway, request, host)
				if err != nil || len(result.Received) != 0 {
					t.Fatalf("unbound provider reply was not retained independently of endpoint fanout: %#v, %v", result, err)
				}
			}
			eventWaitContractProcess(t, store, endpoint.Scope, now.Add(time.Second), 1)
			eventWaitContractProcess(t, store, endpoint.Scope, now.Add(2*time.Second), 0)
			wait, err := store.GetRunEventWait(ctx, endpoint.Scope, run.ID, spec.Key)
			if err != nil || wait.Status != RunEventWaitMatched || wait.EventID != "requested-reply-event" {
				t.Fatalf("verified unbound reply did not satisfy the requested wait: %#v, %v", wait, err)
			}
			resumed, err := store.GetAgentRun(ctx, endpoint.Scope, run.ID)
			if err != nil || resumed.ID != run.ID || resumed.Status != AgentRunStatusQueued || resumed.Revision != run.Revision+1 || resumed.Checkpoint["reportConversationId"] != "original-conversation" {
				t.Fatalf("verified reply lost or duplicated original continuation: %#v, %v", resumed, err)
			}
		})
	}
}

func TestVerifiedConversationIntakeRejectsFailedNormalizationAndPersistence(t *testing.T) {
	ctx := context.Background()
	memory, catalog, endpoint := externalConversationDeliveryFixture(t, ctx, "slack")
	store := &ingressRunEventStore{MemoryStore: memory}
	service := NewExternalConversationTransportService(store, catalog)
	gateway := ExternalConversationIngressGateway{
		Scope: endpoint.Scope, DeploymentID: endpoint.DeploymentID, Adapter: endpoint.Adapter, Provider: endpoint.Provider,
		InstallationID: "installation",
	}
	event := NormalizedExternalConversationEvent{
		ID: "reply-one", Type: capability.ConversationEventMessageReceived, ExternalConversationID: "unbound-channel",
		ExternalMessageID: "message-one", ExternalParticipantID: "person-one", Text: "Reply",
		OrderingKey: "message-one", OccurredAt: time.Now().UTC(),
	}
	host := &externalConversationGatewayHostStub{result: &ExternalConversationGatewayHostResult{
		StatusCode: http.StatusUnauthorized,
		Events:     []ExternalConversationGatewayEvent{{InstallationID: "installation", Address: "unbound-channel", Event: event}},
	}}
	request := ExternalConversationPublicIngressRequest{Route: "route", Method: http.MethodPost, Body: []byte(`{}`)}
	if _, err := service.NormalizeExternalConversationGatewayIngress(ctx, gateway, request, host); err == nil || len(store.events) != 0 {
		t.Fatalf("failed normalization published observations: %v, %#v", err, store.events)
	}
	host.result.StatusCode = http.StatusOK
	host.result.Events[0].Event.Type = "undeclared.provider.event"
	if _, err := service.NormalizeExternalConversationGatewayIngress(ctx, gateway, request, host); err == nil || len(store.events) != 0 {
		t.Fatalf("undeclared normalized event was published: %v, %#v", err, store.events)
	}
	host.result.Events[0].Event = event
	undeclared := host.result.Events[0]
	undeclared.Event.ID = "undeclared-after-valid"
	undeclared.Event.Type = "undeclared.provider.event"
	host.result.Events = append(host.result.Events, undeclared)
	if _, err := service.NormalizeExternalConversationGatewayIngress(ctx, gateway, request, host); err == nil || len(store.events) != 0 {
		t.Fatalf("mixed valid/undeclared normalized batch published partial observations: %v, %#v", err, store.events)
	}
	host.result.Events = host.result.Events[:1]
	store.publishErr = errors.New("event inbox unavailable")
	if _, err := service.NormalizeExternalConversationGatewayIngress(ctx, gateway, request, host); !errors.Is(err, store.publishErr) {
		t.Fatalf("provider ingress acknowledged a persistence failure: %v", err)
	}
}

func TestVerifiedDirectEndpointPublishesAuthoritativeScopeAndBinding(t *testing.T) {
	ctx := context.Background()
	memory, catalog, endpoint := externalConversationDeliveryFixture(t, ctx, "slack")
	store := &ingressRunEventStore{MemoryStore: memory}
	service := NewExternalConversationTransportService(store, catalog)
	host := &externalConversationIngressHostStub{result: &ExternalConversationIngressHostResult{
		StatusCode: http.StatusOK,
		Events: []NormalizedExternalConversationEvent{{
			ID: "direct-ingress-one", Type: capability.ConversationEventMessageReceived,
			ExternalConversationID: endpoint.Address, ExternalMessageID: "message-one", ExternalParticipantID: "person-one",
			Text: "A verified endpoint reply", OrderingKey: "message-one", OccurredAt: time.Now().UTC(),
		}},
	}}
	result, err := service.NormalizeExternalConversationPublicIngress(ctx, ExternalConversationPublicIngressRequest{
		Route: endpoint.IngressRoute, Method: http.MethodPost, Body: []byte(`{}`),
	}, host)
	if err != nil || len(result.Received) != 1 {
		t.Fatalf("direct endpoint ingress = %#v, %v", result, err)
	}
	event := store.onlyEvent(t)
	if event.Scope != endpoint.Scope || event.Source != RunEventBindingSource(endpoint.DeploymentID, endpoint.Adapter.BindingID, endpoint.Adapter.AdapterID) || event.Subject != endpoint.Address {
		t.Fatalf("endpoint observation identity = %#v", event)
	}
}

func TestVerifiedCallbackWorkerPublishesBeforeConsumerAndRecoversFailure(t *testing.T) {
	ctx := context.Background()
	memory := NewMemoryStore()
	store := &ingressRunEventStore{MemoryStore: memory}
	catalog := newCallbackCatalog(t, ctx, memory)
	registration := createActiveCallbackRegistration(t, ctx, store, catalog)
	consumed := 0
	service := NewCallbackIngressService(store, catalog, map[string]CallbackEventConsumer{
		"approvals": CallbackEventConsumerFunc(func(_ context.Context, _ *CallbackRegistration, _ CallbackSubscription, event EventEnvelope) error {
			consumed++
			if len(store.events) != 1 || event.Source != "provider-normalized-source" {
				t.Fatalf("callback consumer ran before durable observation, or original event changed: %#v", event)
			}
			return nil
		}),
	})
	service.SetRunEventObserver(NewRunEventWaitService(store))
	service.SetDurableDispatcher(nil)
	now := time.Now().UTC()
	host := &callbackHostStub{result: &CallbackHostResult{
		StatusCode: http.StatusOK,
		Events: []NormalizedCallbackEvent{{
			ID: "provider-event-one", Type: "approval.decided", Source: "provider-normalized-source", Subject: "resource-one", OccurredAt: now,
			Attributes: map[string]interface{}{"bindingId": "forged-binding", "deploymentId": "forged-agent"},
		}},
	}}
	request := CallbackPublicRequest{Route: registration.IngressRoute, Method: http.MethodPost, Body: []byte("signed-provider-body")}
	store.publishErr = errors.New("durable observation temporarily unavailable")
	if _, err := service.Receive(ctx, request, host); !errors.Is(err, store.publishErr) || consumed != 0 {
		t.Fatalf("callback acknowledged observation persistence failure: %v", err)
	}
	store.publishErr = nil
	for attempt := 0; attempt < 2; attempt++ {
		if _, err := service.Receive(ctx, request, host); err != nil {
			t.Fatal(err)
		}
	}
	if consumed != 0 || len(store.events) != 1 {
		t.Fatal("callback acknowledgement did not preserve observation without consumer dispatch")
	}
	worker, err := NewCallbackEventWorker(store, service, CallbackEventWorkerConfig{WorkerID: "event-worker", BaseRetry: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	worker.now = func() time.Time { return now.Add(time.Second) }
	store.publishErr = errors.New("durable observation temporarily unavailable")
	failed, err := worker.ProcessOne(ctx, registration.Scope)
	if !errors.Is(err, store.publishErr) || failed == nil || failed.Status != CallbackEventPending || consumed != 0 {
		t.Fatalf("event publication failure did not preserve callback work: %#v, %v", failed, err)
	}
	store.publishErr = nil
	worker.now = func() time.Time { return now.Add(3 * time.Second) }
	applied, err := worker.ProcessOne(ctx, registration.Scope)
	if err != nil || applied == nil || applied.Status != CallbackEventApplied || consumed != 1 {
		t.Fatalf("callback recovery = %#v, %v, consumed=%d", applied, err, consumed)
	}
	event := store.onlyEvent(t)
	if event.Scope != registration.Scope || event.Source != RunEventBindingSource(registration.DeploymentID, registration.Adapter.BindingID, registration.Adapter.AdapterID) ||
		event.Subject != "resource-one" || event.Attributes["bindingId"] != registration.Adapter.BindingID ||
		event.Attributes["deploymentId"] != registration.DeploymentID {
		t.Fatalf("trusted callback identity = %#v", event)
	}
}

func TestVerifiedCallbackWithoutSubjectKeepsExistingConsumerContract(t *testing.T) {
	ctx := context.Background()
	memory := NewMemoryStore()
	store := &ingressRunEventStore{MemoryStore: memory}
	catalog := newCallbackCatalog(t, ctx, memory)
	registration := createActiveCallbackRegistration(t, ctx, store, catalog)
	consumed := 0
	service := NewCallbackIngressService(store, catalog, map[string]CallbackEventConsumer{
		"approvals": CallbackEventConsumerFunc(func(context.Context, *CallbackRegistration, CallbackSubscription, EventEnvelope) error {
			consumed++
			return nil
		}),
	})
	host := &callbackHostStub{result: &CallbackHostResult{StatusCode: http.StatusOK, Events: []NormalizedCallbackEvent{{
		ID: "subjectless-event", Type: "approval.decided", Source: "provider", OccurredAt: time.Now().UTC(),
	}}}}
	if _, err := service.Receive(ctx, CallbackPublicRequest{Route: registration.IngressRoute, Method: http.MethodPost, Body: []byte("signed")}, host); err != nil {
		t.Fatal(err)
	}
	if consumed != 1 || len(store.events) != 0 {
		t.Fatalf("subjectless callback invented a wait subject: consumed=%d observations=%#v", consumed, store.events)
	}
}

func TestVerifiedCallbackPrevalidatesEntireNormalizedBatch(t *testing.T) {
	ctx := t.Context()
	memory := NewMemoryStore()
	store := &ingressRunEventStore{MemoryStore: memory}
	catalog := newCallbackCatalog(t, ctx, memory)
	registration := createActiveCallbackRegistration(t, ctx, store, catalog)
	service := NewCallbackIngressService(store, catalog, nil)
	service.SetRunEventObserver(NewRunEventWaitService(store))
	service.SetDurableDispatcher(nil)
	now := time.Now().UTC()
	host := &callbackHostStub{result: &CallbackHostResult{StatusCode: http.StatusOK, Events: []NormalizedCallbackEvent{
		{ID: "valid-before-undeclared", Type: "approval.decided", Source: "provider", Subject: "resource-one", OccurredAt: now},
		{ID: "undeclared-after-valid", Type: "undeclared.provider.event", Source: "provider", Subject: "resource-one", OccurredAt: now},
	}}}
	if _, err := service.Receive(ctx, CallbackPublicRequest{Route: registration.IngressRoute, Method: http.MethodPost, Body: []byte("signed")}, host); !errors.Is(err, ErrCallbackRegistrationConflict) || len(store.events) != 0 {
		t.Fatalf("callback normalization error published partial provider data: %v, %#v", err, store.events)
	}
	if receipt, err := store.ClaimCallbackEvent(ctx, registration.Scope, "probe-worker", now.Add(time.Second), time.Minute); err != nil || receipt != nil {
		t.Fatalf("callback normalization error persisted partial dispatch work: %#v, %v", receipt, err)
	}
}

func TestUserAuthoredRoutingCannotPublishTrustedConnectorObservation(t *testing.T) {
	store := &ingressRunEventStore{MemoryStore: NewMemoryStore()}
	_, err := NewRunbookEventRouter(store).Route(context.Background(), EventEnvelope{
		ID: "forged-provider-event", Scope: Scope{Kind: "tenant", ID: "one"},
		Type: "conversation.message.received", Source: RunEventBindingSource("agent-one", "binding-one", "conversations"),
		Subject: "provider-conversation", OccurredAt: time.Now().UTC(),
		Attributes: map[string]interface{}{"bindingId": "binding-one", "provider": "slack"},
		Actor:      ActivityActor{Type: "user", ID: "ordinary-event-author"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(store.events) != 0 {
		t.Fatalf("user-selected connector source bypassed verified intake: %#v", store.events)
	}
}

func TestVerifiedCallbackRechecksBindingBeforeQueuedConsumer(t *testing.T) {
	ctx := context.Background()
	memory := NewMemoryStore()
	store := &ingressRunEventStore{MemoryStore: memory}
	catalog := newCallbackCatalog(t, ctx, memory)
	registration := createActiveCallbackRegistration(t, ctx, store, catalog)
	consumed := 0
	service := NewCallbackIngressService(store, catalog, map[string]CallbackEventConsumer{
		"approvals": CallbackEventConsumerFunc(func(context.Context, *CallbackRegistration, CallbackSubscription, EventEnvelope) error {
			consumed++
			return nil
		}),
	})
	service.SetRunEventObserver(NewRunEventWaitService(store))
	service.SetDurableDispatcher(nil)
	now := time.Now().UTC()
	host := &callbackHostStub{result: &CallbackHostResult{StatusCode: http.StatusOK, Events: []NormalizedCallbackEvent{{
		ID: "revoked-before-dispatch", Type: "approval.decided", Source: "provider", Subject: "resource-one", OccurredAt: now,
	}}}}
	if _, err := service.Receive(ctx, CallbackPublicRequest{Route: registration.IngressRoute, Method: http.MethodPost, Body: []byte("signed")}, host); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.DisableBinding(ctx, skill.DisableBindingRequest{
		Scope: skill.ScopeReference{Kind: registration.Scope.Kind, ID: registration.Scope.ID}, DeploymentID: registration.DeploymentID,
		BindingID: registration.Adapter.BindingID, ExpectedRevision: registration.Adapter.BindingRevision,
		Actor: skill.BindingActor{Type: "user", ID: "operator"}, Reason: "Revoke callback acquisition before the queued worker runs",
	}); err != nil {
		t.Fatal(err)
	}
	worker, err := NewCallbackEventWorker(store, service, CallbackEventWorkerConfig{WorkerID: "callback-worker"})
	if err != nil {
		t.Fatal(err)
	}
	worker.now = func() time.Time { return now.Add(time.Second) }
	if _, err := worker.ProcessOne(ctx, registration.Scope); !errors.Is(err, ErrCallbackRegistrationConflict) || consumed != 0 || len(store.events) != 1 {
		t.Fatalf("revoked callback binding consumed deferred provider data, or removed acquired observation: %v, consumed=%d observations=%#v", err, consumed, store.events)
	}
}

func TestVerifiedCallbackAcquisitionSurvivesDispatchDelayPastDeadline(t *testing.T) {
	eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
		ctx := t.Context()
		store := fixture.store.(interface {
			CallbackRegistrationStore
			CallbackEventStore
			CallbackRunEventIntakeStore
			skill.CatalogStore
		})
		catalog := skill.NewCatalogWithStore(store)
		registerCallbackFixture(t, ctx, catalog)
		registration := createActiveCallbackRegistration(t, ctx, store, catalog)
		now := eventWaitContractEpoch
		spec := RunEventWaitSpec{
			Key: "provider-response", Type: "approval.decided", Subject: "resource-one",
			Source: RunEventBindingSource(registration.DeploymentID, registration.Adapter.BindingID, registration.Adapter.AdapterID),
			After:  now.Add(-time.Minute), Deadline: now.Add(time.Second),
		}
		run := eventWaitContractCreate(t, fixture.store, registration.Scope, spec, spec.After)
		consumed := 0
		service := NewCallbackIngressService(store, catalog, map[string]CallbackEventConsumer{
			"approvals": CallbackEventConsumerFunc(func(context.Context, *CallbackRegistration, CallbackSubscription, EventEnvelope) error {
				consumed++
				return nil
			}),
		})
		service.now = func() time.Time { return now }
		service.SetDurableDispatcher(nil)
		host := &callbackHostStub{result: &CallbackHostResult{StatusCode: http.StatusOK, Events: []NormalizedCallbackEvent{{
			ID: "provider-before-deadline", Type: spec.Type, Source: "provider", Subject: spec.Subject, OccurredAt: now,
		}}}}
		request := CallbackPublicRequest{Route: registration.IngressRoute, Method: http.MethodPost, Body: []byte("signed")}
		if _, err := service.Receive(ctx, request, host); err != nil || consumed != 0 {
			t.Fatalf("durable acquisition before dispatch = consumed %d, %v", consumed, err)
		}
		if fixture.reopen != nil {
			fixture.store = fixture.reopen()
		}
		// No callback consumer has run. Simulate a process crash followed by
		// wait processing after the deadline; intake's original commit must
		// already contain the observation, rather than relying on replay.
		eventWaitContractProcess(t, fixture.store, registration.Scope, spec.Deadline.Add(time.Hour), 1)
		wait, err := fixture.store.GetRunEventWait(ctx, registration.Scope, run.ID, spec.Key)
		if err != nil || wait.Status != RunEventWaitMatched || wait.EventID != "provider-before-deadline" {
			t.Fatalf("pre-deadline callback lost across dispatch delay/restart: %#v, %v", wait, err)
		}
	})
}

func TestVerifiedCallbackRegistrationsShareStableObservationIdentity(t *testing.T) {
	store := &ingressRunEventStore{MemoryStore: NewMemoryStore()}
	registration := &CallbackRegistration{ID: "first-registration", Scope: Scope{Kind: "tenant", ID: "one"}, DeploymentID: "agent-one", Provider: "provider", Adapter: CallbackAdapterReference{BindingID: "binding-one", AdapterID: "events"}}
	event := EventEnvelope{ID: "provider-event", Scope: registration.Scope, Type: "provider.changed", Source: "provider", Subject: "resource-one", OccurredAt: time.Now().UTC()}
	observer := NewRunEventWaitService(store)
	if err := publishVerifiedCallbackEvent(t.Context(), observer, registration, event, event.OccurredAt); err != nil {
		t.Fatal(err)
	}
	registration.ID = "replacement-registration"
	if err := publishVerifiedCallbackEvent(t.Context(), observer, registration, event, event.OccurredAt.Add(time.Second)); err != nil {
		t.Fatalf("registration provenance changed immutable provider identity: %v", err)
	}
	_ = store.onlyEvent(t)
}

func TestVerifiedCallbackExactNumbersSurviveReplayRestartAndDispatch(t *testing.T) {
	eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
		ctx := t.Context()
		store := fixture.store.(interface {
			CallbackRegistrationStore
			CallbackEventStore
			CallbackRunEventIntakeStore
			skill.CatalogStore
		})
		catalog := skill.NewCatalogWithStore(store)
		registerCallbackFixture(t, ctx, catalog)
		registration := createActiveCallbackRegistration(t, ctx, store, catalog)
		now := eventWaitContractEpoch
		precise := json.Number("9007199254740993")
		spec := RunEventWaitSpec{
			Key: "precise-provider-response", Type: "approval.decided", Subject: "resource-one",
			Source:     RunEventBindingSource(registration.DeploymentID, registration.Adapter.BindingID, registration.Adapter.AdapterID),
			Attributes: map[string]interface{}{"large": precise}, After: now.Add(-time.Minute), Deadline: now.Add(time.Hour),
		}
		run := eventWaitContractCreate(t, fixture.store, registration.Scope, spec, spec.After)
		consumed := 0
		consumers := map[string]CallbackEventConsumer{
			"approvals": CallbackEventConsumerFunc(func(_ context.Context, _ *CallbackRegistration, _ CallbackSubscription, event EventEnvelope) error {
				consumed++
				if event.Attributes["large"] != precise || event.Payload["nested"].(map[string]interface{})["large"] != precise {
					t.Fatalf("callback dispatch rounded signed provider data: %#v", event)
				}
				if revision, ok := integerAttribute(event.Attributes["approvalRevision"]); !ok || revision != 12 {
					t.Fatalf("callback exact numbers broke approval integer parsing: %#v", event.Attributes)
				}
				return nil
			}),
		}
		service := NewCallbackIngressService(store, catalog, consumers)
		service.now = func() time.Time { return now }
		service.SetDurableDispatcher(nil)
		host := &callbackHostStub{result: &CallbackHostResult{StatusCode: http.StatusOK, Events: []NormalizedCallbackEvent{{
			ID: "precise-provider-event", Type: spec.Type, Source: "provider", Subject: spec.Subject, OccurredAt: now,
			Attributes: map[string]interface{}{"large": precise, "approvalRevision": json.Number("12")},
			Payload:    map[string]interface{}{"nested": map[string]interface{}{"large": precise}},
		}}}}
		request := CallbackPublicRequest{Route: registration.IngressRoute, Method: http.MethodPost, Body: []byte("signed")}
		initial, err := service.Receive(ctx, request, host)
		if err != nil || len(initial.Receipts) != 1 || initial.Receipts[0].Event.Attributes["large"] != precise {
			t.Fatalf("callback acquisition rounded provider number: %#v, %v", initial, err)
		}
		if fixture.reopen != nil {
			fixture.store = fixture.reopen()
		}
		store = fixture.store.(interface {
			CallbackRegistrationStore
			CallbackEventStore
			CallbackRunEventIntakeStore
			skill.CatalogStore
		})
		service = NewCallbackIngressService(store, skill.NewCatalogWithStore(store), consumers)
		service.now = func() time.Time { return now.Add(time.Second) }
		service.SetDurableDispatcher(nil)
		replayed, err := service.Receive(ctx, request, host)
		if err != nil || replayed.Replayed != 1 {
			t.Fatalf("exact provider data conflicted after callback restart/replay: %#v, %v", replayed, err)
		}
		worker, err := NewCallbackEventWorker(store, service, CallbackEventWorkerConfig{WorkerID: "precise-callback-worker"})
		if err != nil {
			t.Fatal(err)
		}
		worker.now = func() time.Time { return now.Add(2 * time.Second) }
		if applied, err := worker.ProcessOne(ctx, registration.Scope); err != nil || applied == nil || applied.Status != CallbackEventApplied || consumed != 1 {
			t.Fatalf("callback repair changed observation digest after exact reload: %#v, %v, consumed %d", applied, err, consumed)
		}
		eventWaitContractProcess(t, fixture.store, registration.Scope, now.Add(3*time.Second), 1)
		wait, err := fixture.store.GetRunEventWait(ctx, registration.Scope, run.ID, spec.Key)
		if err != nil || wait.Status != RunEventWaitMatched {
			t.Fatalf("exact callback selector failed after restart: %#v, %v", wait, err)
		}
	})
}

func TestCallbackExactIntegerAttributesRejectLossyConversions(t *testing.T) {
	for _, test := range []struct {
		input json.Number
		want  int64
		valid bool
	}{
		{json.Number("9007199254740993"), 9007199254740993, true},
		{json.Number("12.0"), 12, true},
		{json.Number("1.2e1"), 12, true},
		{json.Number("12.5"), 0, false},
		{json.Number("9223372036854775808"), 0, false},
	} {
		if got, ok := integerAttribute(test.input); got != test.want || ok != test.valid {
			t.Fatalf("integerAttribute(%q) = %d, %t; want %d, %t", test.input, got, ok, test.want, test.valid)
		}
	}
}
