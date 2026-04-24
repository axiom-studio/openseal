package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

const NodeTypeSort = "sort"

// SortExecutor sorts array elements by a field
type SortExecutor struct{}

func NewSortExecutor() *SortExecutor {
	return &SortExecutor{}
}

func (e *SortExecutor) Type() string {
	return NodeTypeSort
}

func (e *SortExecutor) Execute(ctx context.Context, step *StepDefinition, resolver TemplateResolver) (*StepResult, error) {
	config := step.Config
	if config == nil {
		return nil, fmt.Errorf("sort step requires config")
	}

	itemsRaw, ok := config["items"]
	if !ok {
		return nil, fmt.Errorf("sort step requires 'items'")
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
					return nil, fmt.Errorf("sort step 'items' resolved to string that looks like JSON array but failed to parse: %v", err)
				}
			} else {
				return nil, fmt.Errorf("sort step 'items' must resolve to an array, got string: %s", trimmed)
			}
		default:
			return nil, fmt.Errorf("sort step 'items' must resolve to an array, got %T", resolved)
		}
	default:
		return nil, fmt.Errorf("sort step 'items' must be an array or template reference")
	}

	field, _ := config["field"].(string)
	if field == "" {
		return nil, fmt.Errorf("sort step requires 'field'")
	}

	order, _ := config["order"].(string)
	if order == "" {
		order = "asc"
	}

	// Make a copy to avoid mutating the original
	sorted := make([]interface{}, len(items))
	copy(sorted, items)

	// Sort by field
	sort.SliceStable(sorted, func(i, j int) bool {
		iVal := getFieldValue(sorted[i], field)
		jVal := getFieldValue(sorted[j], field)

		cmp := compareValues(iVal, jVal)
		if order == "desc" {
			return cmp > 0
		}
		return cmp < 0
	})

	return &StepResult{
		Output: map[string]interface{}{
			"items": sorted,
			"count": len(sorted),
		},
	}, nil
}

func getFieldValue(item interface{}, field string) interface{} {
	if m, ok := item.(map[string]interface{}); ok {
		return m[field]
	}
	return nil
}

func compareValues(a, b interface{}) int {
	// Handle nil
	if a == nil && b == nil {
		return 0
	}
	if a == nil {
		return -1
	}
	if b == nil {
		return 1
	}

	// Compare by type
	switch aVal := a.(type) {
	case string:
		if bVal, ok := b.(string); ok {
			if aVal < bVal {
				return -1
			} else if aVal > bVal {
				return 1
			}
			return 0
		}
	case float64:
		if bVal, ok := b.(float64); ok {
			if aVal < bVal {
				return -1
			} else if aVal > bVal {
				return 1
			}
			return 0
		}
	case int:
		if bVal, ok := b.(int); ok {
			if aVal < bVal {
				return -1
			} else if aVal > bVal {
				return 1
			}
			return 0
		}
		// Handle int vs float64 comparison
		if bVal, ok := b.(float64); ok {
			aFloat := float64(aVal)
			if aFloat < bVal {
				return -1
			} else if aFloat > bVal {
				return 1
			}
			return 0
		}
	case bool:
		if bVal, ok := b.(bool); ok {
			if !aVal && bVal {
				return -1
			} else if aVal && !bVal {
				return 1
			}
			return 0
		}
	}

	// Fallback: compare string representations
	aStr := fmt.Sprintf("%v", a)
	bStr := fmt.Sprintf("%v", b)
	if aStr < bStr {
		return -1
	} else if aStr > bStr {
		return 1
	}
	return 0
}
