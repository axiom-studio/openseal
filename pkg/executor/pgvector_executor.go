package executor

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	_ "github.com/lib/pq"
)

const (
	StepTypePGVector    = "pgvector"
	maxEmbeddingRetries = 3
	initialRetryDelay   = 1 * time.Second
	maxRetryDelay       = 30 * time.Second
)

type PGVectorExecutor struct {
	client         *http.Client
	connPool       map[string]*sql.DB
	openConnection func(string) (*sql.DB, error)
	mu             sync.RWMutex
}

type VectorConfig struct {
	Column       string
	TextTemplate string
}

func NewPGVectorExecutor() *PGVectorExecutor {
	return &PGVectorExecutor{
		client:         &http.Client{Timeout: 60 * time.Second},
		connPool:       make(map[string]*sql.DB),
		openConnection: openPGVectorConnection,
	}
}

func openPGVectorConnection(connStr string) (*sql.DB, error) {
	return sql.Open("postgres", connStr)
}

func parseVectorConfigs(config map[string]interface{}) ([]VectorConfig, error) {
	// Check for new vectorConfigs array format (INSERT/UPSERT with textTemplates)
	if vectorConfigs, ok := config["vectorConfigs"].([]interface{}); ok && len(vectorConfigs) > 0 {
		configs := make([]VectorConfig, 0, len(vectorConfigs))
		for i, vc := range vectorConfigs {
			vcMap, ok := vc.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("vectorConfigs[%d] must be an object", i)
			}

			column, ok := vcMap["column"].(string)
			if !ok || column == "" {
				return nil, fmt.Errorf("vectorConfigs[%d] requires 'column' field", i)
			}

			textTemplate, ok := vcMap["textTemplate"].(string)
			if !ok || textTemplate == "" {
				return nil, fmt.Errorf("vectorConfigs[%d] requires 'textTemplate' field", i)
			}

			configs = append(configs, VectorConfig{
				Column:       column,
				TextTemplate: textTemplate,
			})
		}
		return configs, nil
	}

	// Parse vectorConfigs from JSON string
	if vectorConfigsStr, ok := config["vectorConfigs"].(string); ok && vectorConfigsStr != "" {
		var configsArray []map[string]interface{}
		if err := json.Unmarshal([]byte(vectorConfigsStr), &configsArray); err == nil && len(configsArray) > 0 {
			configs := make([]VectorConfig, 0, len(configsArray))
			for i, vcMap := range configsArray {
				column, ok := vcMap["column"].(string)
				if !ok || column == "" {
					return nil, fmt.Errorf("vectorConfigs[%d] requires 'column' field", i)
				}

				textTemplate, ok := vcMap["textTemplate"].(string)
				if !ok || textTemplate == "" {
					return nil, fmt.Errorf("vectorConfigs[%d] requires 'textTemplate' field", i)
				}

				configs = append(configs, VectorConfig{
					Column:       column,
					TextTemplate: textTemplate,
				})
			}
			if len(configs) > 0 {
				return configs, nil
			}
		}
	}

	// Check for vectorColumns array (SEARCH - just column names)
	if vectorColumns, ok := config["vectorColumns"].([]interface{}); ok && len(vectorColumns) > 0 {
		configs := make([]VectorConfig, 0, len(vectorColumns))
		for _, vc := range vectorColumns {
			column, ok := vc.(string)
			if !ok || column == "" {
				continue
			}
			configs = append(configs, VectorConfig{
				Column:       column,
				TextTemplate: "", // No template for search
			})
		}
		if len(configs) > 0 {
			return configs, nil
		}
	}

	// Parse vectorColumns from string (JSON array or comma-separated)
	if vectorColumnsStr, ok := config["vectorColumns"].(string); ok && vectorColumnsStr != "" {
		var columns []string

		// Try JSON array first: ["col1", "col2"]
		if err := json.Unmarshal([]byte(vectorColumnsStr), &columns); err == nil && len(columns) > 0 {
			configs := make([]VectorConfig, 0, len(columns))
			for _, column := range columns {
				if column != "" {
					configs = append(configs, VectorConfig{
						Column:       column,
						TextTemplate: "",
					})
				}
			}
			if len(configs) > 0 {
				return configs, nil
			}
		}

		// Try comma-separated: "col1, col2, col3"
		parts := strings.Split(vectorColumnsStr, ",")
		configs := make([]VectorConfig, 0, len(parts))
		for _, part := range parts {
			column := strings.TrimSpace(part)
			if column != "" {
				configs = append(configs, VectorConfig{
					Column:       column,
					TextTemplate: "",
				})
			}
		}
		if len(configs) > 0 {
			return configs, nil
		}
	}

	// Backward compat: single vectorColumn + textColumn
	vectorColumn := "embedding"
	if vc, ok := config["vectorColumn"].(string); ok && vc != "" {
		vectorColumn = vc
	}

	textColumn := "content"
	if tc, ok := config["text"].(string); ok && tc != "" {
		textColumn = tc
	}
	if tc, ok := config["textColumn"].(string); ok && tc != "" {
		textColumn = tc
	}

	return []VectorConfig{
		{
			Column:       vectorColumn,
			TextTemplate: "{{." + textColumn + "}}",
		},
	}, nil
}

func (e *PGVectorExecutor) getConnection(connStr string) (*sql.DB, error) {
	e.mu.RLock()
	if db, ok := e.connPool[connStr]; ok {
		e.mu.RUnlock()
		if err := db.Ping(); err == nil {
			return db, nil
		}
		e.mu.Lock()
		delete(e.connPool, connStr)
		e.mu.Unlock()
	} else {
		e.mu.RUnlock()
	}

	openConnection := e.openConnection
	if openConnection == nil {
		openConnection = openPGVectorConnection
	}
	db, err := openConnection(connStr)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}

	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	e.mu.Lock()
	e.connPool[connStr] = db
	e.mu.Unlock()

	return db, nil
}

func (e *PGVectorExecutor) Type() string {
	return StepTypePGVector
}

