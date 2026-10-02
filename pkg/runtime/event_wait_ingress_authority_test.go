package runtime

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
)

type replacingConversationAuthorityResolver struct {
	ExternalConversationAdapterResolver
	replace func(*skill.Binding)
}

func (r replacingConversationAuthorityResolver) ResolveConversationAdapterBinding(ctx context.Context, scope skill.ScopeReference, deploymentID, bindingID, adapterID string) (*skill.BoundConversationAdapter, error) {
	adapter, err := r.ExternalConversationAdapterResolver.ResolveConversationAdapterBinding(ctx, scope, deploymentID, bindingID, adapterID)
	if err != nil || adapter == nil || adapter.Binding == nil {
		return adapter, err
	}
	copy, binding := *adapter, *adapter.Binding
	r.replace(&binding)
	copy.Binding = &binding
	return &copy, nil
}

type replacingCallbackAuthorityResolver struct {
	CallbackAdapterResolver
	replace func(*skill.Binding)
}

func (r replacingCallbackAuthorityResolver) ResolveCallbackAdapterBinding(ctx context.Context, scope skill.ScopeReference, deploymentID, bindingID, adapterID string) (*skill.BoundCallbackAdapter, error) {
	adapter, err := r.CallbackAdapterResolver.ResolveCallbackAdapterBinding(ctx, scope, deploymentID, bindingID, adapterID)
	if err != nil || adapter == nil || adapter.Binding == nil {
		return adapter, err
	}
	copy, binding := *adapter, *adapter.Binding
	r.replace(&binding)
	copy.Binding = &binding
	return &copy, nil
}

func TestVerifiedIngressRejectsReplacementSkillOrPublisher(t *testing.T) {
	for _, replacement := range []struct {
		name   string
		change func(*skill.Binding)
	}{
		{"different_skill", func(binding *skill.Binding) { binding.SkillID = "replacement-skill" }},
		{"different_publisher", func(binding *skill.Binding) { binding.SourceIdentity = "replacement-publisher" }},
	} {
		t.Run(replacement.name, func(t *testing.T) {
			ctx := t.Context()
			memory, catalog, endpoint := externalConversationDeliveryFixture(t, ctx, "slack")
			store := &ingressRunEventStore{MemoryStore: memory}
			resolver := replacingConversationAuthorityResolver{ExternalConversationAdapterResolver: catalog, replace: replacement.change}
			service := NewExternalConversationTransportService(store, resolver)
			event := NormalizedExternalConversationEvent{
				ID: "foreign-authority-event", Type: capability.ConversationEventMessageReceived,
				ExternalConversationID: endpoint.Address, ExternalMessageID: "message-one", ExternalParticipantID: "person-one",
				Text: "A provider reply", OrderingKey: "message-one", OccurredAt: time.Now().UTC(),
			}
			request := ExternalConversationPublicIngressRequest{Route: endpoint.IngressRoute, Method: http.MethodPost, Body: []byte("signed")}
			directHost := &externalConversationIngressHostStub{result: &ExternalConversationIngressHostResult{StatusCode: http.StatusOK, Events: []NormalizedExternalConversationEvent{event}}}
			if _, err := service.NormalizeExternalConversationPublicIngress(ctx, request, directHost); !errors.Is(err, ErrExternalConversationConflict) {
				t.Fatalf("direct endpoint accepted replacement authority: %v", err)
			}
			gateway := ExternalConversationIngressGateway{Scope: endpoint.Scope, DeploymentID: endpoint.DeploymentID, Adapter: endpoint.Adapter, Provider: endpoint.Provider, InstallationID: "reviewed-installation"}
			gatewayHost := &externalConversationGatewayHostStub{result: &ExternalConversationGatewayHostResult{StatusCode: http.StatusOK, Events: []ExternalConversationGatewayEvent{{InstallationID: gateway.InstallationID, Address: endpoint.Address, Event: event}}}}
			if _, err := service.NormalizeExternalConversationGatewayIngress(ctx, gateway, request, gatewayHost); !errors.Is(err, ErrExternalConversationConflict) {
				t.Fatalf("gateway accepted replacement authority: %v", err)
			}
			if _, err := service.Receive(ctx, ReceiveExternalConversationEventRequest{Scope: endpoint.Scope, EndpointID: endpoint.ID, Event: event}); !errors.Is(err, ErrExternalConversationConflict) {
				t.Fatalf("endpoint receive accepted replacement authority: %v", err)
			}
			if len(store.events) != 0 {
				t.Fatal("replacement publisher acquired observations under previous authority")
			}

			callbackMemory := NewMemoryStore()
			callbackStore := &ingressRunEventStore{MemoryStore: callbackMemory}
			callbackCatalog := newCallbackCatalog(t, ctx, callbackMemory)
			registration := createActiveCallbackRegistration(t, ctx, callbackStore, callbackCatalog)
			callbackResolver := replacingCallbackAuthorityResolver{CallbackAdapterResolver: callbackCatalog, replace: replacement.change}
			callback := NewCallbackIngressService(callbackStore, callbackResolver, nil)
			callback.SetRunEventObserver(NewRunEventWaitService(callbackStore))
			host := &callbackHostStub{result: &CallbackHostResult{StatusCode: http.StatusOK, Events: []NormalizedCallbackEvent{{ID: "callback-replacement-event", Type: "approval.decided", Source: "provider", Subject: "resource-one", OccurredAt: event.OccurredAt}}}}
			if _, err := callback.Receive(ctx, CallbackPublicRequest{Route: registration.IngressRoute, Method: http.MethodPost, Body: []byte("signed")}, host); !errors.Is(err, ErrCallbackRegistrationConflict) || len(callbackStore.events) != 0 {
				t.Fatalf("callback intake accepted replacement authority: %v, %#v", err, callbackStore.events)
			}
		})
	}
}

func TestVerifiedCallbackRejectsReplacementAuthorityBeforeQueuedConsumer(t *testing.T) {
	ctx := t.Context()
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
	host := &callbackHostStub{result: &CallbackHostResult{StatusCode: http.StatusOK, Events: []NormalizedCallbackEvent{{ID: "replace-after-receipt", Type: "approval.decided", Source: "provider", Subject: "resource-one", OccurredAt: now}}}}
	if _, err := service.Receive(ctx, CallbackPublicRequest{Route: registration.IngressRoute, Method: http.MethodPost, Body: []byte("signed")}, host); err != nil {
		t.Fatal(err)
	}
	service.resolver = replacingCallbackAuthorityResolver{CallbackAdapterResolver: catalog, replace: func(binding *skill.Binding) { binding.SourceIdentity = "replacement-publisher" }}
	worker, err := NewCallbackEventWorker(store, service, CallbackEventWorkerConfig{WorkerID: "callback-worker"})
	if err != nil {
		t.Fatal(err)
	}
	worker.now = func() time.Time { return now.Add(time.Second) }
	if _, err := worker.ProcessOne(ctx, registration.Scope); !errors.Is(err, ErrCallbackRegistrationConflict) || consumed != 0 || len(store.events) != 1 {
		t.Fatalf("queued callback consumed under replacement publisher: %v, consumed %d, observations %#v", err, consumed, store.events)
	}
}
