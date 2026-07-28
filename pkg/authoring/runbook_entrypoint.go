package authoring

import (
	"fmt"
	"strings"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

// validateObjectiveRunbookEntrypoints keeps scheduled and event-driven work
// honest at review time. A direct Skill capability is already a complete,
// deterministic operation and therefore has no Runbook entrypoint. A named
// entrypoint is callable only when the assigned candidate Agent embeds that
// exact immutable Runbook operation.
func validateObjectiveRunbookEntrypoints(candidate *WorkforceCandidate, agents map[string]*agent.AgentDefinition) []ValidationIssue {
	if candidate == nil {
		return nil
	}
	issues := make([]ValidationIssue, 0)
	for index, definition := range candidate.Agents {
		if definition == nil {
			continue
		}
		issues = append(issues, validateObjectiveTemplateRunbookEntrypoints(
			fmt.Sprintf("agents[%d].objectiveTemplates", index), definition.ObjectiveTemplates, definition.ID, agents,
		)...)
	}
	if candidate.Team != nil {
		issues = append(issues, validateObjectiveTemplateRunbookEntrypoints(
			"team.objectiveTemplates", candidate.Team.ObjectiveTemplates, "", agents,
		)...)
	}
	return issues
}

func validateObjectiveTemplateRunbookEntrypoints(path string, templates []workforce.ObjectiveTemplate, defaultAgentID string, agents map[string]*agent.AgentDefinition) []ValidationIssue {
	issues := make([]ValidationIssue, 0)
	for templateIndex, template := range templates {
		cadencePath := fmt.Sprintf("%s[%d].cadence", path, templateIndex)
		assignedAgentID, _ := template.Cadence["assignedAgentId"].(string)
		if strings.TrimSpace(assignedAgentID) == "" {
			assignedAgentID = defaultAgentID
		}
		issues = append(issues, validateObjectiveRunTemplateEntrypoint(
			cadencePath+".runTemplate", template.Cadence["runTemplate"], assignedAgentID, agents,
		)...)

		rules, _ := template.EventRules["rules"].([]interface{})
		for ruleIndex, raw := range rules {
			rule, _ := raw.(map[string]interface{})
			ruleAgentID, _ := rule["assignedAgentId"].(string)
			if strings.TrimSpace(ruleAgentID) == "" {
				ruleAgentID = defaultAgentID
			}
			issues = append(issues, validateObjectiveRunTemplateEntrypoint(
				fmt.Sprintf("%s[%d].eventRules.rules[%d].runTemplate", path, templateIndex, ruleIndex),
				rule["runTemplate"], ruleAgentID, agents,
			)...)
		}
	}
	return issues
}

func validateObjectiveRunTemplateEntrypoint(path string, raw interface{}, assignedAgentID string, agents map[string]*agent.AgentDefinition) []ValidationIssue {
	template, _ := raw.(map[string]interface{})
	entrypoint, _ := template["entrypoint"].(string)
	entrypoint = strings.TrimSpace(entrypoint)
	if entrypoint == "" {
		return nil
	}
	if capability, present := template["capability"]; present && capability != nil {
		return []ValidationIssue{issue(
			path+".entrypoint", "runbook_entrypoint_conflicts_with_capability",
			"A direct Skill capability must omit entrypoint; remove capability only when this schedule should invoke a named Agent Runbook operation",
		)}
	}
	assignedAgentID = strings.TrimSpace(assignedAgentID)
	if assignedAgentID == "" {
		return []ValidationIssue{issue(
			path+".entrypoint", "runbook_assigned_agent_required",
			fmt.Sprintf("Runbook entrypoint %s requires an assigned candidate Agent", entrypoint),
		)}
	}
	definition := agents[assignedAgentID]
	if definition == nil || definition.Runbook == nil {
		return []ValidationIssue{issue(
			path+".entrypoint", "runbook_entrypoint_unavailable",
			fmt.Sprintf("Runbook entrypoint %s requires assigned Agent %s to embed that operation in agents[].runbook.entrypoints", entrypoint, assignedAgentID),
		)}
	}
	if _, exists := definition.Runbook.Entrypoints[entrypoint]; !exists {
		return []ValidationIssue{issue(
			path+".entrypoint", "runbook_entrypoint_unavailable",
			fmt.Sprintf("Runbook entrypoint %s is not defined by assigned Agent %s", entrypoint, assignedAgentID),
		)}
	}
	return nil
}
