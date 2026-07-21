package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

// TeamSkillAuthorityCatalog exposes the durable Team state needed to project
// a Team-owned binding to one roster Agent. The Skill catalog remains the
// authority for binding/configuration/credential policy; Team state can only
// narrow it.
type TeamSkillAuthorityCatalog interface {
	GetAgentDeployment(context.Context, skill.ScopeReference, string) (*kernelagent.AgentDeployment, error)
	GetAgentDefinition(context.Context, string, string) (*kernelagent.AgentDefinition, error)
	GetTeamDeployment(context.Context, skill.ScopeReference, string) (*kernelteam.Deployment, error)
	GetTeamDefinition(context.Context, string, string) (*kernelteam.Definition, error)
}

// AuthorizeTeamSkillActivation intersects an immutable Team Skill snapshot
// with the assigned Agent's immutable semantic-role grant. A roster assignment
// is not itself authority: the active role must explicitly grant the exact
// Skill version, actions, prompt visibility, and maximum risk.
func AuthorizeTeamSkillActivation(ctx context.Context, catalog TeamSkillAuthorityCatalog, run *AgentRun, snapshot *skill.ActivationSnapshot) (*skill.ActivationSnapshot, error) {
	authority, err := resolveTeamSkillAuthority(ctx, catalog, run)
	if err != nil {
		return nil, err
	}
	if snapshot == nil || snapshot.Scope != authority.deployment.Scope || snapshot.DeploymentID != authority.deployment.ID || strings.TrimSpace(snapshot.SnapshotID) == "" {
		return nil, errors.New("Team Skill activation does not match the owning Team deployment")
	}
	result := *snapshot
	result.Skills = make([]skill.ActivatedSkill, 0, len(snapshot.Skills))
	result.Unavailable = make([]skill.UnavailableSkill, 0, len(snapshot.Unavailable))
	for _, activated := range snapshot.Skills {
		grant, ok := authority.grants[teamSkillIdentity(activated.SkillID, activated.SkillVersion, activated.SourceIdentity).Key()]
		if !ok || !authority.skillAllowed(grant, activated.SkillID) {
			continue
		}
		projected := activated
		projected.Actions = nil
		for _, action := range activated.Actions {
			if teamSkillGrantAllowsAction(grant, action.Action, action.Risk) && authority.allowsRisk(action.Risk) {
				action.DeploymentID = authority.deployment.ID
				projected.Actions = append(projected.Actions, action)
			}
		}
		if !grant.EnablePrompt {
			projected.Prompt = nil
		}
		if projected.Prompt != nil || len(projected.Actions) > 0 {
			result.Skills = append(result.Skills, projected)
		}
	}
	for _, unavailable := range snapshot.Unavailable {
		grant, granted := authority.grants[teamSkillIdentity(unavailable.SkillID, unavailable.SkillVersion, unavailable.SourceIdentity).Key()]
		if granted && authority.skillAllowed(grant, unavailable.SkillID) {
			result.Unavailable = append(result.Unavailable, unavailable)
		}
	}
	payload, _ := json.Marshal(struct {
		SnapshotID         string                   `json:"snapshotId"`
		DeploymentRevision int64                    `json:"deploymentRevision"`
		AssignmentID       string                   `json:"assignmentId"`
		RoleID             string                   `json:"roleId"`
		Skills             []skill.ActivatedSkill   `json:"skills"`
		Unavailable        []skill.UnavailableSkill `json:"unavailable,omitempty"`
	}{snapshot.SnapshotID, authority.deployment.Revision, authority.assignment.ID, authority.assignment.RoleID, result.Skills, result.Unavailable})
	digest := sha256.Sum256(payload)
	result.SnapshotID = "sha256:" + hex.EncodeToString(digest[:])
	return &result, nil
}

// TeamSkillActionValidator rechecks durable membership and role authority at
// proposal time. This closes the gap between turn snapshot creation and action
// persistence if a roster, role grant, or Team status changes concurrently.
type TeamSkillActionValidator struct{ catalog TeamSkillAuthorityCatalog }

