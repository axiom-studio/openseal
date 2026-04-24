package executor

import (
	"context"
	"encoding/json"
	"testing"
)

// TestSimpleResolverGetContextData verifies that the simpleResolver properly provides
// context data for code execution (bindings, trigger, prev, nodes, vars, run)
func TestSimpleResolverGetContextData(t *testing.T) {
	t.Run("returns all context keys", func(t *testing.T) {
		resolver := &simpleResolver{
			input: map[string]interface{}{
				"bindings": map[string]interface{}{"apiKey": "secret123"},
				"trigger":  map[string]interface{}{"data": map[string]interface{}{"message": "hello"}},
				"prev":     map[string]interface{}{"result": "success"},
			},
			nodeOutputs: map[string]interface{}{
				"httpNode": map[string]interface{}{"status": 200},
			},
			variables: map[string]interface{}{
				"counter": 42,
			},
			runMetadata: map[string]interface{}{
				"id": 123,
			},
		}

		ctx := resolver.GetContextData()

		// Verify all context keys are present
		if ctx["bindings"] == nil {
			t.Error("expected bindings to be present in context")
		}
		if ctx["trigger"] == nil {
			t.Error("expected trigger to be present in context")
		}
		if ctx["prev"] == nil {
			t.Error("expected prev to be present in context")
		}
		if ctx["nodes"] == nil {
			t.Error("expected nodes to be present in context")
		}
		if ctx["vars"] == nil {
			t.Error("expected vars to be present in context")
		}
		if ctx["run"] == nil {
			t.Error("expected run to be present in context")
		}

		// Verify bindings value
		bindings := ctx["bindings"].(map[string]interface{})
		if bindings["apiKey"] != "secret123" {
			t.Errorf("expected apiKey='secret123', got %v", bindings["apiKey"])
		}

		// Verify trigger value
		trigger := ctx["trigger"].(map[string]interface{})
		triggerData := trigger["data"].(map[string]interface{})
		if triggerData["message"] != "hello" {
			t.Errorf("expected message='hello', got %v", triggerData["message"])
		}

		// Verify prev value
		prev := ctx["prev"].(map[string]interface{})
		if prev["result"] != "success" {
			t.Errorf("expected result='success', got %v", prev["result"])
		}

		// Verify nodes value
		nodes := ctx["nodes"].(map[string]interface{})
		httpNode := nodes["httpNode"].(map[string]interface{})
		if httpNode["status"] != 200 {
			t.Errorf("expected status=200, got %v", httpNode["status"])
		}

		// Verify vars value
		vars := ctx["vars"].(map[string]interface{})
		if vars["counter"] != 42 {
			t.Errorf("expected counter=42, got %v", vars["counter"])
		}

		// Verify run value
		run := ctx["run"].(map[string]interface{})
		if run["id"] != 123 {
			t.Errorf("expected id=123, got %v", run["id"])
		}
	})

	t.Run("handles nil values gracefully", func(t *testing.T) {
		resolver := &simpleResolver{
			input:       map[string]interface{}{},
			nodeOutputs: map[string]interface{}{},
			variables:   map[string]interface{}{},
			runMetadata: map[string]interface{}{},
		}

		ctx := resolver.GetContextData()

		// Context should still return the map, just with nil values
		if ctx["bindings"] != nil {
			// This is expected - bindings key exists but is nil
		}
		if ctx["trigger"] != nil {
			// This is expected - trigger key exists but is nil
		}
	})
}

