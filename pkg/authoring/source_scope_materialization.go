package authoring

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

// materializeAnsweredCapabilitySourceScopes is the deterministic boundary
// between an audited source-scope answer and an Objective-owned Runbook. The
// model chooses the operation shape; OpenSeal writes the exact user answer into
// credential-free Runbook values.
func materializeAnsweredCapabilitySourceScopes(candidate *WorkforceCandidate, request GenerateRequest) []ValidationIssue {
	if candidate == nil {
		return nil
	}
	if request.Mode == ModeAmend && request.Existing != nil {
		preserveRefinementWork(candidate, request.Existing)
	}
	answered := refinementAnswers(request.Refinement)
	issues := make([]ValidationIssue, 0)
	for _, rawNeed := range request.Catalog.CapabilityNeeds {
		need := selectedCapabilityNeed(rawNeed, answered)
		requirement := need.SourceScope
		answer, exists := answered[CapabilitySourceScopeQuestionID(need.ID)]
		if !exists && requirement != nil && len(requirement.Targets) > 0 {
			answer, exists = RefinementProviderAnswerValue{Items: requirement.Targets}, true
		}
		if requirement == nil || len(requirement.MaterializationInputKeys) == 0 || !exists {
			continue
		}
		targets := nonEmptyUnique(answer.Items)
		if len(targets) == 0 {
			continue
		}
		actions := preferInvocationWithMaterializationInput(matchingSourceScopeInvocations(candidate, need, request.Catalog), requirement.MaterializationInputKeys)
		if len(actions) == 1 {
			materializeSourceTargets(actions[0], targets, requirement.MaterializationInputKeys)
			issues = append(issues, materializeCatalogSourceMonitor(candidate, need, actions[0])...)
			continue
		}
		routes := matchingSourceScopeRunbooks(candidate, need, request.Catalog)
		if len(routes) == 1 {
			// A Runbook trigger is the stable boundary when several steps use the
			// same source Skill (for example navigate, snapshot, and click). Bind
			// the audited scope once and let exact step references consume it.
			materializeRunbookSourceTargets(routes[0], targets, requirement.MaterializationInputKeys)
			continue
		}
		if len(actions) > 1 {
			paths := make([]string, 0, len(actions))
			for _, action := range actions {
				paths = append(paths, action.path)
			}
			issues = append(issues, issue("runbooks.steps.action.arguments", "source_scope_action_ambiguous", fmt.Sprintf("Source scope %s matches multiple Runbook actions (%s); keep one exact action or one Objective-owned Runbook trigger", strings.Join(targets, ", "), strings.Join(paths, ", "))))
			continue
		}
		if len(routes) > 1 {
			paths := make([]string, 0, len(routes))
			for _, route := range routes {
				paths = append(paths, route.path)
			}
			issues = append(issues, issue("runbooks.triggers", "source_scope_runbook_ambiguous", fmt.Sprintf("Source scope %s matches multiple Runbook triggers (%s); keep one exact trigger", strings.Join(targets, ", "), strings.Join(paths, ", "))))
			continue
		}
		if sourceCapabilityRequiresDurableAction(request) {
			issues = append(issues, issue("runbooks", "source_scope_action_not_found", fmt.Sprintf("Source scope %s cannot be applied: add one Objective-owned Runbook that uses an authorized action for capability need %s", strings.Join(targets, ", "), need.ID)))
		}
	}
	return issues
}

func refinementAnswers(refinement *RefinementContext) map[string]RefinementProviderAnswerValue {
	result := map[string]RefinementProviderAnswerValue{}
	if refinement == nil {
		return result
	}
	for _, answer := range refinement.Answers {
		result[strings.TrimSpace(answer.QuestionID)] = answer.Value
	}
	return result
}

type sourceScopeInvocation struct {
	path         string
	definition   *runbook.Definition
	stepID       string
	action       *runbook.ActionStep
	agentID      string
	objectiveRef string
}

type sourceScopeRunbook struct {
	path         string
	definition   *runbook.Definition
	triggerID    string
	agentID      string
	objectiveRef string
	entrypoint   string
}

func matchingSourceScopeInvocations(candidate *WorkforceCandidate, need CapabilityNeed, catalog CapabilityCatalog) []sourceScopeInvocation {
	allowed := stringSet(need.SkillIDs)
	result := make([]sourceScopeInvocation, 0)
	for _, invocation := range candidateObjectiveCapabilityInvocations(candidate) {
		if sourceScopeInvocationMatches(invocation, allowed, catalog) {
			result = append(result, invocation)
		}
	}
	return result
}

