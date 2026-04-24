package executor

import (
	"context"
	"fmt"
)

// TransformExecutor transforms data using a template
// Config: {"template": {...}} - the template with {{}} placeholders
type TransformExecutor struct{}

func (e *TransformExecutor) Type() string {
	return StepTypeTransform
}

func (e *TransformExecutor) Execute(ctx context.Context, step *StepDefinition, resolver TemplateResolver) (*StepResult, error) {
	config := step.Config
	if config == nil {
		return nil, fmt.Errorf("transform step requires config")
	}

	template, ok := config["template"]
	if !ok {
		return nil, fmt.Errorf("transform step requires 'template'")
	}

	// Resolve the template
	var output map[string]interface{}

	switch t := template.(type) {
	case map[string]interface{}:
		output = resolver.ResolveMap(t)
	case string:
		// If template is a string, treat it as a single value
		resolved := resolver.ResolveString(t)
		output = map[string]interface{}{
			"result": resolved,
		}
	default:
		return nil, fmt.Errorf("transform template must be object or string")
	}

	return &StepResult{
		Output: output,
	}, nil
}
