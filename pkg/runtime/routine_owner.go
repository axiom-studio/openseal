package runtime

import (
	"context"
	"errors"

	"github.com/axiom-studio/openseal/pkg/skill"
)

// runbookActionOwner requires run's acting Agent to hold the scoped binding
// (its own, or its owning Team's) and to be on its owner's current roster.
func runbookActionOwner(ctx context.Context, teams RoutineTeamCatalog, run *AgentRun, bound *skill.BoundAction) error {
	if run == nil || bound == nil || bound.Binding == nil || run.AssignedAgentID == "" || (bound.Binding.DeploymentID != run.AssignedAgentID && !(run.Owner.Type == OwnerTypeTeam && bound.Binding.DeploymentID == run.Owner.ID)) || bound.Binding.Scope.Kind != run.Scope.Kind || bound.Binding.Scope.ID != run.Scope.ID {
		return errors.New("runbook action requires its current Agent's scoped binding")
	}
	var roster TeamRosterReader
	if teams != nil {
		roster = newRoutineTeamRoster(teams)
	} else if run.Owner.Type == OwnerTypeTeam {
		return errors.New("runbook action requires the current authorized Team roster")
	}
	onRoster, err := AgentOnChatRoster(ctx, roster, run.Scope, run.Owner, run.AssignedAgentID)
	if err != nil {
		return err
	}
	if !onRoster {
		return errors.New("runbook action Agent is not on its owner's current roster")
	}
	return nil
}