func (e *PGVectorExecutor) Execute(ctx context.Context, step *StepDefinition, resolver TemplateResolver) (*StepResult, error) {
	config := step.Config
	if config == nil {
		return nil, fmt.Errorf("pgvector step requires config")
	}

	operation := "search"
	if op, ok := config["operation"].(string); ok && op != "" {
		operation = op
	}

	table, ok := config["table"].(string)
	if !ok || table == "" {
		return nil, fmt.Errorf("pgvector step requires 'table'")
	}

	vectorColumn := "embedding"
	if vc, ok := config["vectorColumn"].(string); ok && vc != "" {
		vectorColumn = vc
	}

	connStr := ""
	if cs, ok := config["connectionString"].(string); ok && cs != "" {
		connStr = resolver.ResolveString(cs)
	}
	if connStr == "" {
		return nil, fmt.Errorf("pgvector step requires 'connectionString'")
	}

	db, err := e.getConnection(connStr)
	if err != nil {
		return nil, err
	}

	textColumn := "content"
	if tc, ok := config["text"].(string); ok && tc != "" {
		textColumn = tc
	}
	if tc, ok := config["textColumn"].(string); ok && tc != "" {
		textColumn = tc
	}

	var resolvedData interface{}
	if dataTemplate, ok := config["data"].(string); ok && dataTemplate != "" {
		resolvedStr := resolver.ResolveString(dataTemplate)
		if resolvedStr != "" && resolvedStr != dataTemplate {
			if err := json.Unmarshal([]byte(resolvedStr), &resolvedData); err != nil {
				return nil, fmt.Errorf("failed to parse data: %w (value: %.100s)", err, resolvedStr)
			}
		} else {
			return nil, fmt.Errorf("data template '%s' was not resolved - check previous node output", dataTemplate)
		}
	} else if dataRaw, ok := config["data"]; ok {
		resolvedData = dataRaw
	}

	if resolvedData == nil && (operation == "insert" || operation == "upsert") {
		return nil, fmt.Errorf("insert/upsert operation requires 'data' field")
	}

	enableChunking := false
	if operation == "insert" || operation == "upsert" {
		if ec, ok := config["enableChunking"].(bool); ok {
			enableChunking = ec
		} else if ec, ok := config["enableChunking"].(string); ok {
			enableChunking = ec == "true"
		}
	}

	var vector []float64
	if !((operation == "insert" || operation == "upsert") && enableChunking) {
		vector, err = e.getVector(ctx, config, resolver)
		if err != nil {
			return nil, fmt.Errorf("failed to get vector: %w", err)
		}
	}

	if enableChunking {
		chunkSize := 1000
		if cs, ok := config["chunkSize"].(float64); ok && cs > 0 {
			chunkSize = int(cs)
		}

		chunkOverlap := 0
		if co, ok := config["chunkOverlap"].(float64); ok && co >= 0 {
			chunkOverlap = int(co)
		}

		// Apply chunking to data
		if dataMap, ok := resolvedData.(map[string]interface{}); ok {
			// Single item - check if it has a text field to chunk
			if textVal, ok := dataMap[textColumn].(string); ok && textVal != "" {
				chunks := chunkText(textVal, chunkSize, chunkOverlap)
				if len(chunks) > 1 {
					// Convert single item into multiple chunked items
					chunkedItems := make([]interface{}, 0, len(chunks))
					for idx, chunk := range chunks {
						itemCopy := make(map[string]interface{})
						for k, v := range dataMap {
							itemCopy[k] = v
						}
						itemCopy[textColumn] = chunk
						itemCopy["chunk_index"] = idx
						itemCopy["total_chunks"] = len(chunks)
						chunkedItems = append(chunkedItems, itemCopy)
					}
					resolvedData = chunkedItems
				}
			}
		} else if items, ok := resolvedData.([]interface{}); ok {
			// Array of items - chunk each item's text field
			chunkedItems := make([]interface{}, 0, len(items))
			for _, item := range items {
				if itemMap, ok := item.(map[string]interface{}); ok {
					if textVal, ok := itemMap[textColumn].(string); ok && textVal != "" {
						chunks := chunkText(textVal, chunkSize, chunkOverlap)
						for idx, chunk := range chunks {
							itemCopy := make(map[string]interface{})
							for k, v := range itemMap {
								itemCopy[k] = v
							}
							itemCopy[textColumn] = chunk
							itemCopy["chunk_index"] = idx
							itemCopy["total_chunks"] = len(chunks)
							chunkedItems = append(chunkedItems, itemCopy)
						}
					} else {
						chunkedItems = append(chunkedItems, itemMap)
					}
				}
			}
			if len(chunkedItems) > 0 {
				resolvedData = chunkedItems
			}
		}
	}

	if items, ok := resolvedData.([]interface{}); ok && len(items) > 0 {
		batchItems := make([]map[string]interface{}, 0, len(items))
		for _, item := range items {
			if m, ok := item.(map[string]interface{}); ok {
				batchItems = append(batchItems, m)
			}
		}
		if len(batchItems) > 0 {
			switch operation {
			case "insert":
				return e.executeBatchInsert(ctx, db, table, vectorColumn, textColumn, batchItems, config, resolver)
			case "upsert":
				conflictColumn := "id"
				if cc, ok := config["conflictColumn"].(string); ok && cc != "" {
					conflictColumn = cc
				}
				return e.executeBatchUpsert(ctx, db, table, vectorColumn, textColumn, conflictColumn, batchItems, config, resolver)
			}
		}
	}

	if dataMap, ok := resolvedData.(map[string]interface{}); ok {
		config["data"] = dataMap
	}

	switch operation {
	case "search":
		return e.executeSearch(ctx, db, table, vectorColumn, vector, config, resolver)
	case "insert":
		return e.executeInsert(ctx, db, table, vectorColumn, vector, config, resolver)
	case "upsert":
		return e.executeUpsert(ctx, db, table, vectorColumn, vector, config, resolver)
	case "delete":
		return e.executeDelete(ctx, db, table, config, resolver)
	default:
		return nil, fmt.Errorf("unsupported pgvector operation: %s", operation)
	}
}

func (e *PGVectorExecutor) getVector(ctx context.Context, config map[string]interface{}, resolver TemplateResolver) ([]float64, error) {
	if vecTemplate, ok := config["vector"].(string); ok && vecTemplate != "" {
		resolved := resolver.ResolveString(vecTemplate)
		if resolved != "" && resolved != vecTemplate {
			return parseVector(resolved)
		}
	}
	if vec, ok := config["vector"]; ok && vec != nil {
		if _, isStr := vec.(string); !isStr {
			return parseVector(vec)
		}
	}

	var text string
	if textTemplate, ok := config["text"].(string); ok && textTemplate != "" {
		text = resolver.ResolveString(textTemplate)
	}

	if text == "" {
		if textColumn, ok := config["textColumn"].(string); ok && textColumn != "" {
			if data, ok := getConfigMapWithResolver(config, "data", resolver); ok {
				if textVal, ok := data[textColumn].(string); ok {
					text = textVal
				}
			}
		}
	}

	if text == "" {
		return nil, nil
	}

	return e.generateEmbedding(ctx, text, e.getEmbeddingConfig(config, resolver))
}

