package executor

import (
	"context"
	"encoding/json"
	"testing"
)

func TestToolDefinition_Serialization(t *testing.T) {
	tests := []struct {
		name   string
		tool   *ToolDefinition
		verify func(t *testing.T, data []byte)
	}{
		{
			name: "basic tool definition",
			tool: &ToolDefinition{
				Name:        "search_documents",
				Description: "Search documents by query",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"query": map[string]interface{}{
							"type":        "string",
							"description": "The search query",
						},
					},
					"required": []interface{}{"query"},
				},
				Config: map[string]interface{}{
					"type": "vector_search",
				},
			},
			verify: func(t *testing.T, data []byte) {
				var parsed map[string]interface{}
				if err := json.Unmarshal(data, &parsed); err != nil {
					t.Fatalf("failed to unmarshal: %v", err)
				}
				if parsed["name"] != "search_documents" {
					t.Errorf("expected name 'search_documents', got %v", parsed["name"])
				}
				if parsed["description"] != "Search documents by query" {
					t.Errorf("expected description, got %v", parsed["description"])
				}
			},
		},
		{
			name: "tool with complex parameters",
			tool: &ToolDefinition{
				Name:        "complex_tool",
				Description: "A tool with nested parameters",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"filters": map[string]interface{}{
							"type": "object",
							"properties": map[string]interface{}{
								"category": map[string]interface{}{"type": "string"},
								"limit":    map[string]interface{}{"type": "integer"},
							},
						},
					},
				},
			},
			verify: func(t *testing.T, data []byte) {
				var parsed map[string]interface{}
				if err := json.Unmarshal(data, &parsed); err != nil {
					t.Fatalf("failed to unmarshal: %v", err)
				}
				params := parsed["parameters"].(map[string]interface{})
				props := params["properties"].(map[string]interface{})
				if _, ok := props["filters"]; !ok {
					t.Error("expected filters property")
				}
			},
		},
		{
			name: "minimal tool",
			tool: &ToolDefinition{
				Name:        "minimal",
				Description: "Minimal tool",
			},
			verify: func(t *testing.T, data []byte) {
				var parsed map[string]interface{}
				if err := json.Unmarshal(data, &parsed); err != nil {
					t.Fatalf("failed to unmarshal: %v", err)
				}
				if parsed["parameters"] != nil {
					t.Errorf("expected nil parameters, got %v", parsed["parameters"])
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := json.Marshal(tt.tool)
			if err != nil {
				t.Fatalf("failed to marshal: %v", err)
			}
			tt.verify(t, data)
		})
	}
}

func TestToolRegistry_Creation(t *testing.T) {
	t.Run("creates new registry with default tools", func(t *testing.T) {
		registry := NewToolRegistry()
		if registry == nil {
			t.Fatal("expected non-nil registry")
		}
		if registry.factories == nil {
			t.Error("expected factories map to be initialized")
		}
	})

	t.Run("has vector_search registered by default", func(t *testing.T) {
		registry := NewToolRegistry()
		if !registry.HasToolType("vector_search") {
			t.Error("expected vector_search to be registered")
		}
	})
}

func TestToolRegistry_Lookup(t *testing.T) {
	t.Run("lookup registered tool type", func(t *testing.T) {
		registry := NewToolRegistry()
		if !registry.HasToolType("vector_search") {
			t.Error("expected vector_search to be found")
		}
	})

	t.Run("lookup unregistered tool type", func(t *testing.T) {
		registry := NewToolRegistry()
		if registry.HasToolType("nonexistent_tool") {
			t.Error("expected nonexistent_tool to not be found")
		}
	})
}

func TestToolRegistry_Register(t *testing.T) {
	t.Run("register custom tool factory", func(t *testing.T) {
		registry := NewToolRegistry()

		customFactory := func(def *ToolDefinition, resolver TemplateResolver) (ToolExecutor, error) {
			return &mockToolExecutor{name: def.Name}, nil
		}

		registry.Register("custom_tool", customFactory)

		if !registry.HasToolType("custom_tool") {
			t.Error("expected custom_tool to be registered")
		}
	})

	t.Run("overwrite existing tool factory", func(t *testing.T) {
		registry := NewToolRegistry()

		newFactory := func(def *ToolDefinition, resolver TemplateResolver) (ToolExecutor, error) {
			return &mockToolExecutor{name: "overwritten"}, nil
		}

		registry.Register("vector_search", newFactory)

		def := &ToolDefinition{
			Name: "test",
			Config: map[string]interface{}{
				"type": "vector_search",
			},
		}

		executor, err := registry.CreateExecutor(def, &mockResolver{})
		if err != nil {
			t.Fatalf("failed to create executor: %v", err)
		}

		mock := executor.(*mockToolExecutor)
		if mock.name != "overwritten" {
			t.Errorf("expected overwritten factory, got %s", mock.name)
		}
	})
}