func NewTeamSkillActionValidator(catalog TeamSkillAuthorityCatalog) (*TeamSkillActionValidator, error) {
	if catalog == nil {
		return nil, errors.New("Team Skill authority catalog is required")
	}
	return &TeamSkillActionValidator{catalog: catalog}, nil
}

func (v *TeamSkillActionValidator) ValidateActionProposal(ctx context.Context, input ActionProposalValidationInput) (map[string]interface{}, error) {
	if input.Run == nil || input.Bound == nil || input.Bound.Binding == nil {
		return nil, nil
	}
	binding := input.Bound.Binding
	if binding.Scope != (skill.ScopeReference{Kind: input.Run.Scope.Kind, ID: input.Run.Scope.ID}) {
		return nil, errors.New("Skill binding scope does not match the Run scope")
	}
	if input.Run.Owner.Type != OwnerTypeTeam {
		if binding.DeploymentID != input.Run.AssignedAgentID {
			return nil, errors.New("Agent Run cannot use a Skill binding owned by another deployment")
		}
		return nil, nil
	}
	authority, err := resolveTeamSkillAuthority(ctx, v.catalog, input.Run)
	if err != nil {
		return nil, err
	}
	if binding.DeploymentID == input.Run.AssignedAgentID {
		return nil, nil // the Agent's own binding remains Agent-scoped authority
	}
	if binding.DeploymentID != input.Run.Owner.ID {
		return nil, errors.New("Team Run cannot use a Skill binding owned outside its Agent or Team")
	}
	grant, ok := authority.grants[teamSkillIdentity(binding.SkillID, binding.SkillVersion, binding.SourceIdentity).Key()]
	if !ok || !authority.skillAllowed(grant, binding.SkillID) || !teamSkillGrantAllowsAction(grant, input.Bound.Action.Name, input.Bound.Action.Risk) || !authority.allowsRisk(input.Bound.Action.Risk) {
		return nil, fmt.Errorf("Team role %s is not authorized for %s.%s", authority.assignment.RoleID, binding.SkillID, input.Bound.Action.Name)
	}
	return nil, nil
}

type teamSkillAuthority struct {
	deployment      *kernelteam.Deployment
	definition      *kernelteam.Definition
	agent           *kernelagent.AgentDeployment
	agentDefinition *kernelagent.AgentDefinition
	assignment      kernelteam.RosterAssignment
	grants          map[string]kernelteam.RoleSkillGrant
}

