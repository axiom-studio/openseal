//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/google/uuid"
)

func TestPostgresGatewayAuthorityBackfillIsTenantBoundUniqueAndAudited(t *testing.T) {
	store, pgSchema := openTestPostgresStore(t, "gateway_authority_")
	schema := pgSchema.Name
	dsn := pgSchema.DSN
	replica, err := New(t.Context(), dsn, WithSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = replica.Close() })
	ctx := t.Context()
	// The endpoint and the reviewed, activated gateway the endpoint and
	// gateway services create for a Slack Agent.
	now := time.Now().UTC()
	scope := models.Scope{Kind: "tenant", ID: "slack"}
	owner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "slack-agent"}
	adapter := models.ExternalConversationAdapterReference{SkillID: "slack", SkillVersion: "1.0.0", BindingID: "slack", BindingRevision: 1, AdapterID: "conversations"}
	endpoint := &models.ExternalConversationEndpoint{
		ID: "slack-endpoint", IngressRoute: uuid.NewString(), Scope: scope, Owner: owner, DeploymentID: owner.ID,
		Name: "slack endpoint", Adapter: adapter, Provider: "slack", Mode: capability.ConversationEndpointChannel, Address: "slack-address",
		Handler: models.ExternalConversationHandler{Kind: models.ExternalConversationHandlerAgent, ID: owner.ID},
		Policy: models.ExternalConversationPolicy{
			MessageSelection: models.ExternalConversationSelectAllMessages, ReplyMode: models.ExternalConversationReplyThread, IgnoreBots: true,
		},
		Status: models.ExternalConversationEndpointActive, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	operator := models.ActivityActor{Type: "test", ID: "operator"}
	legacy := &models.ExternalConversationGatewayRegistration{
		ID: "legacy-gateway", IngressRoute: uuid.NewString(), Name: "Legacy provider gateway",
		Gateway: models.ExternalConversationIngressGateway{Scope: scope, Owner: owner, DeploymentID: owner.ID, Adapter: adapter, Provider: "slack"},
		Status:  models.ExternalConversationGatewayActive, Revision: 2, CreatedAt: now, UpdatedAt: now,
		Lifecycle: []models.ExternalConversationGatewayLifecycleEntry{
			{Revision: 1, Action: models.ExternalConversationGatewayCreated, Actor: operator, Reason: "Exercise upgrade from reviewed legacy routing", At: now},
			{Revision: 2, Action: models.ExternalConversationGatewayActivated, Actor: operator, Reason: "activate reviewed provider route", At: now},
		},
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
	// The fixture began at the current schema. Remove only this isolated
	// schema's migration marker to exercise an actual legacy-row upgrade.
	if _, err := pgSchema.DB.ExecContext(ctx, `DELETE FROM `+pgSchema.Table("schema_migrations")+` WHERE version=$1`, externalConversationGatewayAuthorityMigrationVersion); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		tx, err := pgSchema.DB.BeginTx(ctx, nil)
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
	items, err := replica.ListExternalConversationGateways(context.Background(), models.ExternalConversationGatewayFilter{
		Scope: endpoint.Scope, DeploymentID: endpoint.DeploymentID, Limit: 10,
	})
	if err != nil || len(items) != 1 || items[0].ID != legacy.ID {
		t.Fatalf("exact-deployment source lookup leaked another agent: %#v, %v", items, err)
	}
}
