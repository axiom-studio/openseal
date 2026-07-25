package runtime

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
)

type externalConversationIngressHostStub struct {
	request ExternalConversationIngressHostRequest
	result  *ExternalConversationIngressHostResult
}

type externalConversationGatewayHostStub struct {
	request ExternalConversationGatewayHostRequest
	result  *ExternalConversationGatewayHostResult
}

func (h *externalConversationGatewayHostStub) NormalizeExternalConversationGateway(
	_ context.Context,
	request ExternalConversationGatewayHostRequest,
) (*ExternalConversationGatewayHostResult, error) {
	h.request = request
	return h.result, nil
}

func (h *externalConversationIngressHostStub) NormalizeExternalConversation(
	_ context.Context,
	request ExternalConversationIngressHostRequest,
) (*ExternalConversationIngressHostResult, error) {
	h.request = request
	return h.result, nil
}

func TestExternalConversationIngressUsesExactSkillAdapterAndPersistsVerifiedEvents(t *testing.T) {
	ctx := context.Background()
	store, catalog, endpoint := externalConversationDeliveryFixture(t, ctx, "slack")
	now := time.Now().UTC()
	host := &externalConversationIngressHostStub{result: &ExternalConversationIngressHostResult{
		StatusCode: http.StatusOK, ContentType: "application/json", Body: []byte(`{"ok":true}`),
		Events: []NormalizedExternalConversationEvent{{
			ID: "Ev-ingress", Type: capability.ConversationEventMessageReceived,
			ExternalConversationID: "C123", ExternalMessageID: "171.001",
			ExternalParticipantID: "U123", Text: "Hello", OrderingKey: "C123:171.001", OccurredAt: now,
		}},
	}}
	service := NewExternalConversationTransportService(store, catalog)
	request := ExternalConversationIngressRequest{
		Scope: endpoint.Scope, EndpointID: endpoint.ID, Method: http.MethodPost,
		Headers: map[string][]string{
			"X-Slack-Signature": {"v0=signature"}, "X-Slack-Request-Timestamp": {"1710000000"},
		},
		Body: []byte(`{"type":"event_callback"}`),
	}

	first, err := service.NormalizeExternalConversationIngress(ctx, request, host)

	if err != nil || len(first.Received) != 1 || !first.Received[0].Accepted || first.Received[0].Replayed {
		t.Fatalf("first ingress = %#v, %v", first, err)
	}
	if host.request.Endpoint.ID != endpoint.ID || host.request.Adapter.Adapter.Provider != "slack" ||
		host.request.Adapter.Binding.ID != endpoint.Adapter.BindingID {
		t.Fatalf("exact ingress adapter = %#v", host.request)
	}
	replayed, err := service.NormalizeExternalConversationIngress(ctx, request, host)
	if err != nil || len(replayed.Received) != 1 || !replayed.Received[0].Replayed ||
		replayed.Received[0].Item.ID != first.Received[0].Item.ID {
		t.Fatalf("replayed ingress = %#v, %v", replayed, err)
	}
	items, err := store.ListExternalConversationInbox(ctx, ExternalConversationInboxFilter{
		Scope: endpoint.Scope, EndpointID: endpoint.ID, Limit: 10,
	})
	if err != nil || len(items) != 1 {
		t.Fatalf("durable inbox = %#v, %v", items, err)
	}
}

func TestExternalConversationIngressRejectsSensitiveForwardedHeaders(t *testing.T) {
	request := &ExternalConversationIngressRequest{
		Scope: Scope{Kind: "tenant", ID: "one"}, EndpointID: "endpoint", Method: http.MethodPost,
		Headers: map[string][]string{"Authorization": {"Bearer secret"}}, Body: []byte(`{}`),
	}
	if err := request.Validate(); err == nil {
		t.Fatal("provider ingress accepted a forwarded authorization header")
	}
}

