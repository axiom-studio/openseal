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
		outputs := runbookActionOutputs(definition.Runbook, catalog)
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
			issues = append(issues, validateRunbookActionReferences(path, stepID, action.Arguments, contract.InputSchema, definition.Runbook, outputs)...)
		}
	}
	return issues
}

type runbookActionOutput struct {
	stepID     string
	resultPath string
	schema     map[string]interface{}
}

func runbookActionOutputs(definition *runbook.Definition, catalog CapabilityCatalog) []runbookActionOutput {
	result := make([]runbookActionOutput, 0)
	if definition == nil {
		return result
	}
	for stepID, step := range definition.Steps {
		if step.Action == nil || strings.TrimSpace(step.Action.ResultPath) == "" {
			continue
		}
		skill, ok := catalog.Skills[strings.TrimSpace(step.Action.SkillID)]
		if !ok || strings.TrimSpace(skill.Version) != strings.TrimSpace(step.Action.SkillVersion) {
			continue
		}
		contract, ok := skill.ActionContracts[strings.TrimSpace(step.Action.Action)]
		if !ok {
			continue
		}
		result = append(result, runbookActionOutput{stepID: stepID, resultPath: step.Action.ResultPath, schema: contract.OutputSchema})
	}
	sort.Slice(result, func(left, right int) bool {
		if len(result[left].resultPath) == len(result[right].resultPath) {
			return result[left].stepID < result[right].stepID
		}
		return len(result[left].resultPath) > len(result[right].resultPath)
	})
	return result
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

func validateRunbookActionReferences(path, consumerStep string, arguments map[string]runbook.Value, inputSchema map[string]interface{}, definition *runbook.Definition, outputs []runbookActionOutput) []ValidationIssue {
	issues := make([]ValidationIssue, 0)
	properties, _ := inputSchema["properties"].(map[string]interface{})
	names := make([]string, 0, len(arguments))
	for name := range arguments {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		value := arguments[name]
		expected, _ := properties[name].(map[string]interface{})
		if value.Ref != "" {
			issues = append(issues, validateRunbookActionReference(path+"."+name, consumerStep, value.Ref, expected, definition, outputs)...)
		}
		if len(value.Template) > 0 {
			if !schemasTypeCompatible(expected, map[string]interface{}{"type": "string"}) {
				issues = append(issues, issue(path+"."+name, "runbook_action_reference_type_mismatch", fmt.Sprintf("Runbook template argument %s does not match the authorized contract", name)))
			}
			for index, segment := range value.Template {
				if segment.Ref != "" {
					segmentPath := fmt.Sprintf("%s.%s.template[%d].ref", path, name, index)
					issues = append(issues, validateRunbookActionReference(segmentPath, consumerStep, segment.Ref, nil, definition, outputs)...)
				}
			}
		}
	}
	return issues
}

func validateRunbookActionReference(path, consumerStep, reference string, expected map[string]interface{}, definition *runbook.Definition, outputs []runbookActionOutput) []ValidationIssue {
	source, known, invalid, unavailable := resolveRunbookReferenceSchema(reference, consumerStep, definition, outputs)
	switch {
	case unavailable:
		return []ValidationIssue{issue(path, "runbook_action_reference_unavailable", "Runbook action reference is not produced before this step")}
	case invalid:
		return []ValidationIssue{issue(path, "runbook_action_reference_path_invalid", "Runbook action reference does not exist in the producing action contract")}
	case known && len(expected) > 0 && !schemasTypeCompatible(expected, source):
		return []ValidationIssue{issue(path, "runbook_action_reference_type_mismatch", "Runbook action reference does not match the authorized argument contract")}
	default:
		return nil
	}
}

func resolveRunbookReferenceSchema(reference, consumerStep string, definition *runbook.Definition, outputs []runbookActionOutput) (map[string]interface{}, bool, bool, bool) {
	switch reference {
	case "/runtime/runId", "/runtime/rootRunId", "/runtime/objectiveId":
		return map[string]interface{}{"type": "string"}, true, false, false
	}
	for _, output := range outputs {
		if reference != output.resultPath && !strings.HasPrefix(reference, strings.TrimRight(output.resultPath, "/")+"/") {
			continue
		}
		if output.stepID == consumerStep || !runbookStepDominates(definition, output.stepID, consumerStep) {
			return nil, false, false, true
		}
		if len(output.schema) == 0 {
			return nil, false, false, false
		}
		relative := strings.TrimPrefix(reference, output.resultPath)
		resolved, ok := schemaAtJSONPointer(output.schema, relative)
		if !ok {
			return nil, false, true, false
		}
		return resolved, true, false, false
	}
	if strings.HasPrefix(reference, "/input/") {
		if schema, ok := runbookInputReferenceSchema(definition, consumerStep, strings.TrimPrefix(reference, "/input")); ok {
			return schema, true, false, false
		}
	}
	return nil, false, false, false
}

func runbookInputReferenceSchema(definition *runbook.Definition, consumerStep, relative string) (map[string]interface{}, bool) {
	var resolved map[string]interface{}
	for entrypoint, contract := range definition.Interfaces {
		first := definition.Entrypoints[entrypoint]
		if first == "" || (first != consumerStep && !runbookStepReaches(definition, first, consumerStep)) {
			continue
		}
		candidate, ok := schemaAtJSONPointer(contract.InputSchema, relative)
		if !ok {
			return nil, false
		}
		if resolved == nil {
			resolved = candidate
			continue
		}
		if !schemasTypeCompatible(resolved, candidate) || !schemasTypeCompatible(candidate, resolved) {
			return nil, false
		}
	}
	return resolved, resolved != nil
}

func schemaAtJSONPointer(schema map[string]interface{}, pointer string) (map[string]interface{}, bool) {
	current := schema
	if pointer == "" {
		return current, true
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, false
	}
	for _, encoded := range strings.Split(strings.TrimPrefix(pointer, "/"), "/") {
		component := strings.ReplaceAll(strings.ReplaceAll(encoded, "~1", "/"), "~0", "~")
		if properties, ok := current["properties"].(map[string]interface{}); ok {
			next, ok := properties[component].(map[string]interface{})
			if !ok {
				if current["additionalProperties"] == false {
					return nil, false
				}
				return nil, true
			}
			current = next
			continue
		}
		if current["type"] == "array" {
			next, ok := current["items"].(map[string]interface{})
			if !ok {
				return nil, true
			}
			current = next
			continue
		}
		return nil, true
	}
	return current, true
}

func schemasTypeCompatible(expected, source map[string]interface{}) bool {
	expectedTypes, sourceTypes := schemaTypes(expected), schemaTypes(source)
	if len(expectedTypes) == 0 || len(sourceTypes) == 0 {
		return true
	}
	for sourceType := range sourceTypes {
		if !expectedTypes[sourceType] && !(sourceType == "integer" && expectedTypes["number"]) {
			return false
		}
	}
	return true
}

func schemaTypes(schema map[string]interface{}) map[string]bool {
	result := make(map[string]bool)
	switch value := schema["type"].(type) {
	case string:
		result[value] = true
	case []interface{}:
		for _, item := range value {
			if text, ok := item.(string); ok {
				result[text] = true
			}
		}
	}
	for _, keyword := range []string{"oneOf", "anyOf"} {
		alternatives, _ := schema[keyword].([]interface{})
		for _, raw := range alternatives {
			if alternative, ok := raw.(map[string]interface{}); ok {
				for value := range schemaTypes(alternative) {
					result[value] = true
				}
			}
		}
	}
	return result
}

func runbookStepReaches(definition *runbook.Definition, start, target string) bool {
	if definition == nil || start == "" || target == "" {
		return false
	}
	visited, queue := map[string]bool{start: true}, []string{start}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, next := range runbookStepTargets(definition.Steps[current]) {
			if next == target {
				return true
			}
			if next != "" && !visited[next] {
				visited[next], queue = true, append(queue, next)
			}
		}
	}
	return false
}