func candidateObjectiveCapabilityInvocations(candidate *WorkforceCandidate) []sourceScopeInvocation {
	if candidate == nil {
		return nil
	}
	result := make([]sourceScopeInvocation, 0)
	for agentIndex, definition := range candidate.Agents {
		if definition == nil || definition.Runbook == nil {
			continue
		}
		objectiveRef := uniqueRunbookObjective(definition.Runbook)
		stepIDs := make([]string, 0, len(definition.Runbook.Steps))
		for stepID := range definition.Runbook.Steps {
			stepIDs = append(stepIDs, stepID)
		}
		sort.Strings(stepIDs)
		for _, stepID := range stepIDs {
			step := definition.Runbook.Steps[stepID]
			if step.Kind != runbook.StepAction || step.Action == nil {
				continue
			}
			result = append(result, sourceScopeInvocation{
				path: fmt.Sprintf("agents[%d].runbook.steps.%s.action", agentIndex, stepID), definition: definition.Runbook,
				stepID: stepID, action: step.Action, agentID: definition.ID, objectiveRef: objectiveRef,
			})
		}
	}
	return result
}

func uniqueRunbookObjective(definition *runbook.Definition) string {
	result := ""
	for _, trigger := range definition.Triggers {
		objective := strings.TrimSpace(trigger.ObjectiveID)
		if objective == "" {
			continue
		}
		if result != "" && result != objective {
			return ""
		}
		result = objective
	}
	return result
}

func sourceScopeInvocationMatches(invocation sourceScopeInvocation, allowed map[string]bool, catalog CapabilityCatalog) bool {
	if invocation.action == nil {
		return false
	}
	skillID := strings.TrimSpace(invocation.action.SkillID)
	version := strings.TrimSpace(invocation.action.SkillVersion)
	action := strings.TrimSpace(invocation.action.Action)
	skill, exists := catalog.Skills[skillID]
	return allowed[skillID] && exists && version == strings.TrimSpace(skill.Version) && stringSet(skill.Actions)[action]
}

func preferInvocationWithMaterializationInput(matches []sourceScopeInvocation, inputKeys []string) []sourceScopeInvocation {
	preferred := make([]sourceScopeInvocation, 0, len(matches))
	allowed := stringSet(inputKeys)
	for _, match := range matches {
		for key := range match.action.Arguments {
			if allowed[key] {
				preferred = append(preferred, match)
				break
			}
		}
	}
	if len(preferred) == 1 {
		return preferred
	}
	return matches
}

func materializeSourceTargets(invocation sourceScopeInvocation, targets, inputKeys []string) {
	if invocation.action == nil || invocation.definition == nil {
		return
	}
	if invocation.action.Arguments == nil {
		invocation.action.Arguments = map[string]runbook.Value{}
	}
	key := ""
	for _, candidate := range inputKeys {
		if _, exists := invocation.action.Arguments[candidate]; exists {
			key = candidate
			break
		}
	}
	if key == "" {
		key = inputKeys[0]
	}
	var value interface{} = append([]string(nil), targets...)
	if len(targets) == 1 {
		value = targets[0]
	}
	encoded, _ := json.Marshal(value)
	invocation.action.Arguments[key] = runbook.Value{Literal: encoded}
	step := invocation.definition.Steps[invocation.stepID]
	step.Action = invocation.action
	invocation.definition.Steps[invocation.stepID] = step
}

func matchingSourceScopeRunbooks(candidate *WorkforceCandidate, need CapabilityNeed, catalog CapabilityCatalog) []sourceScopeRunbook {
	if candidate == nil {
		return nil
	}
	allowed := stringSet(need.SkillIDs)
	result := make([]sourceScopeRunbook, 0)
	for agentIndex, definition := range candidate.Agents {
		if definition == nil || definition.Runbook == nil || !agentDeclaresSourceSkill(definition, allowed, catalog) {
			continue
		}
		triggerIDs := make([]string, 0, len(definition.Runbook.Triggers))
		for id := range definition.Runbook.Triggers {
			triggerIDs = append(triggerIDs, id)
		}
		sort.Strings(triggerIDs)
		for _, triggerID := range triggerIDs {
			trigger := definition.Runbook.Triggers[triggerID]
			result = append(result, sourceScopeRunbook{
				path: fmt.Sprintf("agents[%d].runbook.triggers.%s", agentIndex, triggerID), definition: definition.Runbook,
				triggerID: triggerID, agentID: definition.ID, objectiveRef: trigger.ObjectiveID, entrypoint: trigger.Entrypoint,
			})
		}
	}
	return result
}