func TestExternalConversationPublicIngressResolvesAuthoritativeScopeFromOpaqueRoute(t *testing.T) {
	ctx := context.Background()
	store, catalog, endpoint := externalConversationDeliveryFixture(t, ctx, "slack")
	host := &externalConversationIngressHostStub{result: &ExternalConversationIngressHostResult{
		StatusCode: http.StatusOK,
		Events: []NormalizedExternalConversationEvent{{
			ID: "Ev-public-route", Type: capability.ConversationEventMessageReceived,
			ExternalConversationID: "C123", ExternalMessageID: "171.002",
			ExternalParticipantID: "U123", Text: "Hello from the public route",
			OrderingKey: "C123:171.002", OccurredAt: time.Now().UTC(),
		}},
	}}
	service := NewExternalConversationTransportService(store, catalog)

	result, err := service.NormalizeExternalConversationPublicIngress(ctx, ExternalConversationPublicIngressRequest{
		Route: endpoint.IngressRoute, Method: http.MethodPost,
		Headers: map[string][]string{"X-Slack-Signature": {"v0=signature"}},
		Body:    []byte(`{"type":"event_callback"}`),
	}, host)

	if err != nil || len(result.Received) != 1 || host.request.Request.Scope != endpoint.Scope ||
		host.request.Request.EndpointID != endpoint.ID {
		t.Fatalf("public ingress = %#v, host = %#v, err = %v", result, host.request, err)
	}
	if _, err := service.NormalizeExternalConversationPublicIngress(ctx, ExternalConversationPublicIngressRequest{
		Route: "unknown-route", Method: http.MethodPost, Body: []byte(`{}`),
	}, host); !errors.Is(err, ErrExternalConversationEndpointNotFound) {
		t.Fatalf("unknown route error = %v", err)
	}
}