type embeddingConfig struct {
	Provider string
	BaseURL  string
	APIKey   string
	Model    string
}

func (e *PGVectorExecutor) getEmbeddingConfig(config map[string]interface{}, resolver TemplateResolver) embeddingConfig {
	cfg := embeddingConfig{
		Provider: "openai",
		Model:    "text-embedding-3-small",
	}

	if p, ok := config["embeddingProvider"].(string); ok && p != "" {
		cfg.Provider = p
	}
	if m, ok := config["embeddingModel"].(string); ok && m != "" {
		cfg.Model = m
	}
	if ak, ok := config["embeddingApiKey"].(string); ok && ak != "" {
		cfg.APIKey = resolver.ResolveString(ak)
	}
	if bu, ok := config["embeddingBaseUrl"].(string); ok && bu != "" {
		cfg.BaseURL = resolver.ResolveString(bu)
	}

	if cfg.BaseURL == "" {
		switch cfg.Provider {
		case "openai", "openai-compatible":
			cfg.BaseURL = "https://api.openai.com/v1"
		case "voyage":
			cfg.BaseURL = "https://api.voyageai.com/v1"
		case "cohere":
			cfg.BaseURL = "https://api.cohere.ai/v1"
		case "gemini":
			cfg.BaseURL = "https://generativelanguage.googleapis.com/v1beta"
		case "ollama":
			cfg.BaseURL = "http://localhost:11434"
		}
	}

	return cfg
}

func (e *PGVectorExecutor) generateEmbedding(ctx context.Context, text string, cfg embeddingConfig) ([]float64, error) {
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("embedding API key required for provider '%s' (use embeddingApiKey config or blueprint binding)", cfg.Provider)
	}

	switch cfg.Provider {
	case "cohere":
		return e.generateCohereEmbedding(ctx, text, cfg)
	case "huggingface":
		return e.generateHuggingFaceEmbedding(ctx, text, cfg)
	case "gemini":
		return e.generateGeminiEmbedding(ctx, text, cfg)
	case "openai", "openai-compatible", "":
		return e.generateOpenAIEmbedding(ctx, text, cfg)
	default:
		return e.generateOpenAIEmbedding(ctx, text, cfg)
	}
}

func (e *PGVectorExecutor) generateOpenAIEmbedding(ctx context.Context, text string, cfg embeddingConfig) ([]float64, error) {
	reqBody := map[string]interface{}{
		"model": cfg.Model,
		"input": text,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	endpoint := strings.TrimSuffix(cfg.BaseURL, "/") + "/embeddings"
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embeddings API error (%s): %s", cfg.Provider, string(respBody))
	}

	var result struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}

	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, err
	}

	if len(result.Data) == 0 {
		return nil, fmt.Errorf("no embedding returned from %s API", cfg.Provider)
	}

	return result.Data[0].Embedding, nil
}

func (e *PGVectorExecutor) generateCohereEmbedding(ctx context.Context, text string, cfg embeddingConfig) ([]float64, error) {
	reqBody := map[string]interface{}{
		"model":      cfg.Model,
		"texts":      []string{text},
		"input_type": "search_document",
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	endpoint := strings.TrimSuffix(cfg.BaseURL, "/") + "/embed"
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Cohere embeddings API error: %s", string(respBody))
	}

	var result struct {
		Embeddings [][]float64 `json:"embeddings"`
	}

	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, err
	}

	if len(result.Embeddings) == 0 {
		return nil, fmt.Errorf("no embedding returned from Cohere API")
	}

	return result.Embeddings[0], nil
}

func (e *PGVectorExecutor) generateHuggingFaceEmbedding(ctx context.Context, text string, cfg embeddingConfig) ([]float64, error) {
	reqBody := map[string]interface{}{
		"inputs": text,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	endpoint := cfg.BaseURL
	if endpoint == "" {
		return nil, fmt.Errorf("huggingface provider requires embeddingBaseUrl with full model endpoint")
	}

	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HuggingFace embeddings API error: %s", string(respBody))
	}

	var embedding []float64
	if err := json.Unmarshal(respBody, &embedding); err != nil {
		var nested [][]float64
		if err := json.Unmarshal(respBody, &nested); err != nil {
			return nil, fmt.Errorf("failed to parse HuggingFace response: %s", string(respBody))
		}
		if len(nested) == 0 {
			return nil, fmt.Errorf("no embedding returned from HuggingFace API")
		}
		embedding = nested[0]
	}

	return embedding, nil
}

func (e *PGVectorExecutor) generateGeminiEmbedding(ctx context.Context, text string, cfg embeddingConfig) ([]float64, error) {
	model := cfg.Model
	if model == "" {
		model = "text-embedding-004"
	}

	reqBody := map[string]interface{}{
		"content": map[string]interface{}{
			"parts": []map[string]interface{}{
				{"text": text},
			},
		},
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	endpoint := fmt.Sprintf("%s/models/%s:embedContent?key=%s", strings.TrimSuffix(cfg.BaseURL, "/"), model, cfg.APIKey)
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Gemini embeddings API error: %s", string(respBody))
	}

	var result struct {
		Embedding struct {
			Values []float64 `json:"values"`
		} `json:"embedding"`
	}

	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, err
	}

	if len(result.Embedding.Values) == 0 {
		return nil, fmt.Errorf("no embedding returned from Gemini API")
	}

	return result.Embedding.Values, nil
}

func (e *PGVectorExecutor) generateBatchGeminiEmbeddings(ctx context.Context, texts []string, cfg embeddingConfig) ([][]float64, error) {
	model := cfg.Model
	if model == "" {
		model = "text-embedding-004"
	}

	requests := make([]map[string]interface{}, len(texts))
	for i, text := range texts {
		requests[i] = map[string]interface{}{
			"content": map[string]interface{}{
				"parts": []map[string]interface{}{
					{"text": text},
				},
			},
		}
	}

	reqBody := map[string]interface{}{
		"requests": requests,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	endpoint := fmt.Sprintf("%s/models/%s:batchEmbedContents?key=%s", strings.TrimSuffix(cfg.BaseURL, "/"), model, cfg.APIKey)
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Gemini batch embeddings API error: %s", string(respBody))
	}

	var result struct {
		Embeddings []struct {
			Values []float64 `json:"values"`
		} `json:"embeddings"`
	}

	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, err
	}

	if len(result.Embeddings) != len(texts) {
		return nil, fmt.Errorf("expected %d embeddings, got %d", len(texts), len(result.Embeddings))
	}

	embeddings := make([][]float64, len(texts))
	for i, emb := range result.Embeddings {
		embeddings[i] = emb.Values
	}

	return embeddings, nil
}

