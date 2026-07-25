package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/axiom-studio/openseal/pkg/skill"
)

func externalConversationGatewayFixture(scope Scope) ExternalConversationIngressGateway {
	return ExternalConversationIngressGateway{
		Scope: scope, DeploymentID: "skill-deployment-1", Provider: "slack",
		Adapter: ExternalConversationAdapterReference{
			SkillID: "openseal.slack-conversations", SkillVersion: "1.0.0",
			SourceIdentity: "oci://example/slack@sha256:abc", BindingID: "binding-1",
			BindingRevision: 1, AdapterID: "slack-conversations",
		},
	}
}

func TestExternalConversationGatewayServicePersistsAndUsesCAS(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kernel.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{Kind: "tenant", ID: "tenant-1"}
	service := NewExternalConversationGatewayService(store)
	created, err := service.Create(ctx, CreateExternalConversationGatewayRequest{
		ID: "shared-slack", Name: "Shared Slack", Gateway: externalConversationGatewayFixture(scope),
		Status: ExternalConversationGatewayActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	service = NewExternalConversationGatewayService(reopened)
	persisted, err := service.GetByIngressRoute(ctx, created.IngressRoute)
	if err != nil || persisted.ID != created.ID || persisted.Status != ExternalConversationGatewayActive {
		t.Fatalf("persisted gateway = %#v, err = %v", persisted, err)
	}

	name := "Renamed Slack gateway"
	updated, err := service.Update(ctx, scope, created.ID, UpdateExternalConversationGatewayRequest{
		ExpectedRevision: created.Revision, Name: &name,
	})
	if err != nil || updated.Revision != 2 || updated.Name != name {
		t.Fatalf("updated gateway = %#v, err = %v", updated, err)
	}
	if _, err := service.Update(ctx, scope, created.ID, UpdateExternalConversationGatewayRequest{
		ExpectedRevision: created.Revision, Name: &name,
	}); !errors.Is(err, ErrExternalConversationConflict) {
		t.Fatalf("stale update error = %v", err)
	}
	retired := ExternalConversationGatewayRetired
	retiredGateway, err := service.Update(ctx, scope, created.ID, UpdateExternalConversationGatewayRequest{
		ExpectedRevision: updated.Revision, Status: &retired,
	})
	if err != nil || retiredGateway.Status != ExternalConversationGatewayRetired {
		t.Fatalf("retired gateway = %#v, err = %v", retiredGateway, err)
	}
	paused := ExternalConversationGatewayPaused
	if _, err := service.Update(ctx, scope, created.ID, UpdateExternalConversationGatewayRequest{
		ExpectedRevision: retiredGateway.Revision, Status: &paused,
	}); !errors.Is(err, ErrExternalConversationConflict) {
		t.Fatalf("retired gateway mutation error = %v", err)
	}
}

func TestExternalConversationGatewayServiceRejectsStaleExactAdapter(t *testing.T) {
	ctx := context.Background()
	store, catalog, endpoint := externalConversationDeliveryFixture(t, ctx, "slack")
	service := NewExternalConversationGatewayService(store, catalog)
	gateway := ExternalConversationIngressGateway{
		Scope: endpoint.Scope, DeploymentID: endpoint.DeploymentID,
		Adapter: endpoint.Adapter, Provider: endpoint.Provider,
	}
	created, err := service.Create(ctx, CreateExternalConversationGatewayRequest{
		ID: "exact-slack", Name: "Exact Slack", Gateway: gateway,
	})
	if err != nil {
		t.Fatal(err)
	}
	gateway.Adapter.BindingRevision++
	if _, err := service.Create(ctx, CreateExternalConversationGatewayRequest{
		ID: "stale-slack", Name: "Stale Slack", Gateway: gateway,
	}); !errors.Is(err, ErrInvalidExternalConversation) {
		t.Fatalf("stale exact adapter error = %v", err)
	}
	if _, err := catalog.DisableBinding(ctx, skill.DisableBindingRequest{
		Scope:        skill.ScopeReference{Kind: endpoint.Scope.Kind, ID: endpoint.Scope.ID},
		DeploymentID: endpoint.DeploymentID, BindingID: endpoint.Adapter.BindingID,
		ExpectedRevision: endpoint.Adapter.BindingRevision,
		Actor:            skill.BindingActor{Type: "test", ID: "operator"}, Reason: "prove safe shutdown",
	}); err != nil {
		t.Fatal(err)
	}
	retired := ExternalConversationGatewayRetired
	if result, err := service.Update(ctx, created.Gateway.Scope, created.ID, UpdateExternalConversationGatewayRequest{
		ExpectedRevision: created.Revision, Status: &retired,
	}); err != nil || result.Status != ExternalConversationGatewayRetired {
		t.Fatalf("stale gateway safety shutdown = %#v, %v", result, err)
	}
}