func agentDeclaresSourceSkill(definition *agent.AgentDefinition, allowed map[string]bool, catalog CapabilityCatalog) bool {
	for _, requirement := range definition.SkillRequirements {
		if !allowed[requirement.SkillID] {
			continue
		}
		skill, exists := catalog.Skills[requirement.SkillID]
		if exists && strings.TrimSpace(skill.Version) == strings.TrimSpace(requirement.VersionConstraint) {
			return true
		}
	}
	return false
}

func materializeRunbookSourceTargets(route sourceScopeRunbook, targets, inputKeys []string) {
	key := sourceScopeMaterializationKey(inputKeys, len(targets))
	if key == "" || route.definition == nil {
		return
	}
	trigger := route.definition.Triggers[route.triggerID]
	if trigger.Input == nil {
		trigger.Input = map[string]runbook.Value{}
	}
	encoded, _ := json.Marshal(append([]string(nil), targets...))
	trigger.Input[key] = runbook.Value{Literal: encoded}
	route.definition.Triggers[route.triggerID] = trigger

	if route.definition.Interfaces == nil {
		route.definition.Interfaces = map[string]runbook.Interface{}
	}
	contract := route.definition.Interfaces[route.entrypoint]
	if contract.InputSchema == nil {
		contract.InputSchema = map[string]interface{}{"type": "object", "additionalProperties": false}
	}
	properties, _ := contract.InputSchema["properties"].(map[string]interface{})
	if properties == nil {
		properties = map[string]interface{}{}
		contract.InputSchema["properties"] = properties
	}
	properties[key] = map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}, "minItems": 1}
	required := schemaStringList(contract.InputSchema["required"])
	if !stringSet(required)[key] {
		required = append(required, key)
		sort.Strings(required)
		contract.InputSchema["required"] = required
	}
	route.definition.Interfaces[route.entrypoint] = contract
	for stepID, step := range route.definition.Steps {
		if step.Kind == runbook.StepDelegate && step.Delegate != nil {
			if step.Delegate.Context == nil {
				step.Delegate.Context = map[string]runbook.Value{}
			}
			step.Delegate.Context[key] = runbook.Value{Ref: "/input/" + key}
			route.definition.Steps[stepID] = step
		}
	}
}

func sourceScopeMaterializationKey(inputKeys []string, targetCount int) string {
	if targetCount > 1 {
		for _, key := range inputKeys {
			if key == "subreddits" || key == "communities" {
				return key
			}
		}
	}
	if len(inputKeys) > 0 {
		return inputKeys[0]
	}
	return ""
}

func capabilitySourceScopeMaterialized(candidate *WorkforceCandidate, need CapabilityNeed, targets, inputKeys []string, catalogs ...CapabilityCatalog) bool {
	targets = nonEmptyUnique(targets)
	if candidate == nil || len(targets) == 0 {
		return false
	}
	allowedSkills, allowedKeys := stringSet(need.SkillIDs), stringSet(inputKeys)
	found := make(map[string]bool, len(targets))
	observe := func(key string, raw []byte) {
		if !allowedKeys[key] {
			return
		}
		materialized := strings.ToLower(string(raw))
		for _, target := range targets {
			if strings.Contains(materialized, strings.ToLower(target)) {
				found[target] = true
			}
		}
	}
	for _, invocation := range candidateObjectiveCapabilityInvocations(candidate) {
		if invocation.action == nil || !allowedSkills[strings.TrimSpace(invocation.action.SkillID)] {
			continue
		}
		for key, value := range invocation.action.Arguments {
			observe(key, value.Literal)
		}
	}
	if len(catalogs) > 0 {
		for _, route := range matchingSourceScopeRunbooks(candidate, need, catalogs[0]) {
			trigger := route.definition.Triggers[route.triggerID]
			for key, value := range trigger.Input {
				observe(key, value.Literal)
			}
		}
	}
	return len(found) == len(targets)
}

