package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding/skillbindingtest"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestSkillBindingTargetFromNonBuilderIsRefused(t *testing.T) {
	f := skillbindingtest.NewTargetFixture(t)
	run := f.ChatRun(t, "writer")
	if _, err := f.Validator.ValidateActionProposal(f.Ctx, agentrun.ActionProposalValidationInput{
		Run: run, Bound: f.Bound(t, "writer", skillbinding.SkillActionUpsertBinding), Arguments: skillbindingtest.ReaderUpsertArguments("researcher"),
	}); err == nil || !strings.Contains(err.Error(), "only an Agent that builds Agents") {
		t.Fatalf("non-builder upsert for another Agent = %v", err)
	}
	if _, err := f.Validator.ValidateActionProposal(f.Ctx, agentrun.ActionProposalValidationInput{
		Run: run, Bound: f.Bound(t, "writer", skillbinding.SkillActionDiscoverBinding), Arguments: map[string]interface{}{"query": "reddit", skillbinding.SkillActionTargetAgentArgument: "researcher"},
	}); err == nil {
		t.Fatal("non-builder discovery for another Agent was accepted")
	}
	if _, err := f.Dispatch(t, run, "upsert", skillbinding.SkillActionUpsertBinding, skillbindingtest.ReaderUpsertArguments("researcher")); err == nil {
		t.Fatal("non-builder dispatch bound a Skill to another Agent")
	}
	result, err := f.Dispatch(t, run, "setup", agentrun.SkillActionRequestSetup, map[string]interface{}{
		"kind": "configure", "skillId": "reddit.reader", "skillVersion": "1.0.0", "reason": "Read communities", skillbinding.SkillActionTargetAgentArgument: "researcher",
	})
	if err != nil || result["refused"] == nil {
		t.Fatalf("non-builder setup for another Agent = %#v, %v", result, err)
	}
	if bindings, _ := f.Catalog.ListBindings(f.Ctx, f.SkillScope, "researcher"); len(bindings) != 0 {
		t.Fatalf("researcher got bindings: %#v", bindings)
	}
	if requests, _ := f.Store.ListSkillSetupRequests(f.Ctx, f.Scope, "", "chat-writer"); len(requests) != 0 {
		t.Fatalf("refused setup saved a request: %#v", requests)
	}
}

func TestSkillBindingTargetFromBuilderBindsTheTarget(t *testing.T) {
	f := skillbindingtest.NewTargetFixture(t)
	run := f.ChatRun(t, "finn")
	// A person connected each account; the builder changes the rest.
	for _, deploymentID := range []string{"researcher", "finn"} {
		if _, err := f.Catalog.UpsertBinding(f.Ctx, skill.UpsertBindingRequest{Binding: &skill.Binding{
			ID: "reddit", Scope: f.SkillScope, DeploymentID: deploymentID, SkillID: "reddit.reader", SkillVersion: "1.0.0",
			AllowedActions: []string{}, MaximumRisk: skill.RiskLevelRead,
			Credentials: map[string]skill.CredentialReference{"reddit": {Kind: "reddit-oauth", ID: "credential://tenant-a/reddit-" + deploymentID}},
		}, Actor: skill.BindingActor{Type: "user", ID: "operator"}, Reason: "Connect Reddit"}); err != nil {
			t.Fatal(err)
		}
	}
	preview, err := f.Validator.ValidateActionProposal(f.Ctx, agentrun.ActionProposalValidationInput{
		Run: run, Bound: f.Bound(t, "finn", skillbinding.SkillActionUpsertBinding), Arguments: skillbindingtest.ReaderUpsertArguments("researcher"),
	})
	if err != nil {
		t.Fatal(err)
	}
	targetAgent, _ := preview["targetAgent"].(map[string]interface{})
	if targetAgent["id"] != "researcher" || targetAgent["displayName"] != "Researcher" || preview["deploymentId"] != "researcher" ||
		preview["owner"] != (models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "researcher"}) {
		t.Fatalf("targeted preview = %#v", preview)
	}
	own, err := f.Validator.ValidateActionProposal(f.Ctx, agentrun.ActionProposalValidationInput{
		Run: run, Bound: f.Bound(t, "finn", skillbinding.SkillActionUpsertBinding), Arguments: skillbindingtest.ReaderUpsertArguments(""),
	})
	if err != nil || own["targetAgent"] != nil || own["deploymentId"] != "finn" {
		t.Fatalf("untargeted preview = %#v, %v", own, err)
	}

	result, err := f.Dispatch(t, run, "upsert", skillbinding.SkillActionUpsertBinding, skillbindingtest.ReaderUpsertArguments("researcher"))
	if err != nil || result["operation"] != skillbinding.SkillActionUpsertBinding {
		t.Fatalf("targeted upsert = %#v, %v", result, err)
	}
	binding, err := f.Catalog.GetBinding(f.Ctx, f.SkillScope, "researcher", "reddit")
	if err != nil || binding == nil || binding.Disabled || binding.DeploymentID != "researcher" || binding.Revision != 2 || len(binding.AllowedActions) != 1 {
		t.Fatalf("target binding = %#v, %v", binding, err)
	}
	if own, _ := f.Catalog.GetBinding(f.Ctx, f.SkillScope, "finn", "reddit"); own == nil || own.Revision != 1 {
		t.Fatalf("builder's own binding changed: %#v", own)
	}
	replayed, err := f.Dispatch(t, run, "upsert", skillbinding.SkillActionUpsertBinding, skillbindingtest.ReaderUpsertArguments("researcher"))
	if err != nil || replayed["replayed"] != true {
		t.Fatalf("replayed targeted upsert = %#v, %v", replayed, err)
	}

	disable := map[string]interface{}{"bindingId": "reddit", "expectedRevision": binding.Revision, skillbinding.SkillActionTargetAgentArgument: "researcher"}
	disablePreview, err := f.Validator.ValidateActionProposal(f.Ctx, agentrun.ActionProposalValidationInput{Run: run, Bound: f.Bound(t, "finn", skillbinding.SkillActionDisableBinding), Arguments: disable})
	if err != nil || disablePreview["targetAgent"] == nil || disablePreview["deploymentId"] != "researcher" {
		t.Fatalf("targeted disable preview = %#v, %v", disablePreview, err)
	}
	if _, err := f.Dispatch(t, run, "disable", skillbinding.SkillActionDisableBinding, disable); err != nil {
		t.Fatal(err)
	}
	if disabled, _ := f.Catalog.GetBinding(f.Ctx, f.SkillScope, "researcher", "reddit"); disabled == nil || !disabled.Disabled {
		t.Fatalf("target binding was not disabled: %#v", disabled)
	}
}

