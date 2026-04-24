package executor

import (
	"context"
	"testing"
)

func TestSortExecutor_Execute(t *testing.T) {
	executor := NewSortExecutor()

	if executor.Type() != NodeTypeSort {
		t.Errorf("Expected type %q, got %q", NodeTypeSort, executor.Type())
	}

	tests := []struct {
		name          string
		config        map[string]interface{}
		expectedOrder []string // expected order of "name" field
		expectError   bool
		errorContains string
	}{
		{
			name: "sort ascending by name",
			config: map[string]interface{}{
				"items": []interface{}{
					map[string]interface{}{"name": "charlie"},
					map[string]interface{}{"name": "alice"},
					map[string]interface{}{"name": "bob"},
				},
				"field": "name",
				"order": "asc",
			},
			expectedOrder: []string{"alice", "bob", "charlie"},
		},
		{
			name: "sort descending by name",
			config: map[string]interface{}{
				"items": []interface{}{
					map[string]interface{}{"name": "charlie"},
					map[string]interface{}{"name": "alice"},
					map[string]interface{}{"name": "bob"},
				},
				"field": "name",
				"order": "desc",
			},
			expectedOrder: []string{"charlie", "bob", "alice"},
		},
		{
			name: "sort by numeric field ascending",
			config: map[string]interface{}{
				"items": []interface{}{
					map[string]interface{}{"name": "c", "value": float64(30)},
					map[string]interface{}{"name": "a", "value": float64(10)},
					map[string]interface{}{"name": "b", "value": float64(20)},
				},
				"field": "value",
				"order": "asc",
			},
			expectedOrder: []string{"a", "b", "c"},
		},
		{
			name: "default order is ascending",
			config: map[string]interface{}{
				"items": []interface{}{
					map[string]interface{}{"name": "b"},
					map[string]interface{}{"name": "a"},
				},
				"field": "name",
			},
			expectedOrder: []string{"a", "b"},
		},
		{
			name: "empty array",
			config: map[string]interface{}{
				"items": []interface{}{},
				"field": "name",
			},
			expectedOrder: []string{},
		},
		{
			name:          "missing items",
			config:        map[string]interface{}{"field": "name"},
			expectError:   true,
			errorContains: "requires 'items'",
		},
		{
			name: "missing field",
			config: map[string]interface{}{
				"items": []interface{}{},
			},
			expectError:   true,
			errorContains: "requires 'field'",
		},
		{
			name:          "nil config",
			config:        nil,
			expectError:   true,
			errorContains: "requires config",
		},
	}

	resolver := &simpleResolver{
		input:       map[string]interface{}{},
		nodeOutputs: map[string]interface{}{},
		variables:   map[string]interface{}{},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			step := &StepDefinition{
				Name:   "test_sort",
				Type:   NodeTypeSort,
				Config: tt.config,
			}

			result, err := executor.Execute(context.Background(), step, resolver)

			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error containing %q, got nil", tt.errorContains)
				} else if tt.errorContains != "" && !containsSubstring(err.Error(), tt.errorContains) {
					t.Errorf("Expected error containing %q, got %q", tt.errorContains, err.Error())
				}
				return
			}

			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}

			items, ok := result.Output["items"].([]interface{})
			if !ok {
				t.Fatalf("Expected items to be []interface{}, got %T", result.Output["items"])
			}

			if len(items) != len(tt.expectedOrder) {
				t.Fatalf("Expected %d items, got %d", len(tt.expectedOrder), len(items))
			}

			for i, expectedName := range tt.expectedOrder {
				item := items[i].(map[string]interface{})
				if item["name"] != expectedName {
					t.Errorf("Item %d: expected name %q, got %q", i, expectedName, item["name"])
				}
			}
		})
	}
}

func TestSortExecutor_StableSort(t *testing.T) {
	executor := NewSortExecutor()

	// Test that sort is stable (preserves order of equal elements)
	resolver := &simpleResolver{
		input:       map[string]interface{}{},
		nodeOutputs: map[string]interface{}{},
		variables:   map[string]interface{}{},
	}

	step := &StepDefinition{
		Name: "test_sort",
		Type: NodeTypeSort,
		Config: map[string]interface{}{
			"items": []interface{}{
				map[string]interface{}{"category": "a", "id": 1},
				map[string]interface{}{"category": "b", "id": 2},
				map[string]interface{}{"category": "a", "id": 3},
				map[string]interface{}{"category": "b", "id": 4},
			},
			"field": "category",
			"order": "asc",
		},
	}

	result, err := executor.Execute(context.Background(), step, resolver)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	items := result.Output["items"].([]interface{})

	// First two should be category "a" with original order preserved (id 1, then id 3)
	if items[0].(map[string]interface{})["id"] != 1 {
		t.Errorf("Expected first item id=1, got %v", items[0].(map[string]interface{})["id"])
	}
	if items[1].(map[string]interface{})["id"] != 3 {
		t.Errorf("Expected second item id=3, got %v", items[1].(map[string]interface{})["id"])
	}
}
