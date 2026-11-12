package runtime

import (
	"context"
	"testing"

	"github.com/axiom-studio/openseal/pkg/mcp"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding/skillbindingtest"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestFindAppsRunsThroughTheActionWorker(t *testing.T) {
	ctx, store, catalog, dispatcher, scope, _ := skillbindingtest.ConnectAppFixture(t)
	f, call := proposeInteractionCall(t, ctx, store, catalog, scope, skillbinding.SkillActionFindApps, map[string]interface{}{"query": "neon", "limit": 3})
	executed := f.Execute(action.NewActionWorker(store, catalog, nil, dispatcher), call.ID)
	apps, _ := executed.Call.Output["apps"].([]interface{})
	if executed.Call.Status != models.ActionCallStatusSucceeded || len(apps) != 1 || apps[0].(map[string]interface{})["slug"] != "neondatabase/mcp-server-neon" {
		t.Fatalf("find_apps: %#v", executed.Call)
	}
	if _, card := executed.Call.Output["setupRequest"]; card {
		t.Fatal("a search must not ask for a connect card")
	}
	assertRunNotParkedByCall(t, f)
}

func TestRequestConnectionParksTheRunOnOneConnectCard(t *testing.T) {
	ctx, store, catalog, dispatcher, scope, connected := skillbindingtest.ConnectAppFixture(t)
	f, call := proposeInteractionCall(t, ctx, store, catalog, scope, agentrun.SkillActionRequestConnection, map[string]interface{}{"app": "linear", "reason": "Connect Linear so I can list your issues"})
	executed := f.Execute(action.NewActionWorker(store, catalog, nil, dispatcher), call.ID)
	if executed.Call.Status != models.ActionCallStatusSucceeded || executed.Call.Output["setupRequest"] == nil {
		t.Fatalf("request_connection: %#v", executed.Call)
	}
	requests, err := store.ListSkillSetupRequests(ctx, scope, "agent", "chat")
	if err != nil || len(requests) != 1 {
		t.Fatalf("requests: %#v %v", requests, err)
	}
	request := requests[0]
	if request.Kind != models.SkillSetupKindConnectApp || request.SkillID != mcp.SkillID || request.SkillName != "Linear" || request.App == nil ||
		*request.App != (models.ConnectAppTarget{CatalogSlug: "linear", Name: "Linear", Official: true, SignIn: models.AppSignInOAuth, LogoSHA256: skillbindingtest.LinearLogo}) {
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
func dispatchSkillManagementAction(t *testing.T, ctx context.Context, catalog *skill.Catalog, dispatcher *skillbinding.SkillBindingActionDispatcher, run *models.AgentRun, action, callID string, arguments map[string]interface{}) (map[string]interface{}, error) {
	t.Helper()
	bound, err := catalog.Resolve(ctx, skill.ScopeReference{Kind: run.Scope.Kind, ID: run.Scope.ID}, "agent", models.SkillManagementSkillID, skillbinding.SkillManagementSkillVersion, action)
	if err != nil {
		t.Fatal(err)
	}
	return dispatcher.DispatchAction(ctx, agentrun.ActionDispatchInput{Run: run, Bound: bound, Arguments: arguments,
		Call: &models.ActionCall{ID: callID, Scope: run.Scope, RunID: run.ID, DeploymentID: "agent"}})
}
