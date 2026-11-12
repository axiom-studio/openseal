package runtime

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/externalconversation"
	"github.com/axiom-studio/openseal/pkg/runtime/externalconversation/externalconversationtest"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

func TestGatewayAuthorityPinsRejectForeignProviderInstallation(t *testing.T) {
	ctx := context.Background()
	store, catalog, endpoint := externalconversationtest.EndpointFixture(t, ctx, "slack")
	service := externalconversation.NewExternalConversationTransportService(store, catalog)
	gateway := models.ExternalConversationIngressGateway{
		Scope: endpoint.Scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: endpoint.DeploymentID}, DeploymentID: endpoint.DeploymentID, Adapter: endpoint.Adapter, Provider: endpoint.Provider,
		InstallationID: "owned-installation", ApplicationID: "owned-application",
	}
	for _, identity := range []struct{ installation, application string }{
		{"foreign-installation", "owned-application"},
		{"owned-installation", "foreign-application"},
		{"owned-installation", ""},
	} {
		host := &externalconversationtest.GatewayHostStub{Result: &externalconversation.ExternalConversationGatewayHostResult{
			StatusCode: http.StatusOK, Events: []externalconversation.ExternalConversationGatewayEvent{{
				InstallationID: identity.installation, ApplicationID: identity.application, Address: "unbound-channel",
				Event: models.NormalizedExternalConversationEvent{
					ID: "foreign-event", Type: capability.ConversationEventMessageReceived, ExternalConversationID: "unbound-channel",
					ExternalMessageID: "message", ExternalParticipantID: "person", Text: "A provider reply", OrderingKey: "message", OccurredAt: time.Now().UTC(),
				},
			}},
		}}
		result, err := service.NormalizeExternalConversationGatewayIngress(ctx, gateway,
			externalconversation.ExternalConversationPublicIngressRequest{Route: "route", Method: http.MethodPost, Body: []byte(`{}`)}, host)
		if err != nil || len(result.Received) != 0 {
			t.Fatalf("foreign provider identity was accepted: %#v, %v", result, err)
		}
	}
}

func TestGatewayAuthorityDerivesOnlyUniqueReviewedExactBinding(t *testing.T) {
	ctx := context.Background()
	store, catalog, endpoint := externalconversationtest.EndpointFixture(t, ctx, "slack")
	previous := endpoint.Revision
	endpoint.InstallationID, endpoint.ApplicationID = "installation-one", "application-one"
	endpoint.Revision++
	if err := store.UpdateExternalConversationEndpoint(ctx, endpoint, previous); err != nil {
		t.Fatal(err)
	}
	for index, mutate := range []func(*models.ExternalConversationEndpoint){
		func(e *models.ExternalConversationEndpoint) { e.Scope.ID = "foreign-tenant" },
		func(e *models.ExternalConversationEndpoint) { e.Adapter.BindingID = "different-binding" },
		func(e *models.ExternalConversationEndpoint) { e.Adapter.SourceIdentity = "different-source" },
		func(e *models.ExternalConversationEndpoint) { e.Status = models.ExternalConversationEndpointPaused },
		func(e *models.ExternalConversationEndpoint) { e.Adapter.BindingRevision++ },
	} {
		other := models.CloneExternalConversationEndpoint(endpoint)
		other.ID, other.IngressRoute = "unrelated-"+string(rune('a'+index)), "unrelated-route-"+string(rune('a'+index))
		other.Revision = 1
		other.InstallationID = "different-installation"
		mutate(other)
		if err := store.CreateExternalConversationEndpoint(ctx, other); err != nil {
			t.Fatal(err)
		}
	}
	gateway := externalconversationtest.CreateActiveGateway(t, ctx, externalconversation.NewExternalConversationGatewayService(store, catalog), externalconversation.CreateExternalConversationGatewayRequest{
		ID: "uniquely-pinned", Name: "Exact reviewed installation",
		Gateway: models.ExternalConversationIngressGateway{Scope: endpoint.Scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: endpoint.DeploymentID}, DeploymentID: endpoint.DeploymentID, Adapter: endpoint.Adapter, Provider: endpoint.Provider},
		Actor:   models.ActivityActor{Type: "test", ID: "operator"}, Reason: "Use reviewed provider connection identity",
	})
	if gateway.Gateway.InstallationID != endpoint.InstallationID || gateway.Gateway.ApplicationID != endpoint.ApplicationID {
		t.Fatalf("gateway picked unrelated or absent authority: %#v", gateway)
	}
	loaded, err := store.GetExternalConversationGateway(ctx, endpoint.Scope, gateway.ID)
	if err != nil || loaded.Gateway.InstallationID != endpoint.InstallationID {
		t.Fatalf("gateway authority was not saved: %#v, %v", loaded, err)
	}
	ambiguous := models.CloneExternalConversationEndpoint(endpoint)
	ambiguous.ID, ambiguous.IngressRoute, ambiguous.Revision = "ambiguous-endpoint", "ambiguous-route", 1
	ambiguous.InstallationID = "second-installation"
	if err := store.CreateExternalConversationEndpoint(ctx, ambiguous); err != nil {
		t.Fatal(err)
	}
	_, err = externalconversation.NewExternalConversationGatewayService(store, catalog).Create(ctx, externalconversation.CreateExternalConversationGatewayRequest{
		ID: "ambiguous-gateway", Name: "Ambiguous identity",
		Gateway: models.ExternalConversationIngressGateway{Scope: endpoint.Scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: endpoint.DeploymentID}, DeploymentID: endpoint.DeploymentID, Adapter: endpoint.Adapter, Provider: endpoint.Provider},
		Actor:   models.ActivityActor{Type: "test", ID: "operator"}, Reason: "Reject ambiguous reviewed account authority",
	})
	if !errors.Is(err, models.ErrExternalConversationConflict) {
		t.Fatalf("ambiguous provider installations gained gateway authority: %v", err)
	}
}