func materializeCatalogSourceMonitor(candidate *WorkforceCandidate, need CapabilityNeed, match sourceScopeInvocation) []ValidationIssue {
	if candidate == nil || candidate.Project == nil || need.SourcePolicyProposal == nil || match.action == nil || match.objectiveRef == "" {
		return nil
	}
	if !stringSet(candidate.Project.ObjectiveRefs)[match.objectiveRef] || !stringSet(need.SourcePolicyProposal.SkillIDs)[match.action.SkillID] {
		return nil
	}
	reference := strings.TrimSpace(need.SourcePolicyProposal.Policy.ID) + "@" + strings.TrimSpace(need.SourcePolicyProposal.Policy.Version)
	monitorID := catalogSourceMonitorID(need.ID)
	for _, existing := range candidate.Project.SourceMonitors {
		if existing.ID == monitorID || existing.ObjectiveRef == match.objectiveRef {
			return nil
		}
	}
	candidate.Project.SourceMonitors = append(candidate.Project.SourceMonitors, ProjectSourceMonitorBlueprint{
		ID: monitorID, ObjectiveRef: match.objectiveRef, AssignedAgentDefinitionID: match.agentID,
		SkillID: match.action.SkillID, SkillVersion: match.action.SkillVersion, Action: match.action.Action,
		SourcePolicyRef: reference, Deduplication: ProjectDeduplicateStableSourceAndContent,
	})
	return nil
}

func catalogSourceMonitorID(needID string) string {
	candidate := "source-" + strings.TrimSpace(needID)
	if validBlueprintID(candidate) {
		return candidate
	}
	digest := sha256.Sum256([]byte(strings.TrimSpace(needID)))
	return fmt.Sprintf("source-%x", digest[:8])
}

func selectedCapabilityNeed(need CapabilityNeed, answered map[string]RefinementProviderAnswerValue) CapabilityNeed {
	if answer, exists := answered[CapabilityNeedQuestionID(need.ID)]; exists && len(answer.SkillIDs) > 0 {
		need.SkillIDs = append([]string(nil), answer.SkillIDs...)
	}
	return need
}

// A refinement answer amends reviewed work. Preserve outcome metadata and an
// already reviewed Runbook when the probabilistic regeneration omits them.
func preserveRefinementWork(candidate, existing *WorkforceCandidate) {
	existingAgents := make(map[string]*agent.AgentDefinition, len(existing.Agents))
	for _, definition := range existing.Agents {
		if definition != nil {
			existingAgents[definition.ID] = definition
		}
	}
	for _, definition := range candidate.Agents {
		if definition == nil {
			continue
		}
		previous := existingAgents[definition.ID]
		if previous == nil {
			continue
		}
		definition.ObjectiveTemplates = mergeObjectiveTemplates(definition.ObjectiveTemplates, previous.ObjectiveTemplates)
		if definition.Runbook == nil && previous.Runbook != nil {
			definition.Runbook = cloneRunbook(previous.Runbook)
		}
	}
	if candidate.Team != nil && existing.Team != nil && candidate.Team.ID == existing.Team.ID {
		candidate.Team.ObjectiveTemplates = mergeObjectiveTemplates(candidate.Team.ObjectiveTemplates, existing.Team.ObjectiveTemplates)
	}
}

func mergeObjectiveTemplates(current, existing []workforce.ObjectiveTemplate) []workforce.ObjectiveTemplate {
	byID := make(map[string]int, len(current))
	for index := range current {
		byID[current[index].ID] = index
	}
	for _, previous := range existing {
		index, found := byID[previous.ID]
		if !found {
			current = append(current, cloneObjectiveTemplate(previous))
			continue
		}
		if len(current[index].SuccessCriteria) == 0 {
			current[index].SuccessCriteria = cloneObjectiveTemplate(previous).SuccessCriteria
		}
		if len(current[index].Constraints) == 0 {
			current[index].Constraints = cloneObjectiveTemplate(previous).Constraints
		}
	}
	return current
}

func cloneObjectiveTemplate(value workforce.ObjectiveTemplate) workforce.ObjectiveTemplate {
	payload, _ := json.Marshal(value)
	var clone workforce.ObjectiveTemplate
	_ = json.Unmarshal(payload, &clone)
	return clone
}

func cloneRunbook(value *runbook.Definition) *runbook.Definition {
	payload, _ := json.Marshal(value)
	var clone runbook.Definition
	_ = json.Unmarshal(payload, &clone)
	return &clone
}
