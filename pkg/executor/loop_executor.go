package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

const NodeTypeLoop = "loop"

// LoopExecutor iterates over array items and collects results.
// Note: The actual iteration execution is handled by the pipeline executor.
// This executor sets up the loop context and returns the iteration results.
type LoopExecutor struct{}

func NewLoopExecutor() *LoopExecutor {
	return &LoopExecutor{}
}

func (e *LoopExecutor) Type() string {
	return NodeTypeLoop
}

func (e *LoopExecutor) Execute(ctx context.Context, step *StepDefinition, resolver TemplateResolver) (*StepResult, error) {
	config := step.Config
	if config == nil {
		return nil, fmt.Errorf("loop step requires config")
	}

	itemsRaw, ok := config["items"]
	if !ok {
		return nil, fmt.Errorf("loop step requires 'items'")
	}

	var items []interface{}
	switch v := itemsRaw.(type) {
	case []interface{}:
		items = v
	case map[string]interface{}:
		// Iterate over object keys
		for key, value := range v {
			items = append(items, map[string]interface{}{
				"key":   key,
				"value": value,
			})
		}
	case string:
		resolved := resolver.ResolveMap(map[string]interface{}{"_": v})["_"]

		switch r := resolved.(type) {
		case []interface{}:
			items = r
		case map[string]interface{}:
			for key, value := range r {
				items = append(items, map[string]interface{}{
					"key":   key,
					"value": value,
				})
			}
		case string:
			trimmed := strings.TrimSpace(r)
			if trimmed == "" {
				return nil, fmt.Errorf("loop step 'items' resolved to empty string. Template '%s' may reference a missing or null field from previous node", v)
			}
			if strings.HasPrefix(trimmed, "[") || strings.HasPrefix(trimmed, "{") {
				var parsed interface{}
				if err := json.Unmarshal([]byte(trimmed), &parsed); err == nil {
					switch p := parsed.(type) {
					case []interface{}:
						items = p
					case map[string]interface{}:
						for key, value := range p {
							items = append(items, map[string]interface{}{
								"key":   key,
								"value": value,
							})
						}
					default:
						return nil, fmt.Errorf("loop step 'items' resolved to unexpected JSON type: %T", parsed)
					}
				} else {
					return nil, fmt.Errorf("loop step 'items' resolved to string that looks like JSON but failed to parse: %v", err)
				}
			} else {
				return nil, fmt.Errorf("loop step 'items' must resolve to an array or object, got string: %s", trimmed)
			}
		default:
			return nil, fmt.Errorf("loop step 'items' must resolve to an array or object, got %T", resolved)
		}
	default:
		return nil, fmt.Errorf("loop step 'items' must be an array, object, or template reference")
	}

	// Get variable names for item and index
	itemVar := "item"
	if v, ok := config["itemVar"].(string); ok && v != "" {
		itemVar = v
	}

	indexVar := "index"
	if v, ok := config["indexVar"].(string); ok && v != "" {
		indexVar = v
	}

	// Check if loop body results are provided (from pipeline executor)
	bodyResults := resolver.GetStepOutput("_loopResults")

	var results []interface{}
	if bodyResults != nil {
		if resultList, ok := bodyResults.([]interface{}); ok {
			results = resultList
		}
	}

	// If no body results, this is the initial invocation - set up the loop
	// The pipeline executor will handle the actual iteration
	if results == nil {
		return &StepResult{
			Output: map[string]interface{}{
				"loop":     true,
				"items":    items,
				"count":    len(items),
				"itemVar":  itemVar,
				"indexVar": indexVar,
				"results":  []interface{}{},
			},
		}, nil
	}

	// Return the collected results
	return &StepResult{
		Output: map[string]interface{}{
			"loop":    true,
			"count":   len(items),
			"results": results,
		},
	}, nil
}
