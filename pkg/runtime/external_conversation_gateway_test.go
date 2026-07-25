package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
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
}
