package executor

import (
	"context"
	"strings"
	"testing"
)

func TestDebugToolExecutor_Execute(t *testing.T) {
	tests := []struct {
		name        string
		config      map[string]interface{}
		args        map[string]interface{}
		wantErr     bool
		checkOutput func(*testing.T, *ToolResult)
	}{
		{
			name: "simple message logging",
			config: map[string]interface{}{
				"type":  "debug",
				"level": "info",
			},
			args: map[string]interface{}{
				"message": "Test debug message",
			},
			wantErr: false,
			checkOutput: func(t *testing.T, result *ToolResult) {
				output, ok := result.Result.(DebugOutput)
				if !ok {
					t.Fatalf("expected DebugOutput, got %T", result.Result)
				}
				if output.Message != "Test debug message" {
					t.Errorf("expected message 'Test debug message', got %q", output.Message)
				}
				if output.Level != "info" {
					t.Errorf("expected level 'info', got %q", output.Level)
				}
			},
		},
		{
			name: "logging with data",
			config: map[string]interface{}{
				"type":  "debug",
				"level": "debug",
			},
			args: map[string]interface{}{
				"message": "Inspecting user data",
				"data": map[string]interface{}{
					"user_id":   123,
					"username":  "testuser",
					"is_active": true,
				},
			},
			wantErr: false,
			checkOutput: func(t *testing.T, result *ToolResult) {
				output, ok := result.Result.(DebugOutput)
				if !ok {
					t.Fatalf("expected DebugOutput, got %T", result.Result)
				}
				if len(output.Data) != 3 {
					t.Errorf("expected 3 data fields, got %d", len(output.Data))
				}
				if output.Data["user_id"] != 123 {
					t.Errorf("expected user_id 123, got %v", output.Data["user_id"])
				}
				if output.Data["username"] != "testuser" {
					t.Errorf("expected username 'testuser', got %v", output.Data["username"])
				}
			},
		},
		{
			name: "logging with variable resolution",
			config: map[string]interface{}{
				"type": "debug",
			},
			args: map[string]interface{}{
				"message":   "Inspecting variables",
				"variables": []interface{}{"request.body", "user.id"},
			},
			wantErr: false,
			checkOutput: func(t *testing.T, result *ToolResult) {
				output, ok := result.Result.(DebugOutput)
				if !ok {
					t.Fatalf("expected DebugOutput, got %T", result.Result)
				}
				if len(output.Data) == 0 {
					t.Errorf("expected some resolved variables in data")
				}
			},
		},
		{
			name: "logging with nested data structures",
			config: map[string]interface{}{
				"type": "debug",
			},
			args: map[string]interface{}{
				"message": "Complex data structure",
				"data": map[string]interface{}{
					"nested": map[string]interface{}{
						"level1": map[string]interface{}{
							"level2": "deep value",
							"array":  []interface{}{1, 2, 3},
						},
					},
					"top_level": "simple value",
				},
			},
			wantErr: false,
			checkOutput: func(t *testing.T, result *ToolResult) {
				output, ok := result.Result.(DebugOutput)
				if !ok {
					t.Fatalf("expected DebugOutput, got %T", result.Result)
				}

				nested, ok := output.Data["nested"].(map[string]interface{})
				if !ok {
					t.Fatalf("expected nested to be a map")
				}

				level1, ok := nested["level1"].(map[string]interface{})
				if !ok {
					t.Fatalf("expected level1 to be a map")
				}

				if level1["level2"] != "deep value" {
					t.Errorf("expected nested value 'deep value', got %v", level1["level2"])
				}
			},
		},
		{
			name: "logging with comma-separated variables string",
			config: map[string]interface{}{
				"type": "debug",
			},
			args: map[string]interface{}{
				"message":   "Multiple variables",
				"variables": "var1, var2, var3",
			},
			wantErr: false,
			checkOutput: func(t *testing.T, result *ToolResult) {
				output, ok := result.Result.(DebugOutput)
				if !ok {
					t.Fatalf("expected DebugOutput, got %T", result.Result)
				}
				if len(output.Data) != 3 {
					t.Errorf("expected 3 variables, got %d", len(output.Data))
				}
			},
		},
		{
			name: "default level when not specified",
			config: map[string]interface{}{
				"type": "debug",
			},
			args: map[string]interface{}{
				"message": "Default level test",
			},
			wantErr: false,
			checkOutput: func(t *testing.T, result *ToolResult) {
				output, ok := result.Result.(DebugOutput)
				if !ok {
					t.Fatalf("expected DebugOutput, got %T", result.Result)
				}
				if output.Level != "info" {
					t.Errorf("expected default level 'info', got %q", output.Level)
				}
			},
		},
		{
			name: "includeRaw config option",
			config: map[string]interface{}{
				"type":       "debug",
				"includeRaw": true,
			},
			args: map[string]interface{}{
				"message": "With raw data",
				"data": map[string]interface{}{
					"key": "value",
				},
			},
			wantErr: false,
			checkOutput: func(t *testing.T, result *ToolResult) {
				output, ok := result.Result.(DebugOutput)
				if !ok {
					t.Fatalf("expected DebugOutput, got %T", result.Result)
				}
				if output.RawData == nil {
					t.Errorf("expected RawData to be present when includeRaw is true")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolver := &debugMockResolver{
				values: map[string]string{
					"request.body": `{"name": "test"}`,
					"user.id":      "42",
					"var1":         "value1",
					"var2":         "value2",
					"var3":         "value3",
				},
			}

			toolDef := &ToolDefinition{
				Name:        "test_debug",
				Description: "Test debug tool",
				Config:      tt.config,
			}

			executor, err := NewDebugToolExecutor(toolDef, resolver)
			if err != nil {
				t.Fatalf("failed to create executor: %v", err)
			}

			result, err := executor.Execute(context.Background(), tt.args)

			if (err != nil) != tt.wantErr {
				t.Errorf("Execute() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.checkOutput != nil && result != nil {
				tt.checkOutput(t, result)
			}
		})
	}
}

func TestDebugToolExecutor_FormatOutput(t *testing.T) {
	resolver := &debugMockResolver{
		values: map[string]string{},
	}

	toolDef := &ToolDefinition{
		Name:   "test_debug",
		Config: map[string]interface{}{"type": "debug"},
	}

	executor, _ := NewDebugToolExecutor(toolDef, resolver)
	debugExec := executor.(*DebugToolExecutor)

	tests := []struct {
		name     string
		message  string
		data     map[string]interface{}
		level    string
		contains []string
	}{
		{
			name:    "simple format",
			message: "Test message",
			data: map[string]interface{}{
				"key": "value",
			},
			level: "info",
			contains: []string{
				"[INFO]",
				"Test message",
				"key:",
				`"value"`,
			},
		},
		{
			name:    "nested object format",
			message: "Nested data",
			data: map[string]interface{}{
				"user": map[string]interface{}{
					"id":   123,
					"name": "John",
				},
			},
			level: "debug",
			contains: []string{
				"[DEBUG]",
				"user:",
				"\"id\":",
				"123",
				"\"name\":",
				"\"John\"",
			},
		},
		{
			name:    "array format",
			message: "Array data",
			data: map[string]interface{}{
				"items": []interface{}{"a", "b", "c"},
			},
			level: "info",
			contains: []string{
				"items:",
				"[0]:",
				"\"a\"",
				"[1]:",
				"\"b\"",
				"[2]:",
				"\"c\"",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			formatted := debugExec.formatDebugOutput(tt.message, tt.data, tt.level)

			for _, expected := range tt.contains {
				if !debugContainsString(formatted, expected) {
					t.Errorf("formatted output missing expected string %q\nGot:\n%s", expected, formatted)
				}
			}
		})
	}
}

func TestDebugToolRegistration(t *testing.T) {
	registry := NewToolRegistry()

	if !registry.HasToolType("debug") {
		t.Error("debug tool type should be registered by default")
	}

	toolDef := &ToolDefinition{
		Name:        "test_debug",
		Description: "Test debug tool",
		Config: map[string]interface{}{
			"type": "debug",
		},
	}

	resolver := &debugMockResolver{values: map[string]string{}}

	executor, err := registry.CreateExecutor(toolDef, resolver)
	if err != nil {
		t.Fatalf("failed to create executor from registry: %v", err)
	}

	if executor == nil {
		t.Fatal("executor should not be nil")
	}

	if _, ok := executor.(*DebugToolExecutor); !ok {
		t.Errorf("expected *DebugToolExecutor, got %T", executor)
	}
}

func TestDebugToolExecutor_JSONParsing(t *testing.T) {
	resolver := &debugMockResolver{
		values: map[string]string{
			"json_data":  `{"key": "value", "number": 42}`,
			"array_data": `[1, 2, 3]`,
			"plain_text": "just a string",
		},
	}

	toolDef := &ToolDefinition{
		Name:   "test_debug",
		Config: map[string]interface{}{"type": "debug"},
	}

	executor, _ := NewDebugToolExecutor(toolDef, resolver)

	result, err := executor.Execute(context.Background(), map[string]interface{}{
		"message":   "Testing JSON parsing",
		"variables": []interface{}{"json_data", "array_data", "plain_text"},
	})

	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	output := result.Result.(DebugOutput)

	jsonData, ok := output.Data["json_data"].(map[string]interface{})
	if !ok {
		t.Errorf("json_data should be parsed as map, got %T", output.Data["json_data"])
	} else {
		if jsonData["key"] != "value" {
			t.Errorf("expected key='value', got %v", jsonData["key"])
		}
		if num, ok := jsonData["number"].(float64); !ok || num != 42 {
			t.Errorf("expected number=42, got %v (type %T)", jsonData["number"], jsonData["number"])
		}
	}

	arrayData, ok := output.Data["array_data"].([]interface{})
	if !ok {
		t.Errorf("array_data should be parsed as array, got %T", output.Data["array_data"])
	} else {
		if len(arrayData) != 3 {
			t.Errorf("expected array length 3, got %d", len(arrayData))
		}
	}

	if plainText, ok := output.Data["plain_text"].(string); !ok || plainText != "just a string" {
		t.Errorf("plain_text should remain as string 'just a string', got %v (type %T)", output.Data["plain_text"], output.Data["plain_text"])
	}
}

func debugContainsString(s, substr string) bool {
	return strings.Contains(s, substr)
}

type debugMockResolver struct {
	values map[string]string
}

func (m *debugMockResolver) ResolveString(template string) string {
	if len(template) > 4 && template[:2] == "{{" && template[len(template)-2:] == "}}" {
		varName := template[2 : len(template)-2]
		if val, ok := m.values[varName]; ok {
			return val
		}
	}
	return template
}

func (m *debugMockResolver) ResolveValue(template interface{}) interface{} {
	if str, ok := template.(string); ok {
		return m.ResolveString(str)
	}
	return template
}

func (m *debugMockResolver) ResolveMap(data map[string]interface{}) map[string]interface{} {
	result := make(map[string]interface{})
	for k, v := range data {
		result[k] = m.ResolveValue(v)
	}
	return result
}

func (m *debugMockResolver) EvaluateCondition(condition string) bool {
	return false
}

func (m *debugMockResolver) SetVariable(name string, value interface{}) {
}

func (m *debugMockResolver) GetStepOutput(stepName string) interface{} {
	return nil
}

func (m *debugMockResolver) SetStepOutput(stepName string, output interface{}) {
}