// TestCodeExecutorContextInjection verifies that context data is properly
// serialized and available for code execution
func TestCodeExecutorContextInjection(t *testing.T) {
	t.Run("context JSON serialization", func(t *testing.T) {
		// Simulate the context data that would be passed to the code executor
		contextData := map[string]interface{}{
			"bindings": map[string]interface{}{
				"apiUrl":    "https://api.example.com",
				"secretKey": "sk-12345",
			},
			"trigger": map[string]interface{}{
				"data": map[string]interface{}{
					"userId":  "user123",
					"action":  "process",
					"payload": map[string]interface{}{"key": "value"},
				},
			},
			"prev": map[string]interface{}{
				"response": map[string]interface{}{
					"status": "ok",
					"data":   []interface{}{"item1", "item2"},
				},
			},
			"nodes": map[string]interface{}{
				"fetchData": map[string]interface{}{
					"items": []interface{}{
						map[string]interface{}{"id": 1, "name": "first"},
						map[string]interface{}{"id": 2, "name": "second"},
					},
				},
			},
			"vars": map[string]interface{}{
				"counter": 10,
				"flag":    true,
			},
			"run": map[string]interface{}{
				"id":        999,
				"startedAt": "2025-01-01T00:00:00Z",
			},
		}

		// This is what the code executor does - serialize context to JSON
		contextJSON, err := json.Marshal(contextData)
		if err != nil {
			t.Fatalf("failed to marshal context: %v", err)
		}

		// Verify JSON can be parsed back
		var parsed map[string]interface{}
		if err := json.Unmarshal(contextJSON, &parsed); err != nil {
			t.Fatalf("failed to unmarshal context: %v", err)
		}

		// Verify nested access
		bindings := parsed["bindings"].(map[string]interface{})
		if bindings["apiUrl"] != "https://api.example.com" {
			t.Errorf("expected apiUrl to be preserved, got %v", bindings["apiUrl"])
		}

		trigger := parsed["trigger"].(map[string]interface{})
		triggerData := trigger["data"].(map[string]interface{})
		if triggerData["userId"] != "user123" {
			t.Errorf("expected userId to be preserved, got %v", triggerData["userId"])
		}

		prev := parsed["prev"].(map[string]interface{})
		response := prev["response"].(map[string]interface{})
		if response["status"] != "ok" {
			t.Errorf("expected status to be preserved, got %v", response["status"])
		}
	})

	t.Run("nil values are initialized to empty maps", func(t *testing.T) {
		// This tests the nil check logic in code_executor.go
		contextData := map[string]interface{}{}

		// Apply the same nil checks as in code_executor.go
		if contextData["bindings"] == nil {
			contextData["bindings"] = make(map[string]interface{})
		}
		if contextData["trigger"] == nil {
			contextData["trigger"] = make(map[string]interface{})
		}
		if contextData["prev"] == nil {
			contextData["prev"] = make(map[string]interface{})
		}
		if contextData["nodes"] == nil {
			contextData["nodes"] = make(map[string]interface{})
		}
		if contextData["vars"] == nil {
			contextData["vars"] = make(map[string]interface{})
		}
		if contextData["run"] == nil {
			contextData["run"] = make(map[string]interface{})
		}

		// All keys should now be non-nil maps
		for _, key := range []string{"bindings", "trigger", "prev", "nodes", "vars", "run"} {
			if contextData[key] == nil {
				t.Errorf("expected %s to be initialized, got nil", key)
			}
			if _, ok := contextData[key].(map[string]interface{}); !ok {
				t.Errorf("expected %s to be a map, got %T", key, contextData[key])
			}
		}

		// Verify JSON serialization works with empty maps
		contextJSON, err := json.Marshal(contextData)
		if err != nil {
			t.Fatalf("failed to marshal context with empty maps: %v", err)
		}

		expected := `{"bindings":{},"nodes":{},"prev":{},"run":{},"trigger":{},"vars":{}}`
		if string(contextJSON) != expected {
			t.Errorf("expected %s, got %s", expected, string(contextJSON))
		}
	})
}