func TestExternalConversationGatewayRoutesOnlyVerifiedInstallationAndAddress(t *testing.T) {
	ctx := context.Background()
	store, catalog, endpoint := externalConversationDeliveryFixture(t, ctx, "slack")
	previousRevision := endpoint.Revision
	endpoint.InstallationID = "T123"
	endpoint.ApplicationID = "A123"
	endpoint.Address = "C123"
	endpoint.Revision++
	endpoint.UpdatedAt = endpoint.UpdatedAt.Add(time.Second)
	if err := store.UpdateExternalConversationEndpoint(ctx, endpoint, previousRevision); err != nil {
		t.Fatal(err)
	}
	event := NormalizedExternalConversationEvent{
		ID: "Ev-shared", Type: capability.ConversationEventMessageReceived,
		ExternalConversationID: "C123", ExternalMessageID: "171.003",
		ExternalParticipantID: "U123", Text: "Hello shared app",
		OrderingKey: "C123:171.003", OccurredAt: time.Now().UTC(),
	}
	host := &externalConversationGatewayHostStub{result: &ExternalConversationGatewayHostResult{
		StatusCode: http.StatusOK,
		Events: []ExternalConversationGatewayEvent{{
			InstallationID: "T123", ApplicationID: "A123", Address: "C123", Event: event,
		}},
	}}
	gateway := ExternalConversationIngressGateway{
		Scope: endpoint.Scope, DeploymentID: endpoint.DeploymentID,
		Adapter: endpoint.Adapter, Provider: endpoint.Provider,
	}
	service := NewExternalConversationTransportService(store, catalog)
	gateways := NewExternalConversationGatewayService(store)
	registration, err := gateways.Create(ctx, CreateExternalConversationGatewayRequest{
		ID: "shared-slack", Name: "Shared Slack events", Gateway: gateway,
		Status: ExternalConversationGatewayActive,
	})
	if err != nil {
		t.Fatal(err)
	}

	first, err := service.NormalizeExternalConversationRegisteredGatewayIngress(ctx, ExternalConversationPublicIngressRequest{
		Route: registration.IngressRoute, Method: http.MethodPost,
		Headers: map[string][]string{"X-Slack-Signature": {"v0=verified-by-skill"}},
		Body:    []byte(`{"team_id":"T123","event":{"channel":"C123"}}`),
	}, host)
	if err != nil || len(first.Received) != 1 || !first.Received[0].Accepted ||
		host.request.Adapter.Adapter.Provider != "slack" {
		t.Fatalf("shared ingress = %#v host=%#v err=%v", first, host.request, err)
	}
	replayed, err := service.NormalizeExternalConversationRegisteredGatewayIngress(ctx, ExternalConversationPublicIngressRequest{
		Route: registration.IngressRoute, Method: http.MethodPost,
		Headers: map[string][]string{"X-Slack-Signature": {"v0=verified-by-skill"}},
		Body:    []byte(`{"team_id":"T123","event":{"channel":"C123"}}`),
	}, host)
	if err != nil || len(replayed.Received) != 1 || !replayed.Received[0].Replayed {
		t.Fatalf("shared replay = %#v err=%v", replayed, err)
	}

	host.result.Events[0].ApplicationID = "wrong-app"
	unmatched, err := service.NormalizeExternalConversationRegisteredGatewayIngress(ctx, ExternalConversationPublicIngressRequest{
		Route: registration.IngressRoute, Method: http.MethodPost,
		Body: []byte(`{"team_id":"T123","event":{"channel":"C123"}}`),
	}, host)
	if err != nil || len(unmatched.Received) != 0 {
		t.Fatalf("wrong app shared ingress = %#v err=%v", unmatched, err)
	}

	paused := ExternalConversationGatewayPaused
	registration, err = gateways.Update(ctx, registration.Gateway.Scope, registration.ID, UpdateExternalConversationGatewayRequest{
		ExpectedRevision: registration.Revision, Status: &paused,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.NormalizeExternalConversationRegisteredGatewayIngress(ctx, ExternalConversationPublicIngressRequest{
		Route: registration.IngressRoute, Method: http.MethodPost, Body: []byte(`{}`),
	}, host); !errors.Is(err, ErrExternalConversationConflict) {
		t.Fatalf("paused gateway error = %v", err)
	}
}

func TestTenantConversationGatewayCannotRouteIntoAnotherTenant(t *testing.T) {
	ctx := context.Background()
	store, catalog, endpoint := externalConversationDeliveryFixture(t, ctx, "slack")
	previousRevision := endpoint.Revision
	endpoint.InstallationID = "T-shared"
	endpoint.ApplicationID = "A-shared"
	endpoint.Address = "C-shared"
	endpoint.Revision++
	endpoint.UpdatedAt = endpoint.UpdatedAt.Add(time.Second)
	if err := store.UpdateExternalConversationEndpoint(ctx, endpoint, previousRevision); err != nil {
		t.Fatal(err)
	}
	other := cloneExternalConversationEndpoint(endpoint)
	other.ID = "other-tenant-endpoint"
	other.IngressRoute = "other-tenant-route"
	other.Scope = Scope{Kind: "tenant", ID: "other"}
	other.CreatedAt = other.CreatedAt.Add(time.Second)
	other.UpdatedAt = other.CreatedAt
	other.Revision = 1
	if err := store.CreateExternalConversationEndpoint(ctx, other); err != nil {
		t.Fatal(err)
	}
	event := NormalizedExternalConversationEvent{
		ID: "Ev-tenant-boundary", Type: capability.ConversationEventMessageReceived,
		ExternalConversationID: "C-shared", ExternalMessageID: "171.004",
		ExternalParticipantID: "U123", Text: "Tenant boundary",
		OrderingKey: "C-shared:171.004", OccurredAt: time.Now().UTC(),
	}
	host := &externalConversationGatewayHostStub{result: &ExternalConversationGatewayHostResult{
		StatusCode: http.StatusOK,
		Events: []ExternalConversationGatewayEvent{{
			InstallationID: "T-shared", ApplicationID: "A-shared", Address: "C-shared", Event: event,
		}},
	}}
	registration, err := NewExternalConversationGatewayService(store).Create(
		ctx,
		CreateExternalConversationGatewayRequest{
			ID: "tenant-slack", Name: "Tenant Slack",
			Gateway: ExternalConversationIngressGateway{
				Scope: endpoint.Scope, DeploymentID: endpoint.DeploymentID,
				Adapter: endpoint.Adapter, Provider: endpoint.Provider,
			},
			Status: ExternalConversationGatewayActive,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := NewExternalConversationTransportService(store, catalog).
		NormalizeExternalConversationRegisteredGatewayIngress(ctx, ExternalConversationPublicIngressRequest{
			Route: registration.IngressRoute, Method: http.MethodPost, Body: []byte(`{}`),
		}, host)
	if err != nil || len(result.Received) != 1 || result.Received[0].Item.Scope != endpoint.Scope {
		t.Fatalf("tenant-bounded ingress = %#v, err = %v", result, err)
	}
	items, err := store.ListExternalConversationInbox(ctx, ExternalConversationInboxFilter{
		Scope: other.Scope, EndpointID: other.ID, Limit: 10,
	})
	if err != nil || len(items) != 0 {
		t.Fatalf("cross-tenant inbox = %#v, err = %v", items, err)
	}
}
