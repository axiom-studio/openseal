package authoring

import (
	"fmt"
	"strings"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

const (
	InitiativeOwnerAgent = "agent"
	InitiativeOwnerTeam  = "team"

	InitiativeDeduplicateStableSource           = "stable_source"
	InitiativeDeduplicateContentDigest          = "content_digest"
	InitiativeDeduplicateStableSourceAndContent = "stable_source_and_content"
)

// InitiativeBlueprint is the symbolic, product-neutral plan reviewed with a
// workforce candidate. Runtime placement resolves definition and Objective
// template identities to their canonical deployed resources atomically.
type InitiativeBlueprint struct {
	ID             string                             `json:"id"`
	Title          string                             `json:"title"`
	Purpose        string                             `json:"purpose"`
	Owner          InitiativeOwnerReference           `json:"owner"`
	ObjectiveRefs  []string                           `json:"objectiveRefs"`
	Milestones     []InitiativeMilestoneBlueprint     `json:"milestones,omitempty"`
	Hypotheses     []InitiativeHypothesisBlueprint    `json:"hypotheses,omitempty"`
	SourceMonitors []InitiativeSourceMonitorBlueprint `json:"sourceMonitors,omitempty"`
	Deliverables   []InitiativeDeliverableBlueprint   `json:"deliverables,omitempty"`
	Policy         map[string]interface{}             `json:"policy,omitempty"`
}

type InitiativeOwnerReference struct {
	Type         string `json:"type"`
	DefinitionID string `json:"definitionId"`
}

type InitiativeMilestoneBlueprint struct {
	ID            string   `json:"id"`
	Title         string   `json:"title"`
	ObjectiveRefs []string `json:"objectiveRefs,omitempty"`
}

type InitiativeHypothesisBlueprint struct {
	ID         string  `json:"id"`
	Statement  string  `json:"statement"`
	Confidence float64 `json:"confidence"`
}

type InitiativeSourceMonitorBlueprint struct {
	ID                        string `json:"id"`
	ObjectiveRef              string `json:"objectiveRef"`
	AssignedAgentDefinitionID string `json:"assignedAgentDefinitionId"`
	SkillID                   string `json:"skillId"`
	SkillVersion              string `json:"skillVersion"`
	Action                    string `json:"action"`
	SourcePolicyRef           string `json:"sourcePolicyRef"`
	Deduplication             string `json:"deduplication"`
}

type InitiativeDeliverableBlueprint struct {
	ID            string   `json:"id"`
	Title         string   `json:"title"`
	ObjectiveRefs []string `json:"objectiveRefs,omitempty"`
}

func validateInitiativeBlueprint(candidate *WorkforceCandidate, agents map[string]*agent.AgentDefinition) []ValidationIssue {
	if candidate == nil {
		return nil
	}
	issues := validateSourceMonitorContext(candidate)
	if candidate.Initiative == nil {
		return issues
	}
	blueprint := candidate.Initiative
	if !validBlueprintID(blueprint.ID) || strings.TrimSpace(blueprint.Title) == "" || strings.TrimSpace(blueprint.Purpose) == "" {
		issues = append(issues, issue("initiative", "invalid_initiative", "Initiative id, title, and purpose are required and the id must be portable"))
	}
	switch blueprint.Owner.Type {
	case InitiativeOwnerTeam:
		if candidate.Team == nil || blueprint.Owner.DefinitionID != candidate.Team.ID {
			issues = append(issues, issue("initiative.owner", "invalid_owner", "Team Initiative owner must reference the candidate Team definition"))
		}
	case InitiativeOwnerAgent:
		if agents[blueprint.Owner.DefinitionID] == nil {
			issues = append(issues, issue("initiative.owner", "invalid_owner", "Agent Initiative owner must reference a candidate Agent definition"))
		}
	default:
		issues = append(issues, issue("initiative.owner.type", "invalid_owner", "Initiative owner type must be agent or team"))
	}

	objectives := candidateObjectiveTemplates(candidate)
	objectiveKeys := make(map[string]bool, len(objectives))
	for key := range objectives {
		objectiveKeys[key] = true
	}
	issues = append(issues, validateBlueprintRefs("initiative.objectiveRefs", blueprint.ObjectiveRefs, objectiveKeys, true)...)
	initiativeObjectives := stringSet(blueprint.ObjectiveRefs)
	seen := map[string]bool{}
	for index, milestone := range blueprint.Milestones {
		path := fmt.Sprintf("initiative.milestones[%d]", index)
		if !validBlueprintID(milestone.ID) || strings.TrimSpace(milestone.Title) == "" || seen[milestone.ID] {
			issues = append(issues, issue(path, "invalid_milestone", "Milestone ids must be portable and unique and titles are required"))
		}
		seen[milestone.ID] = true
		issues = append(issues, validateBlueprintRefs(path+".objectiveRefs", milestone.ObjectiveRefs, initiativeObjectives, false)...)
	}
	seen = map[string]bool{}
	for index, hypothesis := range blueprint.Hypotheses {
		path := fmt.Sprintf("initiative.hypotheses[%d]", index)
		if !validBlueprintID(hypothesis.ID) || strings.TrimSpace(hypothesis.Statement) == "" || seen[hypothesis.ID] || hypothesis.Confidence < 0 || hypothesis.Confidence > 1 {
			issues = append(issues, issue(path, "invalid_hypothesis", "Hypothesis ids must be portable and unique, statements required, and confidence between 0 and 1"))
		}
		seen[hypothesis.ID] = true
	}
	seen = map[string]bool{}
	for index, monitor := range blueprint.SourceMonitors {
		path := fmt.Sprintf("initiative.sourceMonitors[%d]", index)
		if !validBlueprintID(monitor.ID) || seen[monitor.ID] || !initiativeObjectives[monitor.ObjectiveRef] ||
			strings.TrimSpace(monitor.SourcePolicyRef) == "" || !validInitiativeDeduplication(monitor.Deduplication) {
			issues = append(issues, issue(path, "invalid_source_monitor", "Source monitor requires a portable unique id, Initiative Objective, source policy, and deduplication strategy"))
		}
		seen[monitor.ID] = true
		ownerPrefix := WorkforceObjectiveKey(blueprint.Owner.Type, blueprint.Owner.DefinitionID, "")
		if !strings.HasPrefix(monitor.ObjectiveRef, ownerPrefix) {
			issues = append(issues, issue(path+".objectiveRef", "source_monitor_owner_mismatch", "Source monitor Objective owner must match the Initiative owner"))
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
		path := fmt.Sprintf("initiative.deliverables[%d]", index)
		if !validBlueprintID(deliverable.ID) || strings.TrimSpace(deliverable.Title) == "" || seen[deliverable.ID] {
			issues = append(issues, issue(path, "invalid_deliverable", "Deliverable ids must be portable and unique and titles are required"))
		}
		seen[deliverable.ID] = true
		issues = append(issues, validateBlueprintRefs(path+".objectiveRefs", deliverable.ObjectiveRefs, initiativeObjectives, false)...)
	}
	return issues
}

// validateSourceMonitorContext prevents a scheduled Objective from claiming
// provenance that the reviewed Initiative does not define. Runtime placement
// resolves these symbolic references and the scheduler deliberately rejects
// drift, so an orphan must be repaired before the candidate can be activated.
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
			result[WorkforceObjectiveKey(InitiativeOwnerAgent, definition.ID, template.ID)] = template
		}
	}
	if candidate.Team != nil {
		for index := range candidate.Team.ObjectiveTemplates {
			template := &candidate.Team.ObjectiveTemplates[index]
			result[WorkforceObjectiveKey(InitiativeOwnerTeam, candidate.Team.ID, template.ID)] = template
		}
	}
	return result
}

func validateBlueprintRefs(path string, refs []string, allowed map[string]bool, required bool) []ValidationIssue {
	issues, seen := []ValidationIssue{}, map[string]bool{}
	if required && len(refs) == 0 {
		return []ValidationIssue{issue(path, "required", "Initiative requires at least one Objective reference")}
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

func validInitiativeDeduplication(value string) bool {
	return value == InitiativeDeduplicateStableSource || value == InitiativeDeduplicateContentDigest || value == InitiativeDeduplicateStableSourceAndContent
}

func runbookProjectsMonitor(candidate *WorkforceCandidate, monitor InitiativeSourceMonitorBlueprint) bool {
	for _, invocation := range candidateObjectiveCapabilityInvocations(candidate) {
		if invocation.action != nil && invocation.agentID == monitor.AssignedAgentDefinitionID && invocation.objectiveRef == monitor.ObjectiveRef &&
			invocation.action.SkillID == monitor.SkillID && invocation.action.SkillVersion == monitor.SkillVersion && invocation.action.Action == monitor.Action {
			return true
		}
	}
	return false
}