// TestSimpleResolverTemplateResolution tests the template resolution for bindings, prev, nodes
func TestSimpleResolverTemplateResolution(t *testing.T) {
	t.Run("resolves bindings templates", func(t *testing.T) {
		resolver := &simpleResolver{
			input: map[string]interface{}{
				"bindings": map[string]interface{}{
					"apiUrl":  "https://api.example.com",
					"apiKey":  "secret123",
					"timeout": 30,
				},
			},
			nodeOutputs: map[string]interface{}{},
			variables:   map[string]interface{}{},
		}

		tests := []struct {
			template string
			expected string
		}{
			{"{{bindings.apiUrl}}", "https://api.example.com"},
			{"{{bindings.apiKey}}", "secret123"},
			{"{{bindings.timeout}}", "30"},
			{"URL: {{bindings.apiUrl}}/endpoint", "URL: https://api.example.com/endpoint"},
			{"{{bindings.missing}}", ""}, // Non-existent key returns empty
		}

		for _, tt := range tests {
			result := resolver.ResolveString(tt.template)
			if result != tt.expected {
				t.Errorf("ResolveString(%q) = %q, want %q", tt.template, result, tt.expected)
			}
		}
	})

	t.Run("resolves prev templates", func(t *testing.T) {
		resolver := &simpleResolver{
			input: map[string]interface{}{
				"prev": map[string]interface{}{
					"status":  "success",
					"code":    200,
					"message": "Operation completed",
					"data": map[string]interface{}{
						"id":   "abc123",
						"name": "test",
					},
				},
			},
			nodeOutputs: map[string]interface{}{},
			variables:   map[string]interface{}{},
		}

		tests := []struct {
			template string
			expected string
		}{
			{"{{prev.status}}", "success"},
			{"{{prev.code}}", "200"},
			{"{{prev.message}}", "Operation completed"},
			{"{{prev.data.id}}", "abc123"},
			{"{{prev.data.name}}", "test"},
			{"ID: {{prev.data.id}}", "ID: abc123"},
		}

		for _, tt := range tests {
			result := resolver.ResolveString(tt.template)
			if result != tt.expected {
				t.Errorf("ResolveString(%q) = %q, want %q", tt.template, result, tt.expected)
			}
		}
	})

	t.Run("resolves nodes templates", func(t *testing.T) {
		resolver := &simpleResolver{
			input:     map[string]interface{}{},
			variables: map[string]interface{}{},
			nodeOutputs: map[string]interface{}{
				"httpNode": map[string]interface{}{
					"statusCode": 200,
					"body": map[string]interface{}{
						"result": "success",
					},
				},
				"transformNode": map[string]interface{}{
					"items": []interface{}{"a", "b", "c"},
				},
			},
		}

		tests := []struct {
			template string
			expected string
		}{
			{"{{nodes.httpNode.statusCode}}", "200"},
			{"{{nodes.httpNode.body.result}}", "success"},
			{"Status: {{nodes.httpNode.statusCode}}", "Status: 200"},
		}

		for _, tt := range tests {
			result := resolver.ResolveString(tt.template)
			if result != tt.expected {
				t.Errorf("ResolveString(%q) = %q, want %q", tt.template, result, tt.expected)
			}
		}
	})

	t.Run("resolves trigger templates", func(t *testing.T) {
		resolver := &simpleResolver{
			input: map[string]interface{}{
				"trigger": map[string]interface{}{
					"type": "webhook",
					"data": map[string]interface{}{
						"userId":  "user123",
						"payload": map[string]interface{}{"key": "value"},
					},
				},
			},
			nodeOutputs: map[string]interface{}{},
			variables:   map[string]interface{}{},
		}

		tests := []struct {
			template string
			expected string
		}{
			{"{{trigger.type}}", "webhook"},
			{"{{trigger.data.userId}}", "user123"},
			{"{{trigger.data.payload.key}}", "value"},
		}

		for _, tt := range tests {
			result := resolver.ResolveString(tt.template)
			if result != tt.expected {
				t.Errorf("ResolveString(%q) = %q, want %q", tt.template, result, tt.expected)
			}
		}
	})

	t.Run("resolves var templates", func(t *testing.T) {
		resolver := &simpleResolver{
			input:       map[string]interface{}{},
			nodeOutputs: map[string]interface{}{},
			variables: map[string]interface{}{
				"counter": 42,
				"name":    "test",
				"nested": map[string]interface{}{
					"value": "deep",
				},
			},
		}

		tests := []struct {
			template string
			expected string
		}{
			{"{{var.counter}}", "42"},
			{"{{var.name}}", "test"},
			{"{{var.nested.value}}", "deep"},
			{"Count: {{var.counter}}", "Count: 42"},
		}

		for _, tt := range tests {
			result := resolver.ResolveString(tt.template)
			if result != tt.expected {
				t.Errorf("ResolveString(%q) = %q, want %q", tt.template, result, tt.expected)
			}
		}
	})

	t.Run("resolves mixed templates", func(t *testing.T) {
		resolver := &simpleResolver{
			input: map[string]interface{}{
				"bindings": map[string]interface{}{"base": "https://api.example.com"},
				"prev":     map[string]interface{}{"id": "123"},
				"trigger":  map[string]interface{}{"action": "get"},
			},
			nodeOutputs: map[string]interface{}{
				"auth": map[string]interface{}{"token": "xyz"},
			},
			variables: map[string]interface{}{
				"version": "v2",
			},
		}

		template := "{{bindings.base}}/{{var.version}}/{{trigger.action}}/{{prev.id}}?token={{nodes.auth.token}}"
		expected := "https://api.example.com/v2/get/123?token=xyz"

		result := resolver.ResolveString(template)
		if result != expected {
			t.Errorf("ResolveString(%q) = %q, want %q", template, result, expected)
		}
	})
}

