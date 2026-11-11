package runtime

import (
	"context"
	"testing"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

type teamSkillAuthorityCatalogStub struct {
	deployment      *kernelteam.Deployment
	definition      *kernelteam.Definition
	agent           *kernelagent.AgentDeployment
	agentDefinition *kernelagent.AgentDefinition
}

func (s teamSkillAuthorityCatalogStub) GetAgentDeployment(context.Context, skill.ScopeReference, string) (*kernelagent.AgentDeployment, error) {
	return s.agent, nil
}

func (s teamSkillAuthorityCatalogStub) GetAgentDefinition(context.Context, string, string) (*kernelagent.AgentDefinition, error) {
	return s.agentDefinition, nil
}

func (s teamSkillAuthorityCatalogStub) GetTeamDeployment(context.Context, skill.ScopeReference, string) (*kernelteam.Deployment, error) {
	return s.deployment, nil
}

func (s teamSkillAuthorityCatalogStub) GetTeamDefinition(context.Context, string, string) (*kernelteam.Definition, error) {
	return s.definition, nil
}

func TestTeamSkillAuthorityGivesEveryMemberEveryTeamBinding(t *testing.T) {
	scope := Scope{Kind: "tenant", ID: "one"}
	catalog := teamSkillAuthorityFixture(scope)
	run := &AgentRun{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "research"}, AssignedAgentID: "analyst"}
	snapshot := &skill.ActivationSnapshot{
		SnapshotID: "base", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "research",
		Skills: []skill.ActivatedSkill{{
			BindingID: "forum", BindingRevision: 2, SkillID: "forum", SkillVersion: "1", Prompt: &skill.PromptModule{Instructions: "Research."},
			Actions: []capability.ModelAction{
				{Name: "forum.search", SkillID: "forum", Version: "1", Action: "search", Risk: capability.RiskLevelRead},
				{Name: "forum.reply", SkillID: "forum", Version: "1", Action: "reply", Risk: capability.RiskLevelExternal},
				{Name: "forum.purge", SkillID: "forum", Version: "1", Action: "purge", Risk: capability.RiskLevelDestructive},
			},
		}, {
			BindingID: "email", BindingRevision: 1, SkillID: "email", SkillVersion: "1", Actions: []capability.ModelAction{{Name: "email.send", SkillID: "email", Version: "1", Action: "send", Risk: capability.RiskLevelExternal}},
		}},
	}
	// The member Agent's own allowlist does not narrow Team bindings.
	catalog.agentDefinition.Authority.AllowedSkillIDs = []string{"something-else"}
	catalog.agent.Restrictions.AllowedSkillIDs = []string{"something-else"}
	projected, err := AuthorizeTeamSkillActivation(t.Context(), catalog, run, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if projected.SnapshotID == snapshot.SnapshotID || len(projected.Skills) != 2 || projected.Skills[0].Prompt == nil || len(projected.Skills[0].Actions) != 2 ||
		projected.Skills[0].Actions[0].Action != "search" || projected.Skills[0].Actions[1].Action != "reply" || projected.Skills[0].Actions[0].DeploymentID != "research" ||
		projected.Skills[1].SkillID != "email" {
		t.Fatalf("projected Team bindings = %#v", projected.Skills)
	}

	// The Team Skill allowlist and the member Agent's risk ceiling still apply.
	catalog.deployment.Restrictions.AllowedSkillIDs = []string{"forum"}
	catalog.agentDefinition.Authority.MaximumRisk = capability.RiskLevelRead
	projected, err = AuthorizeTeamSkillActivation(t.Context(), catalog, run, snapshot)
	if err != nil || len(projected.Skills) != 1 || len(projected.Skills[0].Actions) != 1 || projected.Skills[0].Actions[0].Action != "search" {
		t.Fatalf("restricted projection = %#v, %v", projected, err)
	}

	foreign := *run
	foreign.AssignedAgentID = "outsider"
	if _, err := AuthorizeTeamSkillActivation(t.Context(), catalog, &foreign, snapshot); err == nil {
		t.Fatal("expected non-member Team Skill projection to fail closed")
	}
}

func TestTeamSkillActionValidatorKeepsMemberConnectionsOutOfTeamRuns(t *testing.T) {
	scope := Scope{Kind: "tenant", ID: "one"}
	catalog := teamSkillAuthorityFixture(scope)
	validator, err := NewTeamSkillActionValidator(catalog)
	if err != nil {
		t.Fatal(err)
	}
	skillScope := skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}
	run := &AgentRun{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "research"}, AssignedAgentID: "analyst"}
	validate := func(run *AgentRun, binding *skill.Binding, action capability.Action) error {
		_, err := validator.ValidateActionProposal(t.Context(), ActionProposalValidationInput{Run: run, Bound: &skill.BoundAction{Binding: binding, Action: action}})
		return err
	}
	teamForum := &skill.Binding{Scope: skillScope, DeploymentID: "research", SkillID: "forum", SkillVersion: "1"}
	if err := validate(run, teamForum, capability.Action{Name: "reply", Risk: capability.RiskLevelExternal}); err != nil {
		t.Fatalf("every member may use every Team binding: %v", err)
	}
	catalog.agentDefinition.Authority.MaximumRisk = capability.RiskLevelRead
	if err := validate(run, teamForum, capability.Action{Name: "reply", Risk: capability.RiskLevelExternal}); err == nil {
		t.Fatal("a Team binding must not widen the member Agent's risk ceiling")
	}
	catalog.agentDefinition.Authority.MaximumRisk = capability.RiskLevelExternal
	catalog.deployment.Restrictions.AllowedSkillIDs = []string{"email"}
	if err := validate(run, teamForum, capability.Action{Name: "search", Risk: capability.RiskLevelRead}); err == nil {
		t.Fatal("the Team Skill allowlist still applies")
	}
	catalog.deployment.Restrictions.AllowedSkillIDs = nil

	memberGmail := &skill.Binding{Scope: skillScope, DeploymentID: "analyst", SkillID: "gmail", SkillVersion: "1",
		Credentials: map[string]skill.CredentialReference{"access": {Kind: "oauth2", ID: "grant-1"}}}
	if err := validate(run, memberGmail, capability.Action{Name: "send", Risk: capability.RiskLevelExternal}); err == nil {
		t.Fatal("a member's own Gmail connection must not be usable in a Team run")
	}
	memberMCP := &skill.Binding{Scope: skillScope, DeploymentID: "analyst", SkillID: "openseal.mcp", SkillVersion: "1.3.0"}
	if err := validate(run, memberMCP, capability.Action{Name: "call", Risk: capability.RiskLevelWrite}); err == nil {
		t.Fatal("a member's own MCP app must not be usable in a Team run")
	}
	memberPlain := &skill.Binding{Scope: skillScope, DeploymentID: "analyst", SkillID: "notes", SkillVersion: "1"}
	if err := validate(run, memberPlain, capability.Action{Name: "write", Risk: capability.RiskLevelWrite}); err != nil {
		t.Fatalf("a member's own plain Skill stays usable in its Team: %v", err)
	}
	if err := validate(run, &skill.Binding{Scope: skillScope, DeploymentID: "finance", SkillID: "forum", SkillVersion: "1"}, capability.Action{Name: "search", Risk: capability.RiskLevelRead}); err == nil {
		t.Fatal("expected foreign deployment binding to fail")
	}

	// A 1:1 run never uses a Team binding, and keeps its own connections.
	agentRun := &AgentRun{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "analyst"}, AssignedAgentID: "analyst"}
	if err := validate(agentRun, teamForum, capability.Action{Name: "search", Risk: capability.RiskLevelRead}); err == nil {
		t.Fatal("a Team connection must not be usable in the member's 1:1 run")
	}
	if err := validate(agentRun, memberGmail, capability.Action{Name: "send", Risk: capability.RiskLevelExternal}); err != nil {
		t.Fatalf("the Agent's own connection works in its 1:1 run: %v", err)
	}
}

