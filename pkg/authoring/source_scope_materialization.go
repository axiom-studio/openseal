package authoring

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/axiom-studio/openseal/pkg/workforce"
)

// materializeAnsweredCapabilitySourceScopes is the deterministic boundary
// between a guided source-scope answer and executable durable work. The model
// may propose the surrounding Objective, but it is never trusted to reproduce
// an audited answer in capability inputs.
func materializeAnsweredCapabilitySourceScopes(candidate *WorkforceCandidate, request GenerateRequest) []ValidationIssue {
	if candidate == nil || request.Refinement == nil {
		return nil
	}
	if request.Mode == ModeAmend && request.Existing != nil {
		preserveRefinementObjectives(candidate, request.Existing)
	}
	answered := make(map[string]RefinementProviderAnswerValue, len(request.Refinement.Answers))
	for _, answer := range request.Refinement.Answers {
		answered[strings.TrimSpace(answer.QuestionID)] = answer.Value
	}
	issues := make([]ValidationIssue, 0)
	for _, need := range request.Catalog.CapabilityNeeds {
		requirement := need.SourceScope
		answer, exists := answered[CapabilitySourceScopeQuestionID(need.ID)]
		if requirement == nil || len(requirement.MaterializationInputKeys) == 0 || !exists {
			continue
		}
		need = selectedCapabilityNeed(need, answered)
		targets := nonEmptyUnique(answer.Items)
		if len(targets) == 0 || capabilitySourceScopeMaterialized(candidate, need, targets, requirement.MaterializationInputKeys) {
			continue
		}
		matches := matchingSourceScopeInvocations(candidate, need, request.Catalog)
		if len(matches) == 0 {
			issues = append(issues, issue(
				"objectives.cadence.runTemplate.capability",
				"source_scope_action_not_found",
				fmt.Sprintf("Source scope %s cannot be applied: add one Objective cadence or event rule that invokes an authorized action for capability need %s", strings.Join(targets, ", "), need.ID),
			))
			continue
		}
		matches = preferInvocationWithMaterializationInput(matches, requirement.MaterializationInputKeys)
		if len(matches) != 1 {
			paths := make([]string, 0, len(matches))
			for _, match := range matches {
				paths = append(paths, match.path)
			}
			issues = append(issues, issue(
				"objectives.cadence.runTemplate.capability",
				"source_scope_action_ambiguous",
				fmt.Sprintf("Source scope %s matches multiple Objective actions (%s); keep exactly one action or preselect one with a %s input", strings.Join(targets, ", "), strings.Join(paths, ", "), strings.Join(requirement.MaterializationInputKeys, "/")),
			))
			continue
		}
		materializeSourceTargets(matches[0].invocation, targets, requirement.MaterializationInputKeys)
	}
	return issues
}

func selectedCapabilityNeed(need CapabilityNeed, answered map[string]RefinementProviderAnswerValue) CapabilityNeed {
	if answer, exists := answered[CapabilityNeedQuestionID(need.ID)]; exists && len(answer.SkillIDs) > 0 {
		need.SkillIDs = append([]string(nil), answer.SkillIDs...)
	}
	return need
}

type sourceScopeInvocation struct {
	path       string
	invocation map[string]interface{}
}

func matchingSourceScopeInvocations(candidate *WorkforceCandidate, need CapabilityNeed, catalog CapabilityCatalog) []sourceScopeInvocation {
	allowed := stringSet(need.SkillIDs)
	result := make([]sourceScopeInvocation, 0)
	for _, candidate := range candidateObjectiveCapabilityInvocations(candidate) {
		if sourceScopeInvocationMatches(candidate.invocation, allowed, catalog) {
			result = append(result, candidate)
		}
	}
	return result
}

