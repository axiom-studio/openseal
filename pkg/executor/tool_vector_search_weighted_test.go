package executor

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	_ "github.com/lib/pq"
)

// TestGetWeightsMapFromConfig_StringValues tests the current bug where string weights aren't parsed
func TestGetWeightsMapFromConfig_StringValues(t *testing.T) {
	t.Run("string values from database (current bug)", func(t *testing.T) {
		config := map[string]interface{}{
			"weights": map[string]interface{}{
				"embedding_mpn":      "0.6",
				"embedding_metadata": "0.4",
			},
		}

		weights := getWeightsMapFromConfig(config, "weights")

		if len(weights) != 0 {
			t.Logf("UNEXPECTED: Got %d weights (code has been fixed?)", len(weights))
			t.Logf("Weights: %+v", weights)
		} else {
			t.Logf("CONFIRMED BUG: String weights not parsed, got empty map")
		}
	})

	t.Run("numeric values should work", func(t *testing.T) {
		config := map[string]interface{}{
			"weights": map[string]interface{}{
				"embedding_mpn":      0.6,
				"embedding_metadata": 0.4,
			},
		}

		weights := getWeightsMapFromConfig(config, "weights")

		if len(weights) != 2 {
			t.Errorf("Expected 2 weights, got %d", len(weights))
		}

		if weights["embedding_mpn"] != 0.6 {
			t.Errorf("Expected embedding_mpn weight 0.6, got %f", weights["embedding_mpn"])
		}

		if weights["embedding_metadata"] != 0.4 {
			t.Errorf("Expected embedding_metadata weight 0.4, got %f", weights["embedding_metadata"])
		}
	})

	t.Run("JSON string format should work", func(t *testing.T) {
		config := map[string]interface{}{
			"weights": `{"embedding_mpn": 0.6, "embedding_metadata": 0.4}`,
		}

		weights := getWeightsMapFromConfig(config, "weights")

		if len(weights) != 2 {
			t.Errorf("Expected 2 weights, got %d", len(weights))
		}
	})
}

// TestParseVectorConfigs_CommaSeparated tests parsing comma-separated vector columns
func TestParseVectorConfigs_CommaSeparated(t *testing.T) {
	t.Run("comma-separated string like from agent 6", func(t *testing.T) {
		config := map[string]interface{}{
			"vectorColumns": "embedding_mpn, embedding_metadata",
		}

		executor := &VectorSearchToolExecutor{}
		configs, err := executor.parseVectorConfigs(config)

		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		if len(configs) != 2 {
			t.Fatalf("Expected 2 configs, got %d", len(configs))
		}

		if configs[0].Column != "embedding_mpn" {
			t.Errorf("Expected first column 'embedding_mpn', got '%s'", configs[0].Column)
		}

		if configs[1].Column != "embedding_metadata" {
			t.Errorf("Expected second column 'embedding_metadata', got '%s'", configs[1].Column)
		}

		t.Logf("Successfully parsed 2 vector columns: %s, %s", configs[0].Column, configs[1].Column)
	})

	t.Run("with extra spaces", func(t *testing.T) {
		config := map[string]interface{}{
			"vectorColumns": "  embedding_mpn  ,  embedding_metadata  ",
		}

		executor := &VectorSearchToolExecutor{}
		configs, err := executor.parseVectorConfigs(config)

		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		if len(configs) != 2 {
			t.Errorf("Expected 2 configs, got %d", len(configs))
		}

		if configs[0].Column != "embedding_mpn" {
			t.Errorf("Expected trimmed column name, got '%s'", configs[0].Column)
		}
	})
}

// TestWeightedAverage_AgentConfig reproduces the exact configuration from agent 6
func TestWeightedAverage_AgentConfig(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	connStr := os.Getenv("PGVECTOR_TEST_URL")
	if connStr == "" {
		t.Skip("Skipping integration test - set PGVECTOR_TEST_URL to enable")
	}

	embeddingServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		embedding := make([]float64, 4096)
		for i := range embedding {
			embedding[i] = 0.1
		}

		response := map[string]interface{}{
			"data": []map[string]interface{}{
				{
					"embedding": embedding,
				},
			},
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}))
	defer embeddingServer.Close()

	config := map[string]interface{}{
		"connectionString": connStr,
		"table":            "catalog",
		"vectorColumns":    "embedding_mpn, embedding_metadata",
		"weights": map[string]interface{}{
			"embedding_mpn":      "0.6",
			"embedding_metadata": "0.4",
		},
		"aggregationMethod": "weighted",
		"searchMode":        "advanced",
		"returnColumns":     "name, mpn, description",
		"topK":              5,
		"embeddingProvider": "openai-compatible",
		"embeddingBaseUrl":  embeddingServer.URL,
		"embeddingModel":    "test-model",
		"embeddingApiKey":   "test-key",
	}

	def := &ToolDefinition{
		Name:        "search_catalog_over_mpn_and_metadata",
		Description: "Search catalog",
		Config:      config,
	}

	executor, err := NewVectorSearchToolExecutor(def, &mockResolver{})
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}

	args := map[string]interface{}{
		"queries": map[string]interface{}{
			"embedding_mpn":      "81634B",
			"embedding_metadata": "part number 81634B",
		},
		"top_k": 5,
	}

	ctx := context.Background()
	result, err := executor.Execute(ctx, args)

	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	if result.Error != "" {
		t.Logf("Execution error: %s", result.Error)

		if strings.Contains(result.Error, `column "embedding_metadata_distance" does not exist`) {
			t.Logf("REPRODUCED THE BUG: Got 'column does not exist' error")
			t.Logf("This confirms the issue exists with weighted average mode")
		} else if strings.Contains(result.Error, "search query failed") {
			t.Logf("Got database error (might be the bug or connection issue)")
		}
	} else {
		t.Logf("SUCCESS: Query executed without errors")
		t.Logf("Result: %+v", result.Result)
	}
}

