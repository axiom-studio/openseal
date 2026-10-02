package runtime

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
)

func TestGatewayAuthorityPinsRejectForeignProviderInstallation(t *testing.T) {
	ctx := context.Background()
	memory, catalog, endpoint := externalConversationDeliveryFixture(t, ctx, "slack")
	store := &ingressRunEventStore{MemoryStore: memory}
	service := NewExternalConversationTransportService(store, catalog)
	gateway := ExternalConversationIngressGateway{
		Scope: endpoint.Scope, DeploymentID: endpoint.DeploymentID, Adapter: endpoint.Adapter, Provider: endpoint.Provider,
		InstallationID: "owned-installation", ApplicationID: "owned-application",
	}
	for _, identity := range []struct{ installation, application string }{
		{"foreign-installation", "owned-application"},
		{"owned-installation", "foreign-application"},
		{"owned-installation", ""},
	} {
		host := &externalConversationGatewayHostStub{result: &ExternalConversationGatewayHostResult{
			StatusCode: http.StatusOK, Events: []ExternalConversationGatewayEvent{{
				InstallationID: identity.installation, ApplicationID: identity.application, Address: "unbound-channel",
				Event: NormalizedExternalConversationEvent{
					ID: "foreign-event", Type: capability.ConversationEventMessageReceived, ExternalConversationID: "unbound-channel",
					ExternalMessageID: "message", ExternalParticipantID: "person", Text: "A provider reply", OrderingKey: "message", OccurredAt: time.Now().UTC(),
				},
			}},
		}}
		result, err := service.NormalizeExternalConversationGatewayIngress(ctx, gateway,
			ExternalConversationPublicIngressRequest{Route: "route", Method: http.MethodPost, Body: []byte(`{}`)}, host)
		if err != nil || len(result.Received) != 0 || len(store.events) != 0 {
			t.Fatalf("foreign provider identity was accepted: %#v, %v, observations=%#v", result, err, store.events)
		}
	}
}

func TestGatewayAuthorityDerivesOnlyUniqueReviewedExactBinding(t *testing.T) {
	ctx := context.Background()
	store, catalog, endpoint := externalConversationDeliveryFixture(t, ctx, "slack")
	previous := endpoint.Revision
	endpoint.InstallationID, endpoint.ApplicationID = "installation-one", "application-one"
	endpoint.Revision++
	if err := store.UpdateExternalConversationEndpoint(ctx, endpoint, previous); err != nil {
		t.Fatal(err)
	}
	for index, mutate := range []func(*ExternalConversationEndpoint){
		func(e *ExternalConversationEndpoint) { e.Scope.ID = "foreign-tenant" },
		func(e *ExternalConversationEndpoint) { e.Adapter.BindingID = "different-binding" },
		func(e *ExternalConversationEndpoint) { e.Adapter.SourceIdentity = "different-source" },
		func(e *ExternalConversationEndpoint) { e.Status = ExternalConversationEndpointPaused },
		func(e *ExternalConversationEndpoint) { e.Adapter.BindingRevision++ },
	} {
		other := cloneExternalConversationEndpoint(endpoint)
		other.ID, other.IngressRoute = "unrelated-"+string(rune('a'+index)), "unrelated-route-"+string(rune('a'+index))
		other.Revision = 1
		other.InstallationID = "different-installation"
		mutate(other)
		if err := store.CreateExternalConversationEndpoint(ctx, other); err != nil {
			t.Fatal(err)
		}
	}
	gateway := createActiveExternalConversationGateway(t, ctx, NewExternalConversationGatewayService(store, catalog), CreateExternalConversationGatewayRequest{
		ID: "uniquely-pinned", Name: "Exact reviewed installation",
		Gateway: ExternalConversationIngressGateway{Scope: endpoint.Scope, DeploymentID: endpoint.DeploymentID, Adapter: endpoint.Adapter, Provider: endpoint.Provider},
		Actor:   ActivityActor{Type: "test", ID: "operator"}, Reason: "Use reviewed provider connection identity",
	})
	if gateway.Gateway.InstallationID != endpoint.InstallationID || gateway.Gateway.ApplicationID != endpoint.ApplicationID {
		t.Fatalf("gateway picked unrelated or absent authority: %#v", gateway)
	}
	loaded, err := store.GetExternalConversationGateway(ctx, endpoint.Scope, gateway.ID)
	if err != nil || loaded.Gateway.InstallationID != endpoint.InstallationID {
		t.Fatalf("gateway authority was not saved: %#v, %v", loaded, err)
	}
	ambiguous := cloneExternalConversationEndpoint(endpoint)
	ambiguous.ID, ambiguous.IngressRoute, ambiguous.Revision = "ambiguous-endpoint", "ambiguous-route", 1
	ambiguous.InstallationID = "second-installation"
	if err := store.CreateExternalConversationEndpoint(ctx, ambiguous); err != nil {
		t.Fatal(err)
	}
	_, err = NewExternalConversationGatewayService(store, catalog).Create(ctx, CreateExternalConversationGatewayRequest{
		ID: "ambiguous-gateway", Name: "Ambiguous identity",
		Gateway: ExternalConversationIngressGateway{Scope: endpoint.Scope, DeploymentID: endpoint.DeploymentID, Adapter: endpoint.Adapter, Provider: endpoint.Provider},
		Actor:   ActivityActor{Type: "test", ID: "operator"}, Reason: "Reject ambiguous reviewed account authority",
	})
	if !errors.Is(err, ErrExternalConversationConflict) {
		t.Fatalf("ambiguous provider installations gained gateway authority: %v", err)
	}
}