func candidateObjectiveCapabilityInvocations(candidate *WorkforceCandidate) []sourceScopeInvocation {
	if candidate == nil {
		return nil
	}
	result := make([]sourceScopeInvocation, 0)
	objectiveKeys := make([]string, 0)
	objectives := candidateObjectiveTemplates(candidate)
	for key := range objectives {
		objectiveKeys = append(objectiveKeys, key)
	}
	sort.Strings(objectiveKeys)
	for _, objectiveKey := range objectiveKeys {
		objective := objectives[objectiveKey]
		if invocation := runTemplateCapability(objective.Cadence["runTemplate"]); invocation != nil {
			result = append(result, sourceScopeInvocation{path: objectiveKey + ".cadence", invocation: invocation})
		}
		rules, _ := objective.EventRules["rules"].([]interface{})
		for index, raw := range rules {
			rule, _ := raw.(map[string]interface{})
			if invocation := runTemplateCapability(rule["runTemplate"]); invocation != nil {
				result = append(result, sourceScopeInvocation{path: fmt.Sprintf("%s.eventRules[%d]", objectiveKey, index), invocation: invocation})
			}
		}
	}
	return result
}

func runTemplateCapability(raw interface{}) map[string]interface{} {
	template, _ := raw.(map[string]interface{})
	invocation, _ := template["capability"].(map[string]interface{})
	return invocation
}

func sourceScopeInvocationMatches(invocation map[string]interface{}, allowed map[string]bool, catalog CapabilityCatalog) bool {
	if invocation == nil {
		return false
	}
	skillID, _ := invocation["skillId"].(string)
	version, _ := invocation["skillVersion"].(string)
	action, _ := invocation["action"].(string)
	skillID, version, action = strings.TrimSpace(skillID), strings.TrimSpace(version), strings.TrimSpace(action)
	skill, exists := catalog.Skills[skillID]
	return allowed[skillID] && exists && version == strings.TrimSpace(skill.Version) && stringSet(skill.Actions)[action]
}

func preferInvocationWithMaterializationInput(matches []sourceScopeInvocation, inputKeys []string) []sourceScopeInvocation {
	preferred := make([]sourceScopeInvocation, 0, len(matches))
	allowed := stringSet(inputKeys)
	for _, match := range matches {
		inputs, _ := match.invocation["inputs"].(map[string]interface{})
		for key := range inputs {
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

func materializeSourceTargets(invocation map[string]interface{}, targets, inputKeys []string) {
	inputs, _ := invocation["inputs"].(map[string]interface{})
	if inputs == nil {
		inputs = map[string]interface{}{}
		invocation["inputs"] = inputs
	}
	key := ""
	for _, candidate := range inputKeys {
		if _, exists := inputs[candidate]; exists {
			key = candidate
			break
		}
	}
	if key == "" {
		key = inputKeys[0]
	}
	if len(targets) == 1 {
		inputs[key] = targets[0]
		return
	}
	inputs[key] = append([]string(nil), targets...)
}

// A refinement answer is an amendment of the reviewed candidate, not a fresh
// authoring request. Preserve prior Objective data that the probabilistic
// generator omitted while allowing fields it explicitly returned to advance.
func preserveRefinementObjectives(candidate, existing *WorkforceCandidate) {
	existingAgents := make(map[string][]workforce.ObjectiveTemplate, len(existing.Agents))
	for _, definition := range existing.Agents {
		if definition != nil {
			existingAgents[definition.ID] = definition.ObjectiveTemplates
		}
	}
	for _, definition := range candidate.Agents {
		if definition != nil {
			definition.ObjectiveTemplates = mergeObjectiveTemplates(definition.ObjectiveTemplates, existingAgents[definition.ID])
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
			byID[previous.ID] = len(current) - 1
			continue
		}
		if len(current[index].Cadence) == 0 {
			current[index].Cadence = cloneObjectiveTemplate(previous).Cadence
		}
		if len(current[index].EventRules) == 0 {
			current[index].EventRules = cloneObjectiveTemplate(previous).EventRules
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
