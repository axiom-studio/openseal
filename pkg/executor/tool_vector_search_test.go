package executor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewVectorSearchToolExecutor(t *testing.T) {
	t.Run("creates executor successfully", func(t *testing.T) {
		def := &ToolDefinition{
			Name:        "search",
			Description: "Search documents",
			Config: map[string]interface{}{
				"type":             "vector_search",
				"connectionString": "postgres://localhost/test",
				"tableName":        "documents",
			},
		}

		executor, err := NewVectorSearchToolExecutor(def, &mockResolver{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if executor == nil {
			t.Fatal("expected non-nil executor")
		}

		vsExecutor := executor.(*VectorSearchToolExecutor)
		if vsExecutor.def != def {
			t.Error("expected definition to be stored")
		}
		if vsExecutor.client == nil {
			t.Error("expected HTTP client to be initialized")
		}
		if vsExecutor.connPool == nil {
			t.Error("expected connection pool to be initialized")
		}
	})
}

func TestVectorSearchToolExecutor_Execute_Validation(t *testing.T) {
	tests := []struct {
		name        string
		config      map[string]interface{}
		args        map[string]interface{}
		expectedErr string
	}{
		{
			name: "missing connection string",
			config: map[string]interface{}{
				"type":      "vector_search",
				"tableName": "documents",
			},
			args: map[string]interface{}{
				"query": "test query",
			},
			expectedErr: "connectionString is required in tool config",
		},
		{
			name: "missing table name",
			config: map[string]interface{}{
				"type":             "vector_search",
				"connectionString": "postgres://localhost/test",
			},
			args: map[string]interface{}{
				"query": "test query",
			},
			expectedErr: "table is required in tool config",
		},
		{
			name: "missing query argument",
			config: map[string]interface{}{
				"type":             "vector_search",
				"connectionString": "postgres://localhost/test",
				"tableName":        "documents",
			},
			args:        map[string]interface{}{},
			expectedErr: "query argument is required",
		},
		{
			name: "empty query argument",
			config: map[string]interface{}{
				"type":             "vector_search",
				"connectionString": "postgres://localhost/test",
				"tableName":        "documents",
			},
			args: map[string]interface{}{
				"query": "",
			},
			expectedErr: "query argument is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			def := &ToolDefinition{
				Name:   "search",
				Config: tt.config,
			}

			executor, _ := NewVectorSearchToolExecutor(def, &mockResolver{})
			result, err := executor.Execute(context.Background(), tt.args)

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result == nil {
				t.Fatal("expected result")
			}
			if result.Error != tt.expectedErr {
				t.Errorf("expected error %q, got %q", tt.expectedErr, result.Error)
			}
		})
	}
}

