/*
 * Copyright (c) 2025. Axiom Studio
 */

package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"
)

// MemoryToolExecutor implements long-term memory using pluggable storage
type MemoryToolExecutor struct {
	def       *ToolDefinition
	resolver  TemplateResolver
	client    *http.Client
	store     MemoryStore
	storeMu   sync.Mutex
	useInMemory bool
}

// NewMemoryToolExecutor creates a new memory tool executor
func NewMemoryToolExecutor(def *ToolDefinition, resolver TemplateResolver) (ToolExecutor, error) {
	useInMemory := false
	if v, ok := def.Config["useInMemory"].(bool); ok {
		useInMemory = v
	}
	// Also check environment variable
	if os.Getenv("MEMORY_TOOL_IN_MEMORY") == "true" {
		useInMemory = true
	}

	executor := &MemoryToolExecutor{
		def:         def,
		resolver:    resolver,
		client:      &http.Client{Timeout: 60 * time.Second},
		useInMemory: useInMemory,
	}

	// Initialize store
	if useInMemory {
		executor.store = NewInMemoryStore()
	}

	return executor, nil
}

// Execute performs the memory operation
func (e *MemoryToolExecutor) Execute(ctx context.Context, args map[string]interface{}) (*ToolResult, error) {
	// Get operation type
	operation := "save"
	if op, ok := args["operation"].(string); ok && op != "" {
		operation = op
	}

	// Initialize store if not already done
	if err := e.initStore(ctx); err != nil {
		return nil, fmt.Errorf("failed to initialize memory store: %w", err)
	}

	switch operation {
	case "save":
		return e.executeSave(ctx, args)
	case "recall":
		return e.executeRecall(ctx, args)
	case "forget":
		return e.executeForget(ctx, args)
	case "list":
		return e.executeList(ctx, args)
	default:
		return nil, fmt.Errorf("unknown memory operation: %s", operation)
	}
}

// initStore initializes the memory store (lazy initialization)
func (e *MemoryToolExecutor) initStore(ctx context.Context) error {
	e.storeMu.Lock()
	defer e.storeMu.Unlock()

	if e.store != nil {
		return nil
	}

	if e.useInMemory {
		e.store = NewInMemoryStore()
		return nil
	}

	// Use pgvector store
	connStr := e.getConnectionString()
	if connStr == "" {
		return fmt.Errorf("memory tool requires a database connection. Configure MEMORY_DB_URL, attach a PGVector service, or set useInMemory=true")
	}

	tableName := e.getTableName()
	store, err := NewPGVectorStore(connStr, tableName)
	if err != nil {
		return fmt.Errorf("failed to connect to memory database: %w", err)
	}

	if err := store.EnsureTable(ctx); err != nil {
		store.Close()
		return fmt.Errorf("failed to initialize memory table: %w", err)
	}

	e.store = store
	return nil
}

// executeSave stores a memory with embedding
func (e *MemoryToolExecutor) executeSave(ctx context.Context, args map[string]interface{}) (*ToolResult, error) {
	content := ""
	if c, ok := args["memory"].(string); ok {
		content = c
	}
	if content == "" {
		return nil, fmt.Errorf("save operation requires 'memory' content")
	}

	// Generate key if not provided
	key := ""
	if k, ok := args["key"].(string); ok && k != "" {
		key = k
	} else {
		// Generate key from content hash
		key = generateKeyFromContent(content)
	}

	scope := "session"
	if s, ok := args["scope"].(string); ok && s != "" {
		scope = s
	}

	// Get session/user IDs from context or config
	sessionID := e.getSessionID()
	userID := e.getUserID()

	// Parse metadata
	metadata := make(map[string]interface{})
	if m, ok := args["metadata"].(map[string]interface{}); ok {
		metadata = m
	}

	// Generate embedding for content
	embedding, err := e.generateEmbedding(ctx, content)
	if err != nil {
		return nil, fmt.Errorf("failed to generate embedding: %w", err)
	}

	// Create memory record
	record := &MemoryRecord{
		Key:       key,
		Content:   content,
		Embedding: embedding,
		Scope:     scope,
		SessionID: sessionID,
		UserID:    userID,
		Metadata:  metadata,
	}

	if err := e.store.Save(ctx, record); err != nil {
		return nil, fmt.Errorf("failed to save memory: %w", err)
	}

	return &ToolResult{
		Name: "memory_save",
		Result: map[string]interface{}{
			"id":      record.ID,
			"key":     key,
			"scope":   scope,
			"message": "Memory saved successfully",
		},
	}, nil
}