func resolveTeamSkillAuthority(ctx context.Context, catalog TeamSkillAuthorityCatalog, run *AgentRun) (*teamSkillAuthority, error) {
	if catalog == nil || run == nil || run.Owner.Type != OwnerTypeTeam || strings.TrimSpace(run.Owner.ID) == "" || strings.TrimSpace(run.AssignedAgentID) == "" {
		return nil, errors.New("Team-owned Skills require a Team Run with an assigned roster Agent")
	}
	scope := skill.ScopeReference{Kind: run.Scope.Kind, ID: run.Scope.ID}
	deployment, err := catalog.GetTeamDeployment(ctx, scope, run.Owner.ID)
	if err != nil || deployment == nil {
		return nil, fmt.Errorf("resolve owning Team deployment: %w", err)
	}
	if deployment.Scope != scope || deployment.Status != kernelteam.DeploymentActive {
		return nil, errors.New("owning Team deployment is not active in the Run scope")
	}
	definition, err := catalog.GetTeamDefinition(ctx, deployment.DefinitionID, deployment.ActiveVersion)
	if err != nil || definition == nil {
		return nil, fmt.Errorf("resolve active Team definition: %w", err)
	}
	var assignment *kernelteam.RosterAssignment
	for index := range deployment.Roster {
		if deployment.Roster[index].AgentDeploymentID == run.AssignedAgentID {
			copy := deployment.Roster[index]
			assignment = &copy
			break
		}
	}
	if assignment == nil {
		return nil, errors.New("assigned Agent is not a member of the owning Team")
	}
	agentDeployment, err := catalog.GetAgentDeployment(ctx, scope, run.AssignedAgentID)
	if err != nil || agentDeployment == nil {
		return nil, fmt.Errorf("resolve assigned Agent deployment: %w", err)
	}
	if agentDeployment.Scope != scope || agentDeployment.RolloutStatus != kernelagent.RolloutActive {
		return nil, errors.New("assigned roster Agent deployment is not active in the Run scope")
	}
	agentDefinition, err := catalog.GetAgentDefinition(ctx, agentDeployment.DefinitionID, agentDeployment.ActiveVersion)
	if err != nil || agentDefinition == nil {
		return nil, fmt.Errorf("resolve assigned Agent definition: %w", err)
	}
	var role *kernelteam.RoleSlot
	for index := range definition.Roles {
		if definition.Roles[index].ID == assignment.RoleID {
			copy := definition.Roles[index]
			role = &copy
			break
		}
	}
	if role == nil {
		return nil, errors.New("assigned Agent role is absent from the active Team definition")
	}
	grants := make(map[string]kernelteam.RoleSkillGrant, len(role.SkillGrants))
	for _, grant := range role.SkillGrants {
		grants[grant.ExactIdentity().Key()] = grant
	}
	return &teamSkillAuthority{deployment: deployment, definition: definition, agent: agentDeployment, agentDefinition: agentDefinition, assignment: *assignment, grants: grants}, nil
}

func (a *teamSkillAuthority) skillAllowed(grant kernelteam.RoleSkillGrant, skillID string) bool {
	identifiers := map[string]bool{strings.TrimSpace(skillID): true, strings.TrimSpace(grant.CatalogID): true}
	for _, allowlist := range [][]string{a.deployment.Restrictions.AllowedSkillIDs, a.agentDefinition.Authority.AllowedSkillIDs, a.agent.Restrictions.AllowedSkillIDs} {
		if len(allowlist) == 0 {
			continue
		}
		allowed := false
		for _, candidate := range allowlist {
			if identifiers[strings.TrimSpace(candidate)] {
				allowed = true
				break
			}
		}
		if !allowed {
			return false
		}
	}
	return true
}

func (a *teamSkillAuthority) allowsRisk(risk capability.RiskLevel) bool {
	maximum := a.definition.Approvals.MaximumRisk
	boundaries := []capability.RiskLevel{a.deployment.Restrictions.MaximumRisk, a.agentDefinition.Authority.MaximumRisk}
	if a.agent.Restrictions.MaximumRisk != nil {
		boundaries = append(boundaries, *a.agent.Restrictions.MaximumRisk)
	}
	for _, boundary := range boundaries {
		if boundary != "" && teamSkillRiskRank(boundary) < teamSkillRiskRank(maximum) {
			maximum = boundary
		}
	}
	return teamSkillRiskRank(risk) <= teamSkillRiskRank(maximum)
}

func teamSkillGrantAllowsAction(g kernelteam.RoleSkillGrant, action string, risk capability.RiskLevel) bool {
	if teamSkillRiskRank(risk) > teamSkillRiskRank(g.MaximumRisk) {
		return false
	}
	for _, allowed := range g.AllowedActions {
		if allowed == action {
			return true
		}
	}
	return false
}

func teamSkillIdentity(id, version, sourceIdentity string) capability.SkillIdentity {
	return capability.NewSkillIdentity(id, version, sourceIdentity)
}

func teamSkillRiskRank(risk capability.RiskLevel) int {
	switch risk {
	case capability.RiskLevelRead:
		return 0
	case capability.RiskLevelWrite:
		return 1
	case capability.RiskLevelExternal:
		return 2
	case capability.RiskLevelProduction:
		return 3
	case capability.RiskLevelDestructive:
		return 4
	default:
		return 1 << 30
	}
}
