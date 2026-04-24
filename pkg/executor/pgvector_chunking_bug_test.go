package executor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func generateLongText(wordCount int) string {
	words := []string{"Lorem", "ipsum", "dolor", "sit", "amet", "consectetur"}
	var result strings.Builder
	for i := 0; i < wordCount; i++ {
		if i > 0 {
			result.WriteString(" ")
		}
		result.WriteString(words[i%len(words)])
	}
	return result.String()
}

func TestPGVectorChunkingWithTokenLimit(t *testing.T) {
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		var reqBody map[string]interface{}
		json.NewDecoder(r.Body).Decode(&reqBody)

		if inputs, ok := reqBody["inputs"].(string); ok && len(inputs) > 2500 {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"error": "`inputs` must have less than 512 tokens. Given: 11189",
			})
			return
		}

		json.NewEncoder(w).Encode([]float64{0.1, 0.2, 0.3})
	}))
	defer server.Close()

	executor := NewPGVectorExecutor()
	resolver := &mockResolver{}
	longText := generateLongText(3000)

	step := &StepDefinition{
		Config: map[string]interface{}{
			"operation":         "upsert",
			"table":             "documents",
			"connectionString":  "postgres://localhost/test",
			"vectorColumn":      "embedding",
			"textColumn":        "content",
			"conflictColumn":    "id",
			"enableChunking":    true,
			"chunkSize":         1000.0,
			"chunkOverlap":      200.0,
			"embeddingProvider": "huggingface",
			"embeddingModel":    "sentence-transformers/all-MiniLM-L6-v2",
			"embeddingApiKey":   "hf_test_key",
			"embeddingBaseUrl":  server.URL,
			"data": map[string]interface{}{
				"id":      "doc1",
				"content": longText,
			},
		},
	}

	_, err := executor.Execute(context.Background(), step, resolver)

	if err != nil {
		if strings.Contains(err.Error(), "failed to get vector") &&
			strings.Contains(err.Error(), "must have less than 512 tokens") {
			t.Fatal("getVector called when chunking enabled")
		}
		if !strings.Contains(err.Error(), "pq:") && !strings.Contains(err.Error(), "SSL") {
			t.Fatalf("unexpected error: %v", err)
		}
	}

	if callCount < 20 || callCount > 30 {
		t.Errorf("expected 20-30 embedding calls, got %d", callCount)
	}
}

func TestPGVectorChunkingDisabled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]float64{0.1, 0.2, 0.3})
	}))
	defer server.Close()

	executor := NewPGVectorExecutor()
	resolver := &mockResolver{}

	step := &StepDefinition{
		Config: map[string]interface{}{
			"operation":         "upsert",
			"table":             "documents",
			"connectionString":  "postgres://localhost/test",
			"textColumn":        "content",
			"conflictColumn":    "id",
			"enableChunking":    false,
			"embeddingProvider": "huggingface",
			"embeddingApiKey":   "hf_test_key",
			"embeddingBaseUrl":  server.URL,
			"data": map[string]interface{}{
				"id":      "doc1",
				"content": "short text",
			},
		},
	}

	_, err := executor.Execute(context.Background(), step, resolver)

	if err != nil && !strings.Contains(err.Error(), "pq:") && !strings.Contains(err.Error(), "SSL") {
		t.Fatalf("unexpected error: %v", err)
	}
}
