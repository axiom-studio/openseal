package executor

import (
	"context"
	"encoding/json"
	"testing"
)

func TestConvertToolsToOpenAI(t *testing.T) {
	tests := []struct {
		name   string
		tools  []*ToolDefinition
		verify func(t *testing.T, result []map[string]interface{})
	}{
		{
			name:  "empty tools",
			tools: []*ToolDefinition{},
			verify: func(t *testing.T, result []map[string]interface{}) {
				if len(result) != 0 {
					t.Errorf("expected empty result, got %d items", len(result))
				}
			},
		},
		{
			name: "single tool",
			tools: []*ToolDefinition{
				{
					Name:        "search",
					Description: "Search documents",
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"query": map[string]interface{}{
								"type":        "string",
								"description": "Search query",
							},
						},
						"required": []string{"query"},
					},
				},
			},
			verify: func(t *testing.T, result []map[string]interface{}) {
				if len(result) != 1 {
					t.Fatalf("expected 1 result, got %d", len(result))
				}
				if result[0]["type"] != "function" {
					t.Errorf("expected type 'function', got %v", result[0]["type"])
				}
				fn := result[0]["function"].(map[string]interface{})
				if fn["name"] != "search" {
					t.Errorf("expected name 'search', got %v", fn["name"])
				}
				if fn["description"] != "Search documents" {
					t.Errorf("expected description, got %v", fn["description"])
				}
				if fn["parameters"] == nil {
					t.Error("expected parameters")
				}
			},
		},
		{
			name: "multiple tools",
			tools: []*ToolDefinition{
				{Name: "search", Description: "Search"},
				{Name: "filter", Description: "Filter"},
				{Name: "sort", Description: "Sort"},
			},
			verify: func(t *testing.T, result []map[string]interface{}) {
				if len(result) != 3 {
					t.Fatalf("expected 3 results, got %d", len(result))
				}
				names := make(map[string]bool)
				for _, r := range result {
					fn := r["function"].(map[string]interface{})
					names[fn["name"].(string)] = true
				}
				for _, expected := range []string{"search", "filter", "sort"} {
					if !names[expected] {
						t.Errorf("expected tool %s", expected)
					}
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ConvertToolsToOpenAI(tt.tools)
			tt.verify(t, result)
		})
	}
}

func TestConvertToolsToAnthropic(t *testing.T) {
	tests := []struct {
		name   string
		tools  []*ToolDefinition
		verify func(t *testing.T, result []map[string]interface{})
	}{
		{
			name:  "empty tools",
			tools: []*ToolDefinition{},
			verify: func(t *testing.T, result []map[string]interface{}) {
				if len(result) != 0 {
					t.Errorf("expected empty result, got %d items", len(result))
				}
			},
		},
		{
			name: "single tool with input_schema",
			tools: []*ToolDefinition{
				{
					Name:        "search",
					Description: "Search documents",
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"query": map[string]interface{}{"type": "string"},
						},
					},
				},
			},
			verify: func(t *testing.T, result []map[string]interface{}) {
				if len(result) != 1 {
					t.Fatalf("expected 1 result, got %d", len(result))
				}
				if result[0]["name"] != "search" {
					t.Errorf("expected name 'search', got %v", result[0]["name"])
				}
				if result[0]["description"] != "Search documents" {
					t.Errorf("expected description, got %v", result[0]["description"])
				}
				if result[0]["input_schema"] == nil {
					t.Error("expected input_schema")
				}
				// Verify no "function" wrapper (unlike OpenAI)
				if _, exists := result[0]["function"]; exists {
					t.Error("Anthropic format should not have function wrapper")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ConvertToolsToAnthropic(tt.tools)
			tt.verify(t, result)
		})
	}
}

