package executor

import (
	"context"
	"fmt"
)

// SetExecutor sets a variable that can be used in later steps
// Config: {"name": "varName", "value": "{{expression}}" or any value}
type SetExecutor struct{}

func (e *SetExecutor) Type() string {
	return StepTypeSet
}

func (e *SetExecutor) Execute(ctx context.Context, step *StepDefinition, resolver TemplateResolver) (*StepResult, error) {
	config := step.Config
	if config == nil {
		return nil, fmt.Errorf("set step requires config")
	}

	output := make(map[string]interface{})

	// Support multiple formats:
	// 1. Single variable: {"name": "varName", "value": "..."} or {"variable": "varName", "value": "..."}
	// 2. Multiple variables: {"variables": [{"key": "varName", "value": "..."}, ...]}

	if variables, ok := config["variables"].([]interface{}); ok {
		// Multiple variables format
		for _, v := range variables {
			varMap, ok := v.(map[string]interface{})
			if !ok {
				continue
			}
			key, _ := varMap["key"].(string)
			if key == "" {
				continue
			}
			value := varMap["value"]

			// Resolve the value
			var resolvedValue interface{}
			switch val := value.(type) {
			case string:
				resolvedValue = resolver.ResolveString(val)
			case map[string]interface{}:
				resolvedValue = resolver.ResolveMap(val)
			default:
				resolvedValue = value
			}

			resolver.SetVariable(key, resolvedValue)
			output[key] = resolvedValue
		}
	} else {
		// Single variable format
		var name string
		if n, ok := config["name"].(string); ok {
			name = n
		} else if v, ok := config["variable"].(string); ok {
			name = v
		} else {
			return nil, fmt.Errorf("set step requires 'name', 'variable', or 'variables' array")
		}

		value, ok := config["value"]
		if !ok {
			return nil, fmt.Errorf("set step requires 'value'")
		}

		// Resolve the value if it contains templates
		var resolvedValue interface{}
		switch v := value.(type) {
		case string:
			resolvedValue = resolver.ResolveString(v)
		case map[string]interface{}:
			resolvedValue = resolver.ResolveMap(v)
		default:
			resolvedValue = value
		}

		// Set the variable in the resolver for later use
		resolver.SetVariable(name, resolvedValue)
		output[name] = resolvedValue
	}

	return &StepResult{
		Output: output,
	}, nil
}
