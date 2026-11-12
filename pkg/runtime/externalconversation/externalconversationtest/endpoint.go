package externalconversationtest

import (
	"bytes"
	"context"
	"io"
	"sort"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation/conversationtest"
	"github.com/axiom-studio/openseal/pkg/runtime/externalconversation"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding/skillbindingtest"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// EndpointFixture is EndpointFixtureWithOperations for message sends only.
func EndpointFixture(
	t *testing.T,
	ctx context.Context,
	provider string,
) (*sqlite.Store, *skill.Catalog, *models.ExternalConversationEndpoint) {
	return EndpointFixtureWithOperations(t, ctx, provider, []skill.ConversationDeliveryOperation{skill.ConversationDeliveryMessageSend})
}

// EndpointFixtureWithOperations creates the active channel endpoint of
// provider, with the delivery operations listed, in a new in-memory store.
// The endpoint's Agent "<provider>-agent" handles every message.
func EndpointFixtureWithOperations(
	t *testing.T,
	ctx context.Context,
	provider string,
	operations []skill.ConversationDeliveryOperation,
) (*sqlite.Store, *skill.Catalog, *models.ExternalConversationEndpoint) {
	t.Helper()
	store := conversationtest.NewStore(t)
	catalog, endpoint := EndpointFixtureOn(t, ctx, store, provider, operations)
	return store, catalog, endpoint
}

// EndpointFixtureOn creates the fixture endpoint in any endpoint store
// (SQLite or PostgreSQL).
func EndpointFixtureOn(
	t *testing.T,
	ctx context.Context,
	store models.ExternalConversationEndpointStore,
	provider string,
	operations []skill.ConversationDeliveryOperation,
) (*skill.Catalog, *models.ExternalConversationEndpoint) {
	t.Helper()
	return EndpointFixtureOwnedBy(t, ctx, store, provider, operations, models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: provider + "-agent"})
}

// EndpointFixtureOwnedBy creates the fixture endpoint for an Agent or a
// Team owner.
func EndpointFixtureOwnedBy(
	t *testing.T,
	ctx context.Context,
	store models.ExternalConversationEndpointStore,
	provider string,
	operations []skill.ConversationDeliveryOperation,
	owner models.ObjectiveOwner,
) (*skill.Catalog, *models.ExternalConversationEndpoint) {
	t.Helper()
	catalog := skill.NewCatalog()
	definition := skillbindingtest.SlackConversationSkillDefinition()
	definition.ID, definition.Name = provider, provider
	adapter := definition.ConversationAdapters["conversations"]
	adapter.Delivery.Operations = append([]skill.ConversationDeliveryOperation(nil), operations...)
	adapter.Provider = provider
	adapter.Name = provider + " conversations"
	adapter.Transport.IngressEndpoint = provider + ".conversation.ingress"
	adapter.Transport.DeliveryEndpoint = provider + ".conversation.deliver"
	if provider == "webchat" {
		adapter.Credentials = nil
		adapter.Transport.DeliveryCredentials = nil
	}
	definition.ConversationAdapters["conversations"] = adapter
	if err := catalog.Register(ctx, definition); err != nil {
		t.Fatal(err)
	}
	scope := models.Scope{Kind: "tenant", ID: provider}
	conversationtest.SeedSpace(t, store.(models.SpaceStore), scope)
	deploymentID := owner.ID
	handler := models.ExternalConversationHandler{Kind: models.ExternalConversationHandlerAgent, ID: deploymentID}
	if owner.Type == models.OwnerTypeTeam {
		handler.Kind = models.ExternalConversationHandlerTeam
	}
	binding := &skill.Binding{
		ID: provider, Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: deploymentID,
		SkillID: definition.ID, SkillVersion: definition.Version,
		EnabledConversationAdapters: []string{"conversations"}, MaximumRisk: skill.RiskLevelRead, Revision: 1,
	}
	if provider == "slack" {
		binding.Credentials = map[string]skill.CredentialReference{
			"SLACK_CONNECTION": {Kind: "slack-oauth", ID: "connection://tenant/slack"},
		}
	}
	if err := catalog.Bind(ctx, binding); err != nil {
		t.Fatal(err)
	}
	endpoint, err := externalconversation.NewExternalConversationEndpointService(store, catalog).Create(
		ctx,
		externalconversation.CreateExternalConversationEndpointRequest{
			ID: provider + "-endpoint", Scope: scope,
			Owner: owner, DeploymentID: deploymentID,
			Name: provider + " endpoint",
			Adapter: models.ExternalConversationAdapterReference{
				SkillID: definition.ID, SkillVersion: definition.Version,
				BindingID: binding.ID, BindingRevision: binding.Revision, AdapterID: "conversations",
			},
			Mode: capability.ConversationEndpointChannel, Address: provider + "-address",
			Handler: handler,
			Policy: models.ExternalConversationPolicy{
				MessageSelection: models.ExternalConversationSelectAllMessages,
				ReplyMode:        models.ExternalConversationReplyThread, IgnoreBots: true,
			},
			Status: models.ExternalConversationEndpointActive,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return catalog, endpoint
}

// FileContentStore keeps the one artifact content written to it in memory.
type FileContentStore struct {
	data  []byte
	scope models.Scope
	owner *models.ObjectiveOwner
	opens int
}

func (s *FileContentStore) Put(_ context.Context, req models.ArtifactContentWrite) (models.ArtifactStoredContent, error) {
	s.data, _ = io.ReadAll(req.Reader)
	s.scope = req.Scope
	s.owner = req.Owner
	return models.ArtifactStoredContent{ContentRef: "opaque-file", SizeBytes: int64(len(s.data)), Digest: conversationtest.Digest(string(s.data))}, nil
}

func (s *FileContentStore) Open(_ context.Context, scope models.Scope, _ string) (io.ReadCloser, error) {
	s.scope = scope
	s.opens++
	return io.NopCloser(bytes.NewReader(s.data)), nil
}

// FileConversationFixture is the Slack endpoint fixture with a catalog
// whose conversation adapter also supports attachments.
func FileConversationFixture(t *testing.T) (*sqlite.Store, *skill.Catalog, *models.ExternalConversationEndpoint) {
	t.Helper()
	store, original, endpoint := EndpointFixture(t, t.Context(), "slack")
	bound, err := original.ResolveConversationAdapterBinding(t.Context(), skill.ScopeReference{Kind: endpoint.Scope.Kind, ID: endpoint.Scope.ID}, endpoint.DeploymentID, endpoint.Adapter.BindingID, endpoint.Adapter.AdapterID)
	if err != nil {
		t.Fatal(err)
	}
	definition := skillbindingtest.SlackConversationSkillDefinition()
	definition.ID = "slack"
	definition.Name = "slack"
	adapter := definition.ConversationAdapters["conversations"]
	adapter.Features = append(adapter.Features, capability.ConversationFeatureAttachments)
	sort.Slice(adapter.Features, func(i, j int) bool { return adapter.Features[i] < adapter.Features[j] })
	definition.ConversationAdapters["conversations"] = adapter
	catalog := skill.NewCatalog()
	if err := catalog.Register(t.Context(), definition); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Bind(t.Context(), bound.Binding); err != nil {
		t.Fatal(err)
	}
	return store, catalog, endpoint
}
