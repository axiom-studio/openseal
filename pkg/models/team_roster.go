package models

import (
	"context"
	"errors"
	"strings"

	"github.com/axiom-studio/openseal/pkg/capability"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

// AgentOnChatRoster reports whether agentID is on the roster of the chat
// owned by owner: the owner of an Agent's chat, or a member of the owning
// Team's current active deployment. An empty agent, an unknown owner, a
// missing Team catalog and a missing or inactive Team are not members; store
// errors are returned.
func AgentOnChatRoster(ctx context.Context, teams TeamRosterReader, scope Scope, owner ObjectiveOwner, agentID string) (bool, error) {
	if agentID == "" || strings.TrimSpace(agentID) != agentID {
		return false, nil
	}
	switch owner.Type {
	case OwnerTypeAgent:
		return owner.ID != "" && owner.ID == agentID, nil
	case OwnerTypeTeam:
		roster, active, err := TeamRoster(ctx, teams, scope, owner.ID)
		if err != nil || !active {
			return false, err
		}
		for _, id := range roster {
			if id == agentID {
				return true, nil
			}
		}
	}
	return false, nil
}

// TeamRoster is the Agent deployment IDs on teamID's current deployment and
// whether that deployment is active. A missing Team, a missing catalog and a
// deployment from another scope have no roster.
func TeamRoster(ctx context.Context, teams TeamRosterReader, scope Scope, teamID string) ([]string, bool, error) {
	if teams == nil || strings.TrimSpace(teamID) == "" {
		return nil, false, nil
	}
	catalogScope := capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}
	deployment, err := teams.GetTeamDeployment(ctx, catalogScope, teamID)
	if errors.Is(err, kernelteam.ErrDeploymentNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if deployment == nil || deployment.ID != teamID || deployment.Scope != catalogScope {
		return nil, false, nil
	}
	seen := make(map[string]bool, len(deployment.Roster))
	roster := make([]string, 0, len(deployment.Roster))
	for _, assignment := range deployment.Roster {
		if id := strings.TrimSpace(assignment.AgentDeploymentID); id != "" && !seen[id] {
			seen[id] = true
			roster = append(roster, id)
		}
	}
	return roster, deployment.Status == kernelteam.DeploymentActive, nil
}
