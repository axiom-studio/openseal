package authoring

import (
	"fmt"
	"strings"

	"github.com/axiom-studio/openseal/pkg/agent"
)

// validateObjectiveRunbookEntrypoints enforces the canonical work hierarchy at
// authoring time: execution enters through an Agent Runbook trigger and every
// trigger belongs to one candidate Objective. Runbook validation itself owns
// entrypoint and deterministic graph integrity.
func validateObjectiveRunbookEntrypoints(candidate *WorkforceCandidate, _ map[string]*agent.AgentDefinition) []ValidationIssue {
	if candidate == nil {
		return nil
	}
	objectives := make(map[string]bool)
	for _, definition := range candidate.Agents {
		if definition == nil {
			continue
		}
		for _, template := range definition.ObjectiveTemplates {
			objectives[WorkforceObjectiveKey("agent", definition.ID, template.ID)] = true
		}
	}
	if candidate.Team != nil {
		for _, template := range candidate.Team.ObjectiveTemplates {
			objectives[WorkforceObjectiveKey("team", candidate.Team.ID, template.ID)] = true
		}
	}

	issues := make([]ValidationIssue, 0)
	for agentIndex, definition := range candidate.Agents {
		if definition == nil || definition.Runbook == nil {
			continue
		}
		for triggerID, trigger := range definition.Runbook.Triggers {
			path := fmt.Sprintf("agents[%d].runbook.triggers.%s.objectiveId", agentIndex, triggerID)
			objectiveID := strings.TrimSpace(trigger.ObjectiveID)
			if objectiveID == "" {
				issues = append(issues, issue(path, "runbook_trigger_objective_required", "Every Runbook trigger must belong to one Objective"))
				continue
			}
			if !objectives[objectiveID] {
				issues = append(issues, issue(path, "runbook_trigger_objective_unknown", fmt.Sprintf("Runbook trigger references unknown candidate Objective %s", objectiveID)))
			}
		}
	}
	return issues
}