// executeRecall searches for similar memories
func (e *MemoryToolExecutor) executeRecall(ctx context.Context, args map[string]interface{}) (*ToolResult, error) {
	query := ""
	if q, ok := args["memory"].(string); ok {
		query = q
	}
	if query == "" {
		return nil, fmt.Errorf("recall operation requires 'memory' query text")
	}

	limit := 5
	if l, ok := args["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}
	if l, ok := args["limit"].(int); ok && l > 0 {
		limit = l
	}

	scope := ""
	if s, ok := args["scope"].(string); ok && s != "" {
		scope = s
	}

	sessionID := e.getSessionID()
	userID := e.getUserID()

	// Generate embedding for query
	embedding, err := e.generateEmbedding(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("failed to generate query embedding: %w", err)
	}

	// Search memories
	results, err := e.store.Recall(ctx, query, embedding, scope, sessionID, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to recall memories: %w", err)
	}

	memories := make([]map[string]interface{}, 0, len(results))
	for _, r := range results {
		memories = append(memories, map[string]interface{}{
			"id":         r.ID,
			"key":        r.Key,
			"content":    r.Content,
			"scope":      r.Scope,
			"similarity": r.Similarity,
			"metadata":   r.Metadata,
			"created_at": r.CreatedAt,
		})
	}

	return &ToolResult{
		Name: "memory_recall",
		Result: map[string]interface{}{
			"query":    query,
			"count":    len(memories),
			"memories": memories,
		},
	}, nil
}

// executeForget deletes memories
func (e *MemoryToolExecutor) executeForget(ctx context.Context, args map[string]interface{}) (*ToolResult, error) {
	key := ""
	if k, ok := args["key"].(string); ok && k != "" {
		key = k
	}

	scope := ""
	if s, ok := args["scope"].(string); ok && s != "" {
		scope = s
	}

	sessionID := e.getSessionID()
	userID := e.getUserID()

	deleted, err := e.store.Forget(ctx, key, scope, sessionID, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to forget memory: %w", err)
	}

	return &ToolResult{
		Name: "memory_forget",
		Result: map[string]interface{}{
			"deleted": deleted,
			"message": fmt.Sprintf("Deleted %d memories", deleted),
		},
	}, nil
}

// executeList lists all memories for the current scope
func (e *MemoryToolExecutor) executeList(ctx context.Context, args map[string]interface{}) (*ToolResult, error) {
	scope := ""
	if s, ok := args["scope"].(string); ok && s != "" {
		scope = s
	}

	sessionID := e.getSessionID()
	userID := e.getUserID()

	limit := 100
	if l, ok := args["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}

	records, err := e.store.List(ctx, scope, sessionID, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to list memories: %w", err)
	}

	memories := make([]map[string]interface{}, 0, len(records))
	for _, r := range records {
		memories = append(memories, map[string]interface{}{
			"id":         r.ID,
			"key":        r.Key,
			"content":    r.Content,
			"scope":      r.Scope,
			"metadata":   r.Metadata,
			"created_at": r.CreatedAt,
		})
	}

	return &ToolResult{
		Name: "memory_list",
		Result: map[string]interface{}{
			"count":    len(memories),
			"memories": memories,
		},
	}, nil
}

// getConnectionString returns the database connection string
func (e *MemoryToolExecutor) getConnectionString() string {
	// Check config first
	if connStr, ok := e.def.Config["connectionString"].(string); ok && connStr != "" {
		return e.resolver.ResolveString(connStr)
	}
	// Check environment
	if connStr := os.Getenv("MEMORY_DB_URL"); connStr != "" {
		return connStr
	}
	// Check for attached PGVector service
	if connStr := os.Getenv("PGVECTOR_CONNECTION_STRING"); connStr != "" {
		return connStr
	}
	return ""
}

// getTableName returns the table name for memories
func (e *MemoryToolExecutor) getTableName() string {
	if table, ok := e.def.Config["tableName"].(string); ok && table != "" {
		return table
	}
	return "agent_memories"
}

// getSessionID returns the current session ID
func (e *MemoryToolExecutor) getSessionID() string {
	if id, ok := e.def.Config["sessionId"].(string); ok && id != "" {
		return id
	}
	if id := os.Getenv("AGENT_SESSION_ID"); id != "" {
		return id
	}
	return "default"
}

// getUserID returns the current user ID
func (e *MemoryToolExecutor) getUserID() string {
	if id, ok := e.def.Config["userId"].(string); ok && id != "" {
		return id
	}
	return os.Getenv("AGENT_USER_ID")
}

// getEmbeddingConfig returns the embedding configuration
func (e *MemoryToolExecutor) getEmbeddingConfig() (provider, model, apiKey, baseURL string) {
	// Provider
	if p, ok := e.def.Config["embeddingProvider"].(string); ok && p != "" {
		provider = p
	} else {
		provider = "openai"
	}

	// Model
	if m, ok := e.def.Config["embeddingModel"].(string); ok && m != "" {
		model = m
	} else {
		model = "text-embedding-3-small"
	}

	// API Key - check config first, then environment
	if k, ok := e.def.Config["embeddingApiKey"].(string); ok && k != "" {
		apiKey = e.resolver.ResolveString(k)
	} else {
		apiKey = os.Getenv("OPENAI_API_KEY")
	}

	// Base URL (optional)
	if u, ok := e.def.Config["embeddingBaseUrl"].(string); ok && u != "" {
		baseURL = e.resolver.ResolveString(u)
	}

	return
}

// generateEmbedding creates an embedding for text
func (e *MemoryToolExecutor) generateEmbedding(ctx context.Context, text string) ([]float64, error) {
	// For in-memory mode with testing, generate deterministic fake embeddings
	if e.useInMemory {
		return generateFakeEmbedding(text), nil
	}

	provider, model, apiKey, baseURL := e.getEmbeddingConfig()

	if apiKey == "" {
		return nil, fmt.Errorf("embedding API key required - set embeddingApiKey config or %s_API_KEY environment variable", provider)
	}

	// Set default base URL based on provider
	if baseURL == "" {
		switch provider {
		case "openai", "openai-compatible":
			baseURL = "https://api.openai.com/v1"
		case "cohere":
			baseURL = "https://api.cohere.ai/v1"
		case "gemini":
			baseURL = "https://generativelanguage.googleapis.com/v1beta"
		case "huggingface":
			baseURL = "https://api-inference.huggingface.co/models"
		}
	}

	// Generate embedding based on provider
	switch provider {
	case "cohere":
		return e.generateCohereEmbedding(ctx, text, model, apiKey, baseURL)
	case "gemini":
		return e.generateGeminiEmbedding(ctx, text, model, apiKey, baseURL)
	case "huggingface":
		return e.generateHuggingFaceEmbedding(ctx, text, model, apiKey, baseURL)
	default:
		return e.generateOpenAIEmbedding(ctx, text, model, apiKey, baseURL)
	}
}

// generateOpenAIEmbedding generates embedding using OpenAI API
func (e *MemoryToolExecutor) generateOpenAIEmbedding(ctx context.Context, text, model, apiKey, baseURL string) ([]float64, error) {
	reqBody := map[string]interface{}{
		"input": text,
		"model": model,
	}

	jsonBody, _ := json.Marshal(reqBody)
	endpoint := baseURL + "/embeddings"
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewBuffer(jsonBody))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		var errResp struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		json.NewDecoder(resp.Body).Decode(&errResp)
		return nil, fmt.Errorf("OpenAI embedding API error: %s", errResp.Error.Message)
	}

	var result struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	if len(result.Data) == 0 {
		return nil, fmt.Errorf("no embedding returned")
	}

	return result.Data[0].Embedding, nil
}