func (e *PGVectorExecutor) generateBatchEmbeddings(ctx context.Context, texts []string, cfg embeddingConfig) ([][]float64, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	var embeddings [][]float64
	var lastErr error

	for attempt := 0; attempt < maxEmbeddingRetries; attempt++ {
		if attempt > 0 {
			// Calculate exponential backoff delay
			delay := initialRetryDelay * time.Duration(1<<uint(attempt-1))
			if delay > maxRetryDelay {
				delay = maxRetryDelay
			}

			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}

		var err error
		switch cfg.Provider {
		case "openai", "openai-compatible", "":
			embeddings, err = e.generateBatchOpenAIEmbeddings(ctx, texts, cfg)
		case "cohere":
			embeddings, err = e.generateBatchCohereEmbeddings(ctx, texts, cfg)
		case "gemini":
			embeddings, err = e.generateBatchGeminiEmbeddings(ctx, texts, cfg)
		default:
			embeddings = make([][]float64, len(texts))
			for i, text := range texts {
				emb, err := e.generateEmbedding(ctx, text, cfg)
				if err != nil {
					return nil, fmt.Errorf("failed to generate embedding for item %d: %w", i, err)
				}
				embeddings[i] = emb
			}
			return embeddings, nil
		}

		if err == nil {
			return embeddings, nil
		}

		lastErr = err

		// Check if error is retryable
		if !isRetryableError(err) {
			return nil, err
		}
	}

	return nil, fmt.Errorf("failed after %d retries: %w", maxEmbeddingRetries, lastErr)
}

// isRetryableError determines if an error should trigger a retry
func isRetryableError(err error) bool {
	if err == nil {
		return false
	}

	errMsg := err.Error()

	// Retryable errors:
	// - Rate limit (429)
	// - Timeout
	// - Temporary network errors
	// - Server errors (500, 502, 503, 504)
	retryablePatterns := []string{
		"429",
		"rate limit",
		"timeout",
		"temporary",
		"500",
		"502",
		"503",
		"504",
		"connection reset",
		"connection refused",
		"too many requests",
	}

	for _, pattern := range retryablePatterns {
		if strings.Contains(strings.ToLower(errMsg), strings.ToLower(pattern)) {
			return true
		}
	}

	return false
}

func (e *PGVectorExecutor) generateBatchOpenAIEmbeddings(ctx context.Context, texts []string, cfg embeddingConfig) ([][]float64, error) {
	model := cfg.Model
	if model == "" {
		model = "text-embedding-3-small"
	}

	reqBody := map[string]interface{}{
		"model": model,
		"input": texts,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	endpoint := strings.TrimSuffix(baseURL, "/") + "/embeddings"

	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("OpenAI batch embeddings API error: %s", string(respBody))
	}

	var result struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}

	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, err
	}

	if len(result.Data) != len(texts) {
		return nil, fmt.Errorf("expected %d embeddings, got %d", len(texts), len(result.Data))
	}

	embeddings := make([][]float64, len(texts))
	for _, item := range result.Data {
		if item.Index < len(embeddings) {
			embeddings[item.Index] = item.Embedding
		}
	}

	return embeddings, nil
}

func (e *PGVectorExecutor) generateBatchCohereEmbeddings(ctx context.Context, texts []string, cfg embeddingConfig) ([][]float64, error) {
	model := cfg.Model
	if model == "" {
		model = "embed-english-v3.0"
	}

	reqBody := map[string]interface{}{
		"model":      model,
		"texts":      texts,
		"input_type": "search_document",
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = "https://api.cohere.ai/v1"
	}
	endpoint := strings.TrimSuffix(baseURL, "/") + "/embed"

	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Cohere batch embeddings API error: %s", string(respBody))
	}

	var result struct {
		Embeddings [][]float64 `json:"embeddings"`
	}

	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, err
	}

	if len(result.Embeddings) != len(texts) {
		return nil, fmt.Errorf("expected %d embeddings, got %d", len(texts), len(result.Embeddings))
	}

	return result.Embeddings, nil
}