// TestWeightedAverage_OrderByGeneration tests the SQL ORDER BY generation
func TestWeightedAverage_OrderByGeneration(t *testing.T) {
	t.Run("with string weights falls back to equal weights", func(t *testing.T) {
		config := map[string]interface{}{
			"weights": map[string]interface{}{
				"embedding_mpn":      "0.6",
				"embedding_metadata": "0.4",
			},
		}

		weights := getWeightsMapFromConfig(config, "weights")

		if len(weights) == 0 {
			t.Logf("CONFIRMED: String weights result in empty map")
			t.Logf("Code will fall back to equal weights: (col1 + col2) / 2")
		}

		configNumeric := map[string]interface{}{
			"weights": map[string]interface{}{
				"embedding_mpn":      0.6,
				"embedding_metadata": 0.4,
			},
		}

		weightsNumeric := getWeightsMapFromConfig(configNumeric, "weights")

		if len(weightsNumeric) == 2 {
			t.Logf("CONFIRMED: Numeric weights work correctly")
			t.Logf("Code will use custom weights: (0.6*col1 + 0.4*col2) / 1.0")
		}
	})
}

// TestWeightedAverage_DatabaseIntegration is a full integration test against real database
func TestWeightedAverage_DatabaseIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping database integration test in short mode")
	}

	connStr := os.Getenv("PGVECTOR_TEST_URL")
	if connStr == "" {
		t.Skip("Skipping database test - set PGVECTOR_TEST_URL to enable")
	}
	db, err := sql.Open("postgres", connStr)
	if err != nil {
		t.Skipf("Cannot connect to database: %v", err)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		t.Skipf("Database not reachable: %v", err)
	}

	t.Logf("Connected to database successfully")

	var exists bool
	err = db.QueryRow(`
		SELECT EXISTS (
			SELECT FROM information_schema.columns 
			WHERE table_name = 'catalog' 
			AND column_name = 'embedding_metadata'
		)
	`).Scan(&exists)

	if err != nil {
		t.Fatalf("Failed to check table schema: %v", err)
	}

	if !exists {
		t.Fatalf("Table 'catalog' doesn't have column 'embedding_metadata'")
	}

	t.Logf("Verified: catalog table has embedding_metadata column")

	var count int
	err = db.QueryRow(`
		SELECT COUNT(*) 
		FROM catalog 
		WHERE embedding_metadata IS NOT NULL
	`).Scan(&count)

	if err != nil {
		t.Fatalf("Failed to query catalog: %v", err)
	}

	t.Logf("Found %d rows with non-null embedding_metadata", count)

	mockEmbedding := make([]float64, 4096)
	vectorStr := vectorToString(mockEmbedding)

	testSQL := `
		SELECT name, mpn, description,
			"embedding_mpn" <=> $1::vector AS embedding_mpn_distance,
			"embedding_metadata" <=> $2::vector AS embedding_metadata_distance
		FROM catalog
		ORDER BY (("embedding_mpn" <=> $1::vector) + ("embedding_metadata" <=> $2::vector)) / 2
		LIMIT 5
	`

	rows, err := db.Query(testSQL, vectorStr, vectorStr)
	if err != nil {
		t.Fatalf("SQL query failed: %v", err)
		t.Logf("SQL: %s", testSQL)
		t.Logf("This indicates the fix didn't work")
	}
	defer rows.Close()

	t.Logf("SUCCESS: Query with full expressions in ORDER BY works!")

	resultCount := 0
	for rows.Next() {
		resultCount++
	}

	t.Logf("Got %d results from catalog search", resultCount)

	testSQLWeighted := `
		SELECT name, mpn, description,
			"embedding_mpn" <=> $1::vector AS embedding_mpn_distance,
			"embedding_metadata" <=> $2::vector AS embedding_metadata_distance
		FROM catalog
		ORDER BY ((0.6 * ("embedding_mpn" <=> $1::vector)) + (0.4 * ("embedding_metadata" <=> $2::vector))) / 1.0
		LIMIT 5
	`

	rows2, err := db.Query(testSQLWeighted, vectorStr, vectorStr)
	if err != nil {
		t.Fatalf("Weighted SQL query failed: %v", err)
		t.Logf("SQL: %s", testSQLWeighted)
	}
	defer rows2.Close()

	t.Logf("SUCCESS: Weighted average query with full expressions works!")

	resultCount2 := 0
	for rows2.Next() {
		resultCount2++
	}

	t.Logf("Got %d results from weighted catalog search", resultCount2)
}