func TestConvertToolsToGemini(t *testing.T) {
	tests := []struct {
		name   string
		tools  []*ToolDefinition
		verify func(t *testing.T, result map[string]interface{})
	}{
		{
			name:  "empty tools",
			tools: []*ToolDefinition{},
			verify: func(t *testing.T, result map[string]interface{}) {
				declarations := result["function_declarations"].([]map[string]interface{})
				if len(declarations) != 0 {
					t.Errorf("expected empty declarations, got %d", len(declarations))
				}
			},
		},
		{
			name: "single tool as function_declaration",
			tools: []*ToolDefinition{
				{
					Name:        "search",
					Description: "Search documents",
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"query": map[string]interface{}{"type": "string"},
						},
					},
				},
			},
			verify: func(t *testing.T, result map[string]interface{}) {
				declarations := result["function_declarations"].([]map[string]interface{})
				if len(declarations) != 1 {
					t.Fatalf("expected 1 declaration, got %d", len(declarations))
				}
				if declarations[0]["name"] != "search" {
					t.Errorf("expected name 'search', got %v", declarations[0]["name"])
				}
				if declarations[0]["description"] != "Search documents" {
					t.Errorf("expected description, got %v", declarations[0]["description"])
				}
				if declarations[0]["parameters"] == nil {
					t.Error("expected parameters")
				}
			},
		},
		{
			name: "multiple tools",
			tools: []*ToolDefinition{
				{Name: "search", Description: "Search"},
				{Name: "filter", Description: "Filter"},
			},
			verify: func(t *testing.T, result map[string]interface{}) {
				declarations := result["function_declarations"].([]map[string]interface{})
				if len(declarations) != 2 {
					t.Fatalf("expected 2 declarations, got %d", len(declarations))
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ConvertToolsToGemini(tt.tools)
			tt.verify(t, result)
		})
	}
}

func TestGenerateToolPrompt(t *testing.T) {
	tests := []struct {
		name     string
		tools    []*ToolDefinition
		contains []string
		excludes []string
	}{
		{
			name:     "empty tools returns empty string",
			tools:    []*ToolDefinition{},
			contains: nil,
		},
		{
			name: "includes tool name and description",
			tools: []*ToolDefinition{
				{
					Name:        "search_documents",
					Description: "Search through documents using semantic similarity",
				},
			},
			contains: []string{
				"search_documents",
				"Search through documents using semantic similarity",
				"Available tools:",
			},
		},
		{
			name: "includes parameter documentation",
			tools: []*ToolDefinition{
				{
					Name:        "search",
					Description: "Search documents",
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"query": map[string]interface{}{
								"type":        "string",
								"description": "The search query text",
							},
							"limit": map[string]interface{}{
								"type":        "integer",
								"description": "Max results to return",
							},
						},
						"required": []interface{}{"query"},
					},
				},
			},
			contains: []string{
				"query",
				"string",
				"The search query text",
				"limit",
				"integer",
				"(required)",
			},
		},
		{
			name: "includes tool call format instructions",
			tools: []*ToolDefinition{
				{Name: "search", Description: "Search"},
			},
			contains: []string{
				"tool_call",
				"tool_name",
				"arguments",
				"```json",
			},
		},
		{
			name: "multiple tools",
			tools: []*ToolDefinition{
				{Name: "search", Description: "Search documents"},
				{Name: "filter", Description: "Filter results"},
				{Name: "sort", Description: "Sort output"},
			},
			contains: []string{
				"### search",
				"### filter",
				"### sort",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := GenerateToolPrompt(tt.tools)

			for _, expected := range tt.contains {
				if !toolTestContainsString(result, expected) {
					t.Errorf("expected prompt to contain %q", expected)
				}
			}

			for _, excluded := range tt.excludes {
				if toolTestContainsString(result, excluded) {
					t.Errorf("expected prompt to not contain %q", excluded)
				}
			}
		})
	}
}

func TestGenerateToolPrompt_EmptyReturnsEmpty(t *testing.T) {
	result := GenerateToolPrompt([]*ToolDefinition{})
	if result != "" {
		t.Errorf("expected empty string, got %q", result)
	}
}