func TestToolRegistry_CreateExecutor(t *testing.T) {
	t.Run("create executor for registered type", func(t *testing.T) {
		registry := NewToolRegistry()

		def := &ToolDefinition{
			Name: "search",
			Config: map[string]interface{}{
				"type": "vector_search",
			},
		}

		executor, err := registry.CreateExecutor(def, &mockResolver{})
		if err != nil {
			t.Fatalf("failed to create executor: %v", err)
		}
		if executor == nil {
			t.Error("expected non-nil executor")
		}
	})

	t.Run("error for missing type in config", func(t *testing.T) {
		registry := NewToolRegistry()

		def := &ToolDefinition{
			Name:   "search",
			Config: map[string]interface{}{},
		}

		_, err := registry.CreateExecutor(def, &mockResolver{})
		if err == nil {
			t.Error("expected error for missing type")
		}
		if err.Error() != "tool definition requires 'type' in config" {
			t.Errorf("unexpected error message: %v", err)
		}
	})

	t.Run("error for unregistered type", func(t *testing.T) {
		registry := NewToolRegistry()

		def := &ToolDefinition{
			Name: "unknown",
			Config: map[string]interface{}{
				"type": "unknown_type",
			},
		}

		_, err := registry.CreateExecutor(def, &mockResolver{})
		if err == nil {
			t.Error("expected error for unregistered type")
		}
	})
}

func TestToolResult_ErrorHandling(t *testing.T) {
	tests := []struct {
		name     string
		result   *ToolResult
		hasError bool
	}{
		{
			name: "successful result",
			result: &ToolResult{
				Name:   "search",
				Result: map[string]interface{}{"data": "found"},
				Error:  "",
			},
			hasError: false,
		},
		{
			name: "result with error",
			result: &ToolResult{
				Name:   "search",
				Result: nil,
				Error:  "connection failed",
			},
			hasError: true,
		},
		{
			name: "result with both data and error",
			result: &ToolResult{
				Name:   "partial",
				Result: map[string]interface{}{"partial": true},
				Error:  "partial failure",
			},
			hasError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hasErr := tt.result.Error != ""
			if hasErr != tt.hasError {
				t.Errorf("expected hasError=%v, got %v", tt.hasError, hasErr)
			}
		})
	}
}

func TestToolResult_Serialization(t *testing.T) {
	t.Run("serialize successful result", func(t *testing.T) {
		result := &ToolResult{
			Name: "search",
			Result: map[string]interface{}{
				"documents": []interface{}{
					map[string]interface{}{"id": 1, "text": "doc1"},
					map[string]interface{}{"id": 2, "text": "doc2"},
				},
			},
		}

		data, err := json.Marshal(result)
		if err != nil {
			t.Fatalf("failed to marshal: %v", err)
		}

		var parsed ToolResult
		if err := json.Unmarshal(data, &parsed); err != nil {
			t.Fatalf("failed to unmarshal: %v", err)
		}

		if parsed.Name != "search" {
			t.Errorf("expected name 'search', got %s", parsed.Name)
		}
	})

	t.Run("serialize error result", func(t *testing.T) {
		result := &ToolResult{
			Name:  "failed_tool",
			Error: "database connection timeout",
		}

		data, err := json.Marshal(result)
		if err != nil {
			t.Fatalf("failed to marshal: %v", err)
		}

		var parsed map[string]interface{}
		if err := json.Unmarshal(data, &parsed); err != nil {
			t.Fatalf("failed to unmarshal: %v", err)
		}

		if parsed["error"] != "database connection timeout" {
			t.Errorf("expected error message, got %v", parsed["error"])
		}
	})

	t.Run("omit empty error", func(t *testing.T) {
		result := &ToolResult{
			Name:   "success",
			Result: "data",
			Error:  "",
		}

		data, err := json.Marshal(result)
		if err != nil {
			t.Fatalf("failed to marshal: %v", err)
		}

		var parsed map[string]interface{}
		if err := json.Unmarshal(data, &parsed); err != nil {
			t.Fatalf("failed to unmarshal: %v", err)
		}

		if _, exists := parsed["error"]; exists {
			t.Error("expected error field to be omitted when empty")
		}
	})
}