func (e *PGVectorExecutor) executeSearch(ctx context.Context, db *sql.DB, table, vectorColumn string, vector []float64, config map[string]interface{}, resolver TemplateResolver) (*StepResult, error) {
	topK := 10
	if k, ok := config["topK"].(float64); ok && k > 0 {
		topK = int(k)
	}

	distanceMetric := "cosine"
	if dm, ok := config["distanceMetric"].(string); ok && dm != "" {
		distanceMetric = dm
	}

	var distanceOp string
	switch distanceMetric {
	case "cosine":
		distanceOp = "<=>"
	case "l2":
		distanceOp = "<->"
	case "inner_product":
		distanceOp = "<#>"
	default:
		distanceOp = "<=>"
	}

	selectColumns := "*"
	if sc, ok := config["selectColumns"].(string); ok && sc != "" {
		selectColumns = sc
	}

	vectorConfigs, err := parseVectorConfigs(config)
	if err != nil {
		return nil, fmt.Errorf("failed to parse vector configs: %w", err)
	}

	embCfg := e.getEmbeddingConfig(config, resolver)
	queryVectors := make(map[string][]float64)

	if len(vectorConfigs) == 1 && vector != nil {
		queryVectors[vectorConfigs[0].Column] = vector
	} else {
		for _, vc := range vectorConfigs {
			var queryText string
			// Check 'query' field first (search operation uses this)
			if queryTemplate, ok := config["query"].(string); ok && queryTemplate != "" {
				queryText = resolver.ResolveString(queryTemplate)
			} else if textTemplate, ok := config["text"].(string); ok && textTemplate != "" {
				queryText = resolver.ResolveString(textTemplate)
			} else {
				queryText = resolver.ResolveString(vc.TextTemplate)
			}

			if queryText == "" {
				return nil, fmt.Errorf("search operation requires 'query' text for column '%s'", vc.Column)
			}

			vec, err := e.generateEmbedding(ctx, queryText, embCfg)
			if err != nil {
				return nil, fmt.Errorf("failed to generate embedding for column '%s': %w", vc.Column, err)
			}
			queryVectors[vc.Column] = vec
		}
	}

	distanceSelects := []string{}
	distanceColumns := []string{}
	distanceExprs := []string{}
	for _, vc := range vectorConfigs {
		vectorStr := vectorToString(queryVectors[vc.Column])
		distanceCol := fmt.Sprintf("distance_%s", vc.Column)
		distanceExpr := fmt.Sprintf("%s %s '%s'::vector", vc.Column, distanceOp, vectorStr)
		distanceSelects = append(distanceSelects, fmt.Sprintf("%s AS %s", distanceExpr, distanceCol))
		distanceColumns = append(distanceColumns, distanceCol)
		distanceExprs = append(distanceExprs, distanceExpr)
	}

	// Get aggregation method (default: minimum)
	aggregationMethod := "minimum"
	if am, ok := config["aggregationMethod"].(string); ok && am != "" {
		aggregationMethod = am
	}

	var aggregatedDistanceExpr string
	if len(distanceExprs) == 1 {
		aggregatedDistanceExpr = distanceExprs[0]
	} else {
		switch aggregationMethod {
		case "maximum":
			// Maximum distance (all must match closely)
			aggregatedDistanceExpr = fmt.Sprintf("GREATEST(%s)", strings.Join(distanceExprs, ", "))
		case "weighted":
			// Weighted average with optional custom weights
			weights := getWeightsMap(config, "weights")

			if len(weights) > 0 {
				// Custom weighted average
				var weightedTerms []string
				var totalWeight float64

				for i, vc := range vectorConfigs {
					weight := 1.0 // default weight
					if w, ok := weights[vc.Column]; ok {
						weight = w
					}
					weightedTerms = append(weightedTerms, fmt.Sprintf("(%f * %s)", weight, distanceExprs[i]))
					totalWeight += weight
				}

				if totalWeight == 0 {
					totalWeight = 1.0 // prevent division by zero
				}

				aggregatedDistanceExpr = fmt.Sprintf("(%s) / %f", strings.Join(weightedTerms, " + "), totalWeight)
			} else {
				// Equal weights (simple average)
				aggregatedDistanceExpr = fmt.Sprintf("(%s) / %d", strings.Join(distanceExprs, " + "), len(distanceExprs))
			}
		default: // "minimum"
			// Minimum distance (best match wins)
			aggregatedDistanceExpr = fmt.Sprintf("LEAST(%s)", strings.Join(distanceExprs, ", "))
		}
	}

	// Build query with proper parameterization for filter values
	var query string
	var queryArgs []interface{}
	paramIndex := 1

	if filter, ok := getConfigMap(config, "filter"); ok && len(filter) > 0 {
		conditions := []string{}
		for key, value := range filter {
			// Validate column name to prevent SQL injection
			if err := validateIdentifier(key); err != nil {
				return nil, fmt.Errorf("invalid filter column name '%s': %w", key, err)
			}

			resolvedValue := value
			if strVal, ok := value.(string); ok {
				resolvedValue = resolver.ResolveString(strVal)
			}

			// Use parameterized query for filter value
			conditions = append(conditions, fmt.Sprintf("%s = $%d", quoteIdentifier(key), paramIndex))
			queryArgs = append(queryArgs, resolvedValue)
			paramIndex++
		}
		query = fmt.Sprintf(
			"SELECT %s, %s, %s AS distance FROM %s WHERE %s ORDER BY distance LIMIT $%d",
			selectColumns, strings.Join(distanceSelects, ", "), aggregatedDistanceExpr, quoteIdentifier(table), strings.Join(conditions, " AND "), paramIndex,
		)
		queryArgs = append(queryArgs, topK)
	} else {
		query = fmt.Sprintf(
			"SELECT %s, %s, %s AS distance FROM %s ORDER BY distance LIMIT $1",
			selectColumns, strings.Join(distanceSelects, ", "), aggregatedDistanceExpr, quoteIdentifier(table),
		)
		queryArgs = []interface{}{topK}
	}

	rows, err := db.QueryContext(ctx, query, queryArgs...)
	if err != nil {
		return nil, fmt.Errorf("search query failed: %w", err)
	}
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}

	results := []map[string]interface{}{}
	for rows.Next() {
		values := make([]interface{}, len(columns))
		valuePtrs := make([]interface{}, len(columns))
		for i := range values {
			valuePtrs[i] = &values[i]
		}

		if err := rows.Scan(valuePtrs...); err != nil {
			return nil, err
		}

		row := make(map[string]interface{})
		for i, col := range columns {
			row[col] = values[i]
		}

		if dist, ok := row["distance"].(float64); ok {
			row["similarity"] = calculateSimilarity(dist, distanceMetric)
		}

		for _, vc := range vectorConfigs {
			distanceCol := fmt.Sprintf("distance_%s", vc.Column)
			if dist, ok := row[distanceCol].(float64); ok {
				similarityCol := fmt.Sprintf("similarity_%s", vc.Column)
				row[similarityCol] = calculateSimilarity(dist, distanceMetric)
			}
		}

		results = append(results, row)
	}

	return &StepResult{
		Output: map[string]interface{}{
			"results": results,
			"count":   len(results),
		},
	}, nil
}

func (e *PGVectorExecutor) executeInsert(ctx context.Context, db *sql.DB, table, vectorColumn string, vector []float64, config map[string]interface{}, resolver TemplateResolver) (*StepResult, error) {
	if vector == nil {
		return nil, fmt.Errorf("insert operation requires vector or text")
	}

	resolvedData, ok := getConfigMapWithResolver(config, "data", resolver)
	if !ok || len(resolvedData) == 0 {
		return nil, fmt.Errorf("insert operation requires 'data'")
	}

	// Validate table and column names
	if err := validateIdentifier(table); err != nil {
		return nil, fmt.Errorf("invalid table name: %w", err)
	}
	if err := validateIdentifier(vectorColumn); err != nil {
		return nil, fmt.Errorf("invalid vector column name: %w", err)
	}

	columns := []string{quoteIdentifier(vectorColumn)}
	placeholders := []string{fmt.Sprintf("'%s'::vector", vectorToString(vector))}
	values := []interface{}{}

	i := 1
	for key, value := range resolvedData {
		if err := validateIdentifier(key); err != nil {
			return nil, fmt.Errorf("invalid column name '%s': %w", key, err)
		}
		columns = append(columns, quoteIdentifier(key))
		placeholders = append(placeholders, fmt.Sprintf("$%d", i))
		values = append(values, value)
		i++
	}

	query := fmt.Sprintf(
		"INSERT INTO %s (%s) VALUES (%s) RETURNING *",
		quoteIdentifier(table), strings.Join(columns, ", "), strings.Join(placeholders, ", "),
	)

	row := db.QueryRowContext(ctx, query, values...)

	var inserted map[string]interface{}
	var id interface{}
	if err := row.Scan(&id); err != nil {
		query = fmt.Sprintf(
			"INSERT INTO %s (%s) VALUES (%s)",
			quoteIdentifier(table), strings.Join(columns, ", "), strings.Join(placeholders, ", "),
		)
		_, err := db.ExecContext(ctx, query, values...)
		if err != nil {
			return nil, fmt.Errorf("insert failed: %w", err)
		}
		inserted = resolvedData
	} else {
		inserted = map[string]interface{}{"id": id}
	}

	return &StepResult{
		Output: map[string]interface{}{
			"inserted": inserted,
			"success":  true,
		},
	}, nil
}