func TestSkillBindingTargetDiscoverReportsTargetReadiness(t *testing.T) {
	f := skillbindingtest.NewTargetFixture(t)
	run := f.ChatRun(t, "finn")
	if _, err := f.Validator.ValidateActionProposal(f.Ctx, agentrun.ActionProposalValidationInput{
		Run: run, Bound: f.Bound(t, "finn", skillbinding.SkillActionDiscoverBinding), Arguments: map[string]interface{}{"query": "reddit", skillbinding.SkillActionTargetAgentArgument: "researcher"},
	}); err != nil {
		t.Fatal(err)
	}
	result, err := f.Dispatch(t, run, "discover", skillbinding.SkillActionDiscoverBinding, map[string]interface{}{"query": "reddit", skillbinding.SkillActionTargetAgentArgument: "researcher"})
	if err != nil {
		t.Fatal(err)
	}
	items, _ := result["items"].([]interface{})
	if len(f.Discovered) != 1 || f.Discovered[0].DeploymentID != "researcher" || len(items) != 1 ||
		items[0].(map[string]interface{})["readiness"] != string(skill.DiscoveryReadinessBindable) {
		t.Fatalf("targeted discovery = %#v, requests %#v", result, f.Discovered)
	}
	own, err := f.Dispatch(t, run, "discover-own", skillbinding.SkillActionDiscoverBinding, map[string]interface{}{"query": "reddit"})
	if err != nil || f.Discovered[1].DeploymentID != "finn" ||
		own["items"].([]interface{})[0].(map[string]interface{})["readiness"] != string(skill.DiscoveryReadinessUnavailable) {
		t.Fatalf("own discovery = %#v, %v", own, err)
	}
	if _, err := f.Dispatch(t, run, "discover-old", skillbinding.SkillActionDiscoverBinding, map[string]interface{}{"query": "reddit", skillbinding.SkillActionTargetAgentArgument: "old"}); err == nil {
		t.Fatal("discovery for a retired Agent was accepted")
	}
}

