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

func TestTeamSkillAuthorityIntersectsBindingRoleDeploymentAndMembership(t *testing.T) {
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
			},
		}, {
			BindingID: "email", BindingRevision: 1, SkillID: "email", SkillVersion: "1", Actions: []capability.ModelAction{{Name: "email.send", SkillID: "email", Version: "1", Action: "send", Risk: capability.RiskLevelExternal}},
		}},
	}
	projected, err := AuthorizeTeamSkillActivation(t.Context(), catalog, run, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if projected.SnapshotID == snapshot.SnapshotID || len(projected.Skills) != 1 || projected.Skills[0].Prompt == nil || len(projected.Skills[0].Actions) != 1 ||
		projected.Skills[0].Actions[0].Action != "search" || projected.Skills[0].Actions[0].DeploymentID != "research" {
		t.Fatalf("projected Team authority = %#v", projected)
	}

	foreign := *run
	foreign.AssignedAgentID = "outsider"
	if _, err := AuthorizeTeamSkillActivation(t.Context(), catalog, &foreign, snapshot); err == nil {
		t.Fatal("expected non-member Team Skill projection to fail closed")
	}
}

func TestTeamSkillActionValidatorRechecksAuthorityWithoutWideningAgentBindings(t *testing.T) {
	scope := Scope{Kind: "tenant", ID: "one"}
	catalog := teamSkillAuthorityFixture(scope)
	validator, err := NewTeamSkillActionValidator(catalog)
	if err != nil {
		t.Fatal(err)
	}
	run := &AgentRun{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "research"}, AssignedAgentID: "analyst"}
	bound := &skill.BoundAction{
		Binding: &skill.Binding{Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "research", SkillID: "forum", SkillVersion: "1"},
		Action:  capability.Action{Name: "search", Risk: capability.RiskLevelRead},
	}
	if _, err := validator.ValidateActionProposal(t.Context(), ActionProposalValidationInput{Run: run, Bound: bound}); err != nil {
		t.Fatal(err)
	}
	bound.Action = capability.Action{Name: "reply", Risk: capability.RiskLevelExternal}
	if _, err := validator.ValidateActionProposal(t.Context(), ActionProposalValidationInput{Run: run, Bound: bound}); err == nil {
		t.Fatal("expected action outside the role grant to fail")
	}
	catalog.definition.Roles[0].SkillGrants[0].AllowedActions = []string{"search", "reply"}
	catalog.definition.Roles[0].SkillGrants[0].MaximumRisk = capability.RiskLevelExternal
	catalog.agentDefinition.Authority.MaximumRisk = capability.RiskLevelRead
	if _, err := validator.ValidateActionProposal(t.Context(), ActionProposalValidationInput{Run: run, Bound: bound}); err == nil {
		t.Fatal("Team role grant must not widen the assigned Agent risk authority")
	}
	catalog.agentDefinition.Authority.MaximumRisk = capability.RiskLevelExternal
	bound.Binding.DeploymentID = "finance"
	if _, err := validator.ValidateActionProposal(t.Context(), ActionProposalValidationInput{Run: run, Bound: bound}); err == nil {
		t.Fatal("expected foreign deployment binding to fail")
	}

	bound.Binding.DeploymentID = "analyst"
	bound.Action = capability.Action{Name: "reply", Risk: capability.RiskLevelExternal}
	if _, err := validator.ValidateActionProposal(t.Context(), ActionProposalValidationInput{Run: run, Bound: bound}); err != nil {
		t.Fatalf("Agent-owned authority should remain independent: %v", err)
	}
	agentRun := &AgentRun{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "analyst"}, AssignedAgentID: "analyst"}
	bound.Binding.DeploymentID = "other"
	if _, err := validator.ValidateActionProposal(t.Context(), ActionProposalValidationInput{Run: agentRun, Bound: bound}); err == nil {
		t.Fatal("expected Agent Run foreign binding to fail")
	}
}

func teamSkillAuthorityFixture(scope Scope) teamSkillAuthorityCatalogStub {
	return teamSkillAuthorityCatalogStub{
		agent:           &kernelagent.AgentDeployment{ID: "analyst", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DefinitionID: "analyst", ActiveVersion: "1", RolloutStatus: kernelagent.RolloutActive},
		agentDefinition: &kernelagent.AgentDefinition{ID: "analyst", Version: "1", Authority: kernelagent.AuthorityPolicy{MaximumRisk: capability.RiskLevelExternal, AllowedSkillIDs: []string{"forum"}}},
		deployment: &kernelteam.Deployment{
			ID: "research", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DefinitionID: "research", ActiveVersion: "1", Status: kernelteam.DeploymentActive, Revision: 3,
			Roster: []kernelteam.RosterAssignment{{ID: "analyst", RoleID: "analyst", AgentDeploymentID: "analyst"}}, Restrictions: kernelteam.DeploymentRestrictions{AllowedSkillIDs: []string{"forum"}, MaximumRisk: capability.RiskLevelExternal},
		},
		definition: &kernelteam.Definition{
			ID: "research", Version: "1", Approvals: kernelteam.ApprovalPolicy{MaximumRisk: capability.RiskLevelExternal},
			Roles: []kernelteam.RoleSlot{{ID: "analyst", SkillGrants: []kernelteam.RoleSkillGrant{{SkillID: "forum", SkillVersion: "1", AllowedActions: []string{"search"}, EnablePrompt: true, MaximumRisk: capability.RiskLevelRead}}}},
		},
	}
}
