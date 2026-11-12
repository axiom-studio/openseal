package skillbinding_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding/skillbindingtest"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestSkillSetupIsExplicitScopedAndIdempotentWithoutGrantingAccess(t *testing.T) {
	ctx := context.Background()
	store := kerneltest.NewStore(t)
	scope := models.Scope{Kind: "tenant", ID: "a"}
	deploymentID := "agent"
	catalog := skillbindingtest.SkillActionCatalog(t, ctx, scope, deploymentID)
	provider := skill.DiscoveryProviderFunc(func(_ context.Context, r skill.DiscoveryRequest) (*skill.DiscoveryPage, error) {
		if r.Scope.ID != "a" || r.DeploymentID != "agent" {
			t.Fatal("authority not derived from run")
		}
		return &skill.DiscoveryPage{Items: []skill.DiscoveryCandidate{{ID: "reddit.reader", Version: "1.0.0", Name: "Reddit", Readiness: skill.DiscoveryReadinessBindable, Actions: []skill.DiscoveryAction{{Name: "read", Risk: skill.RiskLevelRead}}}}}, nil
	})
	dispatcher, err := skillbinding.NewSkillBindingActionDispatcher(store, catalog, nil, provider)
	if err != nil {
		t.Fatal(err)
	}
	run := skillbindingtest.ClaimedSkillActionRun(t, ctx, store, scope, deploymentID, "worker")
	run.Context = map[string]interface{}{"conversationId": "chat", "triggerMessageId": "message"}
	args := map[string]interface{}{"kind": "configure", "skillId": "reddit.reader", "skillVersion": "1.0.0", "reason": "Read the community", "requiredActions": []interface{}{"read"}}
	input := agentrun.ActionDispatchInput{Call: &models.ActionCall{ID: "call-1", Scope: scope, RunID: run.ID}, Arguments: args}
	result, err := dispatcher.RequestSkillSetup(ctx, input, run, deploymentID, models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: deploymentID}, models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: deploymentID})
	if err != nil {
		t.Fatal(err)
	}
	request := result["setupRequest"].(map[string]interface{})
	if request["status"] != "pending" {
		t.Fatal(request)
	}
	input.Call.ID = "call-2"
	repeated, err := dispatcher.RequestSkillSetup(ctx, input, run, deploymentID, models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: deploymentID}, models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: deploymentID})
	if err != nil || repeated["setupRequest"].(map[string]interface{})["id"] != request["id"] {
		t.Fatalf("duplicate not reused: %#v %v", repeated, err)
	}
	bindings, _ := catalog.ListBindings(ctx, skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, deploymentID)
	if len(bindings) != 1 {
		t.Fatalf("request granted access: %#v", bindings)
	}
	args["skillVersion"] = "forged"
	input.Call.ID = "call-3"
	if _, err := dispatcher.RequestSkillSetup(ctx, input, run, deploymentID, models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: deploymentID}, models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: deploymentID}); err == nil {
		t.Fatal("unverified Skill accepted")
	}
	args["skillVersion"] = "1.0.0"
	args["kind"] = "reauthorize"
	args["bindingId"] = "foreign-binding"
	if _, err := dispatcher.RequestSkillSetup(ctx, input, run, deploymentID, models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: deploymentID}, models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: deploymentID}); err == nil {
		t.Fatal("unbound reconnect accepted")
	}
}

