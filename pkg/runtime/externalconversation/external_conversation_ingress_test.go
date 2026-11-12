package externalconversation

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding/skillbindingtest"
	"github.com/axiom-studio/openseal/pkg/skill"
)

type externalConversationIngressHostStub struct {
	request IngressHostRequest
	result  *IngressHostResult
}

type externalConversationGatewayHostStub struct {
	request GatewayHostRequest
	result  *GatewayHostResult
}

func createActiveExternalConversationGateway(t *testing.T, ctx context.Context, service *GatewayService, request CreateExternalConversationGatewayRequest) *models.ExternalConversationGatewayRegistration {
	t.Helper()
	request.Status = models.ExternalConversationGatewayPaused
	created, err := service.Create(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	active := models.ExternalConversationGatewayActive
	activated, err := service.Update(ctx, request.Gateway.Scope, created.ID, UpdateExternalConversationGatewayRequest{
		ExpectedRevision: created.Revision, Status: &active,
		Actor: models.ActivityActor{Type: "test", ID: "operator"}, Reason: "activate reviewed provider route",
	})
	if err != nil {
		t.Fatal(err)
	}
	return activated
}

func (h *externalConversationGatewayHostStub) NormalizeExternalConversationGateway(
	_ context.Context,
	request GatewayHostRequest,
) (*GatewayHostResult, error) {
	h.request = request
	return h.result, nil
}

func (h *externalConversationIngressHostStub) NormalizeExternalConversation(
	_ context.Context,
	request IngressHostRequest,
) (*IngressHostResult, error) {
	h.request = request
	return h.result, nil
}

func TestExternalConversationIngressUsesExactSkillAdapterAndPersistsVerifiedEvents(t *testing.T) {
	ctx := context.Background()
	store, catalog, endpoint := endpointFixture(t, ctx, "slack")
	now := time.Now().UTC()
	host := &externalConversationIngressHostStub{result: &IngressHostResult{
		StatusCode: http.StatusOK, ContentType: "application/json", Body: []byte(`{"ok":true}`),
		Events: []models.NormalizedExternalConversationEvent{{
			ID: "Ev-ingress", Type: capability.ConversationEventMessageReceived,
			ExternalConversationID: "C123", ExternalMessageID: "171.001",
			ExternalParticipantID: "U123", Text: "Hello", OrderingKey: "C123:171.001", OccurredAt: now,
		}},
	}}
	service := NewTransportService(store, catalog)
	request := IngressRequest{
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
	items, err := store.ListExternalConversationInbox(ctx, models.ExternalConversationInboxFilter{
		Scope: endpoint.Scope, EndpointID: endpoint.ID, Limit: 10,
	})
	if err != nil || len(items) != 1 {
		t.Fatalf("durable inbox = %#v, %v", items, err)
	}
}

func TestExternalConversationIngressRejectsSensitiveForwardedHeaders(t *testing.T) {
	request := &IngressRequest{
		Scope: models.Scope{Kind: "tenant", ID: "one"}, EndpointID: "endpoint", Method: http.MethodPost,
		Headers: map[string][]string{"Authorization": {"Bearer secret"}}, Body: []byte(`{}`),
	}
	if err := request.Validate(); err == nil {
		t.Fatal("provider ingress accepted a forwarded authorization header")
	}
}

func TestExternalConversationPublicIngressResolvesAuthoritativeScopeFromOpaqueRoute(t *testing.T) {
	ctx := context.Background()
	store, catalog, endpoint := endpointFixture(t, ctx, "slack")
	host := &externalConversationIngressHostStub{result: &IngressHostResult{
		StatusCode: http.StatusOK,
		Events: []models.NormalizedExternalConversationEvent{{
			ID: "Ev-public-route", Type: capability.ConversationEventMessageReceived,
			ExternalConversationID: "C123", ExternalMessageID: "171.002",
			ExternalParticipantID: "U123", Text: "Hello from the public route",
			OrderingKey: "C123:171.002", OccurredAt: time.Now().UTC(),
		}},
	}}
	service := NewTransportService(store, catalog)

	result, err := service.NormalizeExternalConversationPublicIngress(ctx, PublicIngressRequest{
		Route: endpoint.IngressRoute, Method: http.MethodPost,
		Headers: map[string][]string{"X-Slack-Signature": {"v0=signature"}},
		Body:    []byte(`{"type":"event_callback"}`),
	}, host)

	if err != nil || len(result.Received) != 1 || host.request.Request.Scope != endpoint.Scope ||
		host.request.Request.EndpointID != endpoint.ID {
		t.Fatalf("public ingress = %#v, host = %#v, err = %v", result, host.request, err)
	}
	if _, err := service.NormalizeExternalConversationPublicIngress(ctx, PublicIngressRequest{
		Route: "unknown-route", Method: http.MethodPost, Body: []byte(`{}`),
	}, host); !errors.Is(err, models.ErrExternalConversationEndpointNotFound) {
		t.Fatalf("unknown route error = %v", err)
	}
}

func TestExternalConversationGatewayRoutesOnlyVerifiedInstallationAndAddress(t *testing.T) {
	ctx := context.Background()
	store, catalog, endpoint := endpointFixture(t, ctx, "slack")
	previousRevision := endpoint.Revision
	endpoint.InstallationID = "T123"
	endpoint.ApplicationID = "A123"
	endpoint.Address = "C123"
	endpoint.Revision++
	endpoint.UpdatedAt = endpoint.UpdatedAt.Add(time.Second)
	if err := store.UpdateExternalConversationEndpoint(ctx, endpoint, previousRevision); err != nil {
		t.Fatal(err)
	}
	event := models.NormalizedExternalConversationEvent{
		ID: "Ev-shared", Type: capability.ConversationEventMessageReceived,
		ExternalConversationID: "C123", ExternalMessageID: "171.003",
		ExternalParticipantID: "U123", Text: "Hello shared app",
		OrderingKey: "C123:171.003", OccurredAt: time.Now().UTC(),
	}
	host := &externalConversationGatewayHostStub{result: &GatewayHostResult{
		StatusCode: http.StatusOK,
		Events: []GatewayEvent{{
			InstallationID: "T123", ApplicationID: "A123", Address: "C123", Event: event,
		}},
	}}
	gateway := models.ExternalConversationIngressGateway{
		Scope: endpoint.Scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: endpoint.DeploymentID}, DeploymentID: endpoint.DeploymentID,
		Adapter: endpoint.Adapter, Provider: endpoint.Provider,
	}
	service := NewTransportService(store, catalog)
	gateways := NewGatewayService(store, nil)
	registration := createActiveExternalConversationGateway(t, ctx, gateways, CreateExternalConversationGatewayRequest{
		ID: "shared-slack", Name: "Shared Slack events", Gateway: gateway,
		Actor: models.ActivityActor{Type: "test", ID: "operator"}, Reason: "create verified route acceptance",
	})

	first, err := service.NormalizeExternalConversationRegisteredGatewayIngress(ctx, PublicIngressRequest{
		Route: registration.IngressRoute, Method: http.MethodPost,
		Headers: map[string][]string{"X-Slack-Signature": {"v0=verified-by-skill"}},
		Body:    []byte(`{"team_id":"T123","event":{"channel":"C123"}}`),
	}, host)
	if err != nil || len(first.Received) != 1 || !first.Received[0].Accepted ||
		host.request.Adapter.Adapter.Provider != "slack" {
		t.Fatalf("shared ingress = %#v host=%#v err=%v", first, host.request, err)
	}
	replayed, err := service.NormalizeExternalConversationRegisteredGatewayIngress(ctx, PublicIngressRequest{
		Route: registration.IngressRoute, Method: http.MethodPost,
		Headers: map[string][]string{"X-Slack-Signature": {"v0=verified-by-skill"}},
		Body:    []byte(`{"team_id":"T123","event":{"channel":"C123"}}`),
	}, host)
	if err != nil || len(replayed.Received) != 1 || !replayed.Received[0].Replayed {
		t.Fatalf("shared replay = %#v err=%v", replayed, err)
	}

	host.result.Events[0].ApplicationID = "wrong-app"
	unmatched, err := service.NormalizeExternalConversationRegisteredGatewayIngress(ctx, PublicIngressRequest{
		Route: registration.IngressRoute, Method: http.MethodPost,
		Body: []byte(`{"team_id":"T123","event":{"channel":"C123"}}`),
	}, host)
	if err != nil || len(unmatched.Received) != 0 {
		t.Fatalf("wrong app shared ingress = %#v err=%v", unmatched, err)
	}
	for _, mutation := range []struct {
		name         string
		installation string
		address      string
	}{
		{name: "wrong installation", installation: "wrong-team", address: "C123"},
		{name: "wrong address", installation: "T123", address: "wrong-channel"},
	} {
		host.result.Events[0].InstallationID = mutation.installation
		host.result.Events[0].ApplicationID = "A123"
		host.result.Events[0].Address = mutation.address
		unmatched, err = service.NormalizeExternalConversationRegisteredGatewayIngress(ctx, PublicIngressRequest{
			Route: registration.IngressRoute, Method: http.MethodPost, Body: []byte(`{}`),
		}, host)
		if err != nil || len(unmatched.Received) != 0 {
			t.Fatalf("%s shared ingress = %#v err=%v", mutation.name, unmatched, err)
		}
	}
	host.result.Events[0].InstallationID, host.result.Events[0].Address = "T123", "C123"
	previousRevision = endpoint.Revision
	endpoint.Status, endpoint.Revision = models.ExternalConversationEndpointPaused, endpoint.Revision+1
	endpoint.UpdatedAt = endpoint.UpdatedAt.Add(time.Second)
	if err := store.UpdateExternalConversationEndpoint(ctx, endpoint, previousRevision); err != nil {
		t.Fatal(err)
	}
	unmatched, err = service.NormalizeExternalConversationRegisteredGatewayIngress(ctx, PublicIngressRequest{
		Route: registration.IngressRoute, Method: http.MethodPost, Body: []byte(`{}`),
	}, host)
	if err != nil || len(unmatched.Received) != 0 {
		t.Fatalf("paused endpoint shared ingress = %#v err=%v", unmatched, err)
	}

	paused := models.ExternalConversationGatewayPaused
	registration, err = gateways.Update(ctx, registration.Gateway.Scope, registration.ID, UpdateExternalConversationGatewayRequest{
		ExpectedRevision: registration.Revision, Status: &paused,
		Actor: models.ActivityActor{Type: "test", ID: "operator"}, Reason: "pause ingress acceptance route",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.NormalizeExternalConversationRegisteredGatewayIngress(ctx, PublicIngressRequest{
		Route: registration.IngressRoute, Method: http.MethodPost, Body: []byte(`{}`),
	}, host); !errors.Is(err, models.ErrExternalConversationConflict) {
		t.Fatalf("paused gateway error = %v", err)
	}
}

func TestExternalConversationGatewayRoutesInstallationWideEndpointFromAnyAddress(t *testing.T) {
	ctx := context.Background()
	store, _, endpoint := endpointFixture(t, ctx, "slack")
	previousRevision := endpoint.Revision
	endpoint.InstallationID = "T123"
	endpoint.ApplicationID = "A123"
	endpoint.Address = "C-approvals"
	endpoint.InstallationWide = true
	endpoint.Revision++
	endpoint.UpdatedAt = endpoint.UpdatedAt.Add(time.Second)
	if err := store.UpdateExternalConversationEndpoint(ctx, endpoint, previousRevision); err != nil {
		t.Fatal(err)
	}

	for _, address := range []string{"C-one", "C-two", "D-direct"} {
		matches, err := store.ListExternalConversationEndpointsByVerifiedRoute(ctx, models.ExternalConversationVerifiedRoute{
			Provider: endpoint.Provider, InstallationID: "T123", ApplicationID: "A123", Address: address,
			SkillID: endpoint.Adapter.SkillID, SkillVersion: endpoint.Adapter.SkillVersion,
			SourceIdentity: endpoint.Adapter.SourceIdentity, AdapterID: endpoint.Adapter.AdapterID,
		})
		if err != nil || len(matches) != 1 || matches[0].ID != endpoint.ID {
			t.Fatalf("address %q matches = %#v, %v", address, matches, err)
		}
	}
	matches, err := store.ListExternalConversationEndpointsByVerifiedRoute(ctx, models.ExternalConversationVerifiedRoute{
		Provider: endpoint.Provider, InstallationID: "another-workspace", ApplicationID: "A123", Address: "C-one",
		SkillID: endpoint.Adapter.SkillID, SkillVersion: endpoint.Adapter.SkillVersion,
		SourceIdentity: endpoint.Adapter.SourceIdentity, AdapterID: endpoint.Adapter.AdapterID,
	})
	if err != nil || len(matches) != 0 {
		t.Fatalf("foreign installation matches = %#v, %v", matches, err)
	}
}

func TestTenantConversationGatewayCannotRouteIntoAnotherTenant(t *testing.T) {
	ctx := context.Background()
	store, catalog, endpoint := endpointFixture(t, ctx, "slack")
	previousRevision := endpoint.Revision
	endpoint.InstallationID = "T-shared"
	endpoint.ApplicationID = "A-shared"
	endpoint.Address = "C-shared"
	endpoint.Revision++
	endpoint.UpdatedAt = endpoint.UpdatedAt.Add(time.Second)
	if err := store.UpdateExternalConversationEndpoint(ctx, endpoint, previousRevision); err != nil {
		t.Fatal(err)
	}
	other := models.CloneExternalConversationEndpoint(endpoint)
	other.ID = "other-tenant-endpoint"
	other.IngressRoute = "other-tenant-route"
	other.Scope = models.Scope{Kind: "tenant", ID: "other"}
	other.CreatedAt = other.CreatedAt.Add(time.Second)
	other.UpdatedAt = other.CreatedAt
	other.Revision = 1
	if err := store.CreateExternalConversationEndpoint(ctx, other); err != nil {
		t.Fatal(err)
	}
	event := models.NormalizedExternalConversationEvent{
		ID: "Ev-tenant-boundary", Type: capability.ConversationEventMessageReceived,
		ExternalConversationID: "C-shared", ExternalMessageID: "171.004",
		ExternalParticipantID: "U123", Text: "Tenant boundary",
		OrderingKey: "C-shared:171.004", OccurredAt: time.Now().UTC(),
	}
	host := &externalConversationGatewayHostStub{result: &GatewayHostResult{
		StatusCode: http.StatusOK,
		Events: []GatewayEvent{{
			InstallationID: "T-shared", ApplicationID: "A-shared", Address: "C-shared", Event: event,
		}},
	}}
	registration := createActiveExternalConversationGateway(t, ctx, NewGatewayService(store, nil),
		CreateExternalConversationGatewayRequest{
			ID: "tenant-slack", Name: "Tenant Slack",
			Gateway: models.ExternalConversationIngressGateway{
				Scope: endpoint.Scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: endpoint.DeploymentID}, DeploymentID: endpoint.DeploymentID,
				Adapter: endpoint.Adapter, Provider: endpoint.Provider,
			},
			Actor: models.ActivityActor{Type: "test", ID: "operator"}, Reason: "create tenant isolation route",
		},
	)
	result, err := NewTransportService(store, catalog).
		NormalizeExternalConversationRegisteredGatewayIngress(ctx, PublicIngressRequest{
			Route: registration.IngressRoute, Method: http.MethodPost, Body: []byte(`{}`),
		}, host)
	if err != nil || len(result.Received) != 1 || result.Received[0].Item.Scope != endpoint.Scope {
		t.Fatalf("tenant-bounded ingress = %#v, err = %v", result, err)
	}
	items, err := store.ListExternalConversationInbox(ctx, models.ExternalConversationInboxFilter{
		Scope: other.Scope, EndpointID: other.ID, Limit: 10,
	})
	if err != nil || len(items) != 0 {
		t.Fatalf("cross-tenant inbox = %#v, err = %v", items, err)
	}
}

func TestPlatformConversationGatewayRoutesInstallationsAcrossTenantsAndRejectsCrossTenantAmbiguity(t *testing.T) {
	ctx := context.Background()
	store, catalog, first := endpointFixture(t, ctx, "slack")
	configureRoute := func(endpoint *models.ExternalConversationEndpoint, installation, application, address string) {
		t.Helper()
		previousRevision := endpoint.Revision
		endpoint.InstallationID, endpoint.ApplicationID, endpoint.Address = installation, application, address
		endpoint.Revision++
		endpoint.UpdatedAt = endpoint.UpdatedAt.Add(time.Second)
		if err := store.UpdateExternalConversationEndpoint(ctx, endpoint, previousRevision); err != nil {
			t.Fatal(err)
		}
	}
	configureRoute(first, "T-one", "A-shared", "C-support")

	secondScope := models.Scope{Kind: "tenant", ID: "second"}
	secondBinding := &skill.Binding{
		ID: "slack-second", Scope: skill.ScopeReference{Kind: secondScope.Kind, ID: secondScope.ID}, DeploymentID: "second-agent",
		SkillID: "slack", SkillVersion: "1.0.0", EnabledConversationAdapters: []string{"conversations"},
		MaximumRisk: skill.RiskLevelRead, Revision: 1,
		Credentials: map[string]skill.CredentialReference{
			"SLACK_CONNECTION": {Kind: "slack-oauth", ID: "connection://tenant/second/slack"},
		},
	}
	if err := catalog.Bind(ctx, secondBinding); err != nil {
		t.Fatal(err)
	}
	second, err := NewEndpointService(store, catalog).Create(ctx, CreateExternalConversationEndpointRequest{
		ID: "second-endpoint", Scope: secondScope,
		Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "second-agent"}, DeploymentID: "second-agent",
		Name: "Second support channel",
		Adapter: models.ExternalConversationAdapterReference{
			SkillID: "slack", SkillVersion: "1.0.0", BindingID: secondBinding.ID,
			BindingRevision: secondBinding.Revision, AdapterID: "conversations",
		},
		Mode: capability.ConversationEndpointChannel, Address: "C-support",
		Handler: models.ExternalConversationHandler{Kind: models.ExternalConversationHandlerAgent, ID: "second-agent"},
		Policy:  models.ExternalConversationPolicy{MessageSelection: models.ExternalConversationSelectAllMessages, ReplyMode: models.ExternalConversationReplyThread, IgnoreBots: true},
		Status:  models.ExternalConversationEndpointActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	configureRoute(second, "T-two", "A-shared", "C-support")

	platformScope := models.Scope{Kind: "platform", ID: "default"}
	platformBinding := &skill.Binding{
		ID: "slack-platform", Scope: skill.ScopeReference{Kind: platformScope.Kind, ID: platformScope.ID}, DeploymentID: "slack-platform-verifier",
		SkillID: "slack", SkillVersion: "1.0.0", EnabledConversationAdapters: []string{"conversations"},
		MaximumRisk: skill.RiskLevelRead, Revision: 1,
		Credentials: map[string]skill.CredentialReference{
			"SLACK_CONNECTION": {Kind: "slack-oauth", ID: "connection://platform/slack"},
		},
	}
	if err := catalog.Bind(ctx, platformBinding); err != nil {
		t.Fatal(err)
	}
	registration := createActiveExternalConversationGateway(t, ctx, NewGatewayService(store, catalog), CreateExternalConversationGatewayRequest{
		ID: "platform-slack", Name: "Platform Slack events",
		Gateway: models.ExternalConversationIngressGateway{
			Scope: platformScope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: platformBinding.DeploymentID}, DeploymentID: platformBinding.DeploymentID, Provider: "slack",
			Adapter: models.ExternalConversationAdapterReference{
				SkillID: "slack", SkillVersion: "1.0.0", BindingID: platformBinding.ID,
				BindingRevision: platformBinding.Revision, AdapterID: "conversations",
			},
		},
		Actor: models.ActivityActor{Type: "test", ID: "operator"}, Reason: "create platform routing acceptance",
	})
	host := &externalConversationGatewayHostStub{result: &GatewayHostResult{StatusCode: http.StatusOK}}
	service := NewTransportService(store, catalog)
	invoke := func(eventID, installation string) (*GatewayIngressResult, error) {
		host.result.Events = []GatewayEvent{{
			InstallationID: installation, ApplicationID: "A-shared", Address: "C-support",
			Event: models.NormalizedExternalConversationEvent{
				ID: eventID, Type: capability.ConversationEventMessageReceived,
				ExternalConversationID: "C-support", ExternalMessageID: eventID,
				ExternalParticipantID: "U-support", Text: "hello", OrderingKey: "C-support:" + eventID,
				OccurredAt: time.Now().UTC(),
			},
		}}
		return service.NormalizeExternalConversationRegisteredGatewayIngress(ctx, PublicIngressRequest{
			Route: registration.IngressRoute, Method: http.MethodPost, Body: []byte(`{"verified":true}`),
		}, host)
	}
	firstResult, err := invoke("event-one", "T-one")
	if err != nil || len(firstResult.Received) != 1 || firstResult.Received[0].Item.Scope != first.Scope {
		t.Fatalf("first tenant route = %#v, %v", firstResult, err)
	}
	secondResult, err := invoke("event-two", "T-two")
	if err != nil || len(secondResult.Received) != 1 || secondResult.Received[0].Item.Scope != second.Scope {
		t.Fatalf("second tenant route = %#v, %v", secondResult, err)
	}

	ambiguous := models.CloneExternalConversationEndpoint(first)
	ambiguous.ID, ambiguous.IngressRoute = "ambiguous-endpoint", "ambiguous-route"
	ambiguous.Scope = models.Scope{Kind: "tenant", ID: "ambiguous"}
	ambiguous.Owner.ID, ambiguous.DeploymentID = "ambiguous-agent", "ambiguous-agent"
	ambiguous.Handler.ID = "ambiguous-agent"
	ambiguous.Revision = 1
	ambiguous.CreatedAt, ambiguous.UpdatedAt = ambiguous.CreatedAt.Add(2*time.Second), ambiguous.CreatedAt.Add(2*time.Second)
	if err := store.CreateExternalConversationEndpoint(ctx, ambiguous); err != nil {
		t.Fatal(err)
	}
	if _, err := invoke("event-ambiguous", "T-one"); !errors.Is(err, models.ErrExternalConversationConflict) {
		t.Fatalf("cross-tenant ambiguous route error = %v", err)
	}
	items, err := store.ListExternalConversationInbox(ctx, models.ExternalConversationInboxFilter{
		Scope: first.Scope, EndpointID: first.ID, Limit: 10,
	})
	if err != nil || len(items) != 1 {
		t.Fatalf("ambiguous callback persisted partial inbox work: %#v, %v", items, err)
	}
}

func TestRegisteredConversationGatewayReturnsVerificationResponseBeforeEndpointsExist(t *testing.T) {
	ctx := context.Background()
	store := kerneltest.NewStore(t)
	catalog := skill.NewCatalog()
	definition := skillbindingtest.SlackConversationSkillDefinition()
	if err := catalog.Register(ctx, definition); err != nil {
		t.Fatal(err)
	}
	scope := models.Scope{Kind: "platform", ID: "default"}
	binding := &skill.Binding{
		ID: "verification-binding", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID},
		DeploymentID: "verification-host", SkillID: definition.ID, SkillVersion: definition.Version,
		EnabledConversationAdapters: []string{"conversations"}, MaximumRisk: skill.RiskLevelRead, Revision: 1,
		Credentials: map[string]skill.CredentialReference{
			"SLACK_CONNECTION": {Kind: "slack-oauth", ID: "connection://platform/slack"},
		},
	}
	if err := catalog.Bind(ctx, binding); err != nil {
		t.Fatal(err)
	}
	gateway := createActiveExternalConversationGateway(t, ctx, NewGatewayService(store, catalog), CreateExternalConversationGatewayRequest{
		ID: "verification-gateway", Name: "Verification gateway",
		Gateway: models.ExternalConversationIngressGateway{
			Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: binding.DeploymentID}, DeploymentID: binding.DeploymentID, Provider: "slack",
			Adapter: models.ExternalConversationAdapterReference{
				SkillID: definition.ID, SkillVersion: definition.Version, BindingID: binding.ID,
				BindingRevision: binding.Revision, AdapterID: "conversations",
			},
		},
		Actor: models.ActivityActor{Type: "test", ID: "operator"}, Reason: "create provider verification route",
	})
	host := &externalConversationGatewayHostStub{result: &GatewayHostResult{
		StatusCode: http.StatusOK, ContentType: "text/plain", Body: []byte("verify-me"),
	}}
	result, err := NewTransportService(store, catalog).
		NormalizeExternalConversationRegisteredGatewayIngress(ctx, PublicIngressRequest{
			Route: gateway.IngressRoute, Method: http.MethodPost, Body: []byte(`{"type":"url_verification"}`),
		}, host)
	if err != nil || string(result.Response.Body) != "verify-me" || len(result.Received) != 0 {
		t.Fatalf("endpoint-free verification response = %#v, %v", result, err)
	}
}

