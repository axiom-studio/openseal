package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
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

// AuthorizeTeamSkillActivation projects an immutable Team Skill snapshot to
// the run's acting member. Every active roster member gets every Team
// binding as the binding itself allows; the Team's Skill allowlist and the
// risk ceilings of the Team and of the member Agent still apply.
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
		if !authority.skillAllowed(activated.SkillID) {
			continue
		}
		projected := activated
		projected.Actions = nil
		for _, action := range activated.Actions {
			if authority.allowsRisk(action.Risk) {
				action.DeploymentID = authority.deployment.ID
				projected.Actions = append(projected.Actions, action)
			}
		}
		if projected.Prompt != nil || len(projected.Actions) > 0 {
			result.Skills = append(result.Skills, projected)
		}
	}
	for _, unavailable := range snapshot.Unavailable {
		if authority.skillAllowed(unavailable.SkillID) {
			result.Unavailable = append(result.Unavailable, unavailable)
		}
	}
	payload, _ := json.Marshal(struct {
		SnapshotID         string                   `json:"snapshotId"`
		DeploymentRevision int64                    `json:"deploymentRevision"`
		MemberID           string                   `json:"memberId"`
		Skills             []skill.ActivatedSkill   `json:"skills"`
		Unavailable        []skill.UnavailableSkill `json:"unavailable,omitempty"`
	}{snapshot.SnapshotID, authority.deployment.Revision, authority.agent.ID, result.Skills, result.Unavailable})
	digest := sha256.Sum256(payload)
	result.SnapshotID = "sha256:" + hex.EncodeToString(digest[:])
	return &result, nil
}

// TeamSkillActionValidator rechecks durable membership and Team authority at
// proposal time. This closes the gap between turn snapshot creation and action
// persistence if a roster or Team status changes concurrently.
type TeamSkillActionValidator struct{ catalog TeamSkillAuthorityCatalog }

func NewTeamSkillActionValidator(catalog TeamSkillAuthorityCatalog) (*TeamSkillActionValidator, error) {
	if catalog == nil {
		return nil, errors.New("Team Skill authority catalog is required")
	}
	return &TeamSkillActionValidator{catalog: catalog}, nil
}

func (v *TeamSkillActionValidator) ValidateActionProposal(ctx context.Context, input agentrun.ActionProposalValidationInput) (map[string]interface{}, error) {
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
		// A run delegated to another Agent works without connections: the
		// Agent never lends its own.
		if input.Run.Owner.ID != input.Run.AssignedAgentID && skill.BindingHoldsConnection(binding) {
			return nil, fmt.Errorf("delegated work cannot use %s's own connection %s", input.Run.AssignedAgentID, binding.ID)
		}
		return nil, nil
	}
	authority, err := resolveTeamSkillAuthority(ctx, v.catalog, input.Run)
	if err != nil {
		return nil, err
	}
	if binding.DeploymentID == input.Run.AssignedAgentID {
		// A member's own connections never enter its Team's runs.
		if skill.BindingHoldsConnection(binding) {
			return nil, fmt.Errorf("Team Run cannot use the member's own connection %s; connect it to the Team", binding.ID)
		}
		return nil, nil
	}
	if binding.DeploymentID != input.Run.Owner.ID {
		return nil, errors.New("Team Run cannot use a Skill binding owned outside its Agent or Team")
	}
	if !authority.skillAllowed(binding.SkillID) || !authority.allowsRisk(input.Bound.Action.Risk) {
		return nil, fmt.Errorf("Team %s does not allow %s.%s", authority.deployment.ID, binding.SkillID, input.Bound.Action.Name)
	}
	return nil, nil
}

// TeamSkillActionDispatcher revalidates Team membership and authority at the
// final execution boundary. Approval is intentionally not a permanent
// capability grant: an Agent removed from the roster, a paused Team, or
// narrowed authority must stop an already-approved call before any external
// tool is invoked.
type TeamSkillActionDispatcher struct {
	validator *TeamSkillActionValidator
	next      agentrun.ActionDispatcher
}

func NewTeamSkillActionDispatcher(catalog TeamSkillAuthorityCatalog, next agentrun.ActionDispatcher) (*TeamSkillActionDispatcher, error) {
	if next == nil {
		return nil, errors.New("next action dispatcher is required")
	}
	validator, err := NewTeamSkillActionValidator(catalog)
	if err != nil {
		return nil, err
	}
	return &TeamSkillActionDispatcher{validator: validator, next: next}, nil
}

func (d *TeamSkillActionDispatcher) DispatchAction(ctx context.Context, input agentrun.ActionDispatchInput) (map[string]interface{}, error) {
	if d == nil || d.validator == nil || d.next == nil {
		return nil, errors.New("Team Skill action dispatcher is not configured")
	}
	if _, err := d.validator.ValidateActionProposal(ctx, agentrun.ActionProposalValidationInput{
		Run: input.Run, Bound: input.Bound, Arguments: input.Arguments,
	}); err != nil {
		return nil, fmt.Errorf("revalidate Team Skill authority before dispatch: %w", err)
	}
	return d.next.DispatchAction(ctx, input)
}

type teamSkillAuthority struct {
	deployment      *kernelteam.Deployment
	definition      *kernelteam.Definition
	agent           *kernelagent.AgentDeployment
	agentDefinition *kernelagent.AgentDefinition
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
	if !slices.ContainsFunc(deployment.Roster, func(assignment kernelteam.RosterAssignment) bool {
		return assignment.AgentDeploymentID == run.AssignedAgentID
	}) {
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
	return &teamSkillAuthority{deployment: deployment, definition: definition, agent: agentDeployment, agentDefinition: agentDefinition}, nil
}

// skillAllowed applies the Team deployment's Skill allowlist. The member
// Agent's allowlists do not narrow Team bindings: every member uses every
// Team connection.
func (a *teamSkillAuthority) skillAllowed(skillID string) bool {
	allowlist := a.deployment.Restrictions.AllowedSkillIDs
	return len(allowlist) == 0 || slices.ContainsFunc(allowlist, func(candidate string) bool {
		return strings.TrimSpace(candidate) == strings.TrimSpace(skillID)
	})
}

func (a *teamSkillAuthority) allowsRisk(risk capability.RiskLevel) bool {
	maximum := a.definition.Approvals.MaximumRisk
	boundaries := []capability.RiskLevel{a.deployment.Restrictions.MaximumRisk, a.agentDefinition.Authority.MaximumRisk}
	if a.agent.Restrictions.MaximumRisk != nil {
		boundaries = append(boundaries, *a.agent.Restrictions.MaximumRisk)
	}
	for _, boundary := range boundaries {
		if boundary != "" && boundary.RankUnknownHighest() < maximum.RankUnknownHighest() {
			maximum = boundary
		}
	}
	return risk.RankUnknownHighest() <= maximum.RankUnknownHighest()
}

// teamRosterReader returns store's Team deployment reader, or nil.
func teamRosterReader(store any) TeamRosterReader {
	teams, _ := store.(TeamRosterReader)
	return teams
}