// generateCohereEmbedding generates embedding using Cohere API
func (e *MemoryToolExecutor) generateCohereEmbedding(ctx context.Context, text, model, apiKey, baseURL string) ([]float64, error) {
	reqBody := map[string]interface{}{
		"texts": []string{text},
		"model": model,
	}

	jsonBody, _ := json.Marshal(reqBody)
	req, err := http.NewRequestWithContext(ctx, "POST", baseURL+"/embed", bytes.NewBuffer(jsonBody))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("Cohere embedding API returned status %d", resp.StatusCode)
	}

	var result struct {
		Embeddings [][]float64 `json:"embeddings"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	if len(result.Embeddings) == 0 {
		return nil, fmt.Errorf("no embedding returned")
	}

	return result.Embeddings[0], nil
}

// generateGeminiEmbedding generates embedding using Gemini API
func (e *MemoryToolExecutor) generateGeminiEmbedding(ctx context.Context, text, model, apiKey, baseURL string) ([]float64, error) {
	reqBody := map[string]interface{}{
		"content": map[string]interface{}{
			"parts": []map[string]string{
				{"text": text},
			},
		},
	}

	jsonBody, _ := json.Marshal(reqBody)
	endpoint := fmt.Sprintf("%s/models/%s:embedContent?key=%s", baseURL, model, apiKey)
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewBuffer(jsonBody))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("Gemini embedding API returned status %d", resp.StatusCode)
	}

	var result struct {
		Embedding struct {
			Values []float64 `json:"values"`
		} `json:"embedding"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	return result.Embedding.Values, nil
}

