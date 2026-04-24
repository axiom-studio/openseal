package executor

import (
	"context"
	"fmt"
)

// MergeExecutor combines multiple inputs into one output
// Config: {"sources": ["{{step.output.step1}}", "{{step.output.step2}}"], "strategy": "merge" | "concat"}
type MergeExecutor struct{}

func (e *MergeExecutor) Type() string {
	return StepTypeMerge
}

func (e *MergeExecutor) Execute(ctx context.Context, step *StepDefinition, resolver TemplateResolver) (*StepResult, error) {
	config := step.Config
	if config == nil {
		return nil, fmt.Errorf("merge step requires config")
	}

	sources, ok := config["sources"].([]interface{})
	if !ok {
		return nil, fmt.Errorf("merge step requires 'sources' array")
	}

	strategy, _ := config["strategy"].(string)
	if strategy == "" {
		strategy = "merge"
	}

	switch strategy {
	case "merge":
		return e.mergeObjects(sources, resolver)
	case "concat":
		return e.concatArrays(sources, resolver)
	default:
		return nil, fmt.Errorf("unknown merge strategy: %s", strategy)
	}
}

func (e *MergeExecutor) mergeObjects(sources []interface{}, resolver TemplateResolver) (*StepResult, error) {
	result := make(map[string]interface{})

	for _, source := range sources {
		var resolved interface{}

		switch s := source.(type) {
		case string:
			// If it's a template string, resolve it
			if len(s) > 4 && s[:2] == "{{" && s[len(s)-2:] == "}}" {
				// Direct template reference - get the actual object
				path := s[2 : len(s)-2]
				resolved = resolver.ResolveString(s)
				// Try to get the actual map if it's a step output reference
				if resolved == s {
					// Template not resolved, try to parse the path
					_ = path // For now, skip unresolved
					continue
				}
			} else {
				resolved = resolver.ResolveString(s)
			}
		case map[string]interface{}:
			resolved = resolver.ResolveMap(s)
		default:
			resolved = source
		}

		// Merge into result
		if m, ok := resolved.(map[string]interface{}); ok {
			for k, v := range m {
				result[k] = v
			}
		}
	}

	return &StepResult{
		Output: result,
	}, nil
}

func (e *MergeExecutor) concatArrays(sources []interface{}, resolver TemplateResolver) (*StepResult, error) {
	var result []interface{}

	for _, source := range sources {
		switch s := source.(type) {
		case string:
			resolved := resolver.ResolveString(s)
			result = append(result, resolved)
		case []interface{}:
			for _, item := range s {
				if str, ok := item.(string); ok {
					result = append(result, resolver.ResolveString(str))
				} else {
					result = append(result, item)
				}
			}
		default:
			result = append(result, source)
		}
	}

	return &StepResult{
		Output: map[string]interface{}{
			"result": result,
		},
	}, nil
}
