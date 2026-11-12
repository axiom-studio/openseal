package skillbindingtest

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/mcp"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// ClaimedSkillActionRun creates a chat Run of deploymentID and leases it to
// workerID.
func ClaimedSkillActionRun(t *testing.T, ctx context.Context, store *sqlite.Store, scope models.Scope, deploymentID, workerID string) *models.AgentRun {
	t.Helper()
	run, err := portfolio.NewPortfolioService(store).CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Kind: models.RunKindConversation, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: deploymentID}, AssignedAgentID: deploymentID,
		Goal: "Manage Skills", Source: models.RunSourceChat,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimNextAgentRun(ctx, models.AgentRunClaim{Scope: scope, WorkerID: workerID, Now: time.Now().UTC(), LeaseDuration: time.Minute, AgingInterval: time.Minute})
	if err != nil || claimed == nil || claimed.ID != run.ID {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	return claimed
}

// CredentialRequestDispatchFixture is a dispatcher for an Agent bound to
// request setup and credentials, with an empty discovery catalog.
func CredentialRequestDispatchFixture(t *testing.T) (context.Context, *sqlite.Store, *skill.Catalog, *skillbinding.SkillBindingActionDispatcher, models.Scope) {
	t.Helper()
	ctx := context.Background()
	store := NewStore(t)
	scope := models.Scope{Kind: "tenant", ID: "a"}
	catalog := SkillActionCatalog(t, ctx, scope, "agent")
	if err := catalog.Bind(ctx, &skill.Binding{ID: "interaction-actions", Revision: 1, Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "agent",
		SkillID: models.SkillManagementSkillID, SkillVersion: skillbinding.SkillManagementSkillVersion, AllowedActions: []string{agentrun.SkillActionRequestSetup, agentrun.SkillActionRequestCredential}, MaximumRisk: skill.RiskLevelRead}); err != nil {
		t.Fatal(err)
	}
	// The bundled live browser is never in the installable catalog.
	provider := skill.DiscoveryProviderFunc(func(context.Context, skill.DiscoveryRequest) (*skill.DiscoveryPage, error) {
		return &skill.DiscoveryPage{}, nil
	})
	dispatcher, err := skillbinding.NewSkillBindingActionDispatcher(store, catalog, nil, provider)
	if err != nil {
		t.Fatal(err)
	}
	return ctx, store, catalog, dispatcher, scope
}

// LinearLogo is the logo digest of the Linear app in ConnectAppFixture.
var LinearLogo = strings.Repeat("ab", 32)

// ConnectAppFixture is an Agent whose run sees a Linear connection only once
// connected is set, as the host supplies connected apps per run.
func ConnectAppFixture(t *testing.T) (context.Context, *sqlite.Store, *skill.Catalog, *skillbinding.SkillBindingActionDispatcher, models.Scope, *atomic.Bool) {
	t.Helper()
	ctx, store, catalog, dispatcher, scope := CredentialRequestDispatchFixture(t)
	if err := catalog.Register(ctx, mcp.SkillDefinition()); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Bind(ctx, &skill.Binding{ID: "app-actions", Revision: 1, Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "agent",
		SkillID: models.SkillManagementSkillID, SkillVersion: skillbinding.SkillManagementSkillVersion, AllowedActions: []string{skillbinding.SkillActionFindApps, agentrun.SkillActionRequestConnection}, MaximumRisk: skill.RiskLevelRead}); err != nil {
		t.Fatal(err)
	}
	connected := &atomic.Bool{}
	schema := map[string]interface{}{"type": "object", "properties": map[string]interface{}{"query": map[string]interface{}{"type": "string"}}}
	catalog.SetHostBindingProvider(func(_ context.Context, skillScope skill.ScopeReference, deploymentID string, _ skill.RunActor) (*skill.HostBindingSet, error) {
		if !connected.Load() || skillScope.ID != scope.ID {
			return nil, nil
		}
		return &skill.HostBindingSet{Fingerprint: "linear@2", Services: map[string][]string{"mcp:c1": {"linear", "Linear"}}, Bindings: []*skill.Binding{{
			ID: "mcp:c1", Revision: 2, Scope: skillScope, DeploymentID: deploymentID, SkillID: mcp.SkillID, SkillVersion: mcp.SkillVersion,
			AllowedActions: []string{mcp.Read}, MaximumRisk: skill.RiskLevelRead, Slug: "linear",
			Config: map[string]interface{}{"endpoint": "https://mcp.linear.app/mcp", "transport": "streamable-http", "auth": map[string]interface{}{"kind": "none"}},
			Tools: []skill.BindingTool{{Name: "list_issues", Description: "List issues", Effect: skill.BindingToolEffectRead, InputSchema: schema,
				Arguments: map[string]interface{}{"tool": "list_issues", "hash": strings.Repeat("0", 64)}}},
		}}}, nil
	})
	apps := &skillbinding.AppCatalogRef{}
	apps.Set(AppCatalog{Entries: []skillbinding.AppCatalogEntry{
		{Slug: "linear", Name: "Linear", Summary: "Issue tracking", Official: true, SignIn: models.AppSignInOAuth, LogoSHA256: LinearLogo},
		{Slug: "acme/linear-tools", Name: "Linear Tools", SignIn: "weird"},
		{Slug: "neondatabase/mcp-server-neon", Name: "Neon", SignIn: models.AppSignInOAuth},
	}})
	dispatcher.UseAppCatalog(apps)
	return ctx, store, catalog, dispatcher, scope, connected
}

// AppCatalog is an app catalog of fixed entries; FindApps matches names.
type AppCatalog struct {
	Entries []skillbinding.AppCatalogEntry
}

func (c AppCatalog) FindApps(_ context.Context, _ models.Scope, query string, limit int) ([]skillbinding.AppCatalogEntry, error) {
	var result []skillbinding.AppCatalogEntry
	for _, entry := range c.Entries {
		if strings.Contains(strings.ToLower(entry.Name), strings.ToLower(query)) {
			result = append(result, entry)
		}
	}
	if len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func (c AppCatalog) GetApp(_ context.Context, _ models.Scope, slug string) (*skillbinding.AppCatalogEntry, error) {
	for _, entry := range c.Entries {
		if entry.Slug == slug {
			copy := entry
			return &copy, nil
		}
	}
	return nil, nil
}
