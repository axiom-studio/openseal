package executor

import (
	"context"
	"testing"

	"go.uber.org/zap"
)

func TestSimpleResolver_ResolveString(t *testing.T) {
	resolver := &simpleResolver{
		input: map[string]interface{}{
			"trigger": map[string]interface{}{
				"message": "Hello World",
				"user": map[string]interface{}{
					"name": "John",
				},
			},
			"bindings": map[string]interface{}{
				"apiUrl": "https://api.example.com",
			},
		},
		nodeOutputs: map[string]interface{}{
			"httpCall": map[string]interface{}{
				"statusCode": 200,
				"body":       "success",
			},
		},
		variables: map[string]interface{}{
			"counter": 5,
		},
	}

	tests := []struct {
		name     string
		template string
		expected string
	}{
		{
			name:     "simple trigger value",
			template: "{{trigger.message}}",
			expected: "Hello World",
		},
		{
			name:     "nested trigger value",
			template: "{{trigger.user.name}}",
			expected: "John",
		},
		{
			name:     "binding value",
			template: "{{bindings.apiUrl}}",
			expected: "https://api.example.com",
		},
		{
			name:     "node output",
			template: "{{nodes.httpCall.statusCode}}",
			expected: "200",
		},
		{
			name:     "variable",
			template: "{{var.counter}}",
			expected: "5",
		},
		{
			name:     "mixed template",
			template: "Hello {{trigger.user.name}}, your API is {{bindings.apiUrl}}",
			expected: "Hello John, your API is https://api.example.com",
		},
		{
			name:     "no template",
			template: "Just plain text",
			expected: "Just plain text",
		},
		{
			name:     "undefined value",
			template: "{{trigger.undefined}}",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := resolver.ResolveString(tt.template)
			if result != tt.expected {
				t.Errorf("ResolveString(%q) = %q, want %q", tt.template, result, tt.expected)
			}
		})
	}
}

func TestSimpleResolver_EvaluateCondition(t *testing.T) {
	resolver := &simpleResolver{
		input: map[string]interface{}{
			"trigger": map[string]interface{}{
				"status": "active",
				"count":  "10",
			},
		},
		nodeOutputs: make(map[string]interface{}),
		variables:   make(map[string]interface{}),
	}

	tests := []struct {
		name      string
		condition string
		expected  bool
	}{
		{
			name:      "equality true",
			condition: "{{trigger.status}} == active",
			expected:  true,
		},
		{
			name:      "equality false",
			condition: "{{trigger.status}} == inactive",
			expected:  false,
		},
		{
			name:      "inequality true",
			condition: "{{trigger.status}} != inactive",
			expected:  true,
		},
		{
			name:      "truthy non-empty",
			condition: "{{trigger.status}}",
			expected:  true,
		},
		{
			name:      "truthy empty",
			condition: "{{trigger.undefined}}",
			expected:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := resolver.EvaluateCondition(tt.condition)
			if result != tt.expected {
				t.Errorf("EvaluateCondition(%q) = %v, want %v", tt.condition, result, tt.expected)
			}
		})
	}
}

func TestPipelineExecutor_Execute(t *testing.T) {
	registry := NewRegistry(nil) // Tests don't need K8s cluster management

	logger, _ := zap.NewDevelopment()
	pe := NewPipelineExecutor(registry, logger.Sugar())
	if pe == nil {
		t.Fatal("NewPipelineExecutor returned nil")
	}

	// Create a simple pipeline with a transform node
	nodes := []*NodeDefinition{
		{
			Id:   "trigger1",
			Name: "webhook",
			Type: NodeTypeManual,
		},
		{
			Id:   "transform1",
			Name: "transform",
			Type: NodeTypeTransform,
			Config: map[string]interface{}{
				"template": map[string]interface{}{
					"result": "{{trigger.message}}",
				},
			},
		},
	}

	connections := []*ConnectionDefinition{
		{
			Id:           "conn1",
			SourceNodeId: "trigger1",
			TargetNodeId: "transform1",
		},
	}

	triggerData := map[string]interface{}{
		"message": "test message",
	}

	bindings := map[string]interface{}{}

	result, err := pe.Execute(context.Background(), 1, nodes, connections, "trigger1", triggerData, bindings)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	if result.Status != "completed" {
		t.Errorf("Expected status 'completed', got %q", result.Status)
	}

	if len(result.NodeResults) != 2 {
		t.Errorf("Expected 2 node results, got %d", len(result.NodeResults))
	}
}