func TestVectorSearchToolExecutor_GetEmbeddingConfig(t *testing.T) {
	tests := []struct {
		name           string
		config         map[string]interface{}
		expectedProv   string
		expectedModel  string
		expectedURL    string
		expectedAPIKey string
	}{
		{
			name:          "defaults",
			config:        map[string]interface{}{},
			expectedProv:  "openai",
			expectedModel: "text-embedding-3-small",
			expectedURL:   "https://api.openai.com/v1",
		},
		{
			name: "custom openai config",
			config: map[string]interface{}{
				"embeddingProvider": "openai",
				"embeddingModel":    "text-embedding-3-large",
				"embeddingApiKey":   "sk-test-key",
			},
			expectedProv:   "openai",
			expectedModel:  "text-embedding-3-large",
			expectedURL:    "https://api.openai.com/v1",
			expectedAPIKey: "sk-test-key",
		},
		{
			name: "voyage provider",
			config: map[string]interface{}{
				"embeddingProvider": "voyage",
				"embeddingModel":    "voyage-3",
				"embeddingApiKey":   "voyage-key",
			},
			expectedProv:   "voyage",
			expectedModel:  "voyage-3",
			expectedURL:    "https://api.voyageai.com/v1",
			expectedAPIKey: "voyage-key",
		},
		{
			name: "cohere provider",
			config: map[string]interface{}{
				"embeddingProvider": "cohere",
				"embeddingModel":    "embed-english-v3.0",
				"embeddingApiKey":   "cohere-key",
			},
			expectedProv:   "cohere",
			expectedModel:  "embed-english-v3.0",
			expectedURL:    "https://api.cohere.ai/v1",
			expectedAPIKey: "cohere-key",
		},
		{
			name: "ollama provider",
			config: map[string]interface{}{
				"embeddingProvider": "ollama",
				"embeddingModel":    "nomic-embed-text",
			},
			expectedProv:  "ollama",
			expectedModel: "nomic-embed-text",
			expectedURL:   "http://localhost:11434",
		},
		{
			name: "ollama with custom URL",
			config: map[string]interface{}{
				"embeddingProvider": "ollama",
				"embeddingModel":    "nomic-embed-text",
				"ollamaUrl":         "http://custom:11434",
			},
			expectedProv:  "ollama",
			expectedModel: "nomic-embed-text",
			expectedURL:   "http://custom:11434",
		},
		{
			name: "custom base URL",
			config: map[string]interface{}{
				"embeddingProvider": "custom",
				"embeddingBaseUrl":  "https://custom.api.com/v1",
				"embeddingModel":    "custom-model",
				"embeddingApiKey":   "custom-key",
			},
			expectedProv:   "custom",
			expectedModel:  "custom-model",
			expectedURL:    "https://custom.api.com/v1",
			expectedAPIKey: "custom-key",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			def := &ToolDefinition{
				Name:   "search",
				Config: tt.config,
			}

			executor, _ := NewVectorSearchToolExecutor(def, &mockResolver{})
			vsExecutor := executor.(*VectorSearchToolExecutor)

			cfg := vsExecutor.getEmbeddingConfig(tt.config)

			if cfg.Provider != tt.expectedProv {
				t.Errorf("Provider: expected %q, got %q", tt.expectedProv, cfg.Provider)
			}
			if cfg.Model != tt.expectedModel {
				t.Errorf("Model: expected %q, got %q", tt.expectedModel, cfg.Model)
			}
			if cfg.BaseURL != tt.expectedURL {
				t.Errorf("BaseURL: expected %q, got %q", tt.expectedURL, cfg.BaseURL)
			}
			if cfg.APIKey != tt.expectedAPIKey {
				t.Errorf("APIKey: expected %q, got %q", tt.expectedAPIKey, cfg.APIKey)
			}
		})
	}
}

func TestVectorSearchToolExecutor_GenerateEmbedding(t *testing.T) {
	t.Run("generates embedding via mock server", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "POST" {
				t.Errorf("expected POST, got %s", r.Method)
			}
			if r.Header.Get("Authorization") != "Bearer test-api-key" {
				t.Errorf("expected Authorization header")
			}

			var reqBody map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
				t.Errorf("failed to decode request: %v", err)
			}

			if reqBody["model"] != "text-embedding-3-small" {
				t.Errorf("expected model text-embedding-3-small, got %v", reqBody["model"])
			}
			if reqBody["input"] != "test query" {
				t.Errorf("expected input 'test query', got %v", reqBody["input"])
			}

			response := map[string]interface{}{
				"data": []map[string]interface{}{
					{"embedding": []float64{0.1, 0.2, 0.3, 0.4, 0.5}},
				},
			}
			json.NewEncoder(w).Encode(response)
		}))
		defer server.Close()

		def := &ToolDefinition{
			Name: "search",
			Config: map[string]interface{}{
				"type":             "vector_search",
				"connectionString": "postgres://localhost/test",
				"tableName":        "documents",
				"embeddingApiKey":  "test-api-key",
				"embeddingBaseUrl": server.URL,
			},
		}

		executor, _ := NewVectorSearchToolExecutor(def, &mockResolver{})
		vsExecutor := executor.(*VectorSearchToolExecutor)

		cfg := vsExecutor.getEmbeddingConfig(def.Config)
		cfg.BaseURL = server.URL

		embedding, err := vsExecutor.generateEmbedding(context.Background(), "test query", cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		expected := []float64{0.1, 0.2, 0.3, 0.4, 0.5}
		if len(embedding) != len(expected) {
			t.Fatalf("expected %d dimensions, got %d", len(expected), len(embedding))
		}
		for i, v := range embedding {
			if v != expected[i] {
				t.Errorf("expected %v at index %d, got %v", expected[i], i, v)
			}
		}
	})
}

