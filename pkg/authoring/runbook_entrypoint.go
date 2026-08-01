package authoring

import (
	"fmt"
	"sort"
	"strings"

	"github.com/axiom-studio/openseal/pkg/agent"
)

// canonicalizeGeneratedRunbookObjectiveReferences accepts the provider's
// unambiguous local Objective-template id and expands it into the canonical
// owner-qualified key. This is a lossless compiler projection: ambiguous
// aliases remain untouched and flow into bounded repair.
func canonicalizeGeneratedRunbookObjectiveReferences(candidate *WorkforceCandidate) {
	if candidate == nil {
		return
	}
	objectivesByLocalID := make(map[string][]string)
	for _, definition := range candidate.Agents {
		if definition == nil {
			continue
		}
		for _, template := range definition.ObjectiveTemplates {
			localID := strings.TrimSpace(template.ID)
			if localID != "" {
				objectivesByLocalID[localID] = append(objectivesByLocalID[localID], WorkforceObjectiveKey("agent", definition.ID, localID))
			}
		}
	}
	if candidate.Team != nil {
		for _, template := range candidate.Team.ObjectiveTemplates {
			localID := strings.TrimSpace(template.ID)
			if localID != "" {
				objectivesByLocalID[localID] = append(objectivesByLocalID[localID], WorkforceObjectiveKey("team", candidate.Team.ID, localID))
			}
		}
	}
	for _, definition := range candidate.Agents {
		if definition == nil || definition.Runbook == nil {
			continue
		}
		for triggerID, trigger := range definition.Runbook.Triggers {
			matches := objectivesByLocalID[strings.TrimSpace(trigger.ObjectiveID)]
			if len(matches) == 1 {
				trigger.ObjectiveID = matches[0]
				definition.Runbook.Triggers[triggerID] = trigger
			}
		}
	}
}

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
	allowedObjectives := make([]string, 0, len(objectives))
	for objectiveID := range objectives {
		allowedObjectives = append(allowedObjectives, objectiveID)
	}
	sort.Strings(allowedObjectives)
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
				issues = append(issues, issue(path, "runbook_trigger_objective_unknown", fmt.Sprintf("Runbook trigger references unknown candidate Objective %s; use one exact candidate Objective key: %s", objectiveID, strings.Join(allowedObjectives, ", "))))
			}
		}
	}
	return issues
}
