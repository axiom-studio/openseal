//go:build integration

package runtime

import (
	"context"
	"testing"
)

func TestPostgresGatewayAuthorityBackfillIsTenantBoundUniqueAndAudited(t *testing.T) {
	store, replica := newPostgresRunEventWaitTestStores(t)
	ctx := t.Context()
	memory, _, endpoint := externalConversationDeliveryFixture(t, ctx, "slack")
	legacy := createActiveExternalConversationGateway(t, ctx, NewExternalConversationGatewayService(memory), CreateExternalConversationGatewayRequest{
		ID: "legacy-gateway", Name: "Legacy provider gateway",
		Gateway: ExternalConversationIngressGateway{Scope: endpoint.Scope, DeploymentID: endpoint.DeploymentID, Adapter: endpoint.Adapter, Provider: endpoint.Provider},
		Actor:   ActivityActor{Type: "test", ID: "operator"}, Reason: "Exercise upgrade from reviewed legacy routing",
	})
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
	// The fixture began at the current schema. Remove only this isolated
	// schema's migration marker to exercise an actual legacy-row upgrade.
	if _, err := store.db.ExecContext(ctx, `DELETE FROM `+store.table("schema_migrations")+` WHERE version=$1`, externalConversationGatewayAuthorityMigrationVersion); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		tx, err := store.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.migrateExternalConversationGatewayAuthority(ctx, tx); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		pinned, err := replica.GetExternalConversationGateway(ctx, endpoint.Scope, legacy.ID)
		if err != nil || pinned.Gateway.InstallationID != "owned-installation" || pinned.Gateway.ApplicationID != "owned-application" || pinned.Revision != legacy.Revision+1 {
			t.Fatalf("upgrade attempt %d picked wrong authority or ran twice: %#v, %v", attempt, pinned, err)
		}
		if err := pinned.Validate(); err != nil || pinned.Lifecycle[len(pinned.Lifecycle)-1].Actor.ID != "gateway-authority-migration" {
			t.Fatalf("gateway authority audit was not preserved: %#v, %v", pinned, err)
		}
		ambiguous, err := replica.GetExternalConversationGateway(ctx, endpoint.Scope, ambiguousGateway.ID)
		if err != nil || ambiguous.Gateway.InstallationID != "" || ambiguous.Revision != ambiguousGateway.Revision {
			t.Fatalf("ambiguous installation gained authority: %#v, %v", ambiguous, err)
		}
	}
	items, err := replica.ListExternalConversationGateways(context.Background(), ExternalConversationGatewayFilter{
		Scope: endpoint.Scope, DeploymentID: endpoint.DeploymentID, Limit: 10,
	})
	if err != nil || len(items) != 1 || items[0].ID != legacy.ID {
		t.Fatalf("exact-deployment source lookup leaked another agent: %#v, %v", items, err)
	}
}