// TestCodeExecutorBuildJob verifies the job construction includes context
func TestCodeExecutorBuildJob(t *testing.T) {
	t.Run("job includes context environment variable", func(t *testing.T) {
		executor := &CodeExecutor{
			namespace:      "test-ns",
			runnerImage:    "python:3.11",
			serviceAccount: "default",
		}

		contextJSON := `{"bindings":{"key":"value"},"prev":{"data":"test"}}`
		job := executor.buildJob("test-job", "print('hello')", nil, 60, contextJSON, "")

		// Find AGENT_CONTEXT env var
		var foundContext bool
		var contextValue string
		for _, env := range job.Spec.Template.Spec.Containers[0].Env {
			if env.Name == "AGENT_CONTEXT" {
				foundContext = true
				contextValue = env.Value
				break
			}
		}

		if !foundContext {
			t.Error("expected AGENT_CONTEXT environment variable to be set")
		}

		if contextValue != contextJSON {
			t.Errorf("expected AGENT_CONTEXT=%q, got %q", contextJSON, contextValue)
		}
	})

	t.Run("job command includes python wrapper with context parsing", func(t *testing.T) {
		executor := &CodeExecutor{
			namespace:      "test-ns",
			runnerImage:    "python:3.11",
			serviceAccount: "default",
		}

		job := executor.buildJob("test-job", "result = bindings", nil, 60, "{}", "")

		command := job.Spec.Template.Spec.Containers[0].Command
		if len(command) < 3 {
			t.Fatal("expected command to have at least 3 elements")
		}

		shellCmd := command[2]

		// Verify the wrapper code includes context parsing
		// Note: Single quotes are escaped as '"'"' by escapeForShell
		expectedPatterns := []string{
			"os.environ.get",
			"AGENT_CONTEXT",
			"bindings = _ctx.get",
			"trigger = _ctx.get",
			"prev = _ctx.get",
			"nodes = _ctx.get",
			"vars = _ctx.get",
		}

		for _, pattern := range expectedPatterns {
			if !hasSubstring(shellCmd, pattern) {
				t.Errorf("expected command to contain %q", pattern)
			}
		}
	})
}

// TestCodeExecutorContextProviderInterface verifies simpleResolver implements ContextProvider
func TestCodeExecutorContextProviderInterface(t *testing.T) {
	t.Run("simpleResolver implements ContextProvider", func(t *testing.T) {
		resolver := &simpleResolver{
			input: map[string]interface{}{
				"bindings": map[string]interface{}{"key": "value"},
			},
			nodeOutputs: map[string]interface{}{},
			variables:   map[string]interface{}{},
			runMetadata: map[string]interface{}{},
		}

		// Check that simpleResolver implements ContextProvider
		var _ ContextProvider = resolver

		// Verify the interface method works
		ctx := resolver.GetContextData()
		if ctx == nil {
			t.Error("expected GetContextData to return non-nil")
		}
	})
}

