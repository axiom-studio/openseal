package executor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPGVectorExecutor_Type(t *testing.T) {
	executor := NewPGVectorExecutor()
	if executor.Type() != StepTypePGVector {
		t.Errorf("Expected type %q, got %q", StepTypePGVector, executor.Type())
	}
}

func TestParseVector(t *testing.T) {
	tests := []struct {
		name     string
		input    interface{}
		expected []float64
		wantErr  bool
	}{
		{
			name:     "float64 slice",
			input:    []float64{0.1, 0.2, 0.3},
			expected: []float64{0.1, 0.2, 0.3},
			wantErr:  false,
		},
		{
			name:     "interface slice with floats",
			input:    []interface{}{0.1, 0.2, 0.3},
			expected: []float64{0.1, 0.2, 0.3},
			wantErr:  false,
		},
		{
			name:     "interface slice with ints",
			input:    []interface{}{1, 2, 3},
			expected: []float64{1.0, 2.0, 3.0},
			wantErr:  false,
		},
		{
			name:     "JSON string",
			input:    "[0.1, 0.2, 0.3]",
			expected: []float64{0.1, 0.2, 0.3},
			wantErr:  false,
		},
		{
			name:     "invalid type",
			input:    "not a vector",
			expected: nil,
			wantErr:  true,
		},
		{
			name:     "invalid interface element",
			input:    []interface{}{0.1, "invalid", 0.3},
			expected: nil,
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := parseVector(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Errorf("Expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Errorf("Unexpected error: %v", err)
				return
			}
			if len(result) != len(tt.expected) {
				t.Errorf("Expected length %d, got %d", len(tt.expected), len(result))
				return
			}
			for i, v := range result {
				if v != tt.expected[i] {
					t.Errorf("Expected %v at index %d, got %v", tt.expected[i], i, v)
				}
			}
		})
	}
}

func TestVectorToString(t *testing.T) {
	tests := []struct {
		name     string
		input    []float64
		expected string
	}{
		{
			name:     "simple vector",
			input:    []float64{0.1, 0.2, 0.3},
			expected: "[0.100000,0.200000,0.300000]",
		},
		{
			name:     "empty vector",
			input:    []float64{},
			expected: "[]",
		},
		{
			name:     "single element",
			input:    []float64{1.5},
			expected: "[1.500000]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := vectorToString(tt.input)
			if result != tt.expected {
				t.Errorf("Expected %q, got %q", tt.expected, result)
			}
		})
	}
}

func TestGetEmbeddingConfig(t *testing.T) {
	executor := NewPGVectorExecutor()

	resolver := &mockResolver{}

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
			name: "custom base url",
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
			cfg := executor.getEmbeddingConfig(tt.config, resolver)

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

func TestGenerateOpenAIEmbedding(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("Expected POST, got %s", r.Method)
		}
		if r.Header.Get("Authorization") != "Bearer test-api-key" {
			t.Errorf("Expected Authorization header with Bearer token")
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("Expected Content-Type application/json")
		}

		var reqBody map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
			t.Errorf("Failed to decode request body: %v", err)
		}

		if reqBody["model"] != "text-embedding-3-small" {
			t.Errorf("Expected model text-embedding-3-small, got %v", reqBody["model"])
		}
		if reqBody["input"] != "test text" {
			t.Errorf("Expected input 'test text', got %v", reqBody["input"])
		}

		response := map[string]interface{}{
			"data": []map[string]interface{}{
				{
					"embedding": []float64{0.1, 0.2, 0.3, 0.4, 0.5},
				},
			},
		}
		json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	executor := NewPGVectorExecutor()
	cfg := embeddingConfig{
		Provider: "openai",
		BaseURL:  server.URL,
		APIKey:   "test-api-key",
		Model:    "text-embedding-3-small",
	}

	embedding, err := executor.generateOpenAIEmbedding(context.Background(), "test text", cfg)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	expected := []float64{0.1, 0.2, 0.3, 0.4, 0.5}
	if len(embedding) != len(expected) {
		t.Fatalf("Expected %d dimensions, got %d", len(expected), len(embedding))
	}
	for i, v := range embedding {
		if v != expected[i] {
			t.Errorf("Expected %v at index %d, got %v", expected[i], i, v)
		}
	}
}

