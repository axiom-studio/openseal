package daemon

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
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
	scope   models.Scope
	maximum int
}

func NewDesktopConversationParticipants(catalog DesktopConversationCatalog, scope models.Scope, maximum int) (*DesktopConversationParticipants, error) {
	if catalog == nil || scope.Kind != "local" || scope.Validate() != nil || maximum < 1 || maximum > 256 {
		return nil, errors.New("desktop participation requires a local catalog, scope, and participant limit")
	}
	return &DesktopConversationParticipants{catalog: catalog, scope: scope, maximum: maximum}, nil
}

// ResolveConversationParticipants rechecks status, membership, participation,
// and exact active definitions on every round. Observe-only and disabled members
// never invoke a model. Inactive Agents are excluded; broken canonical identities
// or unsatisfied member requirements fail closed instead of inventing a roster.
func (s *DesktopConversationParticipants) ResolveConversationParticipants(ctx context.Context, query conversation.ConversationParticipantQuery) ([]conversation.ConversationParticipantBinding, error) {
	members, err := s.resolve(ctx, query)
	if err != nil {
		return nil, err
	}
	bindings := make([]conversation.ConversationParticipantBinding, 0, len(members))
	for _, member := range members {
		bindings = append(bindings, member.binding)
	}
	return bindings, nil
}

type desktopConversationMember struct {
	binding        conversation.ConversationParticipantBinding
	definition     *agent.AgentDefinition
	teamDefinition *team.Definition
}

func (s *DesktopConversationParticipants) resolve(ctx context.Context, query conversation.ConversationParticipantQuery) ([]desktopConversationMember, error) {
	channel := query.Conversation
	if channel == nil || channel.Scope != s.scope || channel.Owner.Type != models.OwnerTypeTeam || channel.Owner.ID == "" || channel.Status != models.ConversationStatusActive {
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
	if deployment == nil || deployment.ID != channel.Owner.ID || deployment.Scope != scope || deployment.Status != team.DeploymentActive {
		return nil, errors.New("the participation team must be active in this workspace")
	}
	definition, err := s.catalog.GetTeamDefinition(ctx, deployment.DefinitionID, deployment.ActiveVersion)
	if err != nil {
		return nil, fmt.Errorf("load participation team definition: %w", err)
	}
	if definition == nil || definition.Validate() != nil || deployment.Validate(definition) != nil {
		return nil, errors.New("the participation team has an invalid active definition or roster")
	}
	members := make([]desktopConversationMember, 0, len(deployment.Roster))
	for _, assignment := range deployment.Roster {
		if assignment.ChannelParticipation == team.ChannelObserveOnly || assignment.ChannelParticipation == team.ChannelDisabled {
			continue
		}
		member, err := s.catalog.GetAgentDeployment(ctx, scope, assignment.AgentDeploymentID)
		if err != nil {
			return nil, fmt.Errorf("load participation agent: %w", err)
		}
		if member == nil || member.ID != assignment.AgentDeploymentID || member.Scope != scope || member.Validate() != nil {
			return nil, errors.New("a participation agent is unavailable in this workspace")
		}
		if member.RolloutStatus != agent.RolloutActive {
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
		for _, required := range assignment.RequiredSkillIDs {
			if !slices.ContainsFunc(agentDefinition.SkillRequirements, func(skill agent.SkillRequirement) bool { return skill.SkillID == required }) {
				return nil, errors.New("a participation agent no longer satisfies its member skill requirement")
			}
		}
		binding := conversation.ConversationParticipantBinding{Participant: models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: member.ID}, SemanticRoles: []string{assignment.ID}}
		if err := binding.Validate(); err != nil {
			return nil, err
		}
		members = append(members, desktopConversationMember{binding: binding, definition: agentDefinition, teamDefinition: definition})
		if len(members) > s.maximum {
			return nil, errors.New("the team exceeds the desktop channel participation limit")
		}
	}
	if len(members) == 0 {
		return nil, conversation.ErrNoConversationParticipants
	}
	sort.Slice(members, func(i, j int) bool { return members[i].binding.Participant.ID < members[j].binding.Participant.ID })
	return members, nil
}