func runbookStepDominates(definition *runbook.Definition, producer, consumer string) bool {
	if definition == nil || producer == "" || consumer == "" || producer == consumer {
		return false
	}
	considered := false
	for _, entrypoint := range definition.Entrypoints {
		if entrypoint == "" || (entrypoint != consumer && !runbookStepReaches(definition, entrypoint, consumer)) {
			continue
		}
		considered = true
		if entrypoint == producer {
			continue
		}
		if runbookStepReachesWithout(definition, entrypoint, consumer, producer) {
			return false
		}
	}
	return considered
}

func runbookStepReachesWithout(definition *runbook.Definition, start, target, excluded string) bool {
	if start == excluded {
		return false
	}
	if start == target {
		return true
	}
	visited, queue := map[string]bool{start: true}, []string{start}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, next := range runbookStepTargets(definition.Steps[current]) {
			if next == excluded || next == "" {
				continue
			}
			if next == target {
				return true
			}
			if !visited[next] {
				visited[next], queue = true, append(queue, next)
			}
		}
	}
	return false
}

func runbookStepTargets(step runbook.Step) []string {
	switch {
	case step.Action != nil:
		return []string{step.Action.Next}
	case step.Delegate != nil:
		return []string{step.Delegate.Next}
	case step.Decision != nil:
		result := make([]string, 0, len(step.Decision.Cases)+1)
		for _, candidate := range step.Decision.Cases {
			result = append(result, candidate.Next)
		}
		return append(result, step.Decision.Default)
	case step.Transform != nil:
		return []string{step.Transform.Next}
	case step.Wait != nil:
		return []string{step.Wait.Next}
	case step.Fork != nil:
		result := make([]string, 0, len(step.Fork.Branches)+1)
		for _, branch := range step.Fork.Branches {
			result = append(result, branch)
		}
		return append(result, step.Fork.Join)
	case step.Join != nil:
		return []string{step.Join.Next}
	case step.ForEach != nil:
		return []string{step.ForEach.Body, step.ForEach.Next}
	case step.LoopReturn != nil:
		return []string{step.LoopReturn.ForEach}
	default:
		return nil
	}
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