func TestTeamSkillActionDispatcherStopsRevokedRosterAuthority(t *testing.T) {
	scope := Scope{Kind: "tenant", ID: "one"}
	catalog := teamSkillAuthorityFixture(scope)
	run := &AgentRun{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "research"}, AssignedAgentID: "analyst"}
	bound := &skill.BoundAction{
		Binding: &skill.Binding{Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "research", SkillID: "forum", SkillVersion: "1"},
		Action:  capability.Action{Name: "search", Risk: capability.RiskLevelRead},
	}
	dispatched := 0
	dispatcher, err := NewTeamSkillActionDispatcher(catalog, ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
		dispatched++
		return map[string]interface{}{"ok": true}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = dispatcher.DispatchAction(t.Context(), ActionDispatchInput{Run: run, Bound: bound}); err != nil {
		t.Fatal(err)
	}
	if dispatched != 1 {
		t.Fatalf("authorized dispatches = %d, want 1", dispatched)
	}

	// Simulate removal after proposal/approval but before the worker reaches the
	// transport. The final authority check must fail closed without invoking it.
	catalog.deployment.Roster = nil
	if _, err = dispatcher.DispatchAction(t.Context(), ActionDispatchInput{Run: run, Bound: bound}); err == nil {
		t.Fatal("expected revoked roster membership to stop dispatch")
	}
	if dispatched != 1 {
		t.Fatalf("transport ran after authority revocation: %d dispatches", dispatched)
	}
}

func teamSkillAuthorityFixture(scope Scope) teamSkillAuthorityCatalogStub {
	return teamSkillAuthorityCatalogStub{
		agent:           &kernelagent.AgentDeployment{ID: "analyst", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DefinitionID: "analyst", ActiveVersion: "1", RolloutStatus: kernelagent.RolloutActive},
		agentDefinition: &kernelagent.AgentDefinition{ID: "analyst", Version: "1", Authority: kernelagent.AuthorityPolicy{MaximumRisk: capability.RiskLevelExternal, AllowedSkillIDs: []string{"forum"}}},
		deployment: &kernelteam.Deployment{
			ID: "research", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DefinitionID: "research", ActiveVersion: "1", Status: kernelteam.DeploymentActive, Revision: 3,
			Roster: []kernelteam.RosterAssignment{{ID: "analyst", AgentDeploymentID: "analyst"}}, Restrictions: kernelteam.DeploymentRestrictions{MaximumRisk: capability.RiskLevelExternal},
		},
		definition: &kernelteam.Definition{
			ID: "research", Version: "1", Approvals: kernelteam.ApprovalPolicy{MaximumRisk: capability.RiskLevelExternal},
		},
	}
}
