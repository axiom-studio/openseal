package daemon

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/team"
)

// DesktopConversationCatalog reads canonical deployment and immutable definition
// records. Neither renderer-provided members nor model-proposed identities grant
// channel participation authority.
type DesktopConversationCatalog interface {
	DesktopWorkCatalog
	GetTeamDefinition(context.Context, string, string) (*team.Definition, error)
	GetAgentDefinition(context.Context, string, string) (*agent.AgentDefinition, error)
}

type DesktopConversationParticipants struct {
	catalog DesktopConversationCatalog
	scope   runtime.Scope
	maximum int
}

func NewDesktopConversationParticipants(catalog DesktopConversationCatalog, scope runtime.Scope, maximum int) (*DesktopConversationParticipants, error) {
	if catalog == nil || scope.Kind != "local" || scope.Validate() != nil || maximum < 1 || maximum > 256 {
		return nil, errors.New("desktop participation requires a local catalog, scope, and participant limit")
	}
	return &DesktopConversationParticipants{catalog: catalog, scope: scope, maximum: maximum}, nil
}

// ResolveConversationParticipants rechecks status, membership, role eligibility,
// and exact active definitions on every round. Observe-only and disabled roles
// never invoke a model. Inactive Agents are excluded; broken canonical identities
// or unsatisfied role requirements fail closed instead of inventing a roster.
func (s *DesktopConversationParticipants) ResolveConversationParticipants(ctx context.Context, query runtime.ConversationParticipantQuery) ([]runtime.ConversationParticipantBinding, error) {
	members, err := s.resolve(ctx, query)
	if err != nil {
		return nil, err
	}
	bindings := make([]runtime.ConversationParticipantBinding, 0, len(members))
	for _, member := range members {
		bindings = append(bindings, member.binding)
	}
	return bindings, nil
}

type desktopConversationMember struct {
	binding        runtime.ConversationParticipantBinding
	definition     *agent.AgentDefinition
	role           team.RoleSlot
	teamDefinition *team.Definition
}

func (s *DesktopConversationParticipants) resolve(ctx context.Context, query runtime.ConversationParticipantQuery) ([]desktopConversationMember, error) {
	channel := query.Conversation
	if channel == nil || channel.Scope != s.scope || channel.Owner.Type != runtime.OwnerTypeTeam || channel.Owner.ID == "" || channel.Status != runtime.ConversationStatusActive {
		return nil, errors.New("desktop participation requires an active team channel in this workspace")
	}
	if query.Trigger != nil && (query.Trigger.Scope != s.scope || query.Trigger.ConversationID != channel.ID) {
		return nil, errors.New("participation trigger does not belong to this channel")
	}
	scope := capability.ScopeReference{Kind: s.scope.Kind, ID: s.scope.ID}
	deployment, err := s.catalog.GetTeamDeployment(ctx, scope, channel.Owner.ID)
	if err != nil {
		return nil, fmt.Errorf("load participation team: %w", err)
	}
	if deployment == nil || deployment.ID != channel.Owner.ID || deployment.Scope != scope || deployment.Status != team.DeploymentActive || deployment.Activation != nil {
		return nil, errors.New("the participation team must be active in this workspace")
	}
	definition, err := s.catalog.GetTeamDefinition(ctx, deployment.DefinitionID, deployment.ActiveVersion)
	if err != nil {
		return nil, fmt.Errorf("load participation team definition: %w", err)
	}
	if definition == nil || definition.Validate() != nil || deployment.Validate(definition) != nil {
		return nil, errors.New("the participation team has an invalid active definition or roster")
	}
	roles := make(map[string]team.RoleSlot, len(definition.Roles))
	for _, role := range definition.Roles {
		roles[role.ID] = role
	}
	members := make([]desktopConversationMember, 0, len(deployment.Roster))
	for _, assignment := range deployment.Roster {
		role := roles[assignment.RoleID]
		if role.ChannelParticipation == team.RoleChannelObserveOnly || role.ChannelParticipation == team.RoleChannelDisabled {
			continue
		}
		member, err := s.catalog.GetAgentDeployment(ctx, scope, assignment.AgentDeploymentID)
		if err != nil {
			return nil, fmt.Errorf("load participation agent: %w", err)
		}
		if member == nil || member.ID != assignment.AgentDeploymentID || member.Scope != scope || member.Validate() != nil {
			return nil, errors.New("a participation agent is unavailable in this workspace")
		}
		if member.RolloutStatus != agent.RolloutActive || member.Activation != nil {
			continue
		}
		if _, bound := member.Credentials[agent.ModelProviderCredentialBinding]; bound {
			return nil, errors.New("a participation agent requires a deployment-specific model provider that the desktop cannot use yet")
		}
		agentDefinition, err := s.catalog.GetAgentDefinition(ctx, member.DefinitionID, member.ActiveVersion)
		if err != nil {
			return nil, fmt.Errorf("load participation agent definition: %w", err)
		}
		if agentDefinition == nil || agentDefinition.ID != member.DefinitionID || agentDefinition.Version != member.ActiveVersion || agentDefinition.Validate() != nil {
			return nil, errors.New("a participation agent has an invalid active definition")
		}
		if len(role.RequiredDefinitionIDs) > 0 && !slices.Contains(role.RequiredDefinitionIDs, agentDefinition.ID) {
			return nil, errors.New("a participation agent no longer satisfies its role definition requirement")
		}
		for _, required := range role.RequiredSkillIDs {
			if !slices.ContainsFunc(agentDefinition.SkillRequirements, func(skill agent.SkillRequirement) bool { return skill.SkillID == required }) {
				return nil, errors.New("a participation agent no longer satisfies its role skill requirement")
			}
		}
		binding := runtime.ConversationParticipantBinding{Participant: runtime.ConversationParticipant{Type: runtime.ConversationParticipantAgent, ID: member.ID}, SemanticRoles: []string{role.ID}}
		if err := binding.Validate(); err != nil {
			return nil, err
		}
		members = append(members, desktopConversationMember{binding: binding, definition: agentDefinition, role: role, teamDefinition: definition})
		if len(members) > s.maximum {
			return nil, errors.New("the team exceeds the desktop channel participation limit")
		}
	}
	if len(members) == 0 {
		return nil, runtime.ErrNoConversationParticipants
	}
	sort.Slice(members, func(i, j int) bool { return members[i].binding.Participant.ID < members[j].binding.Participant.ID })
	return members, nil
}
