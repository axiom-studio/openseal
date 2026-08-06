package bundle

import (
	"errors"
	"fmt"
	"strings"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/team"
)

// ExportRequest is the reviewed live-state projection used to create one
// portable workforce artifact. Source deployment identities are used only to
// close references during export and never enter the resulting Bundle.
type ExportRequest struct {
	Metadata      Metadata
	Compatibility Compatibility
	Provenance    Provenance
	Agents        []AgentExport
	Teams         []TeamExport
	Objectives    []ObjectiveExport
	Runbooks      []RunbookExport
}

type AgentExport struct {
	Key     string
	Request agent.BundleExportRequest
}

type TeamExport struct {
	Key        string
	Definition *team.Definition
	Deployment *team.Deployment
}

type ObjectiveExport struct {
	Key       string
	Objective *runtime.Objective
}

type RunbookExport struct {
	Key        string
	Activation *runtime.RunbookActivation
}

// Export creates a deterministic, secret-free workforce artifact from exact
// reviewed resources. It resolves every source identity to a portable key and
// fails closed when the selection is not reference-complete.
func Export(request ExportRequest) (*Bundle, error) {
	result := New(request.Metadata)
	result.Compatibility = request.Compatibility
	if result.Compatibility.FormatRevision == 0 {
		result.Compatibility.FormatRevision = 1
	}
	result.Provenance = request.Provenance
	agentKeys, teamKeys, objectiveKeys := map[string]string{}, map[string]string{}, map[string]string{}

	for _, selected := range request.Agents {
		key := strings.TrimSpace(selected.Key)
		if key == "" || selected.Request.Deployment == nil {
			return nil, errors.New("workforce export Agent requires a portable key and source deployment")
		}
		if _, duplicate := agentKeys[selected.Request.Deployment.ID]; duplicate {
			return nil, fmt.Errorf("workforce export source Agent deployment %s is selected more than once", selected.Request.Deployment.ID)
		}
		artifact, err := agent.ExportBundle(selected.Request)
		if err != nil {
			return nil, fmt.Errorf("export workforce Agent %s: %w", key, err)
		}
		result.Agents = append(result.Agents, Agent{Key: key, Artifact: artifact})
		agentKeys[selected.Request.Deployment.ID] = key
	}
	for _, selected := range request.Teams {
		key := strings.TrimSpace(selected.Key)
		if key == "" || selected.Definition == nil || selected.Deployment == nil {
			return nil, errors.New("workforce export Team requires a portable key, definition, and deployment")
		}
		if _, duplicate := teamKeys[selected.Deployment.ID]; duplicate {
			return nil, fmt.Errorf("workforce export source Team deployment %s is selected more than once", selected.Deployment.ID)
		}
		definition, err := cloneJSON(selected.Definition)
		if err != nil {
			return nil, fmt.Errorf("export workforce Team %s: %w", key, err)
		}
		portable := Team{Key: key, Definition: definition, Deployment: TeamDeployment{Restrictions: selected.Deployment.Restrictions}}
		for _, assignment := range selected.Deployment.Roster {
			agentKey := agentKeys[assignment.AgentDeploymentID]
			if agentKey == "" {
				return nil, fmt.Errorf("workforce export Team %s roster assignment %s references an Agent outside the export", key, assignment.ID)
			}
			portable.Deployment.Roster = append(portable.Deployment.Roster, RosterAssignment{ID: assignment.ID, RoleID: assignment.RoleID, AgentKey: agentKey, DisplayName: assignment.DisplayName})
		}
		result.Teams = append(result.Teams, portable)
		teamKeys[selected.Deployment.ID] = key
	}
	resolveOwner := func(owner runtime.ObjectiveOwner) (OwnerReference, error) {
		switch owner.Type {
		case runtime.OwnerTypeAgent:
			if key := agentKeys[owner.ID]; key != "" {
				return OwnerReference{Kind: OwnerAgent, Key: key}, nil
			}
		case runtime.OwnerTypeTeam:
			if key := teamKeys[owner.ID]; key != "" {
				return OwnerReference{Kind: OwnerTeam, Key: key}, nil
			}
		}
		return OwnerReference{}, fmt.Errorf("owner %s:%s is outside the workforce export", owner.Type, owner.ID)
	}
	for _, selected := range request.Objectives {
		key := strings.TrimSpace(selected.Key)
		if key == "" || selected.Objective == nil {
			return nil, errors.New("workforce export Objective requires a portable key and source Objective")
		}
		if _, duplicate := objectiveKeys[selected.Objective.ID]; duplicate {
			return nil, fmt.Errorf("workforce export source Objective %s is selected more than once", selected.Objective.ID)
		}
		owner, err := resolveOwner(selected.Objective.Owner)
		if err != nil {
			return nil, fmt.Errorf("export workforce Objective %s: %w", key, err)
		}
		result.Objectives = append(result.Objectives, Objective{
			Key: key, Owner: owner, Title: selected.Objective.Title, Goal: selected.Objective.Goal, Status: selected.Objective.Status,
			Priority: selected.Objective.Priority, ExecutionPolicy: selected.Objective.ExecutionPolicy, Budget: selected.Objective.Budget,
			BudgetAllocations: selected.Objective.BudgetAllocations, Constraints: selected.Objective.Constraints, SuccessCriteria: selected.Objective.SuccessCriteria,
		})
		objectiveKeys[selected.Objective.ID] = key
	}
	for _, selected := range request.Runbooks {
		key := strings.TrimSpace(selected.Key)
		if key == "" || selected.Activation == nil {
			return nil, errors.New("workforce export Runbook requires a portable key and source activation")
		}
		owner, err := resolveOwner(selected.Activation.Owner)
		if err != nil {
			return nil, fmt.Errorf("export workforce Runbook %s: %w", key, err)
		}
		objectiveKey, agentKey := objectiveKeys[selected.Activation.ObjectiveID], agentKeys[selected.Activation.AssignedAgentID]
		if objectiveKey == "" || agentKey == "" {
			return nil, fmt.Errorf("workforce export Runbook %s references an Objective or Agent outside the export", key)
		}
		result.Runbooks = append(result.Runbooks, RunbookActivation{
			Key: key, Owner: owner, ObjectiveKey: objectiveKey, AssignedAgentKey: agentKey,
			DefinitionID: selected.Activation.DefinitionID, DefinitionVersion: selected.Activation.DefinitionVersion,
			TriggerID: selected.Activation.TriggerID, Trigger: selected.Activation.Trigger, Input: selected.Activation.Input,
			Policy: selected.Activation.Policy, Budget: selected.Activation.Budget, MaximumConcurrent: selected.Activation.MaximumConcurrent, Status: selected.Activation.Status,
		})
	}
	if err := result.Seal(); err != nil {
		return nil, err
	}
	return result, nil
}
