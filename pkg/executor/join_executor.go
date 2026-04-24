package executor

import (
	"context"
	"fmt"
)

const NodeTypeJoin = "join"

type JoinExecutor struct{}

func NewJoinExecutor() *JoinExecutor {
	return &JoinExecutor{}
}

func (e *JoinExecutor) Type() string {
	return NodeTypeJoin
}

func (e *JoinExecutor) Execute(ctx context.Context, step *StepDefinition, resolver TemplateResolver) (*StepResult, error) {
	config := step.Config

	mode := "all"
	if config != nil {
		if m, ok := config["mode"].(string); ok && m != "" {
			mode = m
		}
	}

	if mode == "stream" {
		var prev interface{}

		if cp, ok := resolver.(ContextProvider); ok {
			contextData := cp.GetContextData()
			prev = contextData["prev"]
		}

		if prev == nil {
			return &StepResult{Output: map[string]interface{}{}}, nil
		}

		if prevMap, ok := prev.(map[string]interface{}); ok {
			return &StepResult{
				Output: prevMap,
			}, nil
		}

		return &StepResult{
			Output: map[string]interface{}{
				"value": prev,
			},
		}, nil
	}

	inputs := resolver.GetStepOutput("_inputs")

	var collected []interface{}
	if inputs != nil {
		if inputList, ok := inputs.([]interface{}); ok {
			collected = inputList
		}
	}

	if len(collected) == 0 {
		prevOutput := resolver.GetStepOutput("_prev")
		if prevOutput != nil {
			collected = append(collected, prevOutput)
		}
	}

	output := map[string]interface{}{
		"mode":    mode,
		"count":   len(collected),
		"results": collected,
	}

	merged := make(map[string]interface{})
	for i, result := range collected {
		if resultMap, ok := result.(map[string]interface{}); ok {
			for k, v := range resultMap {
				key := fmt.Sprintf("%s_%d", k, i)
				merged[key] = v
			}
		}
	}
	output["merged"] = merged

	return &StepResult{
		Output: output,
	}, nil
}
