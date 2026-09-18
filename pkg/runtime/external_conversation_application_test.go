package runtime

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
)

func checkApplicationRouteMatching(t *testing.T, store ExternalConversationEndpointStore) {
	t.Helper()
	ctx := context.Background()
	_, _, endpoint := externalConversationDeliveryFixture(t, ctx, "slack")
	endpoint.InstallationID = "T123"
	endpoint.Address = "C123"
	endpoint.ApplicationID = ""
	if err := store.CreateExternalConversationEndpoint(ctx, endpoint); err != nil {
		t.Fatal(err)
	}
	route := ExternalConversationVerifiedRoute{Provider: endpoint.Provider, InstallationID: "T123", ApplicationID: "A123", Address: "C123", SkillID: endpoint.Adapter.SkillID, SkillVersion: endpoint.Adapter.SkillVersion, SourceIdentity: endpoint.Adapter.SourceIdentity, AdapterID: endpoint.Adapter.AdapterID}
	for _, tc := range []struct {
		pin, event string
		want       int
	}{{"", "A123", 1}, {"A123", "A123", 1}, {"A123", "other", 0}, {"A123", "", 0}} {
		previous := endpoint.Revision
		endpoint.ApplicationID = tc.pin
		endpoint.Revision++
		if err := store.UpdateExternalConversationEndpoint(ctx, endpoint, previous); err != nil {
			t.Fatal(err)
		}
		route.ApplicationID = tc.event
		items, err := store.ListExternalConversationEndpointsByVerifiedRoute(ctx, route)
		if err != nil || len(items) != tc.want {
			t.Fatalf("pin=%q event=%q matches=%d want=%d err=%v", tc.pin, tc.event, len(items), tc.want, err)
		}
	}
}

func TestSQLiteApplicationRouteMatching(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "routes.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	checkApplicationRouteMatching(t, store)
}

func TestUnpinnedApplicationRequiresExactAuthenticatedGateway(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*ExternalConversationEndpoint, *ExternalConversationGatewayHostResult)
		want   int
	}{
		{name: "verified application with no saved pin", want: 1},
		{name: "explicit matching pin", want: 1, mutate: func(e *ExternalConversationEndpoint, _ *ExternalConversationGatewayHostResult) {
			e.ApplicationID = "A123"
		}},
		{name: "wrong app", mutate: func(e *ExternalConversationEndpoint, _ *ExternalConversationGatewayHostResult) {
			e.ApplicationID = "another-app"
		}},
		{name: "missing event app cannot bypass pin", mutate: func(e *ExternalConversationEndpoint, h *ExternalConversationGatewayHostResult) {
			e.ApplicationID = "A123"
			h.Events[0].ApplicationID = ""
		}},
		{name: "other tenant", mutate: func(e *ExternalConversationEndpoint, _ *ExternalConversationGatewayHostResult) { e.Scope.ID = "other" }},
		{name: "other deployment", mutate: func(e *ExternalConversationEndpoint, _ *ExternalConversationGatewayHostResult) {
			e.DeploymentID = "other"
			e.Owner.ID = "other"
			e.Handler.ID = "other"
		}},
		{name: "other binding", mutate: func(e *ExternalConversationEndpoint, _ *ExternalConversationGatewayHostResult) {
			e.Adapter.BindingID = "other"
		}},
		{name: "stale binding", mutate: func(e *ExternalConversationEndpoint, _ *ExternalConversationGatewayHostResult) {
			e.Adapter.BindingRevision++
		}},
		{name: "other workspace", mutate: func(_ *ExternalConversationEndpoint, h *ExternalConversationGatewayHostResult) {
			h.Events[0].InstallationID = "other"
		}},
		{name: "other channel", mutate: func(_ *ExternalConversationEndpoint, h *ExternalConversationGatewayHostResult) {
			h.Events[0].Address = "other"
		}},
		{name: "unverified event", mutate: func(_ *ExternalConversationEndpoint, h *ExternalConversationGatewayHostResult) {
			h.StatusCode = http.StatusUnauthorized
			h.Events = nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store, catalog, endpoint := externalConversationDeliveryFixture(t, ctx, "slack")
			gateway := ExternalConversationIngressGateway{Scope: endpoint.Scope, DeploymentID: endpoint.DeploymentID, Adapter: endpoint.Adapter, Provider: endpoint.Provider}
			endpoint.InstallationID = "T123"
			endpoint.ApplicationID = ""
			endpoint.Address = "C123"
			host := &externalConversationGatewayHostStub{result: &ExternalConversationGatewayHostResult{StatusCode: http.StatusOK, Events: []ExternalConversationGatewayEvent{{
				InstallationID: "T123", ApplicationID: "A123", Address: "C123",
				Event: NormalizedExternalConversationEvent{ID: "event-1", Type: capability.ConversationEventMessageReceived, ExternalConversationID: "C123", ExternalMessageID: "171.003", ExternalParticipantID: "U123", Text: "hi", OrderingKey: "C123:171.003", OccurredAt: time.Now().UTC()},
			}}}}
			if tc.mutate != nil {
				tc.mutate(endpoint, host.result)
			}
			// Reinsert to allow a foreign-scope fixture without weakening normal update checks.
			store.mu.Lock()
			for key := range store.externalEndpoints {
				delete(store.externalEndpoints, key)
			}
			store.mu.Unlock()
			if err := store.CreateExternalConversationEndpoint(ctx, endpoint); err != nil {
				t.Fatal(err)
			}
			result, err := NewExternalConversationTransportService(store, catalog).NormalizeExternalConversationGatewayIngress(ctx, gateway, ExternalConversationPublicIngressRequest{Route: "verified-route", Method: http.MethodPost, Body: []byte(`{}`)}, host)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Received) != tc.want {
				t.Fatalf("received %d events, want %d", len(result.Received), tc.want)
			}
		})
	}
}