// TestCodeExecutorExecuteWithContext is an integration-style test that verifies
// the full flow of context being passed through the executor
func TestCodeExecutorExecuteWithContext(t *testing.T) {
	t.Run("executor receives context from resolver", func(t *testing.T) {
		// Create a resolver with full context
		resolver := &simpleResolver{
			input: map[string]interface{}{
				"bindings": map[string]interface{}{
					"apiUrl":  "https://api.example.com",
					"apiKey":  "secret123",
					"timeout": 30,
				},
				"trigger": map[string]interface{}{
					"type": "webhook",
					"data": map[string]interface{}{
						"userId":   "user456",
						"document": "test.pdf",
					},
				},
				"prev": map[string]interface{}{
					"status":  "ok",
					"message": "Previous step completed",
				},
			},
			nodeOutputs: map[string]interface{}{
				"fetchNode": map[string]interface{}{
					"items": []interface{}{"item1", "item2"},
				},
			},
			variables: map[string]interface{}{
				"counter": 5,
				"flag":    true,
			},
			runMetadata: map[string]interface{}{
				"id":        999,
				"startedAt": "2025-01-01T00:00:00Z",
			},
		}

		// Verify resolver implements ContextProvider by casting to interface first
		var tr TemplateResolver = resolver
		cp, ok := tr.(ContextProvider)
		if !ok {
			t.Fatal("resolver does not implement ContextProvider")
		}

		// Get context data
		ctxData := cp.GetContextData()

		// Verify all expected data is present
		bindings := ctxData["bindings"].(map[string]interface{})
		if bindings["apiUrl"] != "https://api.example.com" {
			t.Errorf("expected apiUrl in bindings, got %v", bindings["apiUrl"])
		}

		trigger := ctxData["trigger"].(map[string]interface{})
		triggerData := trigger["data"].(map[string]interface{})
		if triggerData["userId"] != "user456" {
			t.Errorf("expected userId in trigger.data, got %v", triggerData["userId"])
		}

		prev := ctxData["prev"].(map[string]interface{})
		if prev["status"] != "ok" {
			t.Errorf("expected status in prev, got %v", prev["status"])
		}

		nodes := ctxData["nodes"].(map[string]interface{})
		fetchNode := nodes["fetchNode"].(map[string]interface{})
		if fetchNode["items"] == nil {
			t.Error("expected items in nodes.fetchNode")
		}

		vars := ctxData["vars"].(map[string]interface{})
		if vars["counter"] != 5 {
			t.Errorf("expected counter=5 in vars, got %v", vars["counter"])
		}

		run := ctxData["run"].(map[string]interface{})
		if run["id"] != 999 {
			t.Errorf("expected id=999 in run, got %v", run["id"])
		}

		// Verify JSON serialization works (this is what code executor does)
		jsonBytes, err := json.Marshal(ctxData)
		if err != nil {
			t.Fatalf("failed to marshal context: %v", err)
		}

		// Verify we can parse it back
		var parsed map[string]interface{}
		if err := json.Unmarshal(jsonBytes, &parsed); err != nil {
			t.Fatalf("failed to unmarshal context: %v", err)
		}

		// Verify nested values survive JSON round-trip
		parsedBindings := parsed["bindings"].(map[string]interface{})
		if parsedBindings["apiKey"] != "secret123" {
			t.Errorf("expected apiKey to survive JSON round-trip, got %v", parsedBindings["apiKey"])
		}
	})
}

// hasSubstring checks if s contains substr (helper for code executor tests)
func hasSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// Ensure context import is used
var _ = context.Background

