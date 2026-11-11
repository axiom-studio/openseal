package runtime

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/mcp"
	"github.com/axiom-studio/openseal/pkg/skill"
)

type fakeAppCatalog struct{ entries []AppCatalogEntry }

func (c fakeAppCatalog) FindApps(_ context.Context, _ Scope, query string, limit int) ([]AppCatalogEntry, error) {
	var result []AppCatalogEntry
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

func (c fakeAppCatalog) GetApp(_ context.Context, _ Scope, slug string) (*AppCatalogEntry, error) {
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
func connectAppFixture(t *testing.T) (context.Context, *SQLiteStore, *skill.Catalog, *SkillBindingActionDispatcher, Scope, *atomic.Bool) {
	t.Helper()
	ctx, store, catalog, dispatcher, scope := credentialRequestDispatchFixture(t)
	if err := catalog.Register(ctx, mcp.SkillDefinition()); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Bind(ctx, &skill.Binding{ID: "app-actions", Revision: 1, Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "agent",
		SkillID: SkillManagementSkillID, SkillVersion: SkillManagementSkillVersion, AllowedActions: []string{SkillActionFindApps, SkillActionRequestConnection}, MaximumRisk: skill.RiskLevelRead}); err != nil {
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
	dispatcher.UseAppCatalog(&AppCatalogRef{})
	dispatcher.apps.Set(fakeAppCatalog{entries: []AppCatalogEntry{
		{Slug: "linear", Name: "Linear", Summary: "Issue tracking", Official: true, SignIn: AppSignInOAuth, LogoSHA256: linearLogo},
		{Slug: "acme/linear-tools", Name: "Linear Tools", SignIn: "weird"},
		{Slug: "neondatabase/mcp-server-neon", Name: "Neon", SignIn: AppSignInOAuth},
	}})
	return ctx, store, catalog, dispatcher, scope, connected
}

func TestFindAppsListsCatalogAppsAndWhetherTheyAreConnected(t *testing.T) {
	ctx, store, _, dispatcher, scope, connected := connectAppFixture(t)
	run := createClaimedSkillActionRun(t, ctx, store, scope, "agent", "worker")
	result, err := dispatcher.findApps(ctx, ActionDispatchInput{Arguments: map[string]interface{}{"query": "linear", "limit": 8}}, run, ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	apps := result["apps"].([]interface{})
	first, second := apps[0].(map[string]interface{}), apps[1].(map[string]interface{})
	if len(apps) != 2 || first["slug"] != "linear" || first["official"] != true || first["signIn"] != "oauth" || first["connected"] != false || second["signIn"] != "unknown" {
		t.Fatalf("apps: %#v", apps)
	}
	// Every app says what it is and the one action that connects it.
	for _, app := range apps {
		if item := app.(map[string]interface{}); item["type"] != "mcp-server" || item["setup"] != SkillActionRequestConnection {
			t.Fatalf("app without its type and setup path: %#v", item)
		}
	}
	connected.Store(true)
	result, err = dispatcher.findApps(ctx, ActionDispatchInput{Arguments: map[string]interface{}{"query": "linear"}}, run, ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	first = result["apps"].([]interface{})[0].(map[string]interface{})
	tools, _ := first["tools"].([]interface{})
	if first["connected"] != true || len(tools) != 1 || tools[0] != "mcp.linear.list_issues" {
		t.Fatalf("connected app: %#v", first)
	}
	if _, err := dispatcher.findApps(ctx, ActionDispatchInput{Arguments: map[string]interface{}{"query": "linear", "limit": 9}}, run, ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}); err != nil {
		t.Fatalf("an oversized limit is capped by the schema, not an error here: %v", err)
	}
}

func TestFindAppsAlsoListsSkillsTheUserCanSetUp(t *testing.T) {
	ctx, store, _, dispatcher, scope, _ := connectAppFixture(t)
	var queried skill.DiscoveryRequest
	dispatcher.discovery = skill.DiscoveryProviderFunc(func(_ context.Context, request skill.DiscoveryRequest) (*skill.DiscoveryPage, error) {
		queried = request
		return &skill.DiscoveryPage{Items: []skill.DiscoveryCandidate{
			{ID: "google-workspace", Version: "1.4.0", SourceIdentity: "axiomstudio/skill-google-workspace", Name: "Google Workspace", Description: "Gmail, Calendar and Drive", Readiness: skill.DiscoveryReadinessNeedsInstallation},
			{ID: "gmail-bound", Version: "1.0.0", Name: "Gmail (bound)", Readiness: skill.DiscoveryReadinessBindable},
			{ID: "gmail-blocked", Version: "1.0.0", Name: "Gmail (blocked)", Readiness: skill.DiscoveryReadinessUnavailable,
				Compatibility: []skill.DiscoveryCompatibility{{Requirement: "network", Compatible: false, Evidence: "egress blocked"}}},
		}}, nil
	})
	run := createClaimedSkillActionRun(t, ctx, store, scope, "agent", "worker")
	result, err := dispatcher.findApps(ctx, ActionDispatchInput{Arguments: map[string]interface{}{"query": "gmail", "limit": 3}}, run, ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	if queried.Query != "gmail" || queried.Limit != 3 || queried.DeploymentID != "agent" {
		t.Fatalf("discovery request: %#v", queried)
	}
	skills := result["skills"].([]interface{})
	if len(skills) != 2 {
		t.Fatalf("skills: %#v", skills)
	}
	first, second := skills[0].(map[string]interface{}), skills[1].(map[string]interface{})
	if first["skillId"] != "google-workspace" || first["skillVersion"] != "latest" || first["sourceIdentity"] != "axiomstudio/skill-google-workspace" ||
		first["setupKind"] != "install" || first["summary"] != "Gmail, Calendar and Drive" || second["setupKind"] != "configure" {
		t.Fatalf("skills: %#v", skills)
	}
	for _, found := range skills {
		if item := found.(map[string]interface{}); item["type"] != "skill" || item["setup"] != SkillActionRequestSetup {
			t.Fatalf("skill without its type and setup path: %#v", item)
		}
	}
}

func TestFindAppsRunsThroughTheActionWorker(t *testing.T) {
	ctx, store, catalog, dispatcher, scope, _ := connectAppFixture(t)
	f, call := proposeInteractionCall(t, ctx, store, catalog, scope, SkillActionFindApps, map[string]interface{}{"query": "neon", "limit": 3})
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
	f, call := proposeInteractionCall(t, ctx, store, catalog, scope, SkillActionRequestConnection, map[string]interface{}{"app": "linear", "reason": "Connect Linear so I can list your issues"})
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
	again, err := dispatcher.requestConnection(ctx, ActionDispatchInput{Call: &ActionCall{ID: "again"}, Arguments: map[string]interface{}{"app": "linear"}}, parked, "agent", ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"})
	if err != nil || again["setupRequest"].(map[string]interface{})["id"] != request.ID {
		t.Fatalf("second card: %#v %v", again, err)
	}
	// Once connected for the run, the result is the app's tools.
	connected.Store(true)
	done, err := dispatcher.requestConnection(ctx, ActionDispatchInput{Call: &ActionCall{ID: "after"}, Arguments: map[string]interface{}{"app": "linear"}}, parked, "agent", ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"})
	if err != nil || done["setupRequest"] != nil {
		t.Fatalf("connected app: %#v %v", done, err)
	}
	tools := done["connected"].(map[string]interface{})["tools"].([]interface{})
	if len(tools) != 1 || tools[0] != "mcp.linear.list_issues" {
		t.Fatalf("tools: %#v", tools)
	}
}

func TestRequestConnectionByAddressAndRefusals(t *testing.T) {
	ctx, store, _, dispatcher, scope, connected := connectAppFixture(t)
	run := createClaimedSkillActionRun(t, ctx, store, scope, "agent", "worker")
	run.Context = map[string]interface{}{"conversationId": "chat", "triggerMessageId": "message"}
	result, err := dispatcher.requestConnection(ctx, ActionDispatchInput{Call: &ActionCall{ID: "custom"}, Arguments: map[string]interface{}{"app": map[string]interface{}{"address": "https://MCP.Example.com/mcp"}}}, run, "agent", ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	created := result["setupRequest"].(map[string]interface{})
	app := created["app"].(map[string]interface{})
	if app["address"] != "https://mcp.example.com/mcp" || app["name"] != "mcp.example.com" || app["signIn"] != "unknown" || created["reason"] == "" {
		t.Fatalf("custom request: %#v", created)
	}
	for name, arguments := range map[string]map[string]interface{}{
		"unknown slug":   {"app": "nope"},
		"path slug":      {"app": "../etc"},
		"http address":   {"app": map[string]interface{}{"address": "http://mcp.example.com"}},
		"query address":  {"app": map[string]interface{}{"address": "https://mcp.example.com/?key=1"}},
		"extra property": {"app": map[string]interface{}{"address": "https://mcp.example.com", "token": "x"}},
	} {
		if _, err := dispatcher.requestConnection(ctx, ActionDispatchInput{Call: &ActionCall{ID: "refused"}, Arguments: arguments}, run, "agent", ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}); !isInteractionRefusal(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	slashed, err := dispatcher.requestConnection(ctx, ActionDispatchInput{Call: &ActionCall{ID: "neon"}, Arguments: map[string]interface{}{"app": "neondatabase/mcp-server-neon"}}, run, "agent", ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"})
	if err != nil || slashed["setupRequest"].(map[string]interface{})["app"].(map[string]interface{})["catalogSlug"] != "neondatabase/mcp-server-neon" {
		t.Fatalf("slug with a slash: %#v %v", slashed, err)
	}
	// A connection to the same address counts as connected.
	connected.Store(true)
	done, err := dispatcher.requestConnection(ctx, ActionDispatchInput{Call: &ActionCall{ID: "linear-address"}, Arguments: map[string]interface{}{"app": "https://mcp.linear.app/mcp"}}, run, "agent", ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"})
	if err != nil || done["connected"] == nil {
		t.Fatalf("address of a connected app: %#v %v", done, err)
	}
	// No conversation: the card cannot be shown.
	run.Context = nil
	connected.Store(false)
	if _, err := dispatcher.requestConnection(ctx, ActionDispatchInput{Call: &ActionCall{ID: "routine"}, Arguments: map[string]interface{}{"app": "linear"}}, run, "agent", ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}); !isInteractionRefusal(err) {
		t.Fatalf("no conversation: %v", err)
	}
}

func isInteractionRefusal(err error) bool {
	var refusal *interactionRequestRefusal
	return errors.As(err, &refusal)
}

func TestConnectAppRequestValidation(t *testing.T) {
	now := time.Now().UTC()
	base := func() *SkillSetupRequest {
		return &SkillSetupRequest{ID: "connect-app:call", Scope: Scope{Kind: "tenant", ID: "a"}, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, DeploymentID: "agent", AgentID: "agent", ConversationID: "chat", TriggerMessageID: "message",
			RunID: "run", ActionCallID: "call", Kind: SkillSetupKindConnectApp, SkillID: mcp.SkillID, SkillVersion: mcp.SkillVersion, SkillName: "Linear",
			App: &ConnectAppTarget{CatalogSlug: "linear", Name: "Linear"}, Reason: "Connect Linear", Status: "pending", Revision: 1, CreatedAt: now, UpdatedAt: now}
	}
	if err := base().Validate(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*SkillSetupRequest){
		"no app":          func(r *SkillSetupRequest) { r.App = nil },
		"slug and addr":   func(r *SkillSetupRequest) { r.App.Address = "https://mcp.linear.app/mcp" },
		"bad address":     func(r *SkillSetupRequest) { r.App = &ConnectAppTarget{Address: "http://x", Name: "x"} },
		"binding":         func(r *SkillSetupRequest) { r.BindingID, r.BindingRevision = "mcp:c1", 1 },
		"app on install":  func(r *SkillSetupRequest) { r.Kind = "install" },
		"bad logo":        func(r *SkillSetupRequest) { r.App.LogoSHA256 = "nothex" },
		"unknown sign-in": func(r *SkillSetupRequest) { r.App.SignIn = "magic" },
	} {
		r := base()
		change(r)
		if err := r.Validate(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// An app is only ever connected and a Skill only ever set up: each action
// refuses the other type and names the right one.
func TestAppsAndSkillsCannotTakeEachOthersSetupPath(t *testing.T) {
	ctx, store, _, dispatcher, scope, _ := connectAppFixture(t)
	dispatcher.discovery = skill.DiscoveryProviderFunc(func(_ context.Context, request skill.DiscoveryRequest) (*skill.DiscoveryPage, error) {
		return &skill.DiscoveryPage{Items: []skill.DiscoveryCandidate{{ID: "google-workspace", Version: "1.4.0", SourceIdentity: "axiomstudio/skill-google-workspace",
			Name: "Google Workspace", Readiness: skill.DiscoveryReadinessNeedsInstallation}}}, nil
	})
	run := createClaimedSkillActionRun(t, ctx, store, scope, "agent", "worker")
	run.Context = map[string]interface{}{"conversationId": "chat", "triggerMessageId": "message"}
	var refusal *interactionRequestRefusal

	// The app's slug is not a Skill.
	_, err := dispatcher.requestSkillSetup(ctx, ActionDispatchInput{Call: &ActionCall{ID: "setup-app", Scope: scope, RunID: run.ID},
		Arguments: map[string]interface{}{"kind": "install", "skillId": "linear", "skillVersion": "latest", "reason": "Connect Linear"}}, run, "agent", ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"})
	if !errors.As(err, &refusal) || !strings.Contains(refusal.message, "request_connection") || !strings.Contains(refusal.message, "mcp-server") {
		t.Fatalf("request_setup for an app = %v", err)
	}
	// The Skill's id is not an app.
	_, err = dispatcher.requestConnection(ctx, ActionDispatchInput{Call: &ActionCall{ID: "connect-skill"}, Arguments: map[string]interface{}{"app": "google-workspace"}}, run, "agent", ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"})
	if !errors.As(err, &refusal) || !strings.Contains(refusal.message, "request_setup") {
		t.Fatalf("request_connection for a Skill = %v", err)
	}
	if requests, _ := store.ListSkillSetupRequests(ctx, scope, "agent", "chat"); len(requests) != 0 {
		t.Fatalf("a refused path created a request: %#v", requests)
	}
	// Each takes its own type.
	connect, err := dispatcher.requestConnection(ctx, ActionDispatchInput{Call: &ActionCall{ID: "connect-app"}, Arguments: map[string]interface{}{"app": "linear"}}, run, "agent", ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"})
	if err != nil || connect["setupRequest"] == nil {
		t.Fatalf("request_connection for an app = %#v, %v", connect, err)
	}
	setup, err := dispatcher.requestSkillSetup(ctx, ActionDispatchInput{Call: &ActionCall{ID: "setup-skill", Scope: scope, RunID: run.ID},
		Arguments: map[string]interface{}{"kind": "install", "skillId": "google-workspace", "skillVersion": "latest", "sourceIdentity": "axiomstudio/skill-google-workspace", "reason": "Use Gmail"}}, run, "agent", ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"})
	if errors.As(err, &refusal) && strings.Contains(refusal.message, "mcp-server") {
		t.Fatalf("a Skill was refused as an app: %v", err)
	}
	_ = setup
}

// A per_person app is asked of the person in the run; a card is reused only
// within its own run and for the same person.
func TestRequestConnectionPerPersonModeAndNoCrossRunReuse(t *testing.T) {
	ctx, store, catalog, dispatcher, scope, _ := connectAppFixture(t)
	catalog.SetHostBindingProvider(func(context.Context, skill.ScopeReference, string, skill.RunActor) (*skill.HostBindingSet, error) {
		return &skill.HostBindingSet{Fingerprint: "personal", PersonalApps: map[string]string{"linear": "Linear"}}, nil
	})
	umbrella := ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}
	run := createClaimedSkillActionRun(t, ctx, store, scope, "agent", "worker")
	run.Context = map[string]interface{}{"conversationId": "chat", "triggerMessageId": "message"}
	run.ActingUser = "7"
	first, err := dispatcher.requestConnection(ctx, ActionDispatchInput{Call: &ActionCall{ID: "first"}, Arguments: map[string]interface{}{"app": "linear"}}, run, "agent", umbrella, umbrella)
	if err != nil {
		t.Fatal(err)
	}
	card := first["setupRequest"].(map[string]interface{})
	stored, err := store.GetSkillSetupRequest(ctx, scope, card["id"].(string))
	if err != nil || stored.Mode != "per_person" || stored.RequestedFor != "7" {
		t.Fatalf("per_person card = %#v %v", stored, err)
	}
	again, err := dispatcher.requestConnection(ctx, ActionDispatchInput{Call: &ActionCall{ID: "again"}, Arguments: map[string]interface{}{"app": "linear"}}, run, "agent", umbrella, umbrella)
	if err != nil || again["setupRequest"].(map[string]interface{})["id"] != stored.ID {
		t.Fatalf("same run must reuse its card: %#v %v", again, err)
	}
	other := *run
	other.ID = "other-run"
	second, err := dispatcher.requestConnection(ctx, ActionDispatchInput{Call: &ActionCall{ID: "second"}, Arguments: map[string]interface{}{"app": "linear"}}, &other, "agent", umbrella, umbrella)
	if err != nil || second["setupRequest"].(map[string]interface{})["id"] == stored.ID {
		t.Fatalf("another run reused the card: %#v %v", second, err)
	}
	other.ID, other.ActingUser = "third-run", ""
	if _, err := dispatcher.requestConnection(ctx, ActionDispatchInput{Call: &ActionCall{ID: "third"}, Arguments: map[string]interface{}{"app": "linear"}}, &other, "agent", umbrella, umbrella); !isInteractionRefusal(err) {
		t.Fatalf("a per_person app without a person must be refused: %v", err)
	}
	// An app the umbrella shares (not personal) is asked of its managers.
	catalog.SetHostBindingProvider(func(context.Context, skill.ScopeReference, string, skill.RunActor) (*skill.HostBindingSet, error) {
		return nil, nil
	})
	other.ID, other.ActingUser = "fourth-run", "7"
	shared, err := dispatcher.requestConnection(ctx, ActionDispatchInput{Call: &ActionCall{ID: "fourth"}, Arguments: map[string]interface{}{"app": "linear"}}, &other, "agent", umbrella, umbrella)
	if err != nil {
		t.Fatal(err)
	}
	managed, _ := store.GetSkillSetupRequest(ctx, scope, shared["setupRequest"].(map[string]interface{})["id"].(string))
	if managed == nil || managed.Mode != "" || managed.RequestedFor != "7" {
		t.Fatalf("manager card = %#v", managed)
	}
}
