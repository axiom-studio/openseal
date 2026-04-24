package executor

import (
	"context"
	"testing"
)

func TestFilterExecutor_Execute(t *testing.T) {
	executor := NewFilterExecutor()

	if executor.Type() != NodeTypeFilter {
		t.Errorf("Expected type %q, got %q", NodeTypeFilter, executor.Type())
	}

	tests := []struct {
		name           string
		config         map[string]interface{}
		resolver       *simpleResolver
		expectedCount  int
		expectedItems  []interface{}
		expectError    bool
		errorContains  string
	}{
		{
			name: "filter active items",
			config: map[string]interface{}{
				"items": []interface{}{
					map[string]interface{}{"name": "a", "status": "active"},
					map[string]interface{}{"name": "b", "status": "inactive"},
					map[string]interface{}{"name": "c", "status": "active"},
				},
				"condition": "{{var.item.status}} == active",
			},
			resolver: &simpleResolver{
				input:       map[string]interface{}{},
				nodeOutputs: map[string]interface{}{},
				variables:   map[string]interface{}{},
			},
			expectedCount: 2,
			expectedItems: []interface{}{
				map[string]interface{}{"name": "a", "status": "active"},
				map[string]interface{}{"name": "c", "status": "active"},
			},
		},
		{
			name: "filter with string value match",
			config: map[string]interface{}{
				"items": []interface{}{
					map[string]interface{}{"name": "small", "category": "a"},
					map[string]interface{}{"name": "medium", "category": "b"},
					map[string]interface{}{"name": "large", "category": "a"},
				},
				"condition": "{{var.item.category}} == a",
			},
			resolver: &simpleResolver{
				input:       map[string]interface{}{},
				nodeOutputs: map[string]interface{}{},
				variables:   map[string]interface{}{},
			},
			expectedCount: 2,
		},
		{
			name: "filter all items match",
			config: map[string]interface{}{
				"items": []interface{}{
					map[string]interface{}{"status": "active"},
					map[string]interface{}{"status": "active"},
				},
				"condition": "{{var.item.status}} == active",
			},
			resolver: &simpleResolver{
				input:       map[string]interface{}{},
				nodeOutputs: map[string]interface{}{},
				variables:   map[string]interface{}{},
			},
			expectedCount: 2,
		},
		{
			name: "filter no items match",
			config: map[string]interface{}{
				"items": []interface{}{
					map[string]interface{}{"status": "inactive"},
					map[string]interface{}{"status": "pending"},
				},
				"condition": "{{var.item.status}} == active",
			},
			resolver: &simpleResolver{
				input:       map[string]interface{}{},
				nodeOutputs: map[string]interface{}{},
				variables:   map[string]interface{}{},
			},
			expectedCount: 0,
		},
		{
			name: "filter empty array",
			config: map[string]interface{}{
				"items":     []interface{}{},
				"condition": "{{var.item.status}} == active",
			},
			resolver: &simpleResolver{
				input:       map[string]interface{}{},
				nodeOutputs: map[string]interface{}{},
				variables:   map[string]interface{}{},
			},
			expectedCount: 0,
		},
		{
			name:        "missing items",
			config:      map[string]interface{}{"condition": "{{var.item.status}} == active"},
			resolver:    &simpleResolver{input: map[string]interface{}{}, nodeOutputs: map[string]interface{}{}, variables: map[string]interface{}{}},
			expectError: true,
			errorContains: "requires 'items'",
		},
		{
			name:        "missing condition",
			config:      map[string]interface{}{"items": []interface{}{}},
			resolver:    &simpleResolver{input: map[string]interface{}{}, nodeOutputs: map[string]interface{}{}, variables: map[string]interface{}{}},
			expectError: true,
			errorContains: "requires 'condition'",
		},
		{
			name:        "nil config",
			config:      nil,
			resolver:    &simpleResolver{input: map[string]interface{}{}, nodeOutputs: map[string]interface{}{}, variables: map[string]interface{}{}},
			expectError: true,
			errorContains: "requires config",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			step := &StepDefinition{
				Name:   "test_filter",
				Type:   NodeTypeFilter,
				Config: tt.config,
			}

			result, err := executor.Execute(context.Background(), step, tt.resolver)

			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error containing %q, got nil", tt.errorContains)
				} else if tt.errorContains != "" && !containsString(err.Error(), tt.errorContains) {
					t.Errorf("Expected error containing %q, got %q", tt.errorContains, err.Error())
				}
				return
			}

			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}

			if result == nil {
				t.Fatal("Expected result, got nil")
			}

			count, ok := result.Output["count"].(int)
			if !ok {
				t.Fatalf("Expected count to be int, got %T", result.Output["count"])
			}

			if count != tt.expectedCount {
				t.Errorf("Expected count %d, got %d", tt.expectedCount, count)
			}

			if tt.expectedItems != nil {
				items, ok := result.Output["items"].([]interface{})
				if !ok {
					t.Fatalf("Expected items to be []interface{}, got %T", result.Output["items"])
				}

				if len(items) != len(tt.expectedItems) {
					t.Errorf("Expected %d items, got %d", len(tt.expectedItems), len(items))
				}
			}
		})
	}
}

func TestFilterExecutor_ConditionWithVariableAccess(t *testing.T) {
	executor := NewFilterExecutor()

	// Test accessing item fields via {{var.item.field}} syntax
	resolver := &simpleResolver{
		input: map[string]interface{}{},
		nodeOutputs: map[string]interface{}{},
		variables:   map[string]interface{}{},
	}

	step := &StepDefinition{
		Name: "test_filter",
		Type: NodeTypeFilter,
		Config: map[string]interface{}{
			"items": []interface{}{
				map[string]interface{}{"id": 1, "status": "ok"},
				map[string]interface{}{"id": 2, "status": "error"},
				map[string]interface{}{"id": 3, "status": "ok"},
			},
			"condition": "{{var.item.status}} == ok",
		},
	}

	result, err := executor.Execute(context.Background(), step, resolver)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	count := result.Output["count"].(int)
	if count != 2 {
		t.Errorf("Expected 2 items with status 'ok', got %d", count)
	}

	items := result.Output["items"].([]interface{})
	if len(items) != 2 {
		t.Errorf("Expected 2 filtered items, got %d", len(items))
	}
}

func TestFilterExecutor_IndexVariable(t *testing.T) {
	executor := NewFilterExecutor()

	// Test that index variable is available during filtering
	// Using equality check since EvaluateCondition only supports == and !=
	resolver := &simpleResolver{
		input:       map[string]interface{}{},
		nodeOutputs: map[string]interface{}{},
		variables:   map[string]interface{}{},
	}

	step := &StepDefinition{
		Name: "test_filter",
		Type: NodeTypeFilter,
		Config: map[string]interface{}{
			"items": []interface{}{
				map[string]interface{}{"name": "first"},
				map[string]interface{}{"name": "second"},
				map[string]interface{}{"name": "third"},
			},
			"condition": "{{var.index}} == 1",
		},
	}

	result, err := executor.Execute(context.Background(), step, resolver)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	count := result.Output["count"].(int)
	if count != 1 {
		t.Errorf("Expected 1 item (index 1), got %d", count)
	}

	// Verify the correct item was filtered
	items, ok := result.Output["items"].([]interface{})
	if !ok || len(items) != 1 {
		t.Fatalf("Expected 1 item in result, got %v", result.Output["items"])
	}

	item := items[0].(map[string]interface{})
	if item["name"] != "second" {
		t.Errorf("Expected item name 'second', got %v", item["name"])
	}
}

func containsString(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsSubstring(s, substr))
}

func containsSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
