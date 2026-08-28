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

type externalConversationIngressHostStub struct {
	request ExternalConversationIngressHostRequest
	result  *ExternalConversationIngressHostResult
}

type externalConversationGatewayHostStub struct {
	request ExternalConversationGatewayHostRequest
	result  *ExternalConversationGatewayHostResult
}

func createActiveExternalConversationGateway(t *testing.T, ctx context.Context, service *ExternalConversationGatewayService, request CreateExternalConversationGatewayRequest) *ExternalConversationGatewayRegistration {
	t.Helper()
	request.Status = ExternalConversationGatewayPaused
	created, err := service.Create(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	active := ExternalConversationGatewayActive
	activated, err := service.Update(ctx, request.Gateway.Scope, created.ID, UpdateExternalConversationGatewayRequest{
		ExpectedRevision: created.Revision, Status: &active,
		Actor: ActivityActor{Type: "test", ID: "operator"}, Reason: "activate reviewed provider route",
	})
	if err != nil {
		t.Fatal(err)
	}
	return activated
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
	registration := createActiveExternalConversationGateway(t, ctx, gateways, CreateExternalConversationGatewayRequest{
		ID: "shared-slack", Name: "Shared Slack events", Gateway: gateway,
		Actor: ActivityActor{Type: "test", ID: "operator"}, Reason: "create verified route acceptance",
	})

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
		unmatched, err = service.NormalizeExternalConversationRegisteredGatewayIngress(ctx, ExternalConversationPublicIngressRequest{
			Route: registration.IngressRoute, Method: http.MethodPost, Body: []byte(`{}`),
		}, host)
		if err != nil || len(unmatched.Received) != 0 {
			t.Fatalf("%s shared ingress = %#v err=%v", mutation.name, unmatched, err)
		}
	}
	host.result.Events[0].InstallationID, host.result.Events[0].Address = "T123", "C123"
	previousRevision = endpoint.Revision
	endpoint.Status, endpoint.Revision = ExternalConversationEndpointPaused, endpoint.Revision+1
	endpoint.UpdatedAt = endpoint.UpdatedAt.Add(time.Second)
	if err := store.UpdateExternalConversationEndpoint(ctx, endpoint, previousRevision); err != nil {
		t.Fatal(err)
	}
	unmatched, err = service.NormalizeExternalConversationRegisteredGatewayIngress(ctx, ExternalConversationPublicIngressRequest{
		Route: registration.IngressRoute, Method: http.MethodPost, Body: []byte(`{}`),
	}, host)
	if err != nil || len(unmatched.Received) != 0 {
		t.Fatalf("paused endpoint shared ingress = %#v err=%v", unmatched, err)
	}

	paused := ExternalConversationGatewayPaused
	registration, err = gateways.Update(ctx, registration.Gateway.Scope, registration.ID, UpdateExternalConversationGatewayRequest{
		ExpectedRevision: registration.Revision, Status: &paused,
		Actor: ActivityActor{Type: "test", ID: "operator"}, Reason: "pause ingress acceptance route",
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

func TestExternalConversationGatewayRoutesInstallationWideEndpointFromAnyAddress(t *testing.T) {
	ctx := context.Background()
	store, _, endpoint := externalConversationDeliveryFixture(t, ctx, "slack")
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
		matches, err := store.ListExternalConversationEndpointsByVerifiedRoute(ctx, ExternalConversationVerifiedRoute{
			Provider: endpoint.Provider, InstallationID: "T123", ApplicationID: "A123", Address: address,
			SkillID: endpoint.Adapter.SkillID, SkillVersion: endpoint.Adapter.SkillVersion,
			SourceIdentity: endpoint.Adapter.SourceIdentity, AdapterID: endpoint.Adapter.AdapterID,
		})
		if err != nil || len(matches) != 1 || matches[0].ID != endpoint.ID {
			t.Fatalf("address %q matches = %#v, %v", address, matches, err)
		}
	}
	matches, err := store.ListExternalConversationEndpointsByVerifiedRoute(ctx, ExternalConversationVerifiedRoute{
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
	registration := createActiveExternalConversationGateway(t, ctx, NewExternalConversationGatewayService(store),
		CreateExternalConversationGatewayRequest{
			ID: "tenant-slack", Name: "Tenant Slack",
			Gateway: ExternalConversationIngressGateway{
				Scope: endpoint.Scope, DeploymentID: endpoint.DeploymentID,
				Adapter: endpoint.Adapter, Provider: endpoint.Provider,
			},
			Actor: ActivityActor{Type: "test", ID: "operator"}, Reason: "create tenant isolation route",
		},
	)
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

func TestPlatformConversationGatewayRoutesInstallationsAcrossTenantsAndRejectsCrossTenantAmbiguity(t *testing.T) {
	ctx := context.Background()
	store, catalog, first := externalConversationDeliveryFixture(t, ctx, "slack")
	configureRoute := func(endpoint *ExternalConversationEndpoint, installation, application, address string) {
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

	secondScope := Scope{Kind: "tenant", ID: "second"}
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
	second, err := NewExternalConversationEndpointService(store, catalog).Create(ctx, CreateExternalConversationEndpointRequest{
		ID: "second-endpoint", Scope: secondScope,
		Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "second-agent"}, DeploymentID: "second-agent",
		Name: "Second support channel",
		Adapter: ExternalConversationAdapterReference{
			SkillID: "slack", SkillVersion: "1.0.0", BindingID: secondBinding.ID,
			BindingRevision: secondBinding.Revision, AdapterID: "conversations",
		},
		Mode: capability.ConversationEndpointChannel, Address: "C-support",
		Handler: ExternalConversationHandler{Kind: ExternalConversationHandlerAgent, ID: "second-agent"},
		Policy:  ExternalConversationPolicy{MessageSelection: ExternalConversationSelectAllMessages, ReplyMode: ExternalConversationReplyThread, IgnoreBots: true},
		Status:  ExternalConversationEndpointActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	configureRoute(second, "T-two", "A-shared", "C-support")

	platformScope := Scope{Kind: "platform", ID: "default"}
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
	registration := createActiveExternalConversationGateway(t, ctx, NewExternalConversationGatewayService(store, catalog), CreateExternalConversationGatewayRequest{
		ID: "platform-slack", Name: "Platform Slack events",
		Gateway: ExternalConversationIngressGateway{
			Scope: platformScope, DeploymentID: platformBinding.DeploymentID, Provider: "slack",
			Adapter: ExternalConversationAdapterReference{
				SkillID: "slack", SkillVersion: "1.0.0", BindingID: platformBinding.ID,
				BindingRevision: platformBinding.Revision, AdapterID: "conversations",
			},
		},
		Actor: ActivityActor{Type: "test", ID: "operator"}, Reason: "create platform routing acceptance",
	})
	host := &externalConversationGatewayHostStub{result: &ExternalConversationGatewayHostResult{StatusCode: http.StatusOK}}
	service := NewExternalConversationTransportService(store, catalog)
	invoke := func(eventID, installation string) (*ExternalConversationGatewayIngressResult, error) {
		host.result.Events = []ExternalConversationGatewayEvent{{
			InstallationID: installation, ApplicationID: "A-shared", Address: "C-support",
			Event: NormalizedExternalConversationEvent{
				ID: eventID, Type: capability.ConversationEventMessageReceived,
				ExternalConversationID: "C-support", ExternalMessageID: eventID,
				ExternalParticipantID: "U-support", Text: "hello", OrderingKey: "C-support:" + eventID,
				OccurredAt: time.Now().UTC(),
			},
		}}
		return service.NormalizeExternalConversationRegisteredGatewayIngress(ctx, ExternalConversationPublicIngressRequest{
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

	ambiguous := cloneExternalConversationEndpoint(first)
	ambiguous.ID, ambiguous.IngressRoute = "ambiguous-endpoint", "ambiguous-route"
	ambiguous.Scope = Scope{Kind: "tenant", ID: "ambiguous"}
	ambiguous.Owner.ID, ambiguous.DeploymentID = "ambiguous-agent", "ambiguous-agent"
	ambiguous.Handler.ID = "ambiguous-agent"
	ambiguous.Revision = 1
	ambiguous.CreatedAt, ambiguous.UpdatedAt = ambiguous.CreatedAt.Add(2*time.Second), ambiguous.CreatedAt.Add(2*time.Second)
	if err := store.CreateExternalConversationEndpoint(ctx, ambiguous); err != nil {
		t.Fatal(err)
	}
	if _, err := invoke("event-ambiguous", "T-one"); !errors.Is(err, ErrExternalConversationConflict) {
		t.Fatalf("cross-tenant ambiguous route error = %v", err)
	}
	items, err := store.ListExternalConversationInbox(ctx, ExternalConversationInboxFilter{
		Scope: first.Scope, EndpointID: first.ID, Limit: 10,
	})
	if err != nil || len(items) != 1 {
		t.Fatalf("ambiguous callback persisted partial inbox work: %#v, %v", items, err)
	}
}

func TestRegisteredConversationGatewayReturnsVerificationResponseBeforeEndpointsExist(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	catalog := skill.NewCatalog()
	definition := slackConversationSkillDefinition()
	if err := catalog.Register(ctx, definition); err != nil {
		t.Fatal(err)
	}
	scope := Scope{Kind: "platform", ID: "default"}
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
	gateway := createActiveExternalConversationGateway(t, ctx, NewExternalConversationGatewayService(store, catalog), CreateExternalConversationGatewayRequest{
		ID: "verification-gateway", Name: "Verification gateway",
		Gateway: ExternalConversationIngressGateway{
			Scope: scope, DeploymentID: binding.DeploymentID, Provider: "slack",
			Adapter: ExternalConversationAdapterReference{
				SkillID: definition.ID, SkillVersion: definition.Version, BindingID: binding.ID,
				BindingRevision: binding.Revision, AdapterID: "conversations",
			},
		},
		Actor: ActivityActor{Type: "test", ID: "operator"}, Reason: "create provider verification route",
	})
	host := &externalConversationGatewayHostStub{result: &ExternalConversationGatewayHostResult{
		StatusCode: http.StatusOK, ContentType: "text/plain", Body: []byte("verify-me"),
	}}
	result, err := NewExternalConversationTransportService(store, catalog).
		NormalizeExternalConversationRegisteredGatewayIngress(ctx, ExternalConversationPublicIngressRequest{
			Route: gateway.IngressRoute, Method: http.MethodPost, Body: []byte(`{"type":"url_verification"}`),
		}, host)
	if err != nil || string(result.Response.Body) != "verify-me" || len(result.Received) != 0 {
		t.Fatalf("endpoint-free verification response = %#v, %v", result, err)
	}
}