func TestSkillSetupLatestResolvesExactAuthorizedIdentity(t *testing.T) {
	ctx := context.Background()
	store := kerneltest.NewStore(t)
	scope := models.Scope{Kind: "tenant", ID: "a"}
	deploymentID := "agent"
	catalog := skillbindingtest.SkillActionCatalog(t, ctx, scope, deploymentID)
	provider := skill.DiscoveryProviderFunc(func(_ context.Context, r skill.DiscoveryRequest) (*skill.DiscoveryPage, error) {
		if r.Scope.ID != "a" || r.DeploymentID != "agent" {
			t.Fatal("authority not derived from run")
		}
		return &skill.DiscoveryPage{Items: []skill.DiscoveryCandidate{{ID: "reddit.reader", Version: "2.0.0", Name: "Reddit", Readiness: skill.DiscoveryReadinessBindable, Actions: []skill.DiscoveryAction{{Name: "read", Risk: skill.RiskLevelRead}}}}}, nil
	})
	dispatcher, err := skillbinding.NewSkillBindingActionDispatcher(store, catalog, nil, provider)
	if err != nil {
		t.Fatal(err)
	}
	run := skillbindingtest.ClaimedSkillActionRun(t, ctx, store, scope, deploymentID, "worker")
	run.Context = map[string]interface{}{"conversationId": "chat", "triggerMessageId": "message"}
	args := map[string]interface{}{"kind": "configure", "skillId": "reddit.reader", "skillVersion": "latest", "reason": "Read the community", "requiredActions": []interface{}{"read"}}
	input := agentrun.ActionDispatchInput{Call: &models.ActionCall{ID: "call-1", Scope: scope, RunID: run.ID}, Arguments: args}
	result, err := dispatcher.RequestSkillSetup(ctx, input, run, deploymentID, models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: deploymentID}, models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: deploymentID})
	if err != nil {
		t.Fatal(err)
	}
	request := result["setupRequest"].(map[string]interface{})
	if request["status"] != "pending" || request["skillVersion"] != "2.0.0" {
		t.Fatal(request)
	}
	input.Call.ID = "call-2"
	repeated, err := dispatcher.RequestSkillSetup(ctx, input, run, deploymentID, models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: deploymentID}, models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: deploymentID})
	if err != nil || repeated["setupRequest"].(map[string]interface{})["id"] != request["id"] {
		t.Fatalf("duplicate not reused: %#v %v", repeated, err)
	}
	bindings, _ := catalog.ListBindings(ctx, skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, deploymentID)
	if len(bindings) != 1 {
		t.Fatalf("request granted access: %#v", bindings)
	}
	args["skillVersion"] = "forged"
	input.Call.ID = "call-3"
	if _, err := dispatcher.RequestSkillSetup(ctx, input, run, deploymentID, models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: deploymentID}, models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: deploymentID}); err == nil {
		t.Fatal("unverified Skill accepted")
	}
	args["skillVersion"] = "1.0.0"
	args["kind"] = "reauthorize"
	args["bindingId"] = "foreign-binding"
	if _, err := dispatcher.RequestSkillSetup(ctx, input, run, deploymentID, models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: deploymentID}, models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: deploymentID}); err == nil {
		t.Fatal("unbound reconnect accepted")
	}
}