func TestToolCall_Serialization(t *testing.T) {
	tests := []struct {
		name     string
		call     *ToolCall
		expected map[string]interface{}
	}{
		{
			name: "basic tool call",
			call: &ToolCall{
				ID:   "call_123",
				Name: "search",
				Arguments: map[string]interface{}{
					"query": "test query",
				},
			},
			expected: map[string]interface{}{
				"id":   "call_123",
				"name": "search",
			},
		},
		{
			name: "tool call with complex arguments",
			call: &ToolCall{
				ID:   "call_456",
				Name: "filter",
				Arguments: map[string]interface{}{
					"filters": map[string]interface{}{
						"category": "tech",
						"limit":    10,
					},
				},
			},
			expected: map[string]interface{}{
				"id":   "call_456",
				"name": "filter",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := json.Marshal(tt.call)
			if err != nil {
				t.Fatalf("failed to marshal: %v", err)
			}

			var parsed map[string]interface{}
			if err := json.Unmarshal(data, &parsed); err != nil {
				t.Fatalf("failed to unmarshal: %v", err)
			}

			if parsed["id"] != tt.expected["id"] {
				t.Errorf("expected id %v, got %v", tt.expected["id"], parsed["id"])
			}
			if parsed["name"] != tt.expected["name"] {
				t.Errorf("expected name %v, got %v", tt.expected["name"], parsed["name"])
			}
		})
	}
}

func TestToolCallResult_Serialization(t *testing.T) {
	t.Run("successful tool call result", func(t *testing.T) {
		result := &ToolCallResult{
			ToolCallID: "call_123",
			Name:       "search",
			Content:    map[string]interface{}{"results": []string{"doc1", "doc2"}},
		}

		data, err := json.Marshal(result)
		if err != nil {
			t.Fatalf("failed to marshal: %v", err)
		}

		var parsed map[string]interface{}
		if err := json.Unmarshal(data, &parsed); err != nil {
			t.Fatalf("failed to unmarshal: %v", err)
		}

		if parsed["tool_call_id"] != "call_123" {
			t.Errorf("expected tool_call_id 'call_123', got %v", parsed["tool_call_id"])
		}
	})

	t.Run("failed tool call result", func(t *testing.T) {
		result := &ToolCallResult{
			ToolCallID: "call_456",
			Name:       "search",
			Error:      "search index unavailable",
		}

		data, err := json.Marshal(result)
		if err != nil {
			t.Fatalf("failed to marshal: %v", err)
		}

		var parsed map[string]interface{}
		if err := json.Unmarshal(data, &parsed); err != nil {
			t.Fatalf("failed to unmarshal: %v", err)
		}

		if parsed["error"] != "search index unavailable" {
			t.Errorf("expected error message, got %v", parsed["error"])
		}
	})
}

func TestGlobalToolRegistry(t *testing.T) {
	t.Run("returns same instance", func(t *testing.T) {
		r1 := GetGlobalToolRegistry()
		r2 := GetGlobalToolRegistry()

		if r1 != r2 {
			t.Error("expected same registry instance")
		}
	})

	t.Run("global registry has default tools", func(t *testing.T) {
		registry := GetGlobalToolRegistry()
		if !registry.HasToolType("vector_search") {
			t.Error("expected global registry to have vector_search")
		}
	})
}

func TestToolRegistry_Concurrency(t *testing.T) {
	registry := NewToolRegistry()
	done := make(chan bool, 100)

	// Concurrent reads
	for i := 0; i < 50; i++ {
		go func() {
			_ = registry.HasToolType("vector_search")
			done <- true
		}()
	}

	// Concurrent writes
	for i := 0; i < 50; i++ {
		go func(idx int) {
			factory := func(def *ToolDefinition, resolver TemplateResolver) (ToolExecutor, error) {
				return &mockToolExecutor{}, nil
			}
			registry.Register("concurrent_"+string(rune('a'+idx)), factory)
			done <- true
		}(i % 26)
	}

	// Wait for all goroutines
	for i := 0; i < 100; i++ {
		<-done
	}
}

