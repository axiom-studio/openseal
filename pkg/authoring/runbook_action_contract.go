package authoring

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/axiom-studio/openseal/pkg/runbook"
)

// validateRunbookActionContracts checks prompt-authored deterministic action
// steps against the same exact, host-owned Skill contracts used for direct
// Objective actions. References remain credential-free expressions, but every
// required argument must be wired and every literal must satisfy its schema.
func validateRunbookActionContracts(candidate *WorkforceCandidate, catalog CapabilityCatalog) []ValidationIssue {
	if candidate == nil {
		return nil
	}
	issues := make([]ValidationIssue, 0)
	for agentIndex, definition := range candidate.Agents {
		if definition == nil || definition.Runbook == nil {
			continue
		}
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
			path := fmt.Sprintf("agents[%d].runbook.steps.%s.action.arguments", agentIndex, stepID)
			action := step.Action
			skill, exists := catalog.Skills[strings.TrimSpace(action.SkillID)]
			if !exists || strings.TrimSpace(skill.Version) != strings.TrimSpace(action.SkillVersion) {
				continue // Existing Skill identity validation owns this diagnostic.
			}
			contract, exists := skill.ActionContracts[strings.TrimSpace(action.Action)]
			if !exists || len(contract.InputSchema) == 0 {
				continue
			}
			issues = append(issues, validateRunbookActionArguments(path, action.Arguments, contract.InputSchema)...)
		}
	}
	return issues
}

func validateRunbookActionArguments(path string, arguments map[string]runbook.Value, schema map[string]interface{}) []ValidationIssue {
	issues := make([]ValidationIssue, 0)
	properties, _ := schema["properties"].(map[string]interface{})
	for _, name := range schemaStringList(schema["required"]) {
		if _, exists := arguments[name]; !exists {
			issues = append(issues, issue(path+"."+name, "runbook_action_argument_required", fmt.Sprintf("Runbook action requires argument %s", name)))
		}
	}
	if alternatives, ok := schema["oneOf"].([]interface{}); ok && len(alternatives) > 0 && !runbookArgumentsMatchRequiredAlternative(arguments, alternatives) {
		issues = append(issues, issue(path, "runbook_action_argument_alternative_required", "Runbook action arguments do not satisfy any required input alternative"))
	}
	if additional, exists := schema["additionalProperties"].(bool); exists && !additional {
		argumentNames := make([]string, 0, len(arguments))
		for name := range arguments {
			argumentNames = append(argumentNames, name)
		}
		sort.Strings(argumentNames)
		for _, name := range argumentNames {
			if _, allowed := properties[name]; !allowed {
				issues = append(issues, issue(path+"."+name, "runbook_action_argument_unknown", fmt.Sprintf("Runbook action contract does not define argument %s", name)))
			}
		}
	}
	argumentNames := make([]string, 0, len(arguments))
	for name := range arguments {
		argumentNames = append(argumentNames, name)
	}
	sort.Strings(argumentNames)
	for _, name := range argumentNames {
		value := arguments[name]
		property, ok := properties[name].(map[string]interface{})
		if !ok || len(value.Literal) == 0 {
			continue
		}
		var literal interface{}
		if err := json.Unmarshal(value.Literal, &literal); err != nil {
			issues = append(issues, issue(path+"."+name, "runbook_action_literal_invalid", "Runbook action literal must be valid JSON"))
			continue
		}
		propertyWrapper := map[string]interface{}{
			"type":                 "object",
			"additionalProperties": false,
			"properties":           map[string]interface{}{"value": property},
			"required":             []string{"value"},
		}
		if err := runbook.ValidateInterfaceInput(propertyWrapper, map[string]interface{}{"value": literal}); err != nil {
			issues = append(issues, issue(path+"."+name, "runbook_action_literal_invalid", fmt.Sprintf("Runbook action literal does not match the authorized contract: %v", err)))
		}
	}
	return issues
}

func schemaStringList(value interface{}) []string {
	switch typed := value.(type) {
	case []string:
		return typed
	case []interface{}:
		result := make([]string, 0, len(typed))
		for _, item := range typed {
			if text, ok := item.(string); ok {
				result = append(result, text)
			}
		}
		return result
	default:
		return nil
	}
}

func runbookArgumentsMatchRequiredAlternative(arguments map[string]runbook.Value, alternatives []interface{}) bool {
	for _, raw := range alternatives {
		alternative, _ := raw.(map[string]interface{})
		required := schemaStringList(alternative["required"])
		if len(required) == 0 {
			continue
		}
		matched := true
		for _, name := range required {
			if _, exists := arguments[name]; !exists {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}
