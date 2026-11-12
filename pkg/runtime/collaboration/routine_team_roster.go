package collaboration

import (
	"context"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/skill"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

// RoutineTeamCatalog reads Team deployments so routine actions can check the
// acting Agent against its Team's current roster.
type RoutineTeamCatalog interface {
	GetDeployment(context.Context, skill.ScopeReference, string) (*kernelteam.Deployment, error)
}

// routineTeamRoster reads Team deployments through a RoutineTeamCatalog.
type routineTeamRoster struct{ teams RoutineTeamCatalog }

// NewRoutineTeamRoster reads Team rosters for the chat roster checks of
// routine and Team member actions.
func NewRoutineTeamRoster(teams RoutineTeamCatalog) models.TeamRosterReader {
	return routineTeamRoster{teams: teams}
}

func (r routineTeamRoster) GetTeamDeployment(ctx context.Context, scope capability.ScopeReference, id string) (*kernelteam.Deployment, error) {
	return r.teams.GetDeployment(ctx, scope, id)
}