// getSortedKeys returns map keys in sorted order for deterministic iteration
func getSortedKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// quoteIdentifier quotes a PostgreSQL identifier (table or column name) to prevent SQL injection
func quoteIdentifier(name string) string {
	// PostgreSQL identifier quoting: double quotes and escape any existing double quotes
	// Only allow alphanumeric, underscore, and a few safe characters
	// This prevents SQL injection via malicious table/column names
	if len(name) == 0 {
		return `""`
	}
	// Escape any double quotes in the identifier
	escaped := strings.ReplaceAll(name, `"`, `""`)
	return `"` + escaped + `"`
}

// validateIdentifier checks if a string is a valid PostgreSQL identifier
// Returns error if it contains suspicious characters that could indicate SQL injection
func validateIdentifier(name string) error {
	if len(name) == 0 {
		return fmt.Errorf("identifier cannot be empty")
	}
	if len(name) > 63 {
		return fmt.Errorf("identifier too long (max 63 characters)")
	}
	// Allow: letters, digits, underscore, dash (common in vector column names like "embedding_mpn")
	// First character should not be a digit
	for i, r := range name {
		if i == 0 && (r >= '0' && r <= '9') {
			return fmt.Errorf("identifier cannot start with a digit")
		}
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_') {
			return fmt.Errorf("identifier contains invalid character: %c", r)
		}
	}
	return nil
}

func (e *PGVectorExecutor) executeBatchInsert(ctx context.Context, db *sql.DB, table, vectorColumn, textColumn string, items []map[string]interface{}, config map[string]interface{}, resolver TemplateResolver) (*StepResult, error) {
	if len(items) == 0 {
		return &StepResult{
			Output: map[string]interface{}{
				"inserted": []interface{}{},
				"count":    0,
				"success":  true,
			},
		}, nil
	}

	vectorConfigs, err := parseVectorConfigs(config)
	if err != nil {
		return nil, fmt.Errorf("failed to parse vector configs: %w", err)
	}

	embCfg := e.getEmbeddingConfig(config, resolver)
	allEmbeddings := make(map[string][][]float64)

	for _, vc := range vectorConfigs {
		texts := make([]string, len(items))
		for i, item := range items {
			text := resolveTemplateWithItem(vc.TextTemplate, item, resolver)
			texts[i] = text
		}

		embeddings, err := e.generateBatchEmbeddings(ctx, texts, embCfg)
		if err != nil {
			return nil, fmt.Errorf("batch embedding generation failed for column '%s': %w", vc.Column, err)
		}
		allEmbeddings[vc.Column] = embeddings
	}

	// Validate table and vector column names
	if err := validateIdentifier(table); err != nil {
		return nil, fmt.Errorf("invalid table name: %w", err)
	}

	// Build column names from first item + vector columns (with validation and quoting)
	var columns []string
	for _, vc := range vectorConfigs {
		if err := validateIdentifier(vc.Column); err != nil {
			return nil, fmt.Errorf("invalid vector column name '%s': %w", vc.Column, err)
		}
		columns = append(columns, quoteIdentifier(vc.Column))
	}

	// Get data columns from first item in sorted order for deterministic column ordering
	var dataColumns []string
	if len(items) > 0 {
		dataColumns = getSortedKeys(items[0])
		for _, key := range dataColumns {
			if err := validateIdentifier(key); err != nil {
				return nil, fmt.Errorf("invalid data column name '%s': %w", key, err)
			}
			columns = append(columns, quoteIdentifier(key))
		}
	}

	// Build VALUES clause for bulk insert
	var valueGroups []string
	var allValues []interface{}
	paramIndex := 1

	for i, item := range items {
		var placeholders []string

		// Add vector column placeholders (as literal vectors, not params)
		for _, vc := range vectorConfigs {
			vector := allEmbeddings[vc.Column][i]
			placeholders = append(placeholders, fmt.Sprintf("'%s'::vector", vectorToString(vector)))
		}

		// Add data column placeholders (as parameters) - use sorted order
		for _, key := range dataColumns {
			placeholders = append(placeholders, fmt.Sprintf("$%d", paramIndex))
			allValues = append(allValues, item[key])
			paramIndex++
		}

		valueGroups = append(valueGroups, fmt.Sprintf("(%s)", strings.Join(placeholders, ", ")))
	}

	// Execute single bulk INSERT
	query := fmt.Sprintf(
		"INSERT INTO %s (%s) VALUES %s",
		quoteIdentifier(table),
		strings.Join(columns, ", "),
		strings.Join(valueGroups, ", "),
	)

	_, err = db.ExecContext(ctx, query, allValues...)
	if err != nil {
		return nil, fmt.Errorf("bulk insert failed: %w", err)
	}

	return &StepResult{
		Output: map[string]interface{}{
			"inserted": items,
			"count":    len(items),
			"success":  true,
		},
	}, nil
}

