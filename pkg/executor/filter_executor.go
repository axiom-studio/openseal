package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// FilterExecutor filters array elements based on a condition
type FilterExecutor struct{}

func NewFilterExecutor() *FilterExecutor {
	return &FilterExecutor{}
}

func (e *FilterExecutor) Type() string {
	return NodeTypeFilter
}

func (e *FilterExecutor) Execute(ctx context.Context, step *StepDefinition, resolver TemplateResolver) (*StepResult, error) {
	config := step.Config
	if config == nil {
		return nil, fmt.Errorf("filter step requires config")
	}

	itemsRaw, ok := config["items"]
	if !ok {
		return nil, fmt.Errorf("filter step requires 'items'")
	}

	var items []interface{}
	switch v := itemsRaw.(type) {
	case []interface{}:
		items = v
	case string:
		resolved := resolver.ResolveMap(map[string]interface{}{"_": v})["_"]

		switch r := resolved.(type) {
		case []interface{}:
			items = r
		case string:
			trimmed := strings.TrimSpace(r)
			if strings.HasPrefix(trimmed, "[") {
				var parsed []interface{}
				if err := json.Unmarshal([]byte(trimmed), &parsed); err == nil {
					items = parsed
				} else {
					return nil, fmt.Errorf("filter step 'items' resolved to string that looks like JSON array but failed to parse: %v", err)
				}
			} else {
				return nil, fmt.Errorf("filter step 'items' must resolve to an array, got string: %s", trimmed)
			}
		default:
			return nil, fmt.Errorf("filter step 'items' must resolve to an array, got %T", resolved)
		}
	default:
		return nil, fmt.Errorf("filter step 'items' must be an array or template reference")
	}

	conditionTemplate, ok := config["condition"].(string)
	if !ok || conditionTemplate == "" {
		return nil, fmt.Errorf("filter step requires 'condition'")
	}

	var filtered []interface{}
	for i, item := range items {
		resolver.SetVariable("item", item)
		resolver.SetVariable("index", i)
		if resolver.EvaluateCondition(conditionTemplate) {
			filtered = append(filtered, item)
		}
	}

	// Clean up temporary variables
	resolver.SetVariable("item", nil)
	resolver.SetVariable("index", nil)

	return &StepResult{
		Output: map[string]interface{}{
			"items": filtered,
			"count": len(filtered),
		},
	}, nil
}