func TestConversationGatewayUsesCurrentReviewedBindingAfterSetupSave(t *testing.T) {
	ctx := t.Context()
	store, catalog, endpoint := endpointFixture(t, ctx, "slack")
	endpoint.InstallationID, endpoint.ApplicationID = "T123", ""
	endpoint.Revision++
	if err := store.UpdateExternalConversationEndpoint(ctx, endpoint, endpoint.Revision-1); err != nil {
		t.Fatal(err)
	}
	gateway := models.ExternalConversationIngressGateway{Scope: endpoint.Scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: endpoint.DeploymentID}, DeploymentID: endpoint.DeploymentID, Adapter: endpoint.Adapter, Provider: endpoint.Provider}
	registration := createActiveExternalConversationGateway(t, ctx, NewGatewayService(store, catalog), CreateExternalConversationGatewayRequest{ID: "saved-setup", Name: "Saved setup", Gateway: gateway, Actor: models.ActivityActor{Type: "test", ID: "owner"}, Reason: "Reviewed inbox"})
	bound, err := catalog.ResolveConversationAdapterBinding(ctx, skill.ScopeReference{Kind: endpoint.Scope.Kind, ID: endpoint.Scope.ID}, endpoint.DeploymentID, endpoint.Adapter.BindingID, endpoint.Adapter.AdapterID)
	if err != nil {
		t.Fatal(err)
	}
	binding := bound.Binding
	host := &externalConversationGatewayHostStub{result: &GatewayHostResult{StatusCode: http.StatusOK, Events: []GatewayEvent{{InstallationID: "T123", ApplicationID: "A123", Address: endpoint.Address, Event: models.NormalizedExternalConversationEvent{ID: "new-event", Type: capability.ConversationEventMessageReceived, ExternalConversationID: endpoint.Address, ExternalMessageID: "171.004", ExternalParticipantID: "U123", Text: "hello", OrderingKey: "channel:171.004", OccurredAt: time.Now().UTC()}}}}}
	service := NewTransportService(store, catalog)
	request := PublicIngressRequest{Route: registration.IngressRoute, Method: http.MethodPost, Body: []byte(`{}`)}
	for revision := int64(2); revision <= 4; revision++ {
		binding.Revision = revision
		host.result.Events[0].Event.ID = fmt.Sprintf("event-%d", revision)
		host.result.Events[0].Event.ExternalMessageID = fmt.Sprintf("171.%03d", revision)
		if err := catalog.Bind(ctx, binding); err != nil {
			t.Fatal(err)
		}
		result, err := service.NormalizeExternalConversationRegisteredGatewayIngress(ctx, request, host)
		if err != nil || len(result.Received) != 1 || host.request.Adapter.Binding.Revision != revision || host.request.Gateway.Adapter.BindingRevision != revision {
			t.Fatalf("setup revision %d disconnected ingress: %v %v", revision, result, err)
		}
		directHost := &externalConversationIngressHostStub{result: &IngressHostResult{StatusCode: http.StatusOK}}
		if _, err := service.NormalizeExternalConversationIngress(ctx, IngressRequest{Scope: endpoint.Scope, EndpointID: endpoint.ID, Method: http.MethodPost, Body: []byte(`{}`)}, directHost); err != nil || directHost.request.Endpoint.Adapter.BindingRevision != revision {
			t.Fatalf("direct ingress snapshot drifted: %v", err)
		}
		pending := &models.ExternalConversationDelivery{Scope: endpoint.Scope, EndpointID: endpoint.ID, EndpointRevision: endpoint.Revision, Adapter: endpoint.Adapter, Operation: capability.ConversationDeliveryMessageSend}
		worker := &DeliveryWorker{store: store, resolver: catalog}
		if _, resolved, err := worker.resolve(ctx, pending); err != nil || resolved.Binding.Revision != revision {
			t.Fatalf("queued reply failed after setup save: %v", err)
		}
		if revision > 2 {
			saved := host.result.Events[0].Event
			host.result.Events[0].Event.ID = fmt.Sprintf("event-%d", revision-1)
			host.result.Events[0].Event.ExternalMessageID = fmt.Sprintf("171.%03d", revision-1)
			replayed, err := service.NormalizeExternalConversationRegisteredGatewayIngress(ctx, request, host)
			if err != nil || len(replayed.Received) != 1 || !replayed.Received[0].Replayed || replayed.Received[0].Item.Adapter.BindingRevision != revision-1 {
				t.Fatalf("provider retry after setup save: %v %v", replayed, err)
			}
			host.result.Events[0].Event = saved
		}
	}
	binding.Disabled = true
	binding.Revision++
	if err := catalog.Bind(ctx, binding); err != nil {
		t.Fatal(err)
	}
	if _, err := service.NormalizeExternalConversationRegisteredGatewayIngress(ctx, request, host); !errors.Is(err, models.ErrExternalConversationConflict) {
		t.Fatalf("disabled binding accepted: %v", err)
	}
}