func (e *PGVectorExecutor) executeBatchUpsert(ctx context.Context, db *sql.DB, table, vectorColumn, textColumn, conflictColumn string, items []map[string]interface{}, config map[string]interface{}, resolver TemplateResolver) (*StepResult, error) {
	if len(items) == 0 {
		return &StepResult{
			Output: map[string]interface{}{
				"upserted": []interface{}{},
				"count":    0,
				"success":  true,
			},
		}, nil
	}

	vectorConfigs, err := parseVectorConfigs(config)
	if err != nil {
		return nil, fmt.Errorf("failed to parse vector configs: %w", err)
	}

	embCfg := e.getEmbeddingConfig(config, resolver)
	allEmbeddings := make(map[string][][]float64)

	for _, vc := range vectorConfigs {
		texts := make([]string, len(items))
		for i, item := range items {
			text := resolveTemplateWithItem(vc.TextTemplate, item, resolver)
			texts[i] = text
		}

		embeddings, err := e.generateBatchEmbeddings(ctx, texts, embCfg)
		if err != nil {
			return nil, fmt.Errorf("batch embedding generation failed for column '%s': %w", vc.Column, err)
		}
		allEmbeddings[vc.Column] = embeddings
	}

	// Validate table and conflict column names
	if err := validateIdentifier(table); err != nil {
		return nil, fmt.Errorf("invalid table name: %w", err)
	}
	if err := validateIdentifier(conflictColumn); err != nil {
		return nil, fmt.Errorf("invalid conflict column name: %w", err)
	}

	// Build column names from first item + vector columns (with validation and quoting)
	var columns []string
	for _, vc := range vectorConfigs {
		if err := validateIdentifier(vc.Column); err != nil {
			return nil, fmt.Errorf("invalid vector column name '%s': %w", vc.Column, err)
		}
		columns = append(columns, quoteIdentifier(vc.Column))
	}

	// Get data columns from first item in sorted order for deterministic column ordering
	var dataColumns []string
	if len(items) > 0 {
		dataColumns = getSortedKeys(items[0])
		for _, key := range dataColumns {
			if err := validateIdentifier(key); err != nil {
				return nil, fmt.Errorf("invalid data column name '%s': %w", key, err)
			}
			columns = append(columns, quoteIdentifier(key))
		}
	}

	// Build UPDATE SET clause for ON CONFLICT
	var updates []string
	for _, vc := range vectorConfigs {
		quotedCol := quoteIdentifier(vc.Column)
		updates = append(updates, fmt.Sprintf("%s = EXCLUDED.%s", quotedCol, quotedCol))
	}
	for _, key := range dataColumns {
		if key != conflictColumn {
			quotedKey := quoteIdentifier(key)
			updates = append(updates, fmt.Sprintf("%s = EXCLUDED.%s", quotedKey, quotedKey))
		}
	}

	// Build VALUES clause for bulk upsert
	var valueGroups []string
	var allValues []interface{}
	paramIndex := 1

	for i, item := range items {
		var placeholders []string

		// Add vector column placeholders (as literal vectors)
		for _, vc := range vectorConfigs {
			vector := allEmbeddings[vc.Column][i]
			placeholders = append(placeholders, fmt.Sprintf("'%s'::vector", vectorToString(vector)))
		}

		// Add data column placeholders (as parameters) - use sorted order
		for _, key := range dataColumns {
			placeholders = append(placeholders, fmt.Sprintf("$%d", paramIndex))
			allValues = append(allValues, item[key])
			paramIndex++
		}

		valueGroups = append(valueGroups, fmt.Sprintf("(%s)", strings.Join(placeholders, ", ")))
	}

	// Execute single bulk UPSERT
	query := fmt.Sprintf(
		"INSERT INTO %s (%s) VALUES %s ON CONFLICT (%s) DO UPDATE SET %s",
		quoteIdentifier(table),
		strings.Join(columns, ", "),
		strings.Join(valueGroups, ", "),
		quoteIdentifier(conflictColumn),
		strings.Join(updates, ", "),
	)

	_, err = db.ExecContext(ctx, query, allValues...)
	if err != nil {
		return nil, fmt.Errorf("bulk upsert failed: %w", err)
	}

	return &StepResult{
		Output: map[string]interface{}{
			"upserted": items,
			"count":    len(items),
			"success":  true,
		},
	}, nil
}

func (e *PGVectorExecutor) executeUpsert(ctx context.Context, db *sql.DB, table, vectorColumn string, vector []float64, config map[string]interface{}, resolver TemplateResolver) (*StepResult, error) {
	if vector == nil {
		return nil, fmt.Errorf("upsert operation requires vector or text")
	}

	resolvedData, ok := getConfigMapWithResolver(config, "upsertData", resolver)
	if !ok || len(resolvedData) == 0 {
		resolvedData, ok = getConfigMapWithResolver(config, "data", resolver)
	}
	if !ok || len(resolvedData) == 0 {
		return nil, fmt.Errorf("upsert operation requires 'upsertData'")
	}

	conflictColumn := "id"
	if cc, ok := config["conflictColumn"].(string); ok && cc != "" {
		conflictColumn = cc
	}

	// Validate identifiers
	if err := validateIdentifier(table); err != nil {
		return nil, fmt.Errorf("invalid table name: %w", err)
	}
	if err := validateIdentifier(vectorColumn); err != nil {
		return nil, fmt.Errorf("invalid vector column name: %w", err)
	}
	if err := validateIdentifier(conflictColumn); err != nil {
		return nil, fmt.Errorf("invalid conflict column name: %w", err)
	}

	columns := []string{quoteIdentifier(vectorColumn)}
	placeholders := []string{fmt.Sprintf("'%s'::vector", vectorToString(vector))}
	quotedVecCol := quoteIdentifier(vectorColumn)
	updates := []string{fmt.Sprintf("%s = EXCLUDED.%s", quotedVecCol, quotedVecCol)}
	values := []interface{}{}

	i := 1
	for key, value := range resolvedData {
		if err := validateIdentifier(key); err != nil {
			return nil, fmt.Errorf("invalid column name '%s': %w", key, err)
		}
		quotedKey := quoteIdentifier(key)
		columns = append(columns, quotedKey)
		placeholders = append(placeholders, fmt.Sprintf("$%d", i))
		if key != conflictColumn {
			updates = append(updates, fmt.Sprintf("%s = EXCLUDED.%s", quotedKey, quotedKey))
		}
		values = append(values, value)
		i++
	}

	query := fmt.Sprintf(
		"INSERT INTO %s (%s) VALUES (%s) ON CONFLICT (%s) DO UPDATE SET %s",
		quoteIdentifier(table), strings.Join(columns, ", "), strings.Join(placeholders, ", "),
		quoteIdentifier(conflictColumn), strings.Join(updates, ", "),
	)

	_, err := db.ExecContext(ctx, query, values...)
	if err != nil {
		return nil, fmt.Errorf("upsert failed: %w", err)
	}

	return &StepResult{
		Output: map[string]interface{}{
			"upserted": resolvedData,
			"success":  true,
		},
	}, nil
}