// mockToolExecutor implements ToolExecutor for testing
type mockToolExecutor struct {
	name        string
	executeFunc func(ctx context.Context, args map[string]interface{}) (*ToolResult, error)
}

func (m *mockToolExecutor) Execute(ctx context.Context, args map[string]interface{}) (*ToolResult, error) {
	if m.executeFunc != nil {
		return m.executeFunc(ctx, args)
	}
	return &ToolResult{
		Name:   m.name,
		Result: map[string]interface{}{"mock": true},
	}, nil
}

func TestGetConnectedTools(t *testing.T) {
	tests := []struct {
		name           string
		nodes          []*NodeDefinition
		connections    []*ConnectionDefinition
		targetNodeID   string
		expectedTools  int
		expectedTypes  []string
	}{
		{
			name: "AI node with one tool connected",
			nodes: []*NodeDefinition{
				{Id: "trigger-1", Name: "Trigger", Type: "webhook"},
				{Id: "ai-1", Name: "AI", Type: "ai"},
				{Id: "tool-1", Name: "VectorSearch", Type: "tool_pgvector", Config: map[string]interface{}{
					"toolName":        "search",
					"toolDescription": "Search documents",
				}},
			},
			connections: []*ConnectionDefinition{
				{Id: "conn-1", SourceNodeId: "trigger-1", TargetNodeId: "ai-1"},
				{Id: "conn-2", SourceNodeId: "tool-1", TargetNodeId: "ai-1", TargetHandle: "tools-target"},
			},
			targetNodeID:  "ai-1",
			expectedTools: 1,
			expectedTypes: []string{"tool_pgvector"},
		},
		{
			name: "AI node with multiple tools connected",
			nodes: []*NodeDefinition{
				{Id: "ai-1", Name: "AI", Type: "ai"},
				{Id: "tool-1", Name: "VectorSearch1", Type: "tool_pgvector"},
				{Id: "tool-2", Name: "VectorSearch2", Type: "tool_pgvector"},
			},
			connections: []*ConnectionDefinition{
				{Id: "conn-1", SourceNodeId: "tool-1", TargetNodeId: "ai-1", TargetHandle: "tools-target"},
				{Id: "conn-2", SourceNodeId: "tool-2", TargetNodeId: "ai-1", TargetHandle: "tools-target"},
			},
			targetNodeID:  "ai-1",
			expectedTools: 2,
			expectedTypes: []string{"tool_pgvector", "tool_pgvector"},
		},
		{
			name: "AI node with no tools connected",
			nodes: []*NodeDefinition{
				{Id: "trigger-1", Name: "Trigger", Type: "webhook"},
				{Id: "ai-1", Name: "AI", Type: "ai"},
			},
			connections: []*ConnectionDefinition{
				{Id: "conn-1", SourceNodeId: "trigger-1", TargetNodeId: "ai-1"},
			},
			targetNodeID:  "ai-1",
			expectedTools: 0,
			expectedTypes: nil,
		},
		{
			name: "Connection without tools-target handle is not a tool",
			nodes: []*NodeDefinition{
				{Id: "trigger-1", Name: "Trigger", Type: "webhook"},
				{Id: "ai-1", Name: "AI", Type: "ai"},
				{Id: "tool-1", Name: "VectorSearch", Type: "tool_pgvector"},
			},
			connections: []*ConnectionDefinition{
				{Id: "conn-1", SourceNodeId: "trigger-1", TargetNodeId: "ai-1"},
				{Id: "conn-2", SourceNodeId: "tool-1", TargetNodeId: "ai-1"}, // No TargetHandle
			},
			targetNodeID:  "ai-1",
			expectedTools: 0,
			expectedTypes: nil,
		},
		{
			name: "Non-tool node connected to tools-target is ignored",
			nodes: []*NodeDefinition{
				{Id: "ai-1", Name: "AI", Type: "ai"},
				{Id: "http-1", Name: "HTTP", Type: "http"}, // Not a tool_ prefix
			},
			connections: []*ConnectionDefinition{
				{Id: "conn-1", SourceNodeId: "http-1", TargetNodeId: "ai-1", TargetHandle: "tools-target"},
			},
			targetNodeID:  "ai-1",
			expectedTools: 0,
			expectedTypes: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			graph, err := BuildGraph(tt.nodes, tt.connections)
			if err != nil {
				t.Fatalf("failed to build graph: %v", err)
			}

			tools := graph.GetConnectedTools(tt.targetNodeID)

			if len(tools) != tt.expectedTools {
				t.Errorf("expected %d tools, got %d", tt.expectedTools, len(tools))
			}

			for i, tool := range tools {
				if i < len(tt.expectedTypes) && tool.Type != tt.expectedTypes[i] {
					t.Errorf("expected tool type %s at index %d, got %s", tt.expectedTypes[i], i, tool.Type)
				}
			}
		})
	}
}

