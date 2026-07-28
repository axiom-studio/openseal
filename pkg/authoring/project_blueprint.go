package authoring

import (
	"fmt"
	"strings"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

const (
	ProjectOwnerAgent = "agent"
	ProjectOwnerTeam  = "team"

	ProjectDeduplicateStableSource           = "stable_source"
	ProjectDeduplicateContentDigest          = "content_digest"
	ProjectDeduplicateStableSourceAndContent = "stable_source_and_content"
)

// ProjectBlueprint is the symbolic, product-neutral plan reviewed with a
// workforce candidate. Runtime placement resolves definition and Objective
// template identities to their canonical deployed resources atomically.
type ProjectBlueprint struct {
	ID             string                          `json:"id"`
	Title          string                          `json:"title"`
	Purpose        string                          `json:"purpose"`
	Owner          ProjectOwnerReference           `json:"owner"`
	ObjectiveRefs  []string                        `json:"objectiveRefs"`
	Milestones     []ProjectMilestoneBlueprint     `json:"milestones,omitempty"`
	Hypotheses     []ProjectHypothesisBlueprint    `json:"hypotheses,omitempty"`
	SourceMonitors []ProjectSourceMonitorBlueprint `json:"sourceMonitors,omitempty"`
	Deliverables   []ProjectDeliverableBlueprint   `json:"deliverables,omitempty"`
	Policy         map[string]interface{}          `json:"policy,omitempty"`
}

type ProjectOwnerReference struct {
	Type         string `json:"type"`
	DefinitionID string `json:"definitionId"`
}

type ProjectMilestoneBlueprint struct {
	ID            string   `json:"id"`
	Title         string   `json:"title"`
	ObjectiveRefs []string `json:"objectiveRefs,omitempty"`
}

type ProjectHypothesisBlueprint struct {
	ID         string  `json:"id"`
	Statement  string  `json:"statement"`
	Confidence float64 `json:"confidence"`
}

type ProjectSourceMonitorBlueprint struct {
	ID                        string `json:"id"`
	ObjectiveRef              string `json:"objectiveRef"`
	AssignedAgentDefinitionID string `json:"assignedAgentDefinitionId"`
	SkillID                   string `json:"skillId"`
	SkillVersion              string `json:"skillVersion"`
	Action                    string `json:"action"`
	SourcePolicyRef           string `json:"sourcePolicyRef"`
	Deduplication             string `json:"deduplication"`
}

type ProjectDeliverableBlueprint struct {
	ID            string   `json:"id"`
	Title         string   `json:"title"`
	ObjectiveRefs []string `json:"objectiveRefs,omitempty"`
}

func validateProjectBlueprint(candidate *WorkforceCandidate, agents map[string]*agent.AgentDefinition) []ValidationIssue {
	if candidate == nil {
		return nil
	}
	issues := validateSourceMonitorContext(candidate)
	if candidate.Project == nil {
		return issues
	}
	blueprint := candidate.Project
	if !validBlueprintID(blueprint.ID) || strings.TrimSpace(blueprint.Title) == "" || strings.TrimSpace(blueprint.Purpose) == "" {
		issues = append(issues, issue("project", "invalid_project", "Project id, title, and purpose are required and the id must be portable"))
	}
	switch blueprint.Owner.Type {
	case ProjectOwnerTeam:
		if candidate.Team == nil || blueprint.Owner.DefinitionID != candidate.Team.ID {
			issues = append(issues, issue("project.owner", "invalid_owner", "Team Project owner must reference the candidate Team definition"))
		}
	case ProjectOwnerAgent:
		if agents[blueprint.Owner.DefinitionID] == nil {
			issues = append(issues, issue("project.owner", "invalid_owner", "Agent Project owner must reference a candidate Agent definition"))
		}
	default:
		issues = append(issues, issue("project.owner.type", "invalid_owner", "Project owner type must be agent or team"))
	}

	objectives := candidateObjectiveTemplates(candidate)
	objectiveKeys := make(map[string]bool, len(objectives))
	for key := range objectives {
		objectiveKeys[key] = true
	}
	issues = append(issues, validateBlueprintRefs("project.objectiveRefs", blueprint.ObjectiveRefs, objectiveKeys, true)...)
	projectObjectives := stringSet(blueprint.ObjectiveRefs)
	seen := map[string]bool{}
	for index, milestone := range blueprint.Milestones {
		path := fmt.Sprintf("project.milestones[%d]", index)
		if !validBlueprintID(milestone.ID) || strings.TrimSpace(milestone.Title) == "" || seen[milestone.ID] {
			issues = append(issues, issue(path, "invalid_milestone", "Milestone ids must be portable and unique and titles are required"))
		}
		seen[milestone.ID] = true
		issues = append(issues, validateBlueprintRefs(path+".objectiveRefs", milestone.ObjectiveRefs, projectObjectives, false)...)
	}
	seen = map[string]bool{}
	for index, hypothesis := range blueprint.Hypotheses {
		path := fmt.Sprintf("project.hypotheses[%d]", index)
		if !validBlueprintID(hypothesis.ID) || strings.TrimSpace(hypothesis.Statement) == "" || seen[hypothesis.ID] || hypothesis.Confidence < 0 || hypothesis.Confidence > 1 {
			issues = append(issues, issue(path, "invalid_hypothesis", "Hypothesis ids must be portable and unique, statements required, and confidence between 0 and 1"))
		}
		seen[hypothesis.ID] = true
	}
	seen = map[string]bool{}
	for index, monitor := range blueprint.SourceMonitors {
		path := fmt.Sprintf("project.sourceMonitors[%d]", index)
		if !validBlueprintID(monitor.ID) || seen[monitor.ID] || !projectObjectives[monitor.ObjectiveRef] ||
			strings.TrimSpace(monitor.SourcePolicyRef) == "" || !validProjectDeduplication(monitor.Deduplication) {
			issues = append(issues, issue(path, "invalid_source_monitor", "Source monitor requires a portable unique id, Project Objective, source policy, and deduplication strategy"))
		}
		seen[monitor.ID] = true
		ownerPrefix := WorkforceObjectiveKey(blueprint.Owner.Type, blueprint.Owner.DefinitionID, "")
		if !strings.HasPrefix(monitor.ObjectiveRef, ownerPrefix) {
			issues = append(issues, issue(path+".objectiveRef", "source_monitor_owner_mismatch", "Source monitor Objective owner must match the Project owner"))
		}
		agent := agents[monitor.AssignedAgentDefinitionID]
		if agent == nil || !agentAuthorizes(agent, monitor.SkillID, monitor.SkillVersion, monitor.Action) {
			issues = append(issues, issue(path+".assignedAgentDefinitionId", "invalid_source_monitor_agent", "Source monitor Agent must exist and require the exact Skill action and version"))
		}
		if objectives[monitor.ObjectiveRef] == nil || !runbookProjectsMonitor(candidate, monitor) {
			issues = append(issues, issue(path+".objectiveRef", "invalid_source_monitor_runbook", "Source monitor must be implemented by an exact Objective-owned Runbook action"))
		}
	}
	seen = map[string]bool{}
	for index, deliverable := range blueprint.Deliverables {
		path := fmt.Sprintf("project.deliverables[%d]", index)
		if !validBlueprintID(deliverable.ID) || strings.TrimSpace(deliverable.Title) == "" || seen[deliverable.ID] {
			issues = append(issues, issue(path, "invalid_deliverable", "Deliverable ids must be portable and unique and titles are required"))
		}
		seen[deliverable.ID] = true
		issues = append(issues, validateBlueprintRefs(path+".objectiveRefs", deliverable.ObjectiveRefs, projectObjectives, false)...)
	}
	return issues
}

// validateSourceMonitorContext prevents an Objective-owned Runbook trigger
// from claiming provenance that the reviewed Project does not define. Runtime
// placement resolves these symbolic references and the scheduler deliberately
// rejects drift, so an orphan must be repaired before activation.
func validateSourceMonitorContext(_ *WorkforceCandidate) []ValidationIssue {
	return nil
}

func agentAuthorizes(definition *agent.AgentDefinition, skillID, version, action string) bool {
	if definition == nil || strings.TrimSpace(skillID) == "" || strings.TrimSpace(version) == "" || strings.TrimSpace(action) == "" {
		return false
	}
	for _, requirement := range definition.SkillRequirements {
		if requirement.SkillID != skillID || requirement.VersionConstraint != "" && requirement.VersionConstraint != version {
			continue
		}
		for _, candidate := range requirement.RequiredActions {
			if candidate == action {
				return true
			}
		}
	}
	return false
}

func candidateObjectiveTemplates(candidate *WorkforceCandidate) map[string]*workforce.ObjectiveTemplate {
	result := map[string]*workforce.ObjectiveTemplate{}
	for _, definition := range candidate.Agents {
		if definition == nil {
			continue
		}
		for index := range definition.ObjectiveTemplates {
			template := &definition.ObjectiveTemplates[index]
			result[WorkforceObjectiveKey(ProjectOwnerAgent, definition.ID, template.ID)] = template
		}
	}
	if candidate.Team != nil {
		for index := range candidate.Team.ObjectiveTemplates {
			template := &candidate.Team.ObjectiveTemplates[index]
			result[WorkforceObjectiveKey(ProjectOwnerTeam, candidate.Team.ID, template.ID)] = template
		}
	}
	return result
}

func validateBlueprintRefs(path string, refs []string, allowed map[string]bool, required bool) []ValidationIssue {
	issues, seen := []ValidationIssue{}, map[string]bool{}
	if required && len(refs) == 0 {
		return []ValidationIssue{issue(path, "required", "Project requires at least one Objective reference")}
	}
	for _, reference := range refs {
		if strings.TrimSpace(reference) == "" || seen[reference] || !allowed[reference] {
			issues = append(issues, issue(path, "invalid_objective_ref", "Objective references must be unique and name candidate Objective templates"))
		}
		seen[reference] = true
	}
	return issues
}

func validBlueprintID(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && len(value) <= 128 && !strings.ContainsAny(value, "\r\n?#@/\\") && !strings.Contains(value, "://")
}

func validProjectDeduplication(value string) bool {
	return value == ProjectDeduplicateStableSource || value == ProjectDeduplicateContentDigest || value == ProjectDeduplicateStableSourceAndContent
}

func runbookProjectsMonitor(candidate *WorkforceCandidate, monitor ProjectSourceMonitorBlueprint) bool {
	for _, invocation := range candidateObjectiveCapabilityInvocations(candidate) {
		if invocation.action != nil && invocation.agentID == monitor.AssignedAgentDefinitionID && invocation.objectiveRef == monitor.ObjectiveRef &&
			invocation.action.SkillID == monitor.SkillID && invocation.action.SkillVersion == monitor.SkillVersion && invocation.action.Action == monitor.Action {
			return true
		}
	}
	return false
}
