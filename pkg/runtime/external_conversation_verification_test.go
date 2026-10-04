package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
)

type gatewayVerificationStore struct {
	*MemoryStore
	routeLookups, received, observations int
}

func (s *gatewayVerificationStore) ListExternalConversationEndpointsByVerifiedRoute(ctx context.Context, route ExternalConversationVerifiedRoute) ([]*ExternalConversationEndpoint, error) {
	s.routeLookups++
	return s.MemoryStore.ListExternalConversationEndpointsByVerifiedRoute(ctx, route)
}

func (s *gatewayVerificationStore) ReceiveExternalConversationEvent(ctx context.Context, item *ExternalConversationInboxItem) (*ExternalConversationInboxItem, bool, error) {
	s.received++
	return s.MemoryStore.ReceiveExternalConversationEvent(ctx, item)
}

func (s *gatewayVerificationStore) Publish(context.Context, EventEnvelope) (bool, error) {
	s.observations++
	return true, nil
}

type gatewayVerificationHost struct {
	request              ExternalConversationGatewayHostRequest
	result               *ExternalConversationGatewayHostResult
	normalized, verified int
}

func (h *gatewayVerificationHost) NormalizeExternalConversationGateway(_ context.Context, request ExternalConversationGatewayHostRequest) (*ExternalConversationGatewayHostResult, error) {
	h.normalized++
	h.request = request
	return h.result, nil
}

func (h *gatewayVerificationHost) VerifyExternalConversationGateway(_ context.Context, request ExternalConversationGatewayHostRequest) (*ExternalConversationGatewayHostResult, error) {
	h.verified++
	h.request = request
	return h.result, nil
}

func newGatewayVerificationFixture(t *testing.T) (*gatewayVerificationStore, *skill.Catalog, *ExternalConversationTransportService, *ExternalConversationGatewayRegistration, *ExternalConversationEndpoint) {
	t.Helper()
	base, catalog, endpoint := externalConversationDeliveryFixture(t, t.Context(), "slack")
	previousRevision := endpoint.Revision
	endpoint.InstallationID, endpoint.ApplicationID, endpoint.Address = "T123", "A123", "C123"
	endpoint.Revision++
	endpoint.UpdatedAt = endpoint.UpdatedAt.Add(time.Second)
	if err := base.UpdateExternalConversationEndpoint(t.Context(), endpoint, previousRevision); err != nil {
		t.Fatal(err)
	}
	store := &gatewayVerificationStore{MemoryStore: base}
	registration, err := NewExternalConversationGatewayService(store, catalog).Create(t.Context(), CreateExternalConversationGatewayRequest{
		ID: "verification-gateway", Name: "Paused setup verification",
		Gateway: ExternalConversationIngressGateway{Scope: endpoint.Scope, DeploymentID: endpoint.DeploymentID, Adapter: endpoint.Adapter, Provider: endpoint.Provider, InstallationID: "T123", ApplicationID: "A123"},
		Actor:   ActivityActor{Type: "user", ID: "operator"}, Reason: "Prepare provider setup",
	})
	if err != nil {
		t.Fatal(err)
	}
	service := NewExternalConversationTransportService(store, catalog)
	service.SetRunEventObserver(store)
	return store, catalog, service, registration, endpoint
}

func gatewayVerificationMessage() ExternalConversationGatewayEvent {
	return ExternalConversationGatewayEvent{
		InstallationID: "T123", ApplicationID: "A123", Address: "C123",
		Event: NormalizedExternalConversationEvent{
			ID: "Ev-verification", Type: capability.ConversationEventMessageReceived,
			ExternalConversationID: "C123", ExternalMessageID: "171.003", ExternalParticipantID: "U123",
			Text: "Ordinary message", OrderingKey: "C123:171.003", OccurredAt: time.Now().UTC(),
		},
	}
}