func TestBuildToolDefinitionFromNode(t *testing.T) {
	executor := NewAIExecutor()

	tests := []struct {
		name         string
		node         *NodeDefinition
		expectNil    bool
		expectedName string
		expectedType string
	}{
		{
			name: "pgvector tool node",
			node: &NodeDefinition{
				Id:   "tool-1",
				Name: "VectorSearch",
				Type: "tool_pgvector",
				Config: map[string]interface{}{
					"toolName":         "doc_search",
					"toolDescription":  "Search for documents",
					"connectionString": "postgres://...",
					"tableName":        "documents",
				},
			},
			expectNil:    false,
			expectedName: "doc_search",
			expectedType: "vector_search",
		},
		{
			name: "tool node without toolName defaults to 'tool'",
			node: &NodeDefinition{
				Id:   "tool-1",
				Name: "VectorSearch",
				Type: "tool_pgvector",
				Config: map[string]interface{}{
					"connectionString": "postgres://...",
					"tableName":        "documents",
				},
			},
			expectNil:    false,
			expectedName: "tool",
			expectedType: "vector_search",
		},
		{
			name:      "nil node returns nil",
			node:      nil,
			expectNil: true,
		},
		{
			name: "node with nil config returns nil",
			node: &NodeDefinition{
				Id:     "tool-1",
				Name:   "VectorSearch",
				Type:   "tool_pgvector",
				Config: nil,
			},
			expectNil: true,
		},
		{
			name: "unknown tool type uses generic schema",
			node: &NodeDefinition{
				Id:   "tool-1",
				Name: "CustomTool",
				Type: "tool_custom",
				Config: map[string]interface{}{
					"toolName": "my_tool",
				},
			},
			expectNil:    false,
			expectedName: "my_tool",
			expectedType: "custom",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := executor.buildToolDefinitionFromNode(tt.node, &mockResolver{})

			if tt.expectNil {
				if result != nil {
					t.Errorf("expected nil result, got %+v", result)
				}
				return
			}

			if result == nil {
				t.Fatal("expected non-nil result")
			}

			if result.Name != tt.expectedName {
				t.Errorf("expected name %q, got %q", tt.expectedName, result.Name)
			}

			if result.Config == nil {
				t.Fatal("expected non-nil config")
			}

			toolType, _ := result.Config["type"].(string)
			if toolType != tt.expectedType {
				t.Errorf("expected type %q, got %q", tt.expectedType, toolType)
			}
		})
	}
}

func TestGraphProviderInterface(t *testing.T) {
	t.Run("simpleResolver implements GraphProvider", func(t *testing.T) {
		nodes := []*NodeDefinition{
			{Id: "ai-1", Name: "AI", Type: "ai"},
		}
		connections := []*ConnectionDefinition{}

		graph, err := BuildGraph(nodes, connections)
		if err != nil {
			t.Fatalf("failed to build graph: %v", err)
		}

		resolver := &simpleResolver{
			input:       make(map[string]interface{}),
			nodeOutputs: make(map[string]interface{}),
			variables:   make(map[string]interface{}),
			runMetadata: make(map[string]interface{}),
			graph:       graph,
		}

		var gp GraphProvider = resolver
		retrievedGraph := gp.GetGraph()

		if retrievedGraph != graph {
			t.Error("expected GetGraph to return the same graph instance")
		}
	})

	t.Run("nil graph is handled correctly", func(t *testing.T) {
		resolver := &simpleResolver{
			input:       make(map[string]interface{}),
			nodeOutputs: make(map[string]interface{}),
			variables:   make(map[string]interface{}),
			runMetadata: make(map[string]interface{}),
			graph:       nil,
		}

		var gp GraphProvider = resolver
		retrievedGraph := gp.GetGraph()

		if retrievedGraph != nil {
			t.Error("expected GetGraph to return nil")
		}
	})
}
