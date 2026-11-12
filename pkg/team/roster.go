package team

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

// ErrRosterMinimum reports a removal that would leave a Team without a
// member.
var ErrRosterMinimum = errors.New("a team needs at least one member")

// RosterChange is a Team deployment update (a roster edit, or archiving) the
// registry has validated and not stored: a store commits it together with
// other writes.
type RosterChange struct {
	Deployment       *Deployment
	ExpectedRevision int64
	Activation       workforce.DefinitionActivation
}

// PrepareAgentRemoval takes the Agent off the Team's roster. It returns nil
// when the Agent is not on the roster or the Team is archived.
func (r *Registry) PrepareAgentRemoval(ctx context.Context, scope capability.ScopeReference, deploymentID, agentID, actorType, actorID, reason string) (*RosterChange, error) {
	if r == nil || r.store == nil || r.agents == nil {
		return nil, errors.New("team registry and Agent resolver are required")
	}
	current, err := r.store.GetTeamDeployment(ctx, scope, deploymentID)
	if err != nil {
		return nil, err
	}
	index := slices.IndexFunc(current.Roster, func(assignment RosterAssignment) bool { return assignment.AgentDeploymentID == agentID })
	if index < 0 || current.Status == DeploymentArchived {
		return nil, nil
	}
	if len(current.Roster) == 1 {
		return nil, ErrRosterMinimum
	}
	if strings.TrimSpace(actorType) == "" || strings.TrimSpace(actorID) == "" || strings.TrimSpace(reason) == "" {
		return nil, errors.New("team deployment update actor and reason are required")
	}
	definition, err := r.store.GetTeamDefinition(ctx, current.DefinitionID, current.ActiveVersion)
	if err != nil {
		return nil, err
	}
	proposed := cloneDeployment(current)
	proposed.Roster = slices.Delete(proposed.Roster, index, index+1)
	change := &RosterChange{ExpectedRevision: current.Revision}
	if change.Deployment, change.Activation, err = r.prepareUpdate(ctx, current, proposed, definition, actorType, actorID, reason); err != nil {
		return nil, err
	}
	return change, nil
}
