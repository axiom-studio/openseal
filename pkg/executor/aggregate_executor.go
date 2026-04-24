package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

const NodeTypeAggregate = "aggregate"

// AggregateExecutor performs aggregation operations on arrays
type AggregateExecutor struct{}

func NewAggregateExecutor() *AggregateExecutor {
	return &AggregateExecutor{}
}

func (e *AggregateExecutor) Type() string {
	return NodeTypeAggregate
}

func (e *AggregateExecutor) Execute(ctx context.Context, step *StepDefinition, resolver TemplateResolver) (*StepResult, error) {
	config := step.Config
	if config == nil {
		return nil, fmt.Errorf("aggregate step requires config")
	}

	itemsRaw, ok := config["items"]
	if !ok {
		return nil, fmt.Errorf("aggregate step requires 'items'")
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
					return nil, fmt.Errorf("aggregate step 'items' resolved to string that looks like JSON array but failed to parse: %v", err)
				}
			} else {
				return nil, fmt.Errorf("aggregate step 'items' must resolve to an array, got string: %s", trimmed)
			}
		default:
			return nil, fmt.Errorf("aggregate step 'items' must resolve to an array, got %T", resolved)
		}
	default:
		return nil, fmt.Errorf("aggregate step 'items' must be an array or template reference")
	}

	operation, _ := config["operation"].(string)
	if operation == "" {
		return nil, fmt.Errorf("aggregate step requires 'operation'")
	}

	field, _ := config["field"].(string)

	result, err := e.performAggregation(items, operation, field)
	if err != nil {
		return nil, err
	}

	return &StepResult{
		Output: map[string]interface{}{
			"result": result,
			"count":  len(items),
		},
	}, nil
}

func (e *AggregateExecutor) performAggregation(items []interface{}, operation, field string) (interface{}, error) {
	if len(items) == 0 {
		switch operation {
		case "count":
			return 0, nil
		case "sum", "avg":
			return float64(0), nil
		default:
			return nil, nil
		}
	}

	switch operation {
	case "count":
		return len(items), nil

	case "sum":
		if field == "" {
			return nil, fmt.Errorf("aggregate 'sum' requires 'field'")
		}
		sum := float64(0)
		for _, item := range items {
			val := getFieldValue(item, field)
			if num, ok := toFloat64(val); ok {
				sum += num
			}
		}
		return sum, nil

	case "avg":
		if field == "" {
			return nil, fmt.Errorf("aggregate 'avg' requires 'field'")
		}
		sum := float64(0)
		count := 0
		for _, item := range items {
			val := getFieldValue(item, field)
			if num, ok := toFloat64(val); ok {
				sum += num
				count++
			}
		}
		if count == 0 {
			return float64(0), nil
		}
		return sum / float64(count), nil

	case "min":
		if field == "" {
			return nil, fmt.Errorf("aggregate 'min' requires 'field'")
		}
		var minVal interface{}
		for _, item := range items {
			val := getFieldValue(item, field)
			if val == nil {
				continue
			}
			if minVal == nil || compareValues(val, minVal) < 0 {
				minVal = val
			}
		}
		return minVal, nil

	case "max":
		if field == "" {
			return nil, fmt.Errorf("aggregate 'max' requires 'field'")
		}
		var maxVal interface{}
		for _, item := range items {
			val := getFieldValue(item, field)
			if val == nil {
				continue
			}
			if maxVal == nil || compareValues(val, maxVal) > 0 {
				maxVal = val
			}
		}
		return maxVal, nil

	case "first":
		if len(items) > 0 {
			if field != "" {
				return getFieldValue(items[0], field), nil
			}
			return items[0], nil
		}
		return nil, nil

	case "last":
		if len(items) > 0 {
			if field != "" {
				return getFieldValue(items[len(items)-1], field), nil
			}
			return items[len(items)-1], nil
		}
		return nil, nil

	default:
		return nil, fmt.Errorf("unknown aggregation operation: %s", operation)
	}
}

func toFloat64(v interface{}) (float64, bool) {
	switch val := v.(type) {
	case float64:
		return val, true
	case float32:
		return float64(val), true
	case int:
		return float64(val), true
	case int64:
		return float64(val), true
	case int32:
		return float64(val), true
	default:
		return 0, false
	}
}