func TestSkillBindingTargetConnectionRequestIsOwnedByTheTarget(t *testing.T) {
	f := skillbindingtest.NewTargetFixture(t)
	var actors []skill.RunActor
	f.Catalog.SetHostBindingProvider(func(_ context.Context, _ skill.ScopeReference, deploymentID string, actor skill.RunActor) (*skill.HostBindingSet, error) {
		if deploymentID == "researcher" {
			actors = append(actors, actor)
		}
		return nil, nil
	})
	apps := &skillbinding.AppCatalogRef{}
	apps.Set(skillbindingtest.AppCatalog{Entries: []skillbinding.AppCatalogEntry{{Slug: "linear", Name: "Linear", Official: true, SignIn: models.AppSignInOAuth}}})
	f.Dispatcher.UseAppCatalog(apps)
	run := f.ChatRun(t, "finn")
	result, err := f.Dispatch(t, run, "connect", agentrun.SkillActionRequestConnection, map[string]interface{}{"app": "linear", skillbinding.SkillActionTargetAgentArgument: "researcher"})
	if err != nil || result["setupRequest"] == nil {
		t.Fatalf("targeted connection = %#v, %v", result, err)
	}
	request, err := f.Store.GetSkillSetupRequest(f.Ctx, f.Scope, "connect-app:connect")
	if err != nil || request == nil || request.Owner != (models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "researcher"}) || request.DeploymentID != "researcher" ||
		request.AgentID != "finn" || request.ConversationID != "chat-finn" || request.Kind != models.SkillSetupKindConnectApp || request.Validate() != nil {
		t.Fatalf("targeted connection request = %#v, %v", request, err)
	}
	if len(actors) != 1 || !actors[0].Valid() || actors[0].AgentID != "researcher" || actors[0].Umbrella != (skill.Umbrella{Kind: skill.UmbrellaAgent, ID: "researcher"}) || actors[0].ActingUser != "7" {
		t.Fatalf("target connections read as %#v", actors)
	}
	writer := f.ChatRun(t, "writer")
	refused, err := f.Dispatch(t, writer, "connect-writer", agentrun.SkillActionRequestConnection, map[string]interface{}{"app": "linear", skillbinding.SkillActionTargetAgentArgument: "researcher"})
	if err != nil || refused["refused"] == nil {
		t.Fatalf("non-builder connection for another Agent = %#v, %v", refused, err)
	}
}

func TestTargetAgentHiddenFromNonBuilders(t *testing.T) {
	definition := skillbinding.SkillManagementSkill()
	var actions []capability.ModelAction
	for _, name := range []string{agentrun.SkillActionRequestConnection, skillbinding.SkillActionFindApps, skillbinding.SkillActionUpsertBinding} {
		action := definition.Actions[name]
		actions = append(actions, capability.ModelAction{SkillID: models.SkillManagementSkillID, Action: name, Description: action.Description, InputSchema: action.InputSchema})
	}
	hidden := skillbinding.WithoutUnusableTargetAgent(&skill.ActivationSnapshot{}, actions)
	for _, action := range hidden {
		if properties, _ := action.InputSchema["properties"].(map[string]interface{}); properties[skillbinding.SkillActionTargetAgentArgument] != nil {
			t.Fatalf("%s still offers targetAgentId", action.Action)
		}
		if action.Action == agentrun.SkillActionRequestConnection && strings.Contains(action.Description, "With targetAgentId") {
			t.Fatalf("%s still describes targetAgentId", action.Action)
		}
	}
	if properties := actions[0].InputSchema["properties"].(map[string]interface{}); properties[skillbinding.SkillActionTargetAgentArgument] == nil {
		t.Fatal("hiding targetAgentId mutated the shared definition")
	}
	builder := &skill.ActivationSnapshot{Skills: []skill.ActivatedSkill{{
		SkillID: models.AgentManagementSkillID, SkillVersion: models.AgentManagementSkillVersion,
		Actions: []capability.ModelAction{{Action: models.AgentActionCreateAgent}},
	}}}
	for _, action := range skillbinding.WithoutUnusableTargetAgent(builder, actions) {
		if action.Action == skillbinding.SkillActionFindApps {
			continue
		}
		if properties, _ := action.InputSchema["properties"].(map[string]interface{}); properties[skillbinding.SkillActionTargetAgentArgument] == nil {
			t.Fatalf("builder lost targetAgentId on %s", action.Action)
		}
	}
}

func TestSkillManagementSkillVersionIs170(t *testing.T) {
	if skillbinding.SkillManagementSkillVersion != "1.7.0" {
		t.Fatalf("openseal.skills = %s, want 1.7.0", skillbinding.SkillManagementSkillVersion)
	}
	definition := skillbinding.SkillManagementSkill()
	for _, name := range []string{skillbinding.SkillActionDiscoverBinding, skillbinding.SkillActionUpsertBinding, skillbinding.SkillActionDisableBinding, agentrun.SkillActionRequestSetup, agentrun.SkillActionRequestConnection} {
		properties := definition.Actions[name].InputSchema["properties"].(map[string]interface{})
		if properties[skillbinding.SkillActionTargetAgentArgument] == nil {
			t.Fatalf("%s has no targetAgentId", name)
		}
	}
	for _, name := range []string{agentrun.SkillActionRequestCredential, skillbinding.SkillActionListSetupRequests, skillbinding.SkillActionFindApps} {
		properties, _ := definition.Actions[name].InputSchema["properties"].(map[string]interface{})
		if properties[skillbinding.SkillActionTargetAgentArgument] != nil {
			t.Fatalf("%s accepts targetAgentId", name)
		}
	}
}
