package runtime

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/axiom-studio/openseal/pkg/mcp"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/skill"
)

type fakeAppCatalog struct {
	entries []skillbinding.AppCatalogEntry
}

func (c fakeAppCatalog) FindApps(_ context.Context, _ Scope, query string, limit int) ([]skillbinding.AppCatalogEntry, error) {
	var result []skillbinding.AppCatalogEntry
	for _, entry := range c.entries {
		if strings.Contains(strings.ToLower(entry.Name), strings.ToLower(query)) {
			result = append(result, entry)
		}
	}
	if len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func (c fakeAppCatalog) GetApp(_ context.Context, _ Scope, slug string) (*skillbinding.AppCatalogEntry, error) {
	for _, entry := range c.entries {
		if entry.Slug == slug {
			copy := entry
			return &copy, nil
		}
	}
	return nil, nil
}

var linearLogo = strings.Repeat("ab", 32)

// connectAppFixture is an Agent whose run sees a Linear connection only once
// connected is set, as the host supplies connected apps per run.
func connectAppFixture(t *testing.T) (context.Context, *sqlite.Store, *skill.Catalog, *skillbinding.SkillBindingActionDispatcher, Scope, *atomic.Bool) {
	t.Helper()
	ctx, store, catalog, dispatcher, scope := credentialRequestDispatchFixture(t)
	if err := catalog.Register(ctx, mcp.SkillDefinition()); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Bind(ctx, &skill.Binding{ID: "app-actions", Revision: 1, Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "agent",
		SkillID: SkillManagementSkillID, SkillVersion: skillbinding.SkillManagementSkillVersion, AllowedActions: []string{skillbinding.SkillActionFindApps, agentrun.SkillActionRequestConnection}, MaximumRisk: skill.RiskLevelRead}); err != nil {
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
	apps.Set(fakeAppCatalog{entries: []skillbinding.AppCatalogEntry{
		{Slug: "linear", Name: "Linear", Summary: "Issue tracking", Official: true, SignIn: AppSignInOAuth, LogoSHA256: linearLogo},
		{Slug: "acme/linear-tools", Name: "Linear Tools", SignIn: "weird"},
		{Slug: "neondatabase/mcp-server-neon", Name: "Neon", SignIn: AppSignInOAuth},
	}})
	dispatcher.UseAppCatalog(apps)
	return ctx, store, catalog, dispatcher, scope, connected
}

func TestFindAppsRunsThroughTheActionWorker(t *testing.T) {
	ctx, store, catalog, dispatcher, scope, _ := connectAppFixture(t)
	f, call := proposeInteractionCall(t, ctx, store, catalog, scope, skillbinding.SkillActionFindApps, map[string]interface{}{"query": "neon", "limit": 3})
	executed := f.execute(NewActionWorker(store, catalog, nil, dispatcher), call.ID)
	apps, _ := executed.Call.Output["apps"].([]interface{})
	if executed.Call.Status != ActionCallStatusSucceeded || len(apps) != 1 || apps[0].(map[string]interface{})["slug"] != "neondatabase/mcp-server-neon" {
		t.Fatalf("find_apps: %#v", executed.Call)
	}
	if _, card := executed.Call.Output["setupRequest"]; card {
		t.Fatal("a search must not ask for a connect card")
	}
	assertRunNotParkedByCall(t, f)
}

func TestRequestConnectionParksTheRunOnOneConnectCard(t *testing.T) {
	ctx, store, catalog, dispatcher, scope, connected := connectAppFixture(t)
	f, call := proposeInteractionCall(t, ctx, store, catalog, scope, agentrun.SkillActionRequestConnection, map[string]interface{}{"app": "linear", "reason": "Connect Linear so I can list your issues"})
	executed := f.execute(NewActionWorker(store, catalog, nil, dispatcher), call.ID)
	if executed.Call.Status != ActionCallStatusSucceeded || executed.Call.Output["setupRequest"] == nil {
		t.Fatalf("request_connection: %#v", executed.Call)
	}
	requests, err := store.ListSkillSetupRequests(ctx, scope, "agent", "chat")
	if err != nil || len(requests) != 1 {
		t.Fatalf("requests: %#v %v", requests, err)
	}
	request := requests[0]
	if request.Kind != SkillSetupKindConnectApp || request.SkillID != mcp.SkillID || request.SkillName != "Linear" || request.App == nil ||
		*request.App != (ConnectAppTarget{CatalogSlug: "linear", Name: "Linear", Official: true, SignIn: AppSignInOAuth, LogoSHA256: linearLogo}) {
		t.Fatalf("request: %#v %#v", request, request.App)
	}
	// The call's output names the one card the loop parks the Run on.
	if card, _ := executed.Call.Output["setupRequest"].(map[string]interface{}); card["id"] != request.ID {
		t.Fatalf("request_connection output = %#v", executed.Call.Output)
	}
	parked := assertRunNotParkedByCall(t, f)
	changes, err := mustConversationChanges(t, ctx, store, scope)
	if err != nil || len(changes.SkillSetupRequests) != 1 || changes.SkillSetupRequests[0].App == nil {
		t.Fatalf("change set: %#v %v", changes, err)
	}
	// Asking again in the conversation shows the same card, never a second one.
	again, err := dispatchSkillManagementAction(t, ctx, catalog, dispatcher, parked, agentrun.SkillActionRequestConnection, "again", map[string]interface{}{"app": "linear"})
	if card, _ := again["setupRequest"].(map[string]interface{}); err != nil || card["id"] != request.ID {
		t.Fatalf("second card: %#v %v", again, err)
	}
	// Once connected for the run, the result is the app's tools.
	connected.Store(true)
	done, err := dispatchSkillManagementAction(t, ctx, catalog, dispatcher, parked, agentrun.SkillActionRequestConnection, "after", map[string]interface{}{"app": "linear"})
	if err != nil || done["setupRequest"] != nil || done["refused"] != nil {
		t.Fatalf("connected app: %#v %v", done, err)
	}
	tools := done["connected"].(map[string]interface{})["tools"].([]interface{})
	if len(tools) != 1 || tools[0] != "mcp.linear.list_issues" {
		t.Fatalf("tools: %#v", tools)
	}
}

// dispatchSkillManagementAction dispatches one SkillManagement action of the
// Agent's run as a call of that run.
func dispatchSkillManagementAction(t *testing.T, ctx context.Context, catalog *skill.Catalog, dispatcher *skillbinding.SkillBindingActionDispatcher, run *AgentRun, action, callID string, arguments map[string]interface{}) (map[string]interface{}, error) {
	t.Helper()
	bound, err := catalog.Resolve(ctx, skill.ScopeReference{Kind: run.Scope.Kind, ID: run.Scope.ID}, "agent", SkillManagementSkillID, skillbinding.SkillManagementSkillVersion, action)
	if err != nil {
		t.Fatal(err)
	}
	return dispatcher.DispatchAction(ctx, agentrun.ActionDispatchInput{Run: run, Bound: bound, Arguments: arguments,
		Call: &ActionCall{ID: callID, Scope: run.Scope, RunID: run.ID, DeploymentID: "agent"}})
}