func TestCreateVectorSearchToolDefinition(t *testing.T) {
	t.Run("creates definition with correct structure", func(t *testing.T) {
		config := map[string]interface{}{
			"connectionString": "postgres://localhost/test",
			"tableName":        "documents",
			"embeddingApiKey":  "sk-test",
		}

		def := CreateVectorSearchToolDefinition("search_docs", "Search documents", config)

		if def.Name != "search_docs" {
			t.Errorf("expected name 'search_docs', got %s", def.Name)
		}
		if def.Description != "Search documents" {
			t.Errorf("expected description, got %s", def.Description)
		}
		if def.Config["type"] != "vector_search" {
			t.Errorf("expected type 'vector_search', got %v", def.Config["type"])
		}
		if def.Config["connectionString"] != "postgres://localhost/test" {
			t.Errorf("expected connectionString in config")
		}
	})

	t.Run("includes required parameters schema", func(t *testing.T) {
		def := CreateVectorSearchToolDefinition("search", "Search", nil)

		params := def.Parameters
		if params["type"] != "object" {
			t.Errorf("expected type 'object', got %v", params["type"])
		}

		props := params["properties"].(map[string]interface{})
		if _, ok := props["query"]; !ok {
			t.Error("expected query property")
		}
		if _, ok := props["top_k"]; !ok {
			t.Error("expected top_k property")
		}

		required := params["required"].([]string)
		if len(required) != 1 || required[0] != "query" {
			t.Errorf("expected required to be ['query'], got %v", required)
		}
	})
}