func TestToolFormatConversion_SerializesToJSON(t *testing.T) {
	tools := []*ToolDefinition{
		{
			Name:        "vector_search",
			Description: "Search documents using vector similarity",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"query": map[string]interface{}{
						"type":        "string",
						"description": "The search query",
					},
					"top_k": map[string]interface{}{
						"type":        "integer",
						"description": "Number of results",
					},
				},
				"required": []string{"query"},
			},
		},
	}

	t.Run("OpenAI format serializes correctly", func(t *testing.T) {
		result := ConvertToolsToOpenAI(tools)
		data, err := json.Marshal(result)
		if err != nil {
			t.Fatalf("failed to serialize: %v", err)
		}

		var parsed []map[string]interface{}
		if err := json.Unmarshal(data, &parsed); err != nil {
			t.Fatalf("failed to deserialize: %v", err)
		}

		if len(parsed) != 1 {
			t.Errorf("expected 1 tool, got %d", len(parsed))
		}
	})

	t.Run("Anthropic format serializes correctly", func(t *testing.T) {
		result := ConvertToolsToAnthropic(tools)
		data, err := json.Marshal(result)
		if err != nil {
			t.Fatalf("failed to serialize: %v", err)
		}

		var parsed []map[string]interface{}
		if err := json.Unmarshal(data, &parsed); err != nil {
			t.Fatalf("failed to deserialize: %v", err)
		}

		if len(parsed) != 1 {
			t.Errorf("expected 1 tool, got %d", len(parsed))
		}
	})

	t.Run("Gemini format serializes correctly", func(t *testing.T) {
		result := ConvertToolsToGemini(tools)
		data, err := json.Marshal(result)
		if err != nil {
			t.Fatalf("failed to serialize: %v", err)
		}

		var parsed map[string]interface{}
		if err := json.Unmarshal(data, &parsed); err != nil {
			t.Fatalf("failed to deserialize: %v", err)
		}

		if parsed["function_declarations"] == nil {
			t.Error("expected function_declarations")
		}
	})
}