func TestLegacyUnpinnedGatewayDoesNotAcceptUnboundEvent(t *testing.T) {
	ctx := context.Background()
	store, catalog, endpoint := externalconversationtest.EndpointFixture(t, ctx, "slack")
	service := externalconversation.NewExternalConversationTransportService(store, catalog)
	host := &externalconversationtest.GatewayHostStub{Result: &externalconversation.ExternalConversationGatewayHostResult{StatusCode: http.StatusOK,
		Events: []externalconversation.ExternalConversationGatewayEvent{{InstallationID: "installation", Address: "unbound-channel", Event: models.NormalizedExternalConversationEvent{
			ID: "unbound-event", Type: capability.ConversationEventMessageReceived, ExternalConversationID: "unbound-channel",
			ExternalMessageID: "message", ExternalParticipantID: "person", Text: "A provider reply", OrderingKey: "message", OccurredAt: time.Now().UTC(),
		}}},
	}}
	gateway := models.ExternalConversationIngressGateway{Scope: endpoint.Scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: endpoint.DeploymentID}, DeploymentID: endpoint.DeploymentID, Adapter: endpoint.Adapter, Provider: endpoint.Provider}
	result, err := service.NormalizeExternalConversationGatewayIngress(ctx, gateway,
		externalconversation.ExternalConversationPublicIngressRequest{Route: "route", Method: http.MethodPost, Body: []byte(`{}`)}, host)
	if err != nil || len(result.Received) != 0 {
		t.Fatalf("legacy gateway inferred installation from provider body: %#v, %v", result, err)
	}
}

func TestSQLiteGatewayAuthorityBackfillSurvivesRestartAndRejectsAmbiguity(t *testing.T) {
	ctx := context.Background()
	memory, _, endpoint := externalconversationtest.EndpointFixture(t, ctx, "slack")
	legacy := externalconversationtest.CreateActiveGateway(t, ctx, externalconversation.NewExternalConversationGatewayService(memory, nil), externalconversation.CreateExternalConversationGatewayRequest{
		ID: "legacy-gateway", Name: "Legacy gateway",
		Gateway: models.ExternalConversationIngressGateway{Scope: endpoint.Scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: endpoint.DeploymentID}, DeploymentID: endpoint.DeploymentID, Adapter: endpoint.Adapter, Provider: endpoint.Provider},
		Actor:   models.ActivityActor{Type: "test", ID: "operator"}, Reason: "Create pre-migration provider routing",
	})
	path := filepath.Join(t.TempDir(), "authority.db")
	store, err := sqlite.NewSQLiteStore(path)
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
	foreign := models.CloneExternalConversationEndpoint(endpoint)
	foreign.ID, foreign.IngressRoute, foreign.Scope.ID = "foreign-endpoint", "foreign-route", "foreign-tenant"
	foreign.InstallationID = "foreign-installation"
	if err := store.CreateExternalConversationEndpoint(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	ambiguousGateway := models.CloneExternalConversationGateway(legacy)
	ambiguousGateway.ID, ambiguousGateway.IngressRoute = "ambiguous-gateway", "ambiguous-gateway-route"
	ambiguousGateway.Gateway.DeploymentID, ambiguousGateway.Gateway.Owner.ID = "ambiguous-agent", "ambiguous-agent"
	if err := store.CreateExternalConversationGateway(ctx, ambiguousGateway); err != nil {
		t.Fatal(err)
	}
	for _, identity := range []struct{ id, installation string }{{"first", "installation-a"}, {"second", "installation-b"}} {
		candidate := models.CloneExternalConversationEndpoint(endpoint)
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
		store, err = sqlite.NewSQLiteStore(path)
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
		items, err := store.ListExternalConversationGateways(ctx, models.ExternalConversationGatewayFilter{Scope: endpoint.Scope, DeploymentID: endpoint.DeploymentID, Limit: 10})
		if err != nil || len(items) != 1 || items[0].ID != legacy.ID {
			t.Fatalf("deployment gateway lookup leaked other sources: %#v, %v", items, err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
