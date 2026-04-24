package executor

import (
	"context"
	"fmt"
)

// IfExecutor handles conditional branching
// Config: {"condition": "expr", "then": "stepName", "else": "stepName"}
type IfExecutor struct{}

func (e *IfExecutor) Type() string {
	return StepTypeIf
}

func (e *IfExecutor) Execute(ctx context.Context, step *StepDefinition, resolver TemplateResolver) (*StepResult, error) {
	config := step.Config
	if config == nil {
		return nil, fmt.Errorf("if step requires config")
	}

	condition, ok := config["condition"].(string)
	if !ok {
		return nil, fmt.Errorf("if step requires 'condition' string")
	}

	result := resolver.EvaluateCondition(condition)

	output := map[string]interface{}{
		"condition": condition,
		"result":    result,
	}

	// Return branch identifier that matches the edge sourceHandle
	// The frontend uses "then" and "else" as handle IDs for conditional nodes
	var branchKey string
	if result {
		branchKey = "then"
		output["branch"] = "then"
	} else {
		branchKey = "else"
		output["branch"] = "else"
	}

	return &StepResult{
		Output:   output,
		NextStep: branchKey,
	}, nil
}