func (e *PGVectorExecutor) executeDelete(ctx context.Context, db *sql.DB, table string, config map[string]interface{}, resolver TemplateResolver) (*StepResult, error) {
	filter, ok := getConfigMap(config, "deleteFilter")
	if !ok || len(filter) == 0 {
		filter, ok = getConfigMap(config, "filter")
	}
	if !ok || len(filter) == 0 {
		return nil, fmt.Errorf("delete operation requires 'deleteFilter' to prevent accidental full table deletion")
	}

	conditions := []string{}
	values := []interface{}{}
	i := 1
	for key, value := range filter {
		resolvedValue := value
		if strVal, ok := value.(string); ok {
			resolvedValue = resolver.ResolveString(strVal)
		}
		conditions = append(conditions, fmt.Sprintf("%s = $%d", key, i))
		values = append(values, resolvedValue)
		i++
	}

	query := fmt.Sprintf("DELETE FROM %s WHERE %s", table, strings.Join(conditions, " AND "))

	result, err := db.ExecContext(ctx, query, values...)
	if err != nil {
		return nil, fmt.Errorf("delete failed: %w", err)
	}

	rowsAffected, _ := result.RowsAffected()

	return &StepResult{
		Output: map[string]interface{}{
			"deleted":      rowsAffected,
			"success":      true,
			"rowsAffected": rowsAffected,
		},
	}, nil
}

func parseVector(v interface{}) ([]float64, error) {
	switch vec := v.(type) {
	case []float64:
		return vec, nil
	case []interface{}:
		result := make([]float64, len(vec))
		for i, val := range vec {
			switch n := val.(type) {
			case float64:
				result[i] = n
			case int:
				result[i] = float64(n)
			default:
				return nil, fmt.Errorf("invalid vector element type at index %d", i)
			}
		}
		return result, nil
	case string:
		var arr []float64
		if err := json.Unmarshal([]byte(vec), &arr); err != nil {
			return nil, fmt.Errorf("failed to parse vector string: %w", err)
		}
		return arr, nil
	default:
		return nil, fmt.Errorf("invalid vector type: %T", v)
	}
}

func vectorToString(vector []float64) string {
	parts := make([]string, len(vector))
	for i, v := range vector {
		parts[i] = fmt.Sprintf("%f", v)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func getConfigMap(config map[string]interface{}, key string) (map[string]interface{}, bool) {
	val, exists := config[key]
	if !exists {
		return nil, false
	}

	if m, ok := val.(map[string]interface{}); ok {
		return m, true
	}

	if s, ok := val.(string); ok && s != "" {
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(s), &m); err == nil {
			return m, true
		}
	}

	return nil, false
}

func getConfigMapWithResolver(config map[string]interface{}, key string, resolver TemplateResolver) (map[string]interface{}, bool) {
	val, exists := config[key]
	if !exists {
		return nil, false
	}

	if m, ok := val.(map[string]interface{}); ok {
		return resolver.ResolveMap(m), true
	}

	if s, ok := val.(string); ok && s != "" {
		resolved := resolver.ResolveString(s)
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(resolved), &m); err == nil {
			return m, true
		}
	}

	return nil, false
}

// getWeightsMap parses the weights configuration and returns a map of column names to weight values
func getWeightsMap(config map[string]interface{}, key string) map[string]float64 {
	result := make(map[string]float64)

	val, exists := config[key]
	if !exists {
		return result
	}

	// Handle map[string]interface{} (from JSON)
	if m, ok := val.(map[string]interface{}); ok {
		for column, weight := range m {
			if w, ok := weight.(float64); ok {
				result[column] = w
			} else if w, ok := weight.(int); ok {
				result[column] = float64(w)
			}
		}
		return result
	}

	// Handle JSON string
	if s, ok := val.(string); ok && s != "" {
		var m map[string]float64
		if err := json.Unmarshal([]byte(s), &m); err == nil {
			return m
		}
	}

	return result
}

func calculateSimilarity(distance float64, metric string) float64 {
	switch metric {
	case "cosine":
		return 1 - distance
	case "l2":
		return 1 / (1 + distance)
	case "inner_product":
		return -distance
	default:
		return 1 - distance
	}
}

func resolveTemplateWithItem(template string, item map[string]interface{}, resolver TemplateResolver) string {
	result := template
	for key, value := range item {
		placeholder := "{{." + key + "}}"
		if strVal, ok := value.(string); ok {
			result = strings.ReplaceAll(result, placeholder, strVal)
		} else {
			jsonBytes, _ := json.Marshal(value)
			result = strings.ReplaceAll(result, placeholder, string(jsonBytes))
		}
	}
	result = resolver.ResolveString(result)
	return result
}

// chunkText splits text into smaller chunks based on configuration
func chunkText(text string, chunkSize int, overlap int) []string {
	if chunkSize <= 0 {
		chunkSize = 1000 // default chunk size
	}
	if overlap < 0 {
		overlap = 0
	}
	if overlap >= chunkSize {
		overlap = chunkSize / 2
	}

	if len(text) <= chunkSize {
		return []string{text}
	}

	chunks := []string{}
	start := 0
	textLen := len(text)

	for start < textLen {
		end := start + chunkSize
		if end > textLen {
			end = textLen
		}

		// Try to break at sentence or word boundary
		if end < textLen {
			// Look for sentence ending (.!?) in the last 20% of the chunk
			searchStart := start + (chunkSize * 4 / 5)
			if searchStart < end {
				for i := end - 1; i >= searchStart; i-- {
					if text[i] == '.' || text[i] == '!' || text[i] == '?' {
						if i+1 < textLen && (text[i+1] == ' ' || text[i+1] == '\n') {
							end = i + 1
							break
						}
					}
				}
			}

			// If no sentence boundary, try word boundary
			if end == start+chunkSize {
				for i := end - 1; i >= searchStart; i-- {
					if text[i] == ' ' || text[i] == '\n' {
						end = i
						break
					}
				}
			}
		}

		chunk := strings.TrimSpace(text[start:end])
		if chunk != "" {
			chunks = append(chunks, chunk)
		}

		// Move start position forward, accounting for overlap
		if overlap > 0 && end < textLen {
			start = end - overlap
		} else {
			start = end
		}

		// Avoid infinite loop if we're not making progress
		if start <= end-chunkSize+overlap {
			start = end
		}
	}

	return chunks
}
