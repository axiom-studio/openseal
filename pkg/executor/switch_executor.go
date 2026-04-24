package executor

import (
	"context"
	"fmt"
)

// SwitchExecutor handles multi-way branching
// Config: {"expression": "{{var}}", "cases": {"value1": "stepA", "value2": "stepB"}, "default": "stepC"}
type SwitchExecutor struct{}

func (e *SwitchExecutor) Type() string {
	return StepTypeSwitch
}

func (e *SwitchExecutor) Execute(ctx context.Context, step *StepDefinition, resolver TemplateResolver) (*StepResult, error) {
	config := step.Config
	if config == nil {
		return nil, fmt.Errorf("switch step requires config")
	}

	expression, ok := config["expression"].(string)
	if !ok {
		return nil, fmt.Errorf("switch step requires 'expression' string")
	}

	cases, _ := config["cases"].(map[string]interface{})

	// Resolve the expression
	value := resolver.ResolveString(expression)

	output := map[string]interface{}{
		"expression": expression,
		"value":      value,
	}

	// Find matching case - return the case key as the branch identifier
	// The graph executor uses this to match edge sourceHandle/label
	var branchKey string
	if cases != nil {
		if _, ok := cases[value]; ok {
			branchKey = value
			output["matched"] = value
		}
	}

	// Use "default" as the branch key if no match
	if branchKey == "" {
		branchKey = "default"
		output["matched"] = "default"
	}

	return &StepResult{
		Output:   output,
		NextStep: branchKey,
	}, nil
}
