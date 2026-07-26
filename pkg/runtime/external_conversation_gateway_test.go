package runtime

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
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
	if _, err := service.Create(ctx, CreateExternalConversationGatewayRequest{
		ID: "unsafe-active", Name: "Unsafe active", Gateway: externalConversationGatewayFixture(scope),
		Status: ExternalConversationGatewayActive,
		Actor:  ActivityActor{Type: "test", ID: "operator"}, Reason: "prove review-first creation",
	}); !errors.Is(err, ErrInvalidExternalConversation) {
		t.Fatalf("active creation error = %v", err)
	}
	created, err := service.Create(ctx, CreateExternalConversationGatewayRequest{
		ID: "shared-slack", Name: "Shared Slack", Gateway: externalConversationGatewayFixture(scope),
		Status: ExternalConversationGatewayPaused,
		Actor:  ActivityActor{Type: "test", ID: "operator"}, Reason: "create shared Slack routing",
	})
	if err != nil {
		t.Fatal(err)
	}
	active := ExternalConversationGatewayActive
	created, err = service.Update(ctx, scope, created.ID, UpdateExternalConversationGatewayRequest{
		ExpectedRevision: created.Revision, Status: &active,
		Actor: ActivityActor{Type: "test", ID: "operator"}, Reason: "activate reviewed shared Slack routing",
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
	if len(persisted.Lifecycle) != 2 || persisted.Lifecycle[0].Action != ExternalConversationGatewayCreated || persisted.Lifecycle[0].Reason != "create shared Slack routing" || persisted.Lifecycle[1].Action != ExternalConversationGatewayActivated {
		t.Fatalf("persisted lifecycle = %#v", persisted.Lifecycle)
	}

	name := "Renamed Slack gateway"
	updated, err := service.Update(ctx, scope, created.ID, UpdateExternalConversationGatewayRequest{
		ExpectedRevision: created.Revision, Name: &name,
		Actor: ActivityActor{Type: "test", ID: "operator"}, Reason: "use the reviewed display name",
	})
	if err != nil || updated.Revision != 3 || updated.Name != name {
		t.Fatalf("updated gateway = %#v, err = %v", updated, err)
	}
	if len(updated.Lifecycle) != 3 || updated.Lifecycle[2].Action != ExternalConversationGatewayUpdated || updated.Lifecycle[2].Reason != "use the reviewed display name" {
		t.Fatalf("updated lifecycle = %#v", updated.Lifecycle)
	}
	if _, err := service.Update(ctx, scope, created.ID, UpdateExternalConversationGatewayRequest{
		ExpectedRevision: created.Revision, Name: &name,
		Actor: ActivityActor{Type: "test", ID: "operator"}, Reason: "prove stale revision rejection",
	}); !errors.Is(err, ErrExternalConversationConflict) {
		t.Fatalf("stale update error = %v", err)
	}
	retired := ExternalConversationGatewayRetired
	retiredGateway, err := service.Update(ctx, scope, created.ID, UpdateExternalConversationGatewayRequest{
		ExpectedRevision: updated.Revision, Status: &retired,
		Actor: ActivityActor{Type: "test", ID: "operator"}, Reason: "retire unused routing",
	})
	if err != nil || retiredGateway.Status != ExternalConversationGatewayRetired {
		t.Fatalf("retired gateway = %#v, err = %v", retiredGateway, err)
	}
	if retiredGateway.Lifecycle[3].Action != ExternalConversationGatewayRetiredAction || retiredGateway.Lifecycle[3].Actor.ID != "operator" {
		t.Fatalf("retirement lifecycle = %#v", retiredGateway.Lifecycle)
	}
	paused := ExternalConversationGatewayPaused
	if _, err := service.Update(ctx, scope, created.ID, UpdateExternalConversationGatewayRequest{
		ExpectedRevision: retiredGateway.Revision, Status: &paused,
		Actor: ActivityActor{Type: "test", ID: "operator"}, Reason: "prove retired state is immutable",
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
		Actor: ActivityActor{Type: "test", ID: "operator"}, Reason: "create exact adapter routing",
	})
	if err != nil {
		t.Fatal(err)
	}
	gateway.Adapter.BindingRevision++
	if _, err := service.Create(ctx, CreateExternalConversationGatewayRequest{
		ID: "stale-slack", Name: "Stale Slack", Gateway: gateway,
		Actor: ActivityActor{Type: "test", ID: "operator"}, Reason: "prove stale adapter rejection",
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
		Actor: ActivityActor{Type: "test", ID: "operator"}, Reason: "safely stop stale routing",
	}); err != nil || result.Status != ExternalConversationGatewayRetired {
		t.Fatalf("stale gateway safety shutdown = %#v, %v", result, err)
	}
}

func TestCredentialFreeConversationGatewayRoutesAfterSQLiteRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "synthetic-conversation.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	catalog := skill.NewCatalogWithStore(store)
	adapter, err := capability.NormalizeConversationAdapter(capability.ConversationAdapter{
		ProtocolVersion: capability.ConversationAdapterProtocolV1,
		Name:            "Synthetic conversations", Description: "Receive and deliver synthetic conversations.", Provider: "synthetic",
		EndpointModes:     []capability.ConversationEndpointMode{capability.ConversationEndpointChannel},
		InboundEventTypes: []string{capability.ConversationEventMessageReceived},
		Delivery: capability.ConversationDeliveryCapabilities{
			Operations: []capability.ConversationDeliveryOperation{capability.ConversationDeliveryMessageSend},
			Ordering:   capability.ConversationDeliveryOrderConversation, Idempotency: capability.IdempotencyRequired,
		},
		Transport: capability.ConversationAdapterTransport{
			Kind: "plugin", IngressEndpoint: "synthetic.conversation.ingress", DeliveryEndpoint: "synthetic.conversation.deliver",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	definition := &skill.Definition{
		ID: "synthetic-conversations", Version: "1.0.0", Name: "Synthetic conversations",
		ConversationAdapters: map[string]skill.ConversationAdapter{"conversations": adapter},
	}
	if err := catalog.Register(ctx, definition); err != nil {
		t.Fatal(err)
	}
	scope := Scope{Kind: "tenant", ID: "synthetic"}
	binding := &skill.Binding{
		ID: "synthetic", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "synthetic-agent",
		SkillID: definition.ID, SkillVersion: definition.Version,
		EnabledConversationAdapters: []string{"conversations"}, MaximumRisk: skill.RiskLevelRead, Revision: 1,
	}
	if err := catalog.Bind(ctx, binding); err != nil {
		t.Fatal(err)
	}
	endpoint, err := NewExternalConversationEndpointService(store, catalog).Create(ctx, CreateExternalConversationEndpointRequest{
		ID: "synthetic-endpoint", Scope: scope,
		Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "synthetic-agent"}, DeploymentID: "synthetic-agent",
		Name: "Synthetic channel",
		Adapter: ExternalConversationAdapterReference{
			SkillID: definition.ID, SkillVersion: definition.Version,
			BindingID: binding.ID, BindingRevision: binding.Revision, AdapterID: "conversations",
		},
		Mode: capability.ConversationEndpointChannel, Address: "channel-1",
		Handler: ExternalConversationHandler{Kind: ExternalConversationHandlerAgent, ID: "synthetic-agent"},
		Policy: ExternalConversationPolicy{
			MessageSelection: ExternalConversationSelectAllMessages,
			ReplyMode:        ExternalConversationReplyChannel,
		},
		Status: ExternalConversationEndpointActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	previousRevision := endpoint.Revision
	endpoint.InstallationID, endpoint.ApplicationID = "installation-1", "application-1"
	endpoint.Revision++
	endpoint.UpdatedAt = endpoint.UpdatedAt.Add(time.Second)
	if err := store.UpdateExternalConversationEndpoint(ctx, endpoint, previousRevision); err != nil {
		t.Fatal(err)
	}
	gateway := createActiveExternalConversationGateway(t, ctx, NewExternalConversationGatewayService(store, catalog), CreateExternalConversationGatewayRequest{
		ID: "synthetic-gateway", Name: "Synthetic gateway",
		Gateway: ExternalConversationIngressGateway{
			Scope: scope, DeploymentID: "synthetic-agent", Provider: "synthetic",
			Adapter: ExternalConversationAdapterReference{
				SkillID: definition.ID, SkillVersion: definition.Version,
				BindingID: binding.ID, BindingRevision: binding.Revision, AdapterID: "conversations",
			},
		},
		Actor: ActivityActor{Type: "test", ID: "operator"}, Reason: "create restart acceptance routing",
	})
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restartedCatalog := skill.NewCatalogWithStore(reopened)
	resolved, err := restartedCatalog.ResolveConversationAdapter(
		ctx, binding.Scope, binding.DeploymentID, definition.ID, definition.Version, "conversations",
		skill.BindingReference{ID: binding.ID, Revision: binding.Revision},
	)
	if err != nil || resolved == nil || resolved.Adapter.Features != nil || resolved.Adapter.Credentials != nil {
		t.Fatalf("restarted adapter = %#v, %v", resolved, err)
	}
	host := &externalConversationGatewayHostStub{result: &ExternalConversationGatewayHostResult{
		StatusCode: http.StatusOK,
		Events: []ExternalConversationGatewayEvent{{
			InstallationID: "installation-1", ApplicationID: "application-1", Address: "channel-1",
			Event: NormalizedExternalConversationEvent{
				ID: "event-1", Type: capability.ConversationEventMessageReceived,
				ExternalConversationID: "channel-1", ExternalMessageID: "message-1",
				ExternalParticipantID: "participant-1", Text: "hello after restart",
				OrderingKey: "channel-1:message-1", OccurredAt: time.Now().UTC(),
			},
		}},
	}}
	result, err := NewExternalConversationTransportService(reopened, restartedCatalog).
		NormalizeExternalConversationRegisteredGatewayIngress(ctx, ExternalConversationPublicIngressRequest{
			Route: gateway.IngressRoute, Method: http.MethodPost, Body: []byte(`{"message":"hello"}`),
		}, host)
	if err != nil || len(result.Received) != 1 || !result.Received[0].Accepted || result.Received[0].Item.EndpointID != endpoint.ID {
		t.Fatalf("restarted gateway ingress = %#v, %v", result, err)
	}
}
