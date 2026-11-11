package team

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

// ErrRosterMinimum refuses a removal that would leave a Team without a
// member, or one of its roles with fewer members than it requires.
var ErrRosterMinimum = errors.New("team roster would fall below its minimum")

// RosterChange is a Team deployment update the registry has validated and
// not stored: a store commits it together with other writes.
type RosterChange struct {
	// Definition is a new version to register first; nil keeps the active one.
	Definition       *Definition
	Deployment       *Deployment
	ExpectedRevision int64
	Activation       workforce.DefinitionActivation
}

// PrepareAgentRemoval takes the Agent off the Team's roster. A role nobody
// else holds goes with it, as a new definition version activated by the same
// update. It returns nil when the Agent is not on the roster or the Team is
// archived.
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
		return nil, fmt.Errorf("%w: a team needs at least one member", ErrRosterMinimum)
	}
	if strings.TrimSpace(actorType) == "" || strings.TrimSpace(actorID) == "" || strings.TrimSpace(reason) == "" {
		return nil, errors.New("team deployment update actor and reason are required")
	}
	definition, err := r.store.GetTeamDefinition(ctx, current.DefinitionID, current.ActiveVersion)
	if err != nil {
		return nil, err
	}
	proposed := cloneDeployment(current)
	roleID := proposed.Roster[index].RoleID
	proposed.Roster = slices.Delete(proposed.Roster, index, index+1)
	change := &RosterChange{ExpectedRevision: current.Revision}
	if !slices.ContainsFunc(proposed.Roster, func(assignment RosterAssignment) bool { return assignment.RoleID == roleID }) {
		versions, err := r.store.ListTeamDefinitionVersions(ctx, definition.ID)
		if err != nil {
			return nil, err
		}
		next := cloneDefinition(definition)
		next.Roles = slices.DeleteFunc(next.Roles, func(role RoleSlot) bool { return role.ID == roleID })
		next.Approvals.ApproverRoleIDs = slices.DeleteFunc(next.Approvals.ApproverRoleIDs, func(id string) bool { return id == roleID })
		next.Version = nextDefinitionVersion(definition.Version, versions)
		if definition, err = PrepareDefinition(next, r.now().UTC()); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrRosterMinimum, err)
		}
		change.Definition = definition
		proposed.ActiveVersion = definition.Version
	}
	if err := proposed.Validate(definition); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRosterMinimum, err)
	}
	if change.Deployment, change.Activation, err = r.prepareUpdate(ctx, current, proposed, definition, actorType, actorID, reason); err != nil {
		return nil, err
	}
	return change, nil
}

// nextDefinitionVersion bumps the last numeric part ("1" -> "2", "1.0.0" ->
// "1.0.1"), skipping versions that already exist.
func nextDefinitionVersion(current string, existing []*Definition) string {
	parts := strings.Split(current, ".")
	last, err := strconv.Atoi(parts[len(parts)-1])
	for step := 1; ; step++ {
		candidate := current + "." + strconv.Itoa(step)
		if err == nil {
			parts[len(parts)-1] = strconv.Itoa(last + step)
			candidate = strings.Join(parts, ".")
		}
		if !slices.ContainsFunc(existing, func(definition *Definition) bool { return definition.Version == candidate }) {
			return candidate
		}
	}
}