func TestToolCallParsing_FromOpenAIResponse(t *testing.T) {
	t.Run("parses tool call from OpenAI response structure", func(t *testing.T) {
		// Simulate OpenAI response with tool_calls
		responseJSON := `{
			"choices": [{
				"message": {
					"role": "assistant",
					"content": null,
					"tool_calls": [{
						"id": "call_abc123",
						"type": "function",
						"function": {
							"name": "search",
							"arguments": "{\"query\": \"test query\", \"top_k\": 5}"
						}
					}]
				}
			}]
		}`

		var response struct {
			Choices []struct {
				Message struct {
					ToolCalls []struct {
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"message"`
			} `json:"choices"`
		}

		if err := json.Unmarshal([]byte(responseJSON), &response); err != nil {
			t.Fatalf("failed to parse response: %v", err)
		}

		if len(response.Choices) == 0 || len(response.Choices[0].Message.ToolCalls) == 0 {
			t.Fatal("expected tool calls in response")
		}

		tc := response.Choices[0].Message.ToolCalls[0]
		if tc.ID != "call_abc123" {
			t.Errorf("expected id 'call_abc123', got %s", tc.ID)
		}
		if tc.Function.Name != "search" {
			t.Errorf("expected name 'search', got %s", tc.Function.Name)
		}

		// Parse arguments
		var args map[string]interface{}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			t.Fatalf("failed to parse arguments: %v", err)
		}

		if args["query"] != "test query" {
			t.Errorf("expected query 'test query', got %v", args["query"])
		}
	})
}

func TestToolCallParsing_FromAnthropicResponse(t *testing.T) {
	t.Run("parses tool_use from Anthropic response structure", func(t *testing.T) {
		// Simulate Anthropic response with tool_use
		responseJSON := `{
			"content": [
				{
					"type": "text",
					"text": "I'll search for that."
				},
				{
					"type": "tool_use",
					"id": "toolu_abc123",
					"name": "search",
					"input": {"query": "test query", "top_k": 5}
				}
			],
			"stop_reason": "tool_use"
		}`

		var response struct {
			Content []struct {
				Type  string                 `json:"type"`
				Text  string                 `json:"text,omitempty"`
				ID    string                 `json:"id,omitempty"`
				Name  string                 `json:"name,omitempty"`
				Input map[string]interface{} `json:"input,omitempty"`
			} `json:"content"`
			StopReason string `json:"stop_reason"`
		}

		if err := json.Unmarshal([]byte(responseJSON), &response); err != nil {
			t.Fatalf("failed to parse response: %v", err)
		}

		if response.StopReason != "tool_use" {
			t.Errorf("expected stop_reason 'tool_use', got %s", response.StopReason)
		}

		// Find tool_use block
		var toolUse *struct {
			ID    string
			Name  string
			Input map[string]interface{}
		}
		for _, block := range response.Content {
			if block.Type == "tool_use" {
				toolUse = &struct {
					ID    string
					Name  string
					Input map[string]interface{}
				}{block.ID, block.Name, block.Input}
				break
			}
		}

		if toolUse == nil {
			t.Fatal("expected tool_use block")
		}

		if toolUse.ID != "toolu_abc123" {
			t.Errorf("expected id 'toolu_abc123', got %s", toolUse.ID)
		}
		if toolUse.Name != "search" {
			t.Errorf("expected name 'search', got %s", toolUse.Name)
		}
		if toolUse.Input["query"] != "test query" {
			t.Errorf("expected query 'test query', got %v", toolUse.Input["query"])
		}
	})
}

func TestToolCallParsing_FromGeminiResponse(t *testing.T) {
	t.Run("parses functionCall from Gemini response structure", func(t *testing.T) {
		// Simulate Gemini response with functionCall
		responseJSON := `{
			"candidates": [{
				"content": {
					"parts": [{
						"functionCall": {
							"name": "search",
							"args": {"query": "test query", "top_k": 5}
						}
					}],
					"role": "model"
				}
			}]
		}`

		var response struct {
			Candidates []struct {
				Content struct {
					Parts []struct {
						FunctionCall *struct {
							Name string                 `json:"name"`
							Args map[string]interface{} `json:"args"`
						} `json:"functionCall,omitempty"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"candidates"`
		}

		if err := json.Unmarshal([]byte(responseJSON), &response); err != nil {
			t.Fatalf("failed to parse response: %v", err)
		}

		if len(response.Candidates) == 0 || len(response.Candidates[0].Content.Parts) == 0 {
			t.Fatal("expected parts in response")
		}

		fc := response.Candidates[0].Content.Parts[0].FunctionCall
		if fc == nil {
			t.Fatal("expected functionCall")
		}

		if fc.Name != "search" {
			t.Errorf("expected name 'search', got %s", fc.Name)
		}
		if fc.Args["query"] != "test query" {
			t.Errorf("expected query 'test query', got %v", fc.Args["query"])
		}
	})
}

func TestToolCallingLoop_MockExecution(t *testing.T) {
	t.Run("executes tool call and returns result", func(t *testing.T) {
		// Create a mock tool
		tools := []*ToolDefinition{
			{
				Name:        "get_weather",
				Description: "Get current weather",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"location": map[string]interface{}{"type": "string"},
					},
				},
				Config: map[string]interface{}{
					"type": "mock",
				},
			},
		}

		// Register a mock factory
		registry := NewToolRegistry()
		registry.Register("mock", func(def *ToolDefinition, resolver TemplateResolver) (ToolExecutor, error) {
			return &mockToolExecutor{
				name: def.Name,
				executeFunc: func(ctx context.Context, args map[string]interface{}) (*ToolResult, error) {
					location := args["location"].(string)
					return &ToolResult{
						Name: def.Name,
						Result: map[string]interface{}{
							"location":    location,
							"temperature": 72,
							"conditions":  "sunny",
						},
					}, nil
				},
			}, nil
		})

		// Create tool call
		call := &ToolCall{
			ID:   "call_123",
			Name: "get_weather",
			Arguments: map[string]interface{}{
				"location": "San Francisco",
			},
		}

		// Execute through registry
		tool := tools[0]
		executor, err := registry.CreateExecutor(tool, &mockResolver{})
		if err != nil {
			t.Fatalf("failed to create executor: %v", err)
		}

		result, err := executor.Execute(context.Background(), call.Arguments)
		if err != nil {
			t.Fatalf("execution failed: %v", err)
		}

		if result.Error != "" {
			t.Errorf("unexpected error: %s", result.Error)
		}

		resultMap := result.Result.(map[string]interface{})
		if resultMap["location"] != "San Francisco" {
			t.Errorf("expected location 'San Francisco', got %v", resultMap["location"])
		}
		if resultMap["temperature"].(int) != 72 {
			t.Errorf("expected temperature 72, got %v", resultMap["temperature"])
		}
	})
}

func TestToolCallingLoop_MultipleToolCalls(t *testing.T) {
	t.Run("handles multiple sequential tool calls", func(t *testing.T) {
		calls := []*ToolCall{
			{ID: "call_1", Name: "search", Arguments: map[string]interface{}{"query": "first"}},
			{ID: "call_2", Name: "search", Arguments: map[string]interface{}{"query": "second"}},
			{ID: "call_3", Name: "filter", Arguments: map[string]interface{}{"category": "tech"}},
		}

		// Track execution order
		executionOrder := []string{}

		// Create mock registry
		registry := NewToolRegistry()
		registry.Register("mock", func(def *ToolDefinition, resolver TemplateResolver) (ToolExecutor, error) {
			return &mockToolExecutor{
				name: def.Name,
				executeFunc: func(ctx context.Context, args map[string]interface{}) (*ToolResult, error) {
					executionOrder = append(executionOrder, def.Name)
					return &ToolResult{
						Name:   def.Name,
						Result: map[string]interface{}{"executed": true},
					}, nil
				},
			}, nil
		})

		tools := []*ToolDefinition{
			{Name: "search", Config: map[string]interface{}{"type": "mock"}},
			{Name: "filter", Config: map[string]interface{}{"type": "mock"}},
		}

		// Execute each call
		for _, call := range calls {
			var tool *ToolDefinition
			for _, t := range tools {
				if t.Name == call.Name {
					tool = t
					break
				}
			}
			if tool == nil {
				continue
			}

			executor, err := registry.CreateExecutor(tool, &mockResolver{})
			if err != nil {
				t.Fatalf("failed to create executor: %v", err)
			}

			_, err = executor.Execute(context.Background(), call.Arguments)
			if err != nil {
				t.Fatalf("execution failed: %v", err)
			}
		}

		if len(executionOrder) != 3 {
			t.Errorf("expected 3 executions, got %d", len(executionOrder))
		}
	})
}

func TestToolCallingLoop_ErrorHandling(t *testing.T) {
	t.Run("handles tool execution error gracefully", func(t *testing.T) {
		registry := NewToolRegistry()
		registry.Register("failing", func(def *ToolDefinition, resolver TemplateResolver) (ToolExecutor, error) {
			return &mockToolExecutor{
				name: def.Name,
				executeFunc: func(ctx context.Context, args map[string]interface{}) (*ToolResult, error) {
					return &ToolResult{
						Name:  def.Name,
						Error: "database connection failed",
					}, nil
				},
			}, nil
		})

		tool := &ToolDefinition{
			Name:   "failing_tool",
			Config: map[string]interface{}{"type": "failing"},
		}

		executor, err := registry.CreateExecutor(tool, &mockResolver{})
		if err != nil {
			t.Fatalf("failed to create executor: %v", err)
		}

		result, err := executor.Execute(context.Background(), map[string]interface{}{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if result.Error != "database connection failed" {
			t.Errorf("expected error message, got %q", result.Error)
		}
	})
}

func TestPromptFallback_ToolInjection(t *testing.T) {
	t.Run("injects tool descriptions into system prompt", func(t *testing.T) {
		tools := []*ToolDefinition{
			{
				Name:        "search",
				Description: "Search through documents",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"query": map[string]interface{}{
							"type":        "string",
							"description": "Search query",
						},
					},
					"required": []interface{}{"query"},
				},
			},
		}

		basePrompt := "You are a helpful assistant."
		toolPrompt := GenerateToolPrompt(tools)
		fullPrompt := basePrompt + toolPrompt

		// Verify the prompt contains tool information
		if !toolTestContainsString(fullPrompt, "You are a helpful assistant") {
			t.Error("expected base prompt to be preserved")
		}
		if !toolTestContainsString(fullPrompt, "search") {
			t.Error("expected tool name in prompt")
		}
		if !toolTestContainsString(fullPrompt, "Search through documents") {
			t.Error("expected tool description in prompt")
		}
		if !toolTestContainsString(fullPrompt, "tool_call") {
			t.Error("expected tool call format instructions")
		}
	})
}

func TestPromptFallback_ResponseParsing(t *testing.T) {
	t.Run("parses tool call from text response", func(t *testing.T) {
		response := `I'll search for that information.

{"tool_call": {"name": "search", "arguments": {"query": "kubernetes deployment"}}}

Let me process the results.`

		calls, remaining, err := ParseToolCallsFromPromptResponse(response)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if len(calls) != 1 {
			t.Fatalf("expected 1 call, got %d", len(calls))
		}

		if calls[0].Name != "search" {
			t.Errorf("expected name 'search', got %s", calls[0].Name)
		}

		args := calls[0].Arguments
		if args["query"] != "kubernetes deployment" {
			t.Errorf("expected query argument, got %v", args["query"])
		}

		// Remaining text should not contain the JSON
		if toolTestContainsString(remaining, "tool_call") {
			t.Error("remaining text should not contain tool_call JSON")
		}
	})
}

// toolTestContainsString checks if a string contains a substring
func toolTestContainsString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