func TestLegacyUnpinnedGatewayDoesNotPublishUnboundObservation(t *testing.T) {
	ctx := context.Background()
	memory, catalog, endpoint := externalConversationDeliveryFixture(t, ctx, "slack")
	store := &ingressRunEventStore{MemoryStore: memory}
	service := NewExternalConversationTransportService(store, catalog)
	host := &externalConversationGatewayHostStub{result: &ExternalConversationGatewayHostResult{StatusCode: http.StatusOK,
		Events: []ExternalConversationGatewayEvent{{InstallationID: "installation", Address: "unbound-channel", Event: NormalizedExternalConversationEvent{
			ID: "unbound-event", Type: capability.ConversationEventMessageReceived, ExternalConversationID: "unbound-channel",
			ExternalMessageID: "message", ExternalParticipantID: "person", Text: "A provider reply", OrderingKey: "message", OccurredAt: time.Now().UTC(),
		}}},
	}}
	gateway := ExternalConversationIngressGateway{Scope: endpoint.Scope, DeploymentID: endpoint.DeploymentID, Adapter: endpoint.Adapter, Provider: endpoint.Provider}
	result, err := service.NormalizeExternalConversationGatewayIngress(ctx, gateway,
		ExternalConversationPublicIngressRequest{Route: "route", Method: http.MethodPost, Body: []byte(`{}`)}, host)
	if err != nil || len(result.Received) != 0 || len(store.events) != 0 {
		t.Fatalf("legacy gateway inferred installation from provider body: %#v, %v, %#v", result, err, store.events)
	}
}

func TestSQLiteGatewayAuthorityBackfillSurvivesRestartAndRejectsAmbiguity(t *testing.T) {
	ctx := context.Background()
	memory, _, endpoint := externalConversationDeliveryFixture(t, ctx, "slack")
	legacy := createActiveExternalConversationGateway(t, ctx, NewExternalConversationGatewayService(memory), CreateExternalConversationGatewayRequest{
		ID: "legacy-gateway", Name: "Legacy gateway",
		Gateway: ExternalConversationIngressGateway{Scope: endpoint.Scope, DeploymentID: endpoint.DeploymentID, Adapter: endpoint.Adapter, Provider: endpoint.Provider},
		Actor:   ActivityActor{Type: "test", ID: "operator"}, Reason: "Create pre-migration provider routing",
	})
	path := filepath.Join(t.TempDir(), "authority.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	endpoint.InstallationID, endpoint.ApplicationID = "owned-installation", "owned-application"
	if err := store.CreateExternalConversationEndpoint(ctx, endpoint); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateExternalConversationGateway(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	foreign := cloneExternalConversationEndpoint(endpoint)
	foreign.ID, foreign.IngressRoute, foreign.Scope.ID = "foreign-endpoint", "foreign-route", "foreign-tenant"
	foreign.InstallationID = "foreign-installation"
	if err := store.CreateExternalConversationEndpoint(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	ambiguousGateway := cloneExternalConversationGateway(legacy)
	ambiguousGateway.ID, ambiguousGateway.IngressRoute = "ambiguous-gateway", "ambiguous-gateway-route"
	ambiguousGateway.Gateway.DeploymentID = "ambiguous-agent"
	if err := store.CreateExternalConversationGateway(ctx, ambiguousGateway); err != nil {
		t.Fatal(err)
	}
	for _, identity := range []struct{ id, installation string }{{"first", "installation-a"}, {"second", "installation-b"}} {
		candidate := cloneExternalConversationEndpoint(endpoint)
		candidate.ID, candidate.IngressRoute = "ambiguous-"+identity.id, "ambiguous-route-"+identity.id
		candidate.DeploymentID, candidate.Owner.ID, candidate.Handler.ID = "ambiguous-agent", "ambiguous-agent", "ambiguous-agent"
		candidate.InstallationID = identity.installation
		if err := store.CreateExternalConversationEndpoint(ctx, candidate); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	for restart := 0; restart < 2; restart++ {
		store, err = NewSQLiteStore(path)
		if err != nil {
			t.Fatal(err)
		}
		pinned, err := store.GetExternalConversationGateway(ctx, endpoint.Scope, legacy.ID)
		if err != nil || pinned.Gateway.InstallationID != "owned-installation" || pinned.Gateway.ApplicationID != "owned-application" || pinned.Revision != legacy.Revision+1 {
			t.Fatalf("restart %d did not persist unique exact-binding authority once: %#v, %v", restart, pinned, err)
		}
		if err := pinned.Validate(); err != nil || pinned.Lifecycle[len(pinned.Lifecycle)-1].Actor.ID != "gateway-authority-migration" {
			t.Fatalf("backfill authority audit = %#v, %v", pinned.Lifecycle, err)
		}
		ambiguous, err := store.GetExternalConversationGateway(ctx, endpoint.Scope, ambiguousGateway.ID)
		if err != nil || ambiguous.Gateway.InstallationID != "" || ambiguous.Revision != ambiguousGateway.Revision {
			t.Fatalf("ambiguous gateway inferred account authority: %#v, %v", ambiguous, err)
		}
		items, err := store.ListExternalConversationGateways(ctx, ExternalConversationGatewayFilter{Scope: endpoint.Scope, DeploymentID: endpoint.DeploymentID, Limit: 10})
		if err != nil || len(items) != 1 || items[0].ID != legacy.ID {
			t.Fatalf("deployment gateway lookup leaked other sources: %#v, %v", items, err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
