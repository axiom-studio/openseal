package runtime

import (
	"context"
	"strings"
	"testing"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/mcp"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

// skillTargetFixture is a builder (finn, with an openseal.agents binding that
// allows create_agent), a plain Agent (writer) and the Agents they might act
// for: researcher (changeable), platform (fixed policy) and old (retired).
type skillTargetFixture struct {
	ctx        context.Context
	store      *sqlite.Store
	scope      Scope
	skillScope skill.ScopeReference
	agents     *kernelagent.Registry
	catalog    *skill.Catalog
	validator  *skillbinding.SkillBindingActionValidator
	dispatcher *skillbinding.SkillBindingActionDispatcher
	discovered []skill.DiscoveryRequest
}

func newSkillTargetFixture(t *testing.T) *skillTargetFixture {
	t.Helper()
	f := &skillTargetFixture{ctx: context.Background(), store: newTestKernelStore(t), scope: Scope{Kind: "tenant", ID: "tenant-a"}}
	f.skillScope = skill.ScopeReference{Kind: f.scope.Kind, ID: f.scope.ID}
	reference := capability.ScopeReference{Kind: f.scope.Kind, ID: f.scope.ID}
	f.agents = kernelagent.NewRegistry()
	for _, agent := range []struct {
		id, name   string
		amendments workforce.AmendmentPolicy
		retired    bool
	}{
		{"finn", "Finn", workforce.AmendmentPolicy{}, false},
		{"writer", "Writer", workforce.AmendmentPolicy{AgentMayPropose: true}, false},
		{"researcher", "Researcher", workforce.AmendmentPolicy{AgentMayPropose: true}, false},
		{"platform", "Platform Helper", workforce.AmendmentPolicy{}, false},
		{"old", "Old Agent", workforce.AmendmentPolicy{AgentMayPropose: true}, true},
	} {
		definition, err := f.agents.RegisterDefinition(f.ctx, &kernelagent.AgentDefinition{
			ID: agent.id + "-definition", Version: "1.0.0", DisplayName: agent.name, Purpose: agent.name + " work",
			SystemPrompt: "Work as " + agent.name + ".", Authority: kernelagent.AuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1},
			Amendments: agent.amendments,
		})
		if err != nil {
			t.Fatal(err)
		}
		deployment, _, err := f.agents.CreateDeployment(f.ctx, &kernelagent.AgentDeployment{
			ID: agent.id, Scope: reference, DefinitionID: definition.ID, ActiveVersion: definition.Version,
			RolloutStatus: kernelagent.RolloutActive, Environment: "default", Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 1},
		}, "user", "operator", "fixture")
		if err != nil {
			t.Fatal(err)
		}
		if agent.retired {
			deployment.RolloutStatus = kernelagent.RolloutRetired
			if _, _, err := f.agents.UpdateDeployment(f.ctx, deployment, deployment.Revision, "user", "operator", "retire"); err != nil {
				t.Fatal(err)
			}
		}
	}
	f.catalog = skill.NewCatalog()
	for _, definition := range []*skill.Definition{skillbinding.SkillManagementSkill(), AgentManagementSkill(), redditSkillDefinition(), mcp.SkillDefinition()} {
		if err := f.catalog.Register(f.ctx, definition); err != nil {
			t.Fatal(err)
		}
	}
	for _, member := range []string{"finn", "writer"} {
		if err := f.catalog.Bind(f.ctx, &skill.Binding{
			ID: "skills", Revision: 1, Scope: f.skillScope, DeploymentID: member, SkillID: SkillManagementSkillID, SkillVersion: skillbinding.SkillManagementSkillVersion,
			AllowedActions: []string{skillbinding.SkillActionDiscoverBinding, skillbinding.SkillActionUpsertBinding, skillbinding.SkillActionDisableBinding, agentrun.SkillActionRequestSetup, agentrun.SkillActionRequestConnection, skillbinding.SkillActionListSetupRequests},
			MaximumRisk:    skill.RiskLevelWrite,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.catalog.Bind(f.ctx, &skill.Binding{
		ID: "agents", Revision: 1, Scope: f.skillScope, DeploymentID: "finn", SkillID: AgentManagementSkillID, SkillVersion: AgentManagementSkillVersion,
		AllowedActions: []string{AgentActionCreateAgent}, MaximumRisk: skill.RiskLevelWrite,
	}); err != nil {
		t.Fatal(err)
	}
	// Writer has openseal.agents too, but no builder action.
	if err := f.catalog.Bind(f.ctx, &skill.Binding{
		ID: "agents", Revision: 1, Scope: f.skillScope, DeploymentID: "writer", SkillID: AgentManagementSkillID, SkillVersion: AgentManagementSkillVersion,
		AllowedActions: []string{AgentActionAmendBehavior}, MaximumRisk: skill.RiskLevelWrite,
	}); err != nil {
		t.Fatal(err)
	}
	// Discovery reports readiness per deployment: the reader is bindable for
	// researcher only.
	provider := skill.DiscoveryProviderFunc(func(_ context.Context, request skill.DiscoveryRequest) (*skill.DiscoveryPage, error) {
		f.discovered = append(f.discovered, request)
		readiness := skill.DiscoveryReadinessUnavailable
		if request.DeploymentID == "researcher" {
			readiness = skill.DiscoveryReadinessBindable
		}
		return &skill.DiscoveryPage{Items: []skill.DiscoveryCandidate{{
			ID: "reddit.reader", Version: "1.0.0", Name: "Reddit Reader", Readiness: readiness,
			Actions: []skill.DiscoveryAction{{Name: "read", Risk: skill.RiskLevelRead}},
		}}}, nil
	})
	var err error
	if f.validator, err = skillbinding.NewSkillBindingActionValidator(f.catalog, f.store); err != nil {
		t.Fatal(err)
	}
	f.validator.UseAgentRegistry(f.agents)
	if f.dispatcher, err = skillbinding.NewSkillBindingActionDispatcher(f.store, f.catalog, nil, provider); err != nil {
		t.Fatal(err)
	}
	f.dispatcher.UseAgentRegistry(f.agents)
	return f
}

// chatRun is a reply Run of member in its own chat.
func (f *skillTargetFixture) chatRun(t *testing.T, member string) *AgentRun {
	t.Helper()
	run, err := NewPortfolioService(f.store).CreateAgentRun(f.ctx, CreateAgentRunRequest{
		Scope: f.scope, Kind: RunKindConversation, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: member}, AssignedAgentID: member,
		Goal: "Build an Agent", Source: RunSourceChat, ActingUser: "7",
		Context: map[string]interface{}{"conversationId": "chat-" + member, "triggerMessageId": "message-" + member},
	})
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func (f *skillTargetFixture) bound(t *testing.T, member, action string) *skill.BoundAction {
	t.Helper()
	bound, err := f.catalog.Resolve(f.ctx, f.skillScope, member, SkillManagementSkillID, skillbinding.SkillManagementSkillVersion, action)
	if err != nil {
		t.Fatal(err)
	}
	return bound
}

func (f *skillTargetFixture) dispatch(t *testing.T, run *AgentRun, callID, action string, arguments map[string]interface{}) (map[string]interface{}, error) {
	t.Helper()
	return f.dispatcher.DispatchAction(f.ctx, agentrun.ActionDispatchInput{
		Call:  &ActionCall{ID: callID, Scope: f.scope, RunID: run.ID, DeploymentID: run.AssignedAgentID},
		Bound: f.bound(t, run.AssignedAgentID, action), Arguments: arguments,
	})
}

func readerUpsertArguments(target string) map[string]interface{} {
	arguments := map[string]interface{}{
		"bindingId": "reddit", "expectedRevision": 1, "skillId": "reddit.reader", "skillVersion": "1.0.0",
		"allowedActions": []interface{}{"read"}, "enablePrompt": false, "maximumRisk": "read",
	}
	if target != "" {
		arguments[skillbinding.SkillActionTargetAgentArgument] = target
	}
	return arguments
}

func TestSkillBindingTargetFromNonBuilderIsRefused(t *testing.T) {
	f := newSkillTargetFixture(t)
	run := f.chatRun(t, "writer")
	if _, err := f.validator.ValidateActionProposal(f.ctx, agentrun.ActionProposalValidationInput{
		Run: run, Bound: f.bound(t, "writer", skillbinding.SkillActionUpsertBinding), Arguments: readerUpsertArguments("researcher"),
	}); err == nil || !strings.Contains(err.Error(), "only an Agent that builds Agents") {
		t.Fatalf("non-builder upsert for another Agent = %v", err)
	}
	if _, err := f.validator.ValidateActionProposal(f.ctx, agentrun.ActionProposalValidationInput{
		Run: run, Bound: f.bound(t, "writer", skillbinding.SkillActionDiscoverBinding), Arguments: map[string]interface{}{"query": "reddit", skillbinding.SkillActionTargetAgentArgument: "researcher"},
	}); err == nil {
		t.Fatal("non-builder discovery for another Agent was accepted")
	}
	if _, err := f.dispatch(t, run, "upsert", skillbinding.SkillActionUpsertBinding, readerUpsertArguments("researcher")); err == nil {
		t.Fatal("non-builder dispatch bound a Skill to another Agent")
	}
	result, err := f.dispatch(t, run, "setup", agentrun.SkillActionRequestSetup, map[string]interface{}{
		"kind": "configure", "skillId": "reddit.reader", "skillVersion": "1.0.0", "reason": "Read communities", skillbinding.SkillActionTargetAgentArgument: "researcher",
	})
	if err != nil || result["refused"] == nil {
		t.Fatalf("non-builder setup for another Agent = %#v, %v", result, err)
	}
	if bindings, _ := f.catalog.ListBindings(f.ctx, f.skillScope, "researcher"); len(bindings) != 0 {
		t.Fatalf("researcher got bindings: %#v", bindings)
	}
	if requests, _ := f.store.ListSkillSetupRequests(f.ctx, f.scope, "", "chat-writer"); len(requests) != 0 {
		t.Fatalf("refused setup saved a request: %#v", requests)
	}
}

func TestSkillBindingTargetFromBuilderBindsTheTarget(t *testing.T) {
	f := newSkillTargetFixture(t)
	run := f.chatRun(t, "finn")
	// A person connected each account; the builder changes the rest.
	for _, deploymentID := range []string{"researcher", "finn"} {
		if _, err := f.catalog.UpsertBinding(f.ctx, skill.UpsertBindingRequest{Binding: &skill.Binding{
			ID: "reddit", Scope: f.skillScope, DeploymentID: deploymentID, SkillID: "reddit.reader", SkillVersion: "1.0.0",
			AllowedActions: []string{}, MaximumRisk: skill.RiskLevelRead,
			Credentials: map[string]skill.CredentialReference{"reddit": {Kind: "reddit-oauth", ID: "credential://tenant-a/reddit-" + deploymentID}},
		}, Actor: skill.BindingActor{Type: "user", ID: "operator"}, Reason: "Connect Reddit"}); err != nil {
			t.Fatal(err)
		}
	}
	preview, err := f.validator.ValidateActionProposal(f.ctx, agentrun.ActionProposalValidationInput{
		Run: run, Bound: f.bound(t, "finn", skillbinding.SkillActionUpsertBinding), Arguments: readerUpsertArguments("researcher"),
	})
	if err != nil {
		t.Fatal(err)
	}
	targetAgent, _ := preview["targetAgent"].(map[string]interface{})
	if targetAgent["id"] != "researcher" || targetAgent["displayName"] != "Researcher" || preview["deploymentId"] != "researcher" ||
		preview["owner"] != (ObjectiveOwner{Type: OwnerTypeAgent, ID: "researcher"}) {
		t.Fatalf("targeted preview = %#v", preview)
	}
	own, err := f.validator.ValidateActionProposal(f.ctx, agentrun.ActionProposalValidationInput{
		Run: run, Bound: f.bound(t, "finn", skillbinding.SkillActionUpsertBinding), Arguments: readerUpsertArguments(""),
	})
	if err != nil || own["targetAgent"] != nil || own["deploymentId"] != "finn" {
		t.Fatalf("untargeted preview = %#v, %v", own, err)
	}

	result, err := f.dispatch(t, run, "upsert", skillbinding.SkillActionUpsertBinding, readerUpsertArguments("researcher"))
	if err != nil || result["operation"] != skillbinding.SkillActionUpsertBinding {
		t.Fatalf("targeted upsert = %#v, %v", result, err)
	}
	binding, err := f.catalog.GetBinding(f.ctx, f.skillScope, "researcher", "reddit")
	if err != nil || binding == nil || binding.Disabled || binding.DeploymentID != "researcher" || binding.Revision != 2 || len(binding.AllowedActions) != 1 {
		t.Fatalf("target binding = %#v, %v", binding, err)
	}
	if own, _ := f.catalog.GetBinding(f.ctx, f.skillScope, "finn", "reddit"); own == nil || own.Revision != 1 {
		t.Fatalf("builder's own binding changed: %#v", own)
	}
	replayed, err := f.dispatch(t, run, "upsert", skillbinding.SkillActionUpsertBinding, readerUpsertArguments("researcher"))
	if err != nil || replayed["replayed"] != true {
		t.Fatalf("replayed targeted upsert = %#v, %v", replayed, err)
	}

	disable := map[string]interface{}{"bindingId": "reddit", "expectedRevision": binding.Revision, skillbinding.SkillActionTargetAgentArgument: "researcher"}
	disablePreview, err := f.validator.ValidateActionProposal(f.ctx, agentrun.ActionProposalValidationInput{Run: run, Bound: f.bound(t, "finn", skillbinding.SkillActionDisableBinding), Arguments: disable})
	if err != nil || disablePreview["targetAgent"] == nil || disablePreview["deploymentId"] != "researcher" {
		t.Fatalf("targeted disable preview = %#v, %v", disablePreview, err)
	}
	if _, err := f.dispatch(t, run, "disable", skillbinding.SkillActionDisableBinding, disable); err != nil {
		t.Fatal(err)
	}
	if disabled, _ := f.catalog.GetBinding(f.ctx, f.skillScope, "researcher", "reddit"); disabled == nil || !disabled.Disabled {
		t.Fatalf("target binding was not disabled: %#v", disabled)
	}
}

func TestSkillBindingTargetDiscoverReportsTargetReadiness(t *testing.T) {
	f := newSkillTargetFixture(t)
	run := f.chatRun(t, "finn")
	if _, err := f.validator.ValidateActionProposal(f.ctx, agentrun.ActionProposalValidationInput{
		Run: run, Bound: f.bound(t, "finn", skillbinding.SkillActionDiscoverBinding), Arguments: map[string]interface{}{"query": "reddit", skillbinding.SkillActionTargetAgentArgument: "researcher"},
	}); err != nil {
		t.Fatal(err)
	}
	result, err := f.dispatch(t, run, "discover", skillbinding.SkillActionDiscoverBinding, map[string]interface{}{"query": "reddit", skillbinding.SkillActionTargetAgentArgument: "researcher"})
	if err != nil {
		t.Fatal(err)
	}
	items, _ := result["items"].([]interface{})
	if len(f.discovered) != 1 || f.discovered[0].DeploymentID != "researcher" || len(items) != 1 ||
		items[0].(map[string]interface{})["readiness"] != string(skill.DiscoveryReadinessBindable) {
		t.Fatalf("targeted discovery = %#v, requests %#v", result, f.discovered)
	}
	own, err := f.dispatch(t, run, "discover-own", skillbinding.SkillActionDiscoverBinding, map[string]interface{}{"query": "reddit"})
	if err != nil || f.discovered[1].DeploymentID != "finn" ||
		own["items"].([]interface{})[0].(map[string]interface{})["readiness"] != string(skill.DiscoveryReadinessUnavailable) {
		t.Fatalf("own discovery = %#v, %v", own, err)
	}
	if _, err := f.dispatch(t, run, "discover-old", skillbinding.SkillActionDiscoverBinding, map[string]interface{}{"query": "reddit", skillbinding.SkillActionTargetAgentArgument: "old"}); err == nil {
		t.Fatal("discovery for a retired Agent was accepted")
	}
}

func TestSkillBindingTargetConnectionRequestIsOwnedByTheTarget(t *testing.T) {
	f := newSkillTargetFixture(t)
	var actors []skill.RunActor
	f.catalog.SetHostBindingProvider(func(_ context.Context, _ skill.ScopeReference, deploymentID string, actor skill.RunActor) (*skill.HostBindingSet, error) {
		if deploymentID == "researcher" {
			actors = append(actors, actor)
		}
		return nil, nil
	})
	apps := &skillbinding.AppCatalogRef{}
	apps.Set(fakeAppCatalog{entries: []skillbinding.AppCatalogEntry{{Slug: "linear", Name: "Linear", Official: true, SignIn: AppSignInOAuth}}})
	f.dispatcher.UseAppCatalog(apps)
	run := f.chatRun(t, "finn")
	result, err := f.dispatch(t, run, "connect", agentrun.SkillActionRequestConnection, map[string]interface{}{"app": "linear", skillbinding.SkillActionTargetAgentArgument: "researcher"})
	if err != nil || result["setupRequest"] == nil {
		t.Fatalf("targeted connection = %#v, %v", result, err)
	}
	request, err := f.store.GetSkillSetupRequest(f.ctx, f.scope, "connect-app:connect")
	if err != nil || request == nil || request.Owner != (ObjectiveOwner{Type: OwnerTypeAgent, ID: "researcher"}) || request.DeploymentID != "researcher" ||
		request.AgentID != "finn" || request.ConversationID != "chat-finn" || request.Kind != SkillSetupKindConnectApp || request.Validate() != nil {
		t.Fatalf("targeted connection request = %#v, %v", request, err)
	}
	if len(actors) != 1 || !actors[0].Valid() || actors[0].AgentID != "researcher" || actors[0].Umbrella != (skill.Umbrella{Kind: skill.UmbrellaAgent, ID: "researcher"}) || actors[0].ActingUser != "7" {
		t.Fatalf("target connections read as %#v", actors)
	}
	writer := f.chatRun(t, "writer")
	refused, err := f.dispatch(t, writer, "connect-writer", agentrun.SkillActionRequestConnection, map[string]interface{}{"app": "linear", skillbinding.SkillActionTargetAgentArgument: "researcher"})
	if err != nil || refused["refused"] == nil {
		t.Fatalf("non-builder connection for another Agent = %#v, %v", refused, err)
	}
}

func TestTargetAgentHiddenFromNonBuilders(t *testing.T) {
	definition := skillbinding.SkillManagementSkill()
	var actions []capability.ModelAction
	for _, name := range []string{agentrun.SkillActionRequestConnection, skillbinding.SkillActionFindApps, skillbinding.SkillActionUpsertBinding} {
		action := definition.Actions[name]
		actions = append(actions, capability.ModelAction{SkillID: SkillManagementSkillID, Action: name, Description: action.Description, InputSchema: action.InputSchema})
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
		SkillID: AgentManagementSkillID, SkillVersion: AgentManagementSkillVersion,
		Actions: []capability.ModelAction{{Action: AgentActionCreateAgent}},
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