func TestSkillSetupDoesNotRediscoverAnIdenticalFailedRequestInSameRun(t *testing.T) {
	ctx := context.Background()
	store := kerneltest.NewStore(t)
	scope := models.Scope{Kind: "tenant", ID: "a"}
	catalog := skillbindingtest.SkillActionCatalog(t, ctx, scope, "agent")
	discoveries := 0
	provider := skill.DiscoveryProviderFunc(func(context.Context, skill.DiscoveryRequest) (*skill.DiscoveryPage, error) {
		discoveries++
		return nil, errors.New("permanent catalog conflict")
	})
	dispatcher, err := skillbinding.NewSkillBindingActionDispatcher(store, catalog, nil, provider)
	if err != nil {
		t.Fatal(err)
	}
	run := skillbindingtest.ClaimedSkillActionRun(t, ctx, store, scope, "agent", "worker")
	run.Context = map[string]interface{}{"conversationId": "chat", "triggerMessageId": "message"}
	args := map[string]interface{}{"kind": "configure", "skillId": "reddit.reader", "skillVersion": "latest", "reason": "connect"}
	now := time.Now().UTC()
	failed := &models.ActionCall{ID: "failed", Scope: scope, RunID: run.ID, TurnID: "step-1", DeploymentID: "agent", Origin: models.ActionOriginLoop, Execution: models.ActionExecutionLoop,
		SkillID: models.SkillManagementSkillID, SkillVersion: skillbinding.SkillManagementSkillVersion, Action: agentrun.SkillActionRequestSetup, Status: models.ActionCallStatusFailed, Arguments: args,
		MaxAttempts: 1, Revision: 1, CreatedAt: now, UpdatedAt: now}
	event := &models.ActivityEvent{ID: "failed-setup", Scope: scope, RunID: run.ID, EventType: "action.failed", Summary: "Setup failed",
		Actor: models.ActivityActor{Type: "worker", ID: "worker"}, Visibility: models.ActivityVisibilityScope, CreatedAt: now}
	if _, err := store.CreateActionProposal(ctx, models.ActionProposalRecord{Call: failed, Event: event, Lease: &models.AgentRunLeaseGuard{WorkerID: "worker", Now: now}}); err != nil {
		t.Fatal(err)
	}
	_, err = dispatcher.RequestSkillSetup(ctx, agentrun.ActionDispatchInput{Call: &models.ActionCall{ID: "retry"}, Arguments: args}, run, "agent", models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"})
	if err == nil || discoveries != 0 {
		t.Fatalf("repeated failed setup rediscovered: %v %d", err, discoveries)
	}
	run.ID = "later-user-run"
	_, err = dispatcher.RequestSkillSetup(ctx, agentrun.ActionDispatchInput{Call: &models.ActionCall{ID: "later"}, Arguments: args}, run, "agent", models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"})
	if err == nil || discoveries != 1 {
		t.Fatalf("later run could not retry: %v %d", err, discoveries)
	}
	if skillbinding.SkillSetupAction().Retry.MaxAttempts != 1 {
		t.Fatal("setup automatically retries failures")
	}
}

func TestSkillSetupResolvesAnOmittedSourceFromTheCatalog(t *testing.T) {
	ctx := context.Background()
	store := kerneltest.NewStore(t)
	scope := models.Scope{Kind: "tenant", ID: "a"}
	deploymentID := "agent"
	catalog := skillbindingtest.SkillActionCatalog(t, ctx, scope, deploymentID)
	sources := []string{"https://github.com/axiom-studio/skills::skill-mcp"}
	provider := skill.DiscoveryProviderFunc(func(_ context.Context, r skill.DiscoveryRequest) (*skill.DiscoveryPage, error) {
		items := []skill.DiscoveryCandidate{}
		for _, source := range sources {
			items = append(items, skill.DiscoveryCandidate{ID: "skill-mcp", Version: "0.3.1", Name: "MCP server", SourceIdentity: source, Readiness: skill.DiscoveryReadinessBindable, Actions: []skill.DiscoveryAction{{Name: "mcp-read", Risk: skill.RiskLevelRead}}})
		}
		return &skill.DiscoveryPage{Items: items}, nil
	})
	dispatcher, err := skillbinding.NewSkillBindingActionDispatcher(store, catalog, nil, provider)
	if err != nil {
		t.Fatal(err)
	}
	run := skillbindingtest.ClaimedSkillActionRun(t, ctx, store, scope, deploymentID, "worker")
	run.Context = map[string]interface{}{"conversationId": "chat", "triggerMessageId": "message"}
	// The shape DeepSeek sent: no sourceIdentity.
	args := map[string]interface{}{"kind": "configure", "skillId": "skill-mcp", "skillVersion": "latest", "reason": "Connect Supabase's MCP server"}
	result, err := dispatcher.RequestSkillSetup(ctx, agentrun.ActionDispatchInput{Call: &models.ActionCall{ID: "call-1", Scope: scope, RunID: run.ID}, Arguments: args}, run, deploymentID, models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: deploymentID}, models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: deploymentID})
	if err != nil {
		t.Fatal(err)
	}
	request := result["setupRequest"].(map[string]interface{})
	if request["status"] != "pending" || request["sourceIdentity"] != sources[0] || request["skillVersion"] != "0.3.1" {
		t.Fatal(request)
	}
	sources = append(sources, "https://example.com/other::skill-mcp")
	_, err = dispatcher.RequestSkillSetup(ctx, agentrun.ActionDispatchInput{Call: &models.ActionCall{ID: "call-2", Scope: scope, RunID: run.ID}, Arguments: args}, run, deploymentID, models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: deploymentID}, models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: deploymentID})
	if err == nil || !strings.Contains(err.Error(), "more than one source supplies skill-mcp") {
		t.Fatalf("ambiguous source accepted: %v", err)
	}
}