func TestParseToolDefinitionsFromConfig(t *testing.T) {
	tests := []struct {
		name        string
		config      map[string]interface{}
		expectedLen int
		expectError bool
		verify      func(t *testing.T, tools []*ToolDefinition)
	}{
		{
			name:        "empty config",
			config:      map[string]interface{}{},
			expectedLen: 0,
			expectError: false,
		},
		{
			name: "single tool",
			config: map[string]interface{}{
				"tools": []interface{}{
					map[string]interface{}{
						"name":        "search",
						"description": "Search documents",
						"type":        "vector_search",
						"tableName":   "docs",
					},
				},
			},
			expectedLen: 1,
			expectError: false,
			verify: func(t *testing.T, tools []*ToolDefinition) {
				if tools[0].Name != "search" {
					t.Errorf("expected name 'search', got %s", tools[0].Name)
				}
				if tools[0].Config["type"] != "vector_search" {
					t.Errorf("expected type in config")
				}
				if tools[0].Config["tableName"] != "docs" {
					t.Errorf("expected tableName in config")
				}
			},
		},
		{
			name: "multiple tools",
			config: map[string]interface{}{
				"tools": []interface{}{
					map[string]interface{}{
						"name": "search1",
						"type": "vector_search",
					},
					map[string]interface{}{
						"name": "search2",
						"type": "vector_search",
					},
				},
			},
			expectedLen: 2,
			expectError: false,
		},
		{
			name: "tool with explicit parameters",
			config: map[string]interface{}{
				"tools": []interface{}{
					map[string]interface{}{
						"name": "custom",
						"parameters": map[string]interface{}{
							"type": "object",
							"properties": map[string]interface{}{
								"custom_param": map[string]interface{}{
									"type": "string",
								},
							},
						},
					},
				},
			},
			expectedLen: 1,
			expectError: false,
			verify: func(t *testing.T, tools []*ToolDefinition) {
				params := tools[0].Parameters
				props := params["properties"].(map[string]interface{})
				if _, ok := props["custom_param"]; !ok {
					t.Error("expected custom_param property")
				}
			},
		},
		{
			name: "tool missing name",
			config: map[string]interface{}{
				"tools": []interface{}{
					map[string]interface{}{
						"type": "vector_search",
					},
				},
			},
			expectError: true,
		},
		{
			name: "invalid tools type",
			config: map[string]interface{}{
				"tools": "not an array",
			},
			expectError: true,
		},
		{
			name: "invalid tool entry type",
			config: map[string]interface{}{
				"tools": []interface{}{
					"not an object",
				},
			},
			expectError: true,
		},
		{
			name: "vector_search auto-populates parameters",
			config: map[string]interface{}{
				"tools": []interface{}{
					map[string]interface{}{
						"name": "search",
						"type": "vector_search",
					},
				},
			},
			expectedLen: 1,
			verify: func(t *testing.T, tools []*ToolDefinition) {
				if tools[0].Parameters == nil {
					t.Fatal("expected parameters to be populated")
				}
				props := tools[0].Parameters["properties"].(map[string]interface{})
				if _, ok := props["query"]; !ok {
					t.Error("expected query property to be auto-populated")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tools, err := ParseToolDefinitionsFromConfig(tt.config, &mockResolver{})

			if tt.expectError {
				if err == nil {
					t.Error("expected error")
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if len(tools) != tt.expectedLen {
				t.Errorf("expected %d tools, got %d", tt.expectedLen, len(tools))
			}

			if tt.verify != nil && len(tools) > 0 {
				tt.verify(t, tools)
			}
		})
	}
}

func TestExecuteToolCalls(t *testing.T) {
	t.Run("handles unknown tool", func(t *testing.T) {
		calls := []*ToolCall{
			{ID: "call_1", Name: "unknown_tool", Arguments: map[string]interface{}{}},
		}

		results, err := ExecuteToolCalls(context.Background(), calls, []*ToolDefinition{}, &mockResolver{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if len(results) != 1 {
			t.Fatalf("expected 1 result, got %d", len(results))
		}
		if results[0].Error == "" {
			t.Error("expected error for unknown tool")
		}
		if results[0].ToolCallID != "call_1" {
			t.Errorf("expected tool_call_id 'call_1', got %s", results[0].ToolCallID)
		}
	})

	t.Run("handles missing type in tool config", func(t *testing.T) {
		calls := []*ToolCall{
			{ID: "call_2", Name: "search", Arguments: map[string]interface{}{}},
		}
		tools := []*ToolDefinition{
			{Name: "search", Config: map[string]interface{}{}},
		}

		results, err := ExecuteToolCalls(context.Background(), calls, tools, &mockResolver{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if len(results) != 1 {
			t.Fatalf("expected 1 result, got %d", len(results))
		}
		if results[0].Error == "" {
			t.Error("expected error for missing type")
		}
	})
}

func TestFormatToolResultsForOpenAI(t *testing.T) {
	tests := []struct {
		name    string
		results []*ToolCallResult
		verify  func(t *testing.T, messages []map[string]interface{})
	}{
		{
			name: "successful result with string content",
			results: []*ToolCallResult{
				{ToolCallID: "call_1", Name: "search", Content: "found 3 results"},
			},
			verify: func(t *testing.T, messages []map[string]interface{}) {
				if len(messages) != 1 {
					t.Fatalf("expected 1 message, got %d", len(messages))
				}
				if messages[0]["role"] != "tool" {
					t.Errorf("expected role 'tool', got %v", messages[0]["role"])
				}
				if messages[0]["tool_call_id"] != "call_1" {
					t.Errorf("expected tool_call_id 'call_1', got %v", messages[0]["tool_call_id"])
				}
				if messages[0]["content"] != "found 3 results" {
					t.Errorf("expected content, got %v", messages[0]["content"])
				}
			},
		},
		{
			name: "successful result with object content",
			results: []*ToolCallResult{
				{ToolCallID: "call_2", Name: "search", Content: map[string]interface{}{"count": 3}},
			},
			verify: func(t *testing.T, messages []map[string]interface{}) {
				content := messages[0]["content"].(string)
				var parsed map[string]interface{}
				if err := json.Unmarshal([]byte(content), &parsed); err != nil {
					t.Fatalf("content should be valid JSON: %v", err)
				}
				if parsed["count"].(float64) != 3 {
					t.Errorf("expected count 3, got %v", parsed["count"])
				}
			},
		},
		{
			name: "error result",
			results: []*ToolCallResult{
				{ToolCallID: "call_3", Name: "search", Error: "connection failed"},
			},
			verify: func(t *testing.T, messages []map[string]interface{}) {
				content := messages[0]["content"].(string)
				if content != "Error: connection failed" {
					t.Errorf("expected error message, got %s", content)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			messages := FormatToolResultsForOpenAI(tt.results)
			tt.verify(t, messages)
		})
	}
}

func TestFormatToolResultsForAnthropic(t *testing.T) {
	t.Run("formats as tool_result blocks", func(t *testing.T) {
		results := []*ToolCallResult{
			{ToolCallID: "toolu_123", Name: "search", Content: "found data"},
		}

		blocks := FormatToolResultsForAnthropic(results)

		if len(blocks) != 1 {
			t.Fatalf("expected 1 block, got %d", len(blocks))
		}
		if blocks[0]["type"] != "tool_result" {
			t.Errorf("expected type 'tool_result', got %v", blocks[0]["type"])
		}
		if blocks[0]["tool_use_id"] != "toolu_123" {
			t.Errorf("expected tool_use_id 'toolu_123', got %v", blocks[0]["tool_use_id"])
		}
		if blocks[0]["content"] != "found data" {
			t.Errorf("expected content, got %v", blocks[0]["content"])
		}
	})

	t.Run("formats error result", func(t *testing.T) {
		results := []*ToolCallResult{
			{ToolCallID: "toolu_456", Name: "search", Error: "failed"},
		}

		blocks := FormatToolResultsForAnthropic(results)

		content := blocks[0]["content"].(string)
		if content != "Error: failed" {
			t.Errorf("expected error message, got %s", content)
		}
	})
}

func TestFormatToolResultsForGemini(t *testing.T) {
	t.Run("formats as function response parts", func(t *testing.T) {
		results := []*ToolCallResult{
			{ToolCallID: "call_1", Name: "search", Content: map[string]interface{}{"data": "found"}},
		}

		parts := FormatToolResultsForGemini(results)

		if len(parts) != 1 {
			t.Fatalf("expected 1 part, got %d", len(parts))
		}

		funcResp := parts[0]["functionResponse"].(map[string]interface{})
		if funcResp["name"] != "search" {
			t.Errorf("expected name 'search', got %v", funcResp["name"])
		}

		response := funcResp["response"].(map[string]interface{})
		if response["result"] == nil {
			t.Error("expected result in response")
		}
	})

	t.Run("formats error result", func(t *testing.T) {
		results := []*ToolCallResult{
			{ToolCallID: "call_2", Name: "search", Error: "failed"},
		}

		parts := FormatToolResultsForGemini(results)

		funcResp := parts[0]["functionResponse"].(map[string]interface{})
		response := funcResp["response"].(map[string]interface{})
		if response["error"] != "failed" {
			t.Errorf("expected error 'failed', got %v", response["error"])
		}
	})
}

func TestVectorSearchToolExecutor_ParseVectorConfigs(t *testing.T) {
	tests := []struct {
		name           string
		config         map[string]interface{}
		expectedLen    int
		expectError    bool
		expectedColumn string
	}{
		{
			name: "single vector config from vectorConfigs",
			config: map[string]interface{}{
				"vectorConfigs": []interface{}{
					map[string]interface{}{
						"column":       "title_embedding",
						"textTemplate": "{{.title}}",
					},
				},
			},
			expectedLen:    1,
			expectError:    false,
			expectedColumn: "title_embedding",
		},
		{
			name: "multiple vector configs",
			config: map[string]interface{}{
				"vectorConfigs": []interface{}{
					map[string]interface{}{
						"column":       "title_embedding",
						"textTemplate": "{{.title}}",
					},
					map[string]interface{}{
						"column":       "content_embedding",
						"textTemplate": "{{.content}}",
					},
				},
			},
			expectedLen: 2,
			expectError: false,
		},
		{
			name:           "fallback to vectorColumn",
			config:         map[string]interface{}{"vectorColumn": "custom_embedding"},
			expectedLen:    1,
			expectError:    false,
			expectedColumn: "custom_embedding",
		},
		{
			name:           "fallback to embeddingColumn",
			config:         map[string]interface{}{"embeddingColumn": "embedding_field"},
			expectedLen:    1,
			expectError:    false,
			expectedColumn: "embedding_field",
		},
		{
			name:           "default to embedding",
			config:         map[string]interface{}{},
			expectedLen:    1,
			expectError:    false,
			expectedColumn: "embedding",
		},
		{
			name: "vector config missing column",
			config: map[string]interface{}{
				"vectorConfigs": []interface{}{
					map[string]interface{}{
						"textTemplate": "{{.title}}",
					},
				},
			},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			def := &ToolDefinition{
				Name:   "search",
				Config: tt.config,
			}

			executor, _ := NewVectorSearchToolExecutor(def, &mockResolver{})
			vsExecutor := executor.(*VectorSearchToolExecutor)

			configs, err := vsExecutor.parseVectorConfigs(tt.config)

			if tt.expectError {
				if err == nil {
					t.Error("expected error")
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if len(configs) != tt.expectedLen {
				t.Errorf("expected %d configs, got %d", tt.expectedLen, len(configs))
			}

			if tt.expectedColumn != "" && len(configs) > 0 {
				if configs[0].Column != tt.expectedColumn {
					t.Errorf("expected column %q, got %q", tt.expectedColumn, configs[0].Column)
				}
			}
		})
	}
}

func TestParseToolCallsFromPromptResponse(t *testing.T) {
	tests := []struct {
		name              string
		response          string
		expectToolCalls   bool
		expectedToolName  string
		expectedRemaining string
	}{
		{
			name:              "no tool call",
			response:          "This is a regular response without any tool calls.",
			expectToolCalls:   false,
			expectedRemaining: "This is a regular response without any tool calls.",
		},
		{
			name:             "tool call inline",
			response:         `{"tool_call": {"name": "search", "arguments": {"query": "test"}}}`,
			expectToolCalls:  true,
			expectedToolName: "search",
		},
		{
			name:             "tool call with surrounding text",
			response:         `I will search for that. {"tool_call": {"name": "search", "arguments": {"query": "test"}}} Here is more text.`,
			expectToolCalls:  true,
			expectedToolName: "search",
		},
		{
			name:             "tool call in code block",
			response:         "Let me search for that.\n```json\n{\"tool_call\": {\"name\": \"search\", \"arguments\": {\"query\": \"test\"}}}\n```",
			expectToolCalls:  true,
			expectedToolName: "search",
		},
		{
			name:              "malformed JSON",
			response:          `{"tool_call": {"name": "search", "arguments": {"query": "test"}`,
			expectToolCalls:   false,
			expectedRemaining: `{"tool_call": {"name": "search", "arguments": {"query": "test"}`,
		},
		{
			name:              "empty tool name",
			response:          `{"tool_call": {"name": "", "arguments": {}}}`,
			expectToolCalls:   false,
			expectedRemaining: `{"tool_call": {"name": "", "arguments": {}}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls, remaining, err := ParseToolCallsFromPromptResponse(tt.response)

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if tt.expectToolCalls {
				if len(calls) == 0 {
					t.Error("expected tool calls to be parsed")
					return
				}
				if calls[0].Name != tt.expectedToolName {
					t.Errorf("expected tool name %q, got %q", tt.expectedToolName, calls[0].Name)
				}
				if calls[0].ID == "" {
					t.Error("expected tool call to have an ID")
				}
			} else {
				if len(calls) > 0 {
					t.Error("expected no tool calls")
				}
				if remaining != tt.expectedRemaining {
					t.Errorf("expected remaining %q, got %q", tt.expectedRemaining, remaining)
				}
			}
		})
	}
}
