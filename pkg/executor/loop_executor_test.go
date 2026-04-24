package executor

import (
	"context"
	"testing"
)

func TestLoopExecutor_Execute(t *testing.T) {
	executor := NewLoopExecutor()

	if executor.Type() != NodeTypeLoop {
		t.Errorf("Expected type %q, got %q", NodeTypeLoop, executor.Type())
	}

	resolver := &simpleResolver{
		input:       map[string]interface{}{},
		nodeOutputs: map[string]interface{}{},
		variables:   map[string]interface{}{},
	}

	tests := []struct {
		name          string
		config        map[string]interface{}
		expectedCount int
		expectError   bool
		errorContains string
	}{
		{
			name: "loop with array items",
			config: map[string]interface{}{
				"items": []interface{}{
					map[string]interface{}{"id": 1},
					map[string]interface{}{"id": 2},
					map[string]interface{}{"id": 3},
				},
			},
			expectedCount: 3,
		},
		{
			name: "loop with custom variable names",
			config: map[string]interface{}{
				"items": []interface{}{
					"a", "b", "c",
				},
				"itemVar":  "element",
				"indexVar": "i",
			},
			expectedCount: 3,
		},
		{
			name: "loop with empty array",
			config: map[string]interface{}{
				"items": []interface{}{},
			},
			expectedCount: 0,
		},
		{
			name:          "missing items",
			config:        map[string]interface{}{},
			expectError:   true,
			errorContains: "requires 'items'",
		},
		{
			name:          "nil config",
			config:        nil,
			expectError:   true,
			errorContains: "requires config",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			step := &StepDefinition{
				Name:   "test_loop",
				Type:   NodeTypeLoop,
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

			count, ok := result.Output["count"].(int)
			if !ok {
				t.Fatalf("Expected count to be int, got %T", result.Output["count"])
			}

			if count != tt.expectedCount {
				t.Errorf("Expected count %d, got %d", tt.expectedCount, count)
			}

			loop, ok := result.Output["loop"].(bool)
			if !ok || !loop {
				t.Errorf("Expected loop=true in output")
			}
		})
	}
}

func TestLoopExecutor_VariableNames(t *testing.T) {
	executor := NewLoopExecutor()

	resolver := &simpleResolver{
		input:       map[string]interface{}{},
		nodeOutputs: map[string]interface{}{},
		variables:   map[string]interface{}{},
	}

	step := &StepDefinition{
		Name: "test_loop",
		Type: NodeTypeLoop,
		Config: map[string]interface{}{
			"items":    []interface{}{"a", "b"},
			"itemVar":  "element",
			"indexVar": "idx",
		},
	}

	result, err := executor.Execute(context.Background(), step, resolver)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if result.Output["itemVar"] != "element" {
		t.Errorf("Expected itemVar='element', got %v", result.Output["itemVar"])
	}

	if result.Output["indexVar"] != "idx" {
		t.Errorf("Expected indexVar='idx', got %v", result.Output["indexVar"])
	}
}

type mockResolveResolver struct {
	simpleResolver
	resolveMapResult map[string]interface{}
}

func (m *mockResolveResolver) ResolveMap(input map[string]interface{}) map[string]interface{} {
	if m.resolveMapResult != nil {
		return m.resolveMapResult
	}
	return m.simpleResolver.ResolveMap(input)
}

func TestLoopExecutor_JSONStringItems(t *testing.T) {
	executor := NewLoopExecutor()

	tests := []struct {
		name           string
		itemsTemplate  string
		resolveMapMock map[string]interface{}
		expectedCount  int
		expectError    bool
	}{
		{
			name:          "template resolves to JSON array string",
			itemsTemplate: "{{prev.tickets}}",
			resolveMapMock: map[string]interface{}{
				"_": `[{"id":1,"name":"ticket1"},{"id":2,"name":"ticket2"},{"id":3,"name":"ticket3"}]`,
			},
			expectedCount: 3,
		},
		{
			name:          "template resolves to JSON object string",
			itemsTemplate: "{{prev.config}}",
			resolveMapMock: map[string]interface{}{
				"_": `{"key1":"value1","key2":"value2"}`,
			},
			expectedCount: 2,
		},
		{
			name:          "template resolves to actual array",
			itemsTemplate: "{{prev.items}}",
			resolveMapMock: map[string]interface{}{
				"_": []interface{}{"a", "b", "c", "d"},
			},
			expectedCount: 4,
		},
		{
			name:          "template resolves to actual object",
			itemsTemplate: "{{prev.settings}}",
			resolveMapMock: map[string]interface{}{
				"_": map[string]interface{}{"a": 1, "b": 2},
			},
			expectedCount: 2,
		},
		{
			name:          "template resolves to non-array string",
			itemsTemplate: "{{prev.value}}",
			resolveMapMock: map[string]interface{}{
				"_": "not an array or object",
			},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolver := &mockResolveResolver{
				resolveMapResult: tt.resolveMapMock,
			}

			step := &StepDefinition{
				Name: "test_loop",
				Type: NodeTypeLoop,
				Config: map[string]interface{}{
					"items": tt.itemsTemplate,
				},
			}

			result, err := executor.Execute(context.Background(), step, resolver)

			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}

			count, ok := result.Output["count"].(int)
			if !ok {
				t.Fatalf("Expected count to be int, got %T", result.Output["count"])
			}

			if count != tt.expectedCount {
				t.Errorf("Expected count %d, got %d", tt.expectedCount, count)
			}
		})
	}
}
