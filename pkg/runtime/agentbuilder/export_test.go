package agentbuilder

import (
	"context"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/skill"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

// Test-only access to agent builder internals for the external agent
// builder tests, which use fixtures shared with tests of other packages and
// so cannot be in-package tests.

const (
	AgentBuilderDefinitionVersion = agentBuilderDefinitionVersion
	AgentTrialMaxTurns            = agentTrialMaxTurns
	SharedAgentMCPSkillID         = sharedAgentMCPSkillID
)

func AgentBuilderResourceID(kind string, scope models.Scope, callID string) string {
	return agentBuilderResourceID(kind, scope, callID)
}

func AgentBuilderBindingID(deploymentID, skillID string) string {
	return agentBuilderBindingID(deploymentID, skillID)
}

func AgentBehaviorActionCandidateVersion(baseVersion, actionID string) string {
	return agentBehaviorActionCandidateVersion(baseVersion, actionID)
}

func CloneAgentDefinitionForAction(value *kernelagent.AgentDefinition) *kernelagent.AgentDefinition {
	return cloneAgentDefinitionForAction(value)
}

func AgentTrialRunID(parentRunID, callID string) string { return agentTrialRunID(parentRunID, callID) }

func SharedAgentNameMatches(current, shared string) bool {
	return sharedAgentNameMatches(current, shared)
}

// TeamBuilderDefinition is the definition create_team builds for a Team
// named displayName whose members run the given Agent definitions.
func TeamBuilderDefinition(id, displayName string, members []*kernelagent.AgentDefinition, callID, approver string) *kernelteam.Definition {
	return teamBuilderDefinition(id, teamCreateArguments{DisplayName: displayName}, resolvedTeamMembers(members), callID, approver)
}

// TeamMembersMaximumRisk is the Team ceiling for members that run the given
// Agent definitions.
func TeamMembersMaximumRisk(members []*kernelagent.AgentDefinition) capability.RiskLevel {
	return teamMembersMaximumRisk(resolvedTeamMembers(members))
}

func resolvedTeamMembers(definitions []*kernelagent.AgentDefinition) []resolvedTeamMember {
	members := make([]resolvedTeamMember, len(definitions))
	for i, definition := range definitions {
		members[i] = resolvedTeamMember{definition: definition, name: definition.DisplayName}
	}
	return members
}

func (i *AgentInstaller) GrantAgentSkill(ctx context.Context, scope models.Scope, deploymentID string, grant models.AgentSkillGrant, credentials map[string]skill.CredentialReference, actor skill.BindingActor, reason string) (*skill.Binding, error) {
	return i.grantAgentSkill(ctx, scope, deploymentID, grant, credentials, actor, reason)
}

func (i *AgentInstaller) ActivateApprovedAgentVersion(ctx context.Context, reference capability.ScopeReference, deployment *kernelagent.AgentDeployment, candidate *kernelagent.AgentDefinition, approver string) (*kernelagent.AgentDeployment, *kernelagent.DefinitionActivation, error) {
	return i.activateApprovedAgentVersion(ctx, reference, deployment, candidate, approver)
}

func (i *AgentInstaller) DisableAgentSkill(ctx context.Context, scope skill.ScopeReference, deploymentID, bindingID string, actor skill.BindingActor) error {
	return i.disableAgentSkill(ctx, scope, deploymentID, bindingID, actor)
}
