package executor

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
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

type pgVectorChunkingTestConnector struct{}

func (pgVectorChunkingTestConnector) Connect(context.Context) (driver.Conn, error) {
	return pgVectorChunkingTestConn{}, nil
}

func (pgVectorChunkingTestConnector) Driver() driver.Driver {
	return pgVectorChunkingTestDriver{}
}

type pgVectorChunkingTestDriver struct{}

func (pgVectorChunkingTestDriver) Open(string) (driver.Conn, error) {
	return pgVectorChunkingTestConn{}, nil
}

type pgVectorChunkingTestConn struct{}

func (pgVectorChunkingTestConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepared statement")
}

func (pgVectorChunkingTestConn) Close() error { return nil }

func (pgVectorChunkingTestConn) Begin() (driver.Tx, error) {
	return nil, errors.New("unexpected transaction")
}

func (pgVectorChunkingTestConn) Ping(context.Context) error { return nil }

func (pgVectorChunkingTestConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	return driver.RowsAffected(1), nil
}

func newHermeticPGVectorExecutor() *PGVectorExecutor {
	executor := NewPGVectorExecutor()
	executor.openConnection = func(string) (*sql.DB, error) {
		return sql.OpenDB(pgVectorChunkingTestConnector{}), nil
	}
	return executor
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

	executor := newHermeticPGVectorExecutor()
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

	result, err := executor.Execute(context.Background(), step, resolver)
	if err != nil {
		if strings.Contains(err.Error(), "failed to get vector") && strings.Contains(err.Error(), "must have less than 512 tokens") {
			t.Fatal("getVector called when chunking enabled")
		}
		t.Fatalf("unexpected error: %v", err)
	}
	if count := result.Output["count"]; count == nil {
		t.Fatalf("chunked upsert did not report a count: %#v", result.Output)
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

	executor := newHermeticPGVectorExecutor()
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

	result, err := executor.Execute(context.Background(), step, resolver)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if success, _ := result.Output["success"].(bool); !success {
		t.Fatalf("unchunked upsert did not succeed: %#v", result.Output)
	}
	if upserted, _ := result.Output["upserted"].(map[string]interface{}); upserted["content"] != "short text" {
		t.Fatalf("unchunked upsert payload = %#v", result.Output)
	}
}
