package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

const observationRefSchemaExtension = "x-openseal-observationRef"

// ObservationRefActionProposalValidator enforces portable references from a
// Skill action schema against the latest kernel-owned observation. This lets a
// Skill describe semantic target constraints without giving the kernel any
// browser- or connector-specific knowledge.
type ObservationRefActionProposalValidator struct{}

func (ObservationRefActionProposalValidator) ValidateActionProposal(_ context.Context, input ActionProposalValidationInput) (map[string]interface{}, error) {
	if input.Bound == nil || input.Bound.Action.InputSchema == nil {
		return nil, nil
	}
	properties, _ := input.Bound.Action.InputSchema["properties"].(map[string]interface{})
	for argument, rawProperty := range properties {
		property, _ := rawProperty.(map[string]interface{})
		constraint, _ := property[observationRefSchemaExtension].(map[string]interface{})
		if constraint == nil {
			continue
		}
		ref, _ := input.Arguments[argument].(string)
		ref = strings.TrimSpace(ref)
		if ref == "" {
			return nil, fmt.Errorf("action argument %s requires a current observation reference", argument)
		}
		element, err := currentObservationElement(input.Run, ref)
		if err != nil {
			return nil, fmt.Errorf("action argument %s: %w", argument, err)
		}
		if !observationRoleAllowed(fmt.Sprint(element["role"]), constraint["roles"]) {
			return nil, fmt.Errorf("action argument %s reference %s has role %q, which is not allowed", argument, ref, element["role"])
		}
		if requireEnabled, _ := constraint["requireEnabled"].(bool); requireEnabled {
			state, _ := element["state"].(map[string]interface{})
			if disabled, _ := state["disabled"].(bool); disabled {
				return nil, fmt.Errorf("action argument %s reference %s is disabled", argument, ref)
			}
		}
	}
	return nil, nil
}

func currentObservationElement(run *AgentRun, ref string) (map[string]interface{}, error) {
	if run == nil || run.Checkpoint == nil {
		return nil, errors.New("current observation is unavailable")
	}
	last, _ := run.Checkpoint["lastAction"].(map[string]interface{})
	if last == nil || fmt.Sprint(last["status"]) != string(ActionCallStatusSucceeded) {
		return nil, errors.New("latest action is not a successful observation; take a new observation")
	}
	result, _ := last["result"].(map[string]interface{})
	if compacted, _ := result["value"].(map[string]interface{}); compacted != nil {
		result = compacted
	}
	generation := fmt.Sprint(result["generation"])
	if generation == "" || !strings.HasPrefix(ref, "s"+generation+":") {
		return nil, errors.New("reference is not from the latest observation; take a new observation")
	}
	switch elements := result["elements"].(type) {
	case []interface{}:
		for _, raw := range elements {
			element, _ := raw.(map[string]interface{})
			if fmt.Sprint(element["ref"]) == ref {
				return element, nil
			}
		}
	case []map[string]interface{}:
		for _, element := range elements {
			if fmt.Sprint(element["ref"]) == ref {
				return element, nil
			}
		}
	}
	return nil, errors.New("reference is absent from the latest observation; take a new observation")
}

func observationRoleAllowed(role string, raw interface{}) bool {
	role = strings.TrimSpace(role)
	var values []string
	switch typed := raw.(type) {
	case []interface{}:
		for _, value := range typed {
			values = append(values, fmt.Sprint(value))
		}
	case []string:
		values = append(values, typed...)
	}
	if len(values) == 0 {
		return true
	}
	for _, value := range values {
		if role == strings.TrimSpace(value) {
			return true
		}
	}
	return false
}