func TestGenerateCohereEmbedding(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("Expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/embed" {
			t.Errorf("Expected path /embed, got %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer cohere-key" {
			t.Errorf("Expected Authorization header with Bearer token")
		}

		var reqBody map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
			t.Errorf("Failed to decode request body: %v", err)
		}

		if reqBody["model"] != "embed-english-v3.0" {
			t.Errorf("Expected model embed-english-v3.0, got %v", reqBody["model"])
		}
		texts, ok := reqBody["texts"].([]interface{})
		if !ok || len(texts) != 1 || texts[0] != "test text" {
			t.Errorf("Expected texts ['test text'], got %v", reqBody["texts"])
		}

		response := map[string]interface{}{
			"embeddings": [][]float64{
				{0.8, 0.9, 1.0},
			},
		}
		json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	executor := NewPGVectorExecutor()
	cfg := embeddingConfig{
		Provider: "cohere",
		BaseURL:  server.URL,
		APIKey:   "cohere-key",
		Model:    "embed-english-v3.0",
	}

	embedding, err := executor.generateCohereEmbedding(context.Background(), "test text", cfg)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	expected := []float64{0.8, 0.9, 1.0}
	if len(embedding) != len(expected) {
		t.Fatalf("Expected %d dimensions, got %d", len(expected), len(embedding))
	}
	for i, v := range embedding {
		if v != expected[i] {
			t.Errorf("Expected %v at index %d, got %v", expected[i], i, v)
		}
	}
}

func TestGenerateEmbedding_RequiresAPIKey(t *testing.T) {
	executor := NewPGVectorExecutor()

	cfg := embeddingConfig{
		Provider: "openai",
		BaseURL:  "https://api.openai.com/v1",
		Model:    "text-embedding-3-small",
		APIKey:   "",
	}

	_, err := executor.generateEmbedding(context.Background(), "test", cfg)
	if err == nil {
		t.Error("Expected error for missing API key")
	}
}

func TestPGVectorExecutor_MissingConnectionString(t *testing.T) {
	executor := NewPGVectorExecutor()
	resolver := &mockResolver{}

	step := &StepDefinition{
		Config: map[string]interface{}{
			"operation": "search",
			"table":     "documents",
			"text":      "test query",
		},
	}

	_, err := executor.Execute(context.Background(), step, resolver)
	if err == nil {
		t.Error("Expected error for missing connection string")
	}
}

func TestPGVectorExecutor_MissingTable(t *testing.T) {
	executor := NewPGVectorExecutor()
	resolver := &mockResolver{}

	step := &StepDefinition{
		Config: map[string]interface{}{
			"operation":        "search",
			"connectionString": "postgres://localhost/test",
		},
	}

	_, err := executor.Execute(context.Background(), step, resolver)
	if err == nil {
		t.Error("Expected error for missing table")
	}
}

type mockResolver struct {
	variables   map[string]interface{}
	stepOutputs map[string]interface{}
}

func (m *mockResolver) ResolveString(template string) string {
	return template
}

func (m *mockResolver) ResolveMap(data map[string]interface{}) map[string]interface{} {
	return data
}

func (m *mockResolver) ResolveValue(template interface{}) interface{} {
	if str, ok := template.(string); ok {
		return m.ResolveString(str)
	}
	return template
}

func (m *mockResolver) EvaluateCondition(condition string) bool {
	return false
}

func (m *mockResolver) SetVariable(name string, value interface{}) {
	if m.variables == nil {
		m.variables = make(map[string]interface{})
	}
	m.variables[name] = value
}

func (m *mockResolver) GetStepOutput(stepName string) interface{} {
	if m.stepOutputs == nil {
		return nil
	}
	return m.stepOutputs[stepName]
}

func (m *mockResolver) SetStepOutput(stepName string, output interface{}) {
	if m.stepOutputs == nil {
		m.stepOutputs = make(map[string]interface{})
	}
	m.stepOutputs[stepName] = output
}