func TestPausedGatewayVerificationRequiresOptInAndNeverAcceptsEvents(t *testing.T) {
	for name, result := range map[string]*ExternalConversationGatewayHostResult{
		"signed challenge":  {StatusCode: http.StatusOK, ContentType: "application/json", Body: []byte(`{"challenge":"challenge-value"}`)},
		"invalid signature": {StatusCode: http.StatusUnauthorized, Body: []byte(`{"error":"invalid_signature"}`)},
		"malicious event":   {StatusCode: http.StatusOK, Events: []ExternalConversationGatewayEvent{gatewayVerificationMessage()}},
		"invalid result":    {StatusCode: 999},
	} {
		t.Run(name, func(t *testing.T) {
			store, _, service, registration, _ := newGatewayVerificationFixture(t)
			host := &gatewayVerificationHost{result: result}
			response, err := service.NormalizeExternalConversationRegisteredGatewayIngress(t.Context(), ExternalConversationPublicIngressRequest{
				Route: registration.IngressRoute, Method: http.MethodPost, Headers: map[string][]string{"X-Slack-Signature": {"v0=provider-verifier-input"}}, Body: []byte(`{"type":"url_verification"}`),
			}, host)
			if name == "malicious event" {
				if !errors.Is(err, ErrExternalConversationConflict) || response != nil {
					t.Fatalf("verification accepted event: %#v, %v", response, err)
				}
			} else if name == "invalid result" {
				if !errors.Is(err, ErrInvalidExternalConversation) || response != nil {
					t.Fatalf("verification accepted malformed response: %#v, %v", response, err)
				}
			} else if err != nil || response == nil || response.Response != result || response.Received != nil {
				t.Fatalf("verification response = %#v, %v", response, err)
			}
			if host.verified != 1 || host.normalized != 0 || !host.request.VerificationOnly || host.request.Gateway != registration.Gateway ||
				host.request.Adapter.Binding.ID != registration.Gateway.Adapter.BindingID || host.request.Adapter.Binding.Revision != registration.Gateway.Adapter.BindingRevision {
				t.Fatalf("verification lost explicit host authority or exact binding: %#v", host)
			}
			if store.routeLookups != 0 || store.received != 0 || store.observations != 0 {
				t.Fatalf("paused verifier caused intake effects: lookups %d writes %d observations %d", store.routeLookups, store.received, store.observations)
			}
			current, err := store.GetExternalConversationGateway(t.Context(), registration.Gateway.Scope, registration.ID)
			if err != nil || !reflect.DeepEqual(current, registration) {
				t.Fatalf("verification changed gateway lifecycle: %#v, %v", current, err)
			}
		})
	}
	t.Run("legacy host never invoked", func(t *testing.T) {
		_, _, service, registration, _ := newGatewayVerificationFixture(t)
		host := &externalConversationGatewayHostStub{result: &ExternalConversationGatewayHostResult{StatusCode: http.StatusOK}}
		_, err := service.NormalizeExternalConversationRegisteredGatewayIngress(t.Context(), ExternalConversationPublicIngressRequest{Route: registration.IngressRoute, Method: http.MethodPost, Body: []byte(`{}`)}, host)
		if !errors.Is(err, ErrExternalConversationConflict) || host.request.Request != nil {
			t.Fatalf("paused route invoked non-opt-in host: %#v, %v", host.request, err)
		}
	})
}

func TestGatewayVerificationPreservesActiveIngressAndRejectsUnavailableAuthority(t *testing.T) {
	for _, state := range []string{"active", "retired", "missing", "disabled binding"} {
		t.Run(state, func(t *testing.T) {
			store, catalog, service, registration, endpoint := newGatewayVerificationFixture(t)
			if state == "active" || state == "retired" {
				status := ExternalConversationGatewayActive
				if state == "retired" {
					status = ExternalConversationGatewayRetired
				}
				var err error
				registration, err = NewExternalConversationGatewayService(store, catalog).Update(t.Context(), registration.Gateway.Scope, registration.ID, UpdateExternalConversationGatewayRequest{
					ExpectedRevision: registration.Revision, Status: &status, Actor: ActivityActor{Type: "user", ID: "operator"}, Reason: "Change gateway state",
				})
				if err != nil {
					t.Fatal(err)
				}
			} else if state == "disabled binding" {
				if _, err := catalog.DisableBinding(t.Context(), skill.DisableBindingRequest{
					Scope: skill.ScopeReference(endpoint.Scope), DeploymentID: endpoint.DeploymentID, BindingID: endpoint.Adapter.BindingID, ExpectedRevision: endpoint.Adapter.BindingRevision,
					Actor: skill.BindingActor{Type: "user", ID: "operator"}, Reason: "Revoke provider connection",
				}); err != nil {
					t.Fatal(err)
				}
			}
			route := registration.IngressRoute
			if state == "missing" {
				route = "unknown-route"
			}
			host := &gatewayVerificationHost{result: &ExternalConversationGatewayHostResult{StatusCode: http.StatusOK, Events: []ExternalConversationGatewayEvent{gatewayVerificationMessage()}}}
			response, err := service.NormalizeExternalConversationRegisteredGatewayIngress(t.Context(), ExternalConversationPublicIngressRequest{Route: route, Method: http.MethodPost, Body: []byte(`{"type":"event_callback"}`)}, host)
			if state == "active" {
				if err != nil || response == nil || len(response.Received) != 1 || !response.Received[0].Accepted || host.verified != 0 || host.normalized != 1 || host.request.VerificationOnly || store.routeLookups != 1 || store.received != 1 || store.observations != 1 {
					t.Fatalf("normal active intake changed: %#v, %v; host %#v; reads %d writes %d observations %d", response, err, host, store.routeLookups, store.received, store.observations)
				}
			} else if err == nil || response != nil || host.normalized != 0 || host.verified != 0 || store.routeLookups != 0 || store.received != 0 || store.observations != 0 {
				t.Fatalf("unavailable authority reached verifier or intake: %#v, %v; host %#v", response, err, host)
			}
		})
	}
}

func TestGatewayVerificationAuthorityIsNeverJSONInput(t *testing.T) {
	encoded, err := json.Marshal(ExternalConversationGatewayHostRequest{VerificationOnly: true})
	if err != nil || strings.Contains(string(encoded), "verification") || strings.Contains(string(encoded), "Verification") {
		t.Fatalf("internal authority serialized: %s, %v", encoded, err)
	}
	var request ExternalConversationGatewayHostRequest
	if err := json.Unmarshal([]byte(`{"verificationOnly":true,"VerificationOnly":true}`), &request); err != nil || request.VerificationOnly {
		t.Fatalf("JSON supplied verification authority: %#v, %v", request, err)
	}
}