func TestSimpleResolver_NodeOutputs(t *testing.T) {
	// Test that node outputs can be resolved by node name, including names with dashes
	resolver := &simpleResolver{
		input: map[string]interface{}{
			"trigger": map[string]interface{}{
				"data": map[string]interface{}{
					"example": "trigger data",
				},
			},
			"prev": map[string]interface{}{
				"response": "previous node output",
				"status":   200,
			},
		},
		nodeOutputs: map[string]interface{}{
			"Manual-1": map[string]interface{}{
				"example": "manual trigger data",
			},
			"HTTP Request": map[string]interface{}{
				"body":       `{"result": "success"}`,
				"statusCode": 200,
			},
			"Transform Data": map[string]interface{}{
				"processed": true,
				"items":     []interface{}{"a", "b", "c"},
			},
		},
		variables: map[string]interface{}{},
	}

	tests := []struct {
		name     string
		template string
		expected string
	}{
		{
			name:     "node with dash in name",
			template: "{{nodes.Manual-1.example}}",
			expected: "manual trigger data",
		},
		{
			name:     "node with space in name",
			template: "{{nodes.HTTP Request.body}}",
			expected: `{"result": "success"}`,
		},
		{
			name:     "node with numeric field",
			template: "{{nodes.HTTP Request.statusCode}}",
			expected: "200",
		},
		{
			name:     "previous node output",
			template: "{{prev.response}}",
			expected: "previous node output",
		},
		{
			name:     "previous node numeric field",
			template: "{{prev.status}}",
			expected: "200",
		},
		{
			name:     "mixed template with nodes",
			template: "Result: {{nodes.Manual-1.example}} and {{prev.response}}",
			expected: "Result: manual trigger data and previous node output",
		},
		{
			name:     "trigger data access",
			template: "{{trigger.data.example}}",
			expected: "trigger data",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := resolver.ResolveString(tt.template)
			if result != tt.expected {
				t.Errorf("ResolveString(%q) = %q, want %q", tt.template, result, tt.expected)
			}
		})
	}
}

func TestPipelineExecutor_NodeOutputResolution(t *testing.T) {
	// Test that downstream nodes can access upstream node outputs via templates
	resolver := &simpleResolver{
		input: map[string]interface{}{
			"trigger": map[string]interface{}{
				"data": map[string]interface{}{
					"message": "Hello from trigger",
				},
			},
			"prev": map[string]interface{}{
				"response": "Previous node response",
			},
		},
		nodeOutputs: map[string]interface{}{
			"Manual-1": map[string]interface{}{
				"example": "Data from Manual-1",
			},
		},
	}

	testCases := []struct {
		template string
		expected string
	}{
		// Standard templates
		{"{{nodes.Manual-1.example}}", "Data from Manual-1"},
		{"{{prev.response}}", "Previous node response"},
		{"{{trigger.data.message}}", "Hello from trigger"},
		{"Combine: {{nodes.Manual-1.example}} and {{prev.response}}", "Combine: Data from Manual-1 and Previous node response"},
		// Templates with whitespace (common user input)
		{"{{ nodes.Manual-1.example }}", "Data from Manual-1"},
		{"{{ prev.response }}", "Previous node response"},
		{"{{  nodes.Manual-1.example  }}", "Data from Manual-1"},
		{"{{ trigger.data.message }}", "Hello from trigger"},
	}

	for _, tc := range testCases {
		result := resolver.ResolveString(tc.template)
		if result != tc.expected {
			t.Errorf("ResolveString(%q) = %q, want %q", tc.template, result, tc.expected)
		}
	}
}

func TestBuildGraph(t *testing.T) {
	nodes := []*NodeDefinition{
		{Id: "node1", Type: NodeTypeWebhook},
		{Id: "node2", Type: NodeTypeTransform},
		{Id: "node3", Type: NodeTypeHTTP},
	}

	connections := []*ConnectionDefinition{
		{SourceNodeId: "node1", TargetNodeId: "node2"},
		{SourceNodeId: "node2", TargetNodeId: "node3"},
	}

	graph, err := BuildGraph(nodes, connections)
	if err != nil {
		t.Fatalf("BuildGraph failed: %v", err)
	}

	if len(graph.Nodes) != 3 {
		t.Errorf("Expected 3 nodes, got %d", len(graph.Nodes))
	}

	if len(graph.StartNodes) != 1 {
		t.Errorf("Expected 1 start node, got %d", len(graph.StartNodes))
	}

	if graph.StartNodes[0] != "node1" {
		t.Errorf("Expected start node 'node1', got %q", graph.StartNodes[0])
	}

	// Check connections
	node1 := graph.Nodes["node1"]
	if len(node1.OutgoingEdges) != 1 {
		t.Errorf("Expected 1 outgoing edge from node1, got %d", len(node1.OutgoingEdges))
	}

	node2 := graph.Nodes["node2"]
	if len(node2.IncomingEdges) != 1 {
		t.Errorf("Expected 1 incoming edge to node2, got %d", len(node2.IncomingEdges))
	}
}
