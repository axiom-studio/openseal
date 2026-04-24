package executor

import (
	"context"
)

const NodeTypeSplit = "split"

type SplitExecutor struct{}

func NewSplitExecutor() *SplitExecutor {
	return &SplitExecutor{}
}

func (e *SplitExecutor) Type() string {
	return NodeTypeSplit
}

func (e *SplitExecutor) Execute(ctx context.Context, step *StepDefinition, resolver TemplateResolver) (*StepResult, error) {
	var prev interface{}

	if cp, ok := resolver.(ContextProvider); ok {
		contextData := cp.GetContextData()
		prev = contextData["prev"]
	}

	if prev == nil {
		return &StepResult{Output: map[string]interface{}{}}, nil
	}

	if prevMap, ok := prev.(map[string]interface{}); ok {
		return &StepResult{Output: prevMap}, nil
	}

	return &StepResult{Output: map[string]interface{}{"value": prev}}, nil
}