// generateHuggingFaceEmbedding generates embedding using HuggingFace API
func (e *MemoryToolExecutor) generateHuggingFaceEmbedding(ctx context.Context, text, model, apiKey, baseURL string) ([]float64, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", baseURL+"/"+model, bytes.NewBuffer([]byte(text)))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HuggingFace embedding API returned status %d", resp.StatusCode)
	}

	var result [][]float64
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	if len(result) == 0 {
		return nil, fmt.Errorf("no embedding returned")
	}

	return result[0], nil
}

// generateFakeEmbedding creates a deterministic fake embedding for testing
// This is only used in in-memory mode for testing purposes
func generateFakeEmbedding(text string) []float64 {
	// Create a 1536-dimensional embedding (same as OpenAI text-embedding-3-small)
	embedding := make([]float64, 1536)

	// Use simple hash-based approach for deterministic results
	for i := range embedding {
		// Mix character values to create variation
		charSum := 0
		for j, c := range text {
			charSum += int(c) * (j + 1)
		}
		// Create a pseudo-random but deterministic value
		embedding[i] = float64((charSum*(i+1))%1000) / 1000.0
	}

	// Normalize the vector
	var norm float64
	for _, v := range embedding {
		norm += v * v
	}
	norm = sqrt(norm)
	if norm > 0 {
		for i := range embedding {
			embedding[i] /= norm
		}
	}

	return embedding
}

func sqrt(x float64) float64 {
	if x <= 0 {
		return 0
	}
	// Newton's method
	z := x
	for i := 0; i < 100; i++ {
		z = z - (z*z-x)/(2*z)
	}
	return z
}

// generateKeyFromContent generates a short key from content hash
func generateKeyFromContent(content string) string {
	hash := sha256.Sum256([]byte(content))
	return hex.EncodeToString(hash[:8])
}