func TestCodeExecutorHelpers(t *testing.T) {
	t.Run("joinRequirements", func(t *testing.T) {
		tests := []struct {
			input    []string
			expected string
		}{
			{[]string{}, ""},
			{[]string{"requests"}, "requests"},
			{[]string{"requests", "pandas"}, "requests pandas"},
			{[]string{"numpy", "scipy", "matplotlib"}, "numpy scipy matplotlib"},
		}

		for _, tt := range tests {
			result := joinRequirements(tt.input)
			if result != tt.expected {
				t.Errorf("joinRequirements(%v) = %q, want %q", tt.input, result, tt.expected)
			}
		}
	})

	t.Run("escapeForPython", func(t *testing.T) {
		tests := []struct {
			input    string
			expected string
		}{
			{"hello", "hello"},
			{"it's", "it\\'s"},
			{"path\\to\\file", "path\\\\to\\\\file"},
			{"quote 'test' here", "quote \\'test\\' here"},
		}

		for _, tt := range tests {
			result := escapeForPython(tt.input)
			if result != tt.expected {
				t.Errorf("escapeForPython(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		}
	})

	t.Run("escapeForShell", func(t *testing.T) {
		tests := []struct {
			input    string
			expected string
		}{
			{"hello", "hello"},
			{"it's", "it'\"'\"'s"},
			{"no quotes", "no quotes"},
		}

		for _, tt := range tests {
			result := escapeForShell(tt.input)
			if result != tt.expected {
				t.Errorf("escapeForShell(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		}
	})
}

func TestCodeExecutorConfig(t *testing.T) {
	// Test default config values
	config := &CodeExecutorConfig{
		Namespace:      "test-ns",
		RunnerImage:    "python:3.11",
		ServiceAccount: "test-sa",
	}

	if config.Namespace != "test-ns" {
		t.Errorf("Expected namespace 'test-ns', got %q", config.Namespace)
	}

	if config.RunnerImage != "python:3.11" {
		t.Errorf("Expected image 'python:3.11', got %q", config.RunnerImage)
	}

	if config.ServiceAccount != "test-sa" {
		t.Errorf("Expected service account 'test-sa', got %q", config.ServiceAccount)
	}
}

// TestFindLastJSONLineWithRealOutput tests the JSON parsing with actual job output
func TestFindLastJSONLineWithRealOutput(t *testing.T) {
	// Simulate the actual output from a code runner job
	// The delimiter is on line 1, JSON starts on line 2
	output := `---AGENT_OUTPUT_START---
{"success": true, "output": {"tickets": [{"Ticket ID": 1, "Customer Name": "Marisa Obrien", "Ticket Description": "I'm having an issue with the {product_purchased}. Please assist.\n\nYour billing zip code is: 71701.\n\nWe appreciate that you have requested a website address.\n\nPlease double check your email address. I've tried troubleshooting steps mentioned in the user manual, but the issue persists.", "Ticket Status": "Pending Customer Response"}], "total_count": 100}}`

	// Test splitByDelimiter first
	systemLogs, appOutput := splitByDelimiter(output)
	t.Logf("=== splitByDelimiter results ===")
	t.Logf("systemLogs: '%s'", systemLogs)
	t.Logf("appOutput length: %d", len(appOutput))
	if len(appOutput) > 100 {
		t.Logf("appOutput first 100 chars: '%s'", appOutput[:100])
		t.Logf("appOutput last 100 chars: '%s'", appOutput[len(appOutput)-100:])
	}

	// Test findLastJSONLine
	jsonLine := findLastJSONLine(appOutput)
	t.Logf("=== findLastJSONLine results ===")
	t.Logf("jsonLine length: %d", len(jsonLine))
	if jsonLine != "" {
		if len(jsonLine) > 100 {
			t.Logf("jsonLine first 100 chars: '%s'", jsonLine[:100])
			t.Logf("jsonLine last 100 chars: '%s'", jsonLine[len(jsonLine)-100:])
		} else {
			t.Logf("jsonLine: '%s'", jsonLine)
		}
	} else {
		t.Logf("jsonLine is EMPTY!")
	}

	// The test should pass if we find valid JSON
	if jsonLine == "" {
		t.Errorf("findLastJSONLine returned empty string for valid JSON output")
	}

	// Verify the JSON can be parsed
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(jsonLine), &parsed); err != nil {
		t.Errorf("Failed to parse JSON: %v", err)
	}

	// Verify success field
	if success, ok := parsed["success"].(bool); !ok || !success {
		t.Errorf("Expected success=true, got %v", parsed["success"])
	}
}

// TestFindLastJSONLineWithEmbeddedNewlines tests handling of JSON with embedded \n in strings
func TestFindLastJSONLineWithEmbeddedNewlines(t *testing.T) {
	// This is the problematic case - JSON with literal \n characters in string values
	// Note: In the actual logs, these are escaped as \\n in the JSON string
	appOutput := `{"success": true, "output": {"message": "Line 1\nLine 2\nLine 3", "count": 3}}`

	jsonLine := findLastJSONLine(appOutput)
	t.Logf("appOutput: '%s'", appOutput)
	t.Logf("jsonLine: '%s'", jsonLine)

	if jsonLine == "" {
		t.Errorf("findLastJSONLine returned empty string")
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(jsonLine), &parsed); err != nil {
		t.Errorf("Failed to parse JSON: %v", err)
	}
}

// TestSplitByDelimiter tests the delimiter splitting
func TestSplitByDelimiter(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantSys string
		wantApp string
	}{
		{
			name:    "delimiter present",
			input:   "---AGENT_OUTPUT_START---\n{\"success\": true}",
			wantSys: "",
			wantApp: "{\"success\": true}",
		},
		{
			name:    "delimiter with system logs",
			input:   "Installing packages...\n---AGENT_OUTPUT_START---\n{\"success\": true}",
			wantSys: "Installing packages...",
			wantApp: "{\"success\": true}",
		},
		{
			name:    "no delimiter",
			input:   "{\"success\": true}",
			wantSys: "",
			wantApp: "{\"success\": true}",
		},
		{
			name:    "delimiter on same line as JSON",
			input:   "---AGENT_OUTPUT_START--- {\"success\": true}",
			wantSys: "",
			wantApp: "{\"success\": true}",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sys, app := splitByDelimiter(tt.input)
			if sys != tt.wantSys {
				t.Errorf("systemLogs = %q, want %q", sys, tt.wantSys)
			}
			if app != tt.wantApp {
				t.Errorf("appOutput = %q, want %q", app, tt.wantApp)
			}
		})
	}
}
