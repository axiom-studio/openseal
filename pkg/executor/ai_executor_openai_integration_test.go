package executor

import (
	"context"
	"os"
	"testing"
)

// Integration test for OpenAI API - both Chat Completions and Responses API formats
// Requires OPENAI_API_KEY environment variable to be set
func TestOpenAI_ChatCompletionsAPI(t *testing.T) {
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		t.Skip("OPENAI_API_KEY not set, skipping integration test")
	}

	executor := NewAIExecutor()

	tests := []struct {
		name    string
		model   string
		baseUrl string
		prompt  string
	}{
		{
			name:    "gpt-4o-mini with chat completions",
			model:   "gpt-4o-mini",
			baseUrl: "https://api.openai.com/v1/chat/completions",
			prompt:  "Say 'test successful' and nothing else",
		},
		{
			name:    "gpt-5.2 with chat completions",
			model:   "gpt-5.2",
			baseUrl: "https://api.openai.com/v1/chat/completions",
			prompt:  "Say 'test successful' and nothing else",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()

			response, fullResp, err := executor.callOpenAI(
				ctx,
				tt.model,
				tt.prompt,
				"You are a helpful assistant.",
				0.7,
				100,
				apiKey,
				tt.baseUrl,
				nil, // no files
			)

			if err != nil {
				t.Fatalf("callOpenAI failed: %v", err)
			}

			if response == "" {
				t.Error("Response is empty")
			}

			t.Logf("Response: %s", response)

			// Verify full response structure
			if fullResp == nil {
				t.Error("Full response is nil")
			}

			// Verify it has expected fields
			if model, ok := fullResp["model"].(string); ok {
				t.Logf("Model: %s", model)
			} else {
				t.Error("Full response missing 'model' field")
			}

			if id, ok := fullResp["id"].(string); ok {
				t.Logf("ID: %s", id)
			} else {
				t.Error("Full response missing 'id' field")
			}

			if usage, ok := fullResp["usage"].(map[string]interface{}); ok {
				t.Logf("Usage: %+v", usage)
			} else {
				t.Error("Full response missing 'usage' field")
			}
		})
	}
}

func TestOpenAI_ResponsesAPI(t *testing.T) {
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		t.Skip("OPENAI_API_KEY not set, skipping integration test")
	}

	executor := NewAIExecutor()

	tests := []struct {
		name    string
		model   string
		baseUrl string
		prompt  string
	}{
		{
			name:    "gpt-4.1 with responses API",
			model:   "gpt-4.1",
			baseUrl: "https://api.openai.com/v1/responses",
			prompt:  "Say 'test successful' and nothing else",
		},
		{
			name:    "gpt-5.2 with responses API",
			model:   "gpt-5.2",
			baseUrl: "https://api.openai.com/v1/responses",
			prompt:  "Say 'test successful' and nothing else",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()

			response, fullResp, err := executor.callOpenAI(
				ctx,
				tt.model,
				tt.prompt,
				"You are a helpful assistant.",
				0.7,
				100,
				apiKey,
				tt.baseUrl,
				nil, // no files
			)

			if err != nil {
				t.Fatalf("callOpenAI failed: %v", err)
			}

			if response == "" {
				t.Errorf("Response is empty! Full response: %+v", fullResp)
			} else {
				t.Logf("Response: %s", response)
			}

			// Verify full response structure for Responses API
			if fullResp == nil {
				t.Error("Full response is nil")
			}

			// Verify Responses API specific fields
			if model, ok := fullResp["model"].(string); ok {
				t.Logf("Model: %s", model)
			} else {
				t.Error("Full response missing 'model' field")
			}

			if id, ok := fullResp["id"].(string); ok {
				t.Logf("ID: %s", id)
			} else {
				t.Error("Full response missing 'id' field")
			}

			if status, ok := fullResp["status"].(string); ok {
				t.Logf("Status: %s", status)
			} else {
				t.Error("Full response missing 'status' field")
			}

			if usage, ok := fullResp["usage"].(map[string]interface{}); ok {
				t.Logf("Usage: %+v", usage)
			} else {
				t.Error("Full response missing 'usage' field")
			}

			if output, ok := fullResp["output"].([]interface{}); ok {
				t.Logf("Output array length: %d", len(output))
				if len(output) == 0 {
					t.Error("Output array is empty")
				}
			} else {
				t.Error("Full response missing 'output' field")
			}
		})
	}
}

func TestOpenAI_ResponsesAPIWithTools(t *testing.T) {
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		t.Skip("OPENAI_API_KEY not set, skipping integration test")
	}

	executor := NewAIExecutor()

	// Define a simple tool
	tools := []*ToolDefinition{
		{
			Name:        "get_weather",
			Description: "Get the current weather for a location",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"location": map[string]interface{}{
						"type":        "string",
						"description": "The city and state, e.g. San Francisco, CA",
					},
				},
				"required": []string{"location"},
			},
		},
	}

	mockResolver := &mockTemplateResolver{}

	tests := []struct {
		name    string
		model   string
		baseUrl string
		prompt  string
	}{
		{
			name:    "gpt-5.2 with responses API and tools",
			model:   "gpt-5.2",
			baseUrl: "https://api.openai.com/v1/responses",
			prompt:  "What's the weather like in San Francisco?",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()

			response, fullResp, toolHistory, err := executor.executeOpenAIToolLoop(
				ctx,
				tt.model,
				tt.prompt,
				"You are a helpful assistant.",
				0.7,
				500,
				apiKey,
				tt.baseUrl,
				tools,
				mockResolver,
				10,
			)

			if err != nil {
				t.Fatalf("executeOpenAIToolLoop failed: %v", err)
			}

			t.Logf("Response: %s", response)
			t.Logf("Tool history: %+v", toolHistory)
			t.Logf("Full response keys: %+v", getKeys(fullResp))

			// Verify we got a response
			if response == "" {
				t.Errorf("Response is empty! Full response: %+v", fullResp)
			}

			// Verify full response exists
			if fullResp == nil {
				t.Error("Full response is nil")
			}
		})
	}
}

// Helper to get map keys
func getKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// Mock template resolver for testing
type mockTemplateResolver struct {
	variables   map[string]interface{}
	stepOutputs map[string]interface{}
}

func (m *mockTemplateResolver) ResolveString(template string) string {
	return template
}

func (m *mockTemplateResolver) ResolveMap(data map[string]interface{}) map[string]interface{} {
	return data
}

func (m *mockTemplateResolver) EvaluateCondition(condition string) bool {
	return false
}

func (m *mockTemplateResolver) SetVariable(name string, value interface{}) {
	if m.variables == nil {
		m.variables = make(map[string]interface{})
	}
	m.variables[name] = value
}

func (m *mockTemplateResolver) GetStepOutput(stepName string) interface{} {
	if m.stepOutputs == nil {
		return nil
	}
	return m.stepOutputs[stepName]
}

func (m *mockTemplateResolver) SetStepOutput(stepName string, output interface{}) {
	if m.stepOutputs == nil {
		m.stepOutputs = make(map[string]interface{})
	}
	m.stepOutputs[stepName] = output
}
