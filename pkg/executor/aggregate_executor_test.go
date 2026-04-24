package executor

import (
	"context"
	"testing"
)

func TestAggregateExecutor_Execute(t *testing.T) {
	executor := NewAggregateExecutor()

	if executor.Type() != NodeTypeAggregate {
		t.Errorf("Expected type %q, got %q", NodeTypeAggregate, executor.Type())
	}

	resolver := &simpleResolver{
		input:       map[string]interface{}{},
		nodeOutputs: map[string]interface{}{},
		variables:   map[string]interface{}{},
	}

	tests := []struct {
		name           string
		config         map[string]interface{}
		expectedResult interface{}
		expectError    bool
		errorContains  string
	}{
		{
			name: "count items",
			config: map[string]interface{}{
				"items": []interface{}{
					map[string]interface{}{"name": "a"},
					map[string]interface{}{"name": "b"},
					map[string]interface{}{"name": "c"},
				},
				"operation": "count",
			},
			expectedResult: 3,
		},
		{
			name: "sum values",
			config: map[string]interface{}{
				"items": []interface{}{
					map[string]interface{}{"amount": float64(10)},
					map[string]interface{}{"amount": float64(20)},
					map[string]interface{}{"amount": float64(30)},
				},
				"operation": "sum",
				"field":     "amount",
			},
			expectedResult: float64(60),
		},
		{
			name: "average values",
			config: map[string]interface{}{
				"items": []interface{}{
					map[string]interface{}{"score": float64(80)},
					map[string]interface{}{"score": float64(90)},
					map[string]interface{}{"score": float64(100)},
				},
				"operation": "avg",
				"field":     "score",
			},
			expectedResult: float64(90),
		},
		{
			name: "min value",
			config: map[string]interface{}{
				"items": []interface{}{
					map[string]interface{}{"value": float64(30)},
					map[string]interface{}{"value": float64(10)},
					map[string]interface{}{"value": float64(20)},
				},
				"operation": "min",
				"field":     "value",
			},
			expectedResult: float64(10),
		},
		{
			name: "max value",
			config: map[string]interface{}{
				"items": []interface{}{
					map[string]interface{}{"value": float64(30)},
					map[string]interface{}{"value": float64(10)},
					map[string]interface{}{"value": float64(20)},
				},
				"operation": "max",
				"field":     "value",
			},
			expectedResult: float64(30),
		},
		{
			name: "first item",
			config: map[string]interface{}{
				"items": []interface{}{
					map[string]interface{}{"name": "first"},
					map[string]interface{}{"name": "second"},
				},
				"operation": "first",
				"field":     "name",
			},
			expectedResult: "first",
		},
		{
			name: "last item",
			config: map[string]interface{}{
				"items": []interface{}{
					map[string]interface{}{"name": "first"},
					map[string]interface{}{"name": "last"},
				},
				"operation": "last",
				"field":     "name",
			},
			expectedResult: "last",
		},
		{
			name: "count empty array",
			config: map[string]interface{}{
				"items":     []interface{}{},
				"operation": "count",
			},
			expectedResult: 0,
		},
		{
			name: "sum empty array",
			config: map[string]interface{}{
				"items":     []interface{}{},
				"operation": "sum",
				"field":     "amount",
			},
			expectedResult: float64(0),
		},
		{
			name:          "missing items",
			config:        map[string]interface{}{"operation": "count"},
			expectError:   true,
			errorContains: "requires 'items'",
		},
		{
			name: "missing operation",
			config: map[string]interface{}{
				"items": []interface{}{},
			},
			expectError:   true,
			errorContains: "requires 'operation'",
		},
		{
			name: "sum without field",
			config: map[string]interface{}{
				"items": []interface{}{
					map[string]interface{}{"value": float64(10)},
				},
				"operation": "sum",
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

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			step := &StepDefinition{
				Name:   "test_aggregate",
				Type:   NodeTypeAggregate,
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

			if result.Output["result"] != tt.expectedResult {
				t.Errorf("Expected result %v (%T), got %v (%T)",
					tt.expectedResult, tt.expectedResult,
					result.Output["result"], result.Output["result"])
			}
		})
	}
}
