package runtime

import (
	"context"
	"errors"
	"fmt"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// ErrRunUmbrellaUnavailable means a run no longer acts under a valid
// umbrella: an Agent run for another Agent, an inactive Team, or a member
// that left the Team or is no longer active.
var ErrRunUmbrellaUnavailable = errors.New("run umbrella is unavailable")

type umbrellaAgentReader interface {
	GetAgentDeployment(context.Context, capability.ScopeReference, string) (*kernelagent.AgentDeployment, error)
}

// umbrellaAgentStore is the kernel Agent store's reader of the same record.
type umbrellaAgentStore interface {
	GetDeployment(context.Context, capability.ScopeReference, string) (*kernelagent.AgentDeployment, error)
}

func umbrellaAgentDeployment(ctx context.Context, roster TeamRosterReader, scope capability.ScopeReference, id string) (*kernelagent.AgentDeployment, bool, error) {
	if agents, ok := roster.(umbrellaAgentReader); ok {
		deployment, err := agents.GetAgentDeployment(ctx, scope, id)
		return deployment, true, err
	}
	if agents, ok := roster.(umbrellaAgentStore); ok {
		deployment, err := agents.GetDeployment(ctx, scope, id)
		return deployment, true, err
	}
	return nil, false, nil
}

// RunUmbrella is the one place a run's umbrella is derived: run.Owner, the
// Team in a Team chat, task or hand-off and the Agent itself in a 1:1 chat.
// It fails closed. An Agent umbrella must be the run's assigned Agent. A Team
// umbrella must be an active Team whose roster holds the assigned Agent; when
// roster also reads Agent deployments, that member must be active too.
func RunUmbrella(ctx context.Context, roster TeamRosterReader, run *AgentRun) (ObjectiveOwner, error) {
	if run == nil || run.Owner.Validate() != nil || run.AssignedAgentID == "" {
		return ObjectiveOwner{}, fmt.Errorf("%w: run has no owner and acting Agent", ErrRunUmbrellaUnavailable)
	}
	switch run.Owner.Type {
	case OwnerTypeAgent:
		if run.Owner.ID != run.AssignedAgentID {
			return ObjectiveOwner{}, fmt.Errorf("%w: Agent run acts for another Agent", ErrRunUmbrellaUnavailable)
		}
		return run.Owner, nil
	case OwnerTypeTeam:
		member, err := AgentOnChatRoster(ctx, roster, run.Scope, run.Owner, run.AssignedAgentID)
		if err != nil {
			return ObjectiveOwner{}, err
		}
		if !member {
			return ObjectiveOwner{}, fmt.Errorf("%w: %s is not on the active Team %s", ErrRunUmbrellaUnavailable, run.AssignedAgentID, run.Owner.ID)
		}
		deployment, readable, err := umbrellaAgentDeployment(ctx, roster, capability.ScopeReference{Kind: run.Scope.Kind, ID: run.Scope.ID}, run.AssignedAgentID)
		if readable {
			if errors.Is(err, kernelagent.ErrDeploymentNotFound) || err == nil && (deployment == nil || deployment.RolloutStatus != kernelagent.RolloutActive) {
				return ObjectiveOwner{}, fmt.Errorf("%w: Team member %s is not active", ErrRunUmbrellaUnavailable, run.AssignedAgentID)
			}
			if err != nil {
				return ObjectiveOwner{}, err
			}
		}
		return run.Owner, nil
	}
	return ObjectiveOwner{}, ErrRunUmbrellaUnavailable
}

// ConversationUmbrella is the umbrella of every run in conversation.
func ConversationUmbrella(conversation *Conversation) ObjectiveOwner {
	if conversation == nil {
		return ObjectiveOwner{}
	}
	return conversation.Owner
}

// TaskUmbrella returns a task's umbrella and its acting member.
func TaskUmbrella(task *ConversationTask) (ObjectiveOwner, string) {
	if task == nil {
		return ObjectiveOwner{}, ""
	}
	return task.Owner, task.TargetAgentID
}

// OwnerOfSkillUmbrella converts the skill package's umbrella to an owner.
func OwnerOfSkillUmbrella(umbrella skill.Umbrella) ObjectiveOwner {
	return ObjectiveOwner{Type: OwnerType(umbrella.Kind), ID: umbrella.ID}
}

// InteractionRunResumable reports whether a settled setup, connection or
// credential request may resume run: the run still acts as agentID under the
// umbrella of the chat the request was made in (owner, or the builder's own
// chat for a request it made for another Agent). A member removed from its
// Team, or a Team that is no longer active, makes the request settle without
// resuming anything.
func InteractionRunResumable(ctx context.Context, roster TeamRosterReader, run *AgentRun, owner ObjectiveOwner, agentID string) (bool, error) {
	if run == nil || run.AssignedAgentID != agentID {
		return false, nil
	}
	umbrella, err := RunUmbrella(ctx, roster, run)
	if errors.Is(err, ErrRunUmbrellaUnavailable) {
		return false, nil
	}
	return err == nil && umbrella == interactionChatOwner(owner, agentID), err
}
