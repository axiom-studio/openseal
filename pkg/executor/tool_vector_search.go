package executor

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// VectorSearchToolExecutor implements vector similarity search as a tool
type VectorSearchToolExecutor struct {
	def      *ToolDefinition
	resolver TemplateResolver
	client   *http.Client
	connPool map[string]*sql.DB
	mu       sync.RWMutex
}

// NewVectorSearchToolExecutor creates a new vector search tool executor
func NewVectorSearchToolExecutor(def *ToolDefinition, resolver TemplateResolver) (ToolExecutor, error) {
	return &VectorSearchToolExecutor{
		def:      def,
		resolver: resolver,
		client:   &http.Client{Timeout: 60 * time.Second},
		connPool: make(map[string]*sql.DB),
	}, nil
}

// parseVectorConfigs parses vector configurations from config
func (e *VectorSearchToolExecutor) parseVectorConfigs(config map[string]interface{}) ([]VectorConfig, error) {
	// Check for vectorColumns array (just column names - preferred format)
	if vectorColumns, ok := config["vectorColumns"].([]interface{}); ok && len(vectorColumns) > 0 {
		configs := make([]VectorConfig, 0, len(vectorColumns))
		for _, vc := range vectorColumns {
			column, ok := vc.(string)
			if !ok || column == "" {
				continue
			}
			configs = append(configs, VectorConfig{
				Column:       column,
				TextTemplate: "",
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

	// Check for vectorConfigs array format (with textTemplates - alternative)
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

			textTemplate := ""
			if tt, ok := vcMap["textTemplate"].(string); ok {
				textTemplate = tt
			}

			configs = append(configs, VectorConfig{
				Column:       column,
				TextTemplate: textTemplate,
			})
		}
		return configs, nil
	}

	// Fallback to single vector column for backward compatibility
	embeddingColumn := "embedding"
	if ec, ok := config["vectorColumn"].(string); ok && ec != "" {
		embeddingColumn = ec
	} else if ec, ok := config["embeddingColumn"].(string); ok && ec != "" {
		embeddingColumn = ec
	}

	return []VectorConfig{
		{
			Column:       embeddingColumn,
			TextTemplate: "",
		},
	}, nil
}

// Execute runs the vector search with the given arguments
func (e *VectorSearchToolExecutor) Execute(ctx context.Context, args map[string]interface{}) (*ToolResult, error) {
	config := e.def.Config

	// Get connection string from config
	connStr := ""
	if cs, ok := config["connectionString"].(string); ok && cs != "" {
		connStr = e.resolver.ResolveString(cs)
	}
	if connStr == "" {
		return &ToolResult{
			Name:  e.def.Name,
			Error: "connectionString is required in tool config",
		}, nil
	}

	// Get table name (support both 'table' and 'tableName' for compatibility)
	tableName := ""
	if tn, ok := config["table"].(string); ok && tn != "" {
		tableName = tn
	} else if tn, ok := config["tableName"].(string); ok && tn != "" {
		tableName = tn
	}
	if tableName == "" {
		return &ToolResult{
			Name:  e.def.Name,
			Error: "table is required in tool config",
		}, nil
	}

	// Get search mode (simple or advanced)
	searchMode := "simple"
	if sm, ok := config["searchMode"].(string); ok && sm != "" {
		searchMode = sm
	}

	// Get query text from arguments (for simple mode only)
	query := ""
	if q, ok := args["query"].(string); ok {
		query = q
	}

	// Get per-field queries from arguments (for advanced mode)
	var fieldQueries map[string]string
	if searchMode == "advanced" {
		if fq, ok := args["queries"].(map[string]interface{}); ok {
			fieldQueries = make(map[string]string)
			for k, v := range fq {
				if str, ok := v.(string); ok {
					fieldQueries[k] = str
				}
			}
		}
	}

	// Validate required arguments based on mode
	if searchMode == "simple" && query == "" {
		return &ToolResult{
			Name:  e.def.Name,
			Error: "query argument is required for simple mode",
		}, nil
	}
	if searchMode == "advanced" && len(fieldQueries) == 0 {
		// Provide more helpful error message
		if queriesVal, exists := args["queries"]; exists {
			return &ToolResult{
				Name:  e.def.Name,
				Error: fmt.Sprintf("queries argument must be an object mapping column names to queries, got %T. Example: {\"embedding_mpn\": \"PROD-123\", \"embedding_description\": \"widget\"}", queriesVal),
			}, nil
		}
		return &ToolResult{
			Name:  e.def.Name,
			Error: "queries argument is required for advanced mode",
		}, nil
	}

	// Get columns to return (support both field names)
	contentColumns := "*"
	if cc, ok := config["returnColumns"].(string); ok && cc != "" {
		contentColumns = cc
	} else if cc, ok := config["contentColumns"].(string); ok && cc != "" {
		contentColumns = cc
	}

	// Get distance metric (default: cosine)
	distanceMetric := "cosine"
	if dm, ok := config["distanceMetric"].(string); ok && dm != "" {
		distanceMetric = dm
	}

	// Get topK (default: 10, can be overridden by args)
	topK := 10
	if tk, ok := config["topK"].(float64); ok && tk > 0 {
		topK = int(tk)
	}
	if tk, ok := args["top_k"].(float64); ok && tk > 0 {
		topK = int(tk)
	}

	// Parse vector configs (multi-field or single field)
	vectorConfigs, err := e.parseVectorConfigs(config)
	if err != nil {
		return &ToolResult{
			Name:  e.def.Name,
			Error: fmt.Sprintf("failed to parse vector configs: %v", err),
		}, nil
	}

	// Get embedding configuration
	embCfg := e.getEmbeddingConfig(config)

	// Generate embeddings for each vector config
	embeddings := make(map[string][]float64, len(vectorConfigs))
	for _, vc := range vectorConfigs {
		var queryText string

		if searchMode == "advanced" {
			// Advanced mode: use field-specific query (must be provided for each column)
			if fieldQuery, ok := fieldQueries[vc.Column]; ok {
				queryText = fieldQuery
			} else {
				return &ToolResult{
					Name:  e.def.Name,
					Error: fmt.Sprintf("no query provided for column %s in advanced mode", vc.Column),
				}, nil
			}
		} else {
			// Simple mode: use the same query for all columns
			queryText = query
		}

		embedding, err := e.generateEmbedding(ctx, queryText, embCfg)
		if err != nil {
			return &ToolResult{
				Name:  e.def.Name,
				Error: fmt.Sprintf("failed to generate embedding for column %s: %v", vc.Column, err),
			}, nil
		}
		embeddings[vc.Column] = embedding
	}

	// Get database connection
	db, err := e.getConnection(connStr)
	if err != nil {
		return &ToolResult{
			Name:  e.def.Name,
			Error: fmt.Sprintf("failed to connect to database: %v", err),
		}, nil
	}

	// Get aggregation method (default: minimum)
	aggregationMethod := "minimum"
	if am, ok := config["aggregationMethod"].(string); ok && am != "" {
		aggregationMethod = am
	}

	// Execute similarity search
	results, err := e.executeSearch(ctx, db, tableName, contentColumns, vectorConfigs, embeddings, topK, distanceMetric, aggregationMethod, config)
	if err != nil {
		return &ToolResult{
			Name:  e.def.Name,
			Error: fmt.Sprintf("search failed: %v", err),
		}, nil
	}

	return &ToolResult{
		Name:   e.def.Name,
		Result: results,
	}, nil
}

func (e *VectorSearchToolExecutor) getConnection(connStr string) (*sql.DB, error) {
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

	db, err := sql.Open("postgres", connStr)
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

func (e *VectorSearchToolExecutor) getEmbeddingConfig(config map[string]interface{}) embeddingConfig {
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
		cfg.APIKey = e.resolver.ResolveString(ak)
	}
	if bu, ok := config["embeddingBaseUrl"].(string); ok && bu != "" {
		cfg.BaseURL = e.resolver.ResolveString(bu)
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
		}
	}

	return cfg
}

func (e *VectorSearchToolExecutor) generateEmbedding(ctx context.Context, text string, cfg embeddingConfig) ([]float64, error) {
	// Create a temporary PGVectorExecutor to reuse embedding generation logic
	pgvec := &PGVectorExecutor{
		client:   e.client,
		connPool: make(map[string]*sql.DB),
	}
	return pgvec.generateEmbedding(ctx, text, cfg)
}

func (e *VectorSearchToolExecutor) executeSearch(ctx context.Context, db *sql.DB, tableName, contentColumns string, vectorConfigs []VectorConfig, embeddings map[string][]float64, topK int, distanceMetric, aggregationMethod string, config map[string]interface{}) ([]map[string]interface{}, error) {
	// Validate table name to prevent SQL injection
	if err := validateIdentifier(tableName); err != nil {
		return nil, fmt.Errorf("invalid table name: %w", err)
	}

	// Select distance operator based on metric
	var distanceOp string
	switch distanceMetric {
	case "l2":
		distanceOp = "<->" // L2 distance
	case "inner_product":
		distanceOp = "<#>" // Inner product (negative for similarity)
	default: // cosine
		distanceOp = "<=>" // Cosine distance
	}

	// Build SELECT clause with distance calculations for each vector column
	var selectParts []string
	selectParts = append(selectParts, contentColumns)

	var distanceColumns []string
	var distanceExpressions []string // Full expressions for ORDER BY
	for _, vc := range vectorConfigs {
		// Validate vector column name
		if err := validateIdentifier(vc.Column); err != nil {
			return nil, fmt.Errorf("invalid vector column name '%s': %w", vc.Column, err)
		}

		embedding := embeddings[vc.Column]
		vectorStr := vectorToString(embedding)
		distanceCol := fmt.Sprintf("%s_distance", vc.Column)

		// Full distance expression (for ORDER BY - PostgreSQL doesn't allow aliases in ORDER BY expressions)
		distanceExpr := fmt.Sprintf("%s %s '%s'::vector", quoteIdentifier(vc.Column), distanceOp, vectorStr)

		selectParts = append(selectParts, fmt.Sprintf("%s AS %s", distanceExpr, distanceCol))
		distanceColumns = append(distanceColumns, distanceCol)
		distanceExpressions = append(distanceExpressions, distanceExpr)
	}

	// Build ORDER BY clause based on aggregation method
	// Note: PostgreSQL does NOT allow using column aliases defined in SELECT within ORDER BY expressions
	// We must use the full distance expressions
	var orderByExpr string
	if len(vectorConfigs) == 1 {
		// Single vector: can use alias directly when not in an expression
		orderByExpr = distanceColumns[0]
	} else {
		// Multiple vectors: must use full expressions (not aliases) in aggregate functions/expressions
		switch aggregationMethod {
		case "maximum":
			// Maximum distance (all must match closely)
			orderByExpr = fmt.Sprintf("GREATEST(%s)", strings.Join(distanceExpressions, ", "))
		case "weighted":
			// Weighted average with optional custom weights
			weights := getWeightsMapFromConfig(config, "weights")

			if len(weights) > 0 {
				// Custom weighted average
				var weightedTerms []string
				var totalWeight float64

				for i, vc := range vectorConfigs {
					weight := 1.0 // default weight
					if w, ok := weights[vc.Column]; ok {
						weight = w
					}
					weightedTerms = append(weightedTerms, fmt.Sprintf("(%f * (%s))", weight, distanceExpressions[i]))
					totalWeight += weight
				}

				if totalWeight == 0 {
					totalWeight = 1.0 // prevent division by zero
				}

				orderByExpr = fmt.Sprintf("(%s) / %f", strings.Join(weightedTerms, " + "), totalWeight)
			} else {
				// Equal weights (simple average)
				// Wrap each expression in parentheses to avoid operator precedence issues
				var wrappedExpressions []string
				for _, expr := range distanceExpressions {
					wrappedExpressions = append(wrappedExpressions, fmt.Sprintf("(%s)", expr))
				}
				orderByExpr = fmt.Sprintf("(%s) / %d", strings.Join(wrappedExpressions, " + "), len(wrappedExpressions))
			}
		default: // "minimum"
			// Minimum distance (best match wins)
			orderByExpr = fmt.Sprintf("LEAST(%s)", strings.Join(distanceExpressions, ", "))
		}
	}

	query := fmt.Sprintf(
		"SELECT %s FROM %s ORDER BY %s LIMIT $1",
		strings.Join(selectParts, ", "), quoteIdentifier(tableName), orderByExpr,
	)

	rows, err := db.QueryContext(ctx, query, topK)
	if err != nil {
		log.Printf("[VECTOR SEARCH ERROR] Query failed: %v", err)
		log.Printf("[VECTOR SEARCH ERROR] Failed SQL: %s", query)
		return nil, fmt.Errorf("search query failed: %w", err)
	}
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}

	var results []map[string]interface{}
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
			// Skip embedding columns in results (too large)
			isEmbeddingColumn := false
			for _, vc := range vectorConfigs {
				if col == vc.Column {
					isEmbeddingColumn = true
					break
				}
			}
			if isEmbeddingColumn {
				continue
			}

			// Convert []byte to string for text columns
			if b, ok := values[i].([]byte); ok {
				row[col] = string(b)
			} else {
				row[col] = values[i]
			}
		}

		// Calculate similarity for each distance column
		for _, distCol := range distanceColumns {
			if dist, ok := row[distCol].(float64); ok {
				simCol := strings.Replace(distCol, "_distance", "_similarity", 1)
				row[simCol] = calculateSimilarity(dist, distanceMetric)
			}
		}

		// Add overall distance and similarity (minimum across all vectors)
		if len(distanceColumns) > 0 {
			minDist := -1.0
			for _, distCol := range distanceColumns {
				if dist, ok := row[distCol].(float64); ok {
					if minDist < 0 || dist < minDist {
						minDist = dist
					}
				}
			}
			if minDist >= 0 {
				row["distance"] = minDist
				row["similarity"] = calculateSimilarity(minDist, distanceMetric)
			}
		}

		results = append(results, row)
	}

	return results, nil
}

// CreateVectorSearchToolDefinition creates a ToolDefinition for vector search
func CreateVectorSearchToolDefinition(name, description string, config map[string]interface{}) *ToolDefinition {
	// Merge type into config
	mergedConfig := make(map[string]interface{})
	for k, v := range config {
		mergedConfig[k] = v
	}
	mergedConfig["type"] = "vector_search"

	// Determine search mode
	searchMode := "simple"
	if sm, ok := config["searchMode"].(string); ok && sm != "" {
		searchMode = sm
	}

	// Build parameters based on search mode
	var parameters map[string]interface{}
	if searchMode == "advanced" {
		// Advanced mode: AI provides queries object with field-specific queries
		parameters = map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"queries": map[string]interface{}{
					"type":        "object",
					"description": "Object mapping vector column names to search queries. Must provide a query for each vector column. Example: {\"embedding_mpn\": \"PROD-123\", \"embedding_description\": \"industrial widget\"}",
					"additionalProperties": map[string]interface{}{
						"type": "string",
					},
				},
				"top_k": map[string]interface{}{
					"type":        "integer",
					"description": "Number of results to return (default: 10)",
				},
			},
			"required": []string{"queries"},
		}
	} else {
		// Simple mode: AI provides single query for all columns
		parameters = map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"query": map[string]interface{}{
					"type":        "string",
					"description": "The search query to find similar content across all vector columns",
				},
				"top_k": map[string]interface{}{
					"type":        "integer",
					"description": "Number of results to return (default: 10)",
				},
			},
			"required": []string{"query"},
		}
	}

	return &ToolDefinition{
		Name:        name,
		Description: description,
		Parameters:  parameters,
		Config:      mergedConfig,
	}
}

// ParseToolDefinitionsFromConfig extracts tool definitions from AI step config
// Supports two formats:
// 1. Flat fields: toolEnabled, toolType, toolName, toolDescription, toolConnectionString, etc.
// 2. Array format: tools: [{name, description, type, ...config}]
func ParseToolDefinitionsFromConfig(config map[string]interface{}, resolver TemplateResolver) ([]*ToolDefinition, error) {
	// Check for flat field format (from UI)
	if toolEnabled, ok := config["toolEnabled"].(bool); ok && toolEnabled {
		return parseToolFromFlatFields(config, resolver)
	}

	// Check for array format
	toolsRaw, ok := config["tools"]
	if !ok {
		return nil, nil
	}

	toolsList, ok := toolsRaw.([]interface{})
	if !ok {
		return nil, fmt.Errorf("tools must be an array")
	}

	var tools []*ToolDefinition
	for i, toolRaw := range toolsList {
		toolMap, ok := toolRaw.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("tool at index %d must be an object", i)
		}

		name, _ := toolMap["name"].(string)
		if name == "" {
			return nil, fmt.Errorf("tool at index %d requires 'name'", i)
		}

		description, _ := toolMap["description"].(string)

		var parameters map[string]interface{}
		if p, ok := toolMap["parameters"].(map[string]interface{}); ok {
			parameters = p
		}

		var toolConfig map[string]interface{}
		if c, ok := toolMap["config"].(map[string]interface{}); ok {
			toolConfig = c
		} else {
			toolConfig = make(map[string]interface{})
		}

		// Check for shorthand tool types
		if toolType, ok := toolMap["type"].(string); ok {
			toolConfig["type"] = toolType

			// Auto-populate parameters for known types
			if toolType == "vector_search" && parameters == nil {
				parameters = map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"query": map[string]interface{}{
							"type":        "string",
							"description": "The search query to find similar content",
						},
						"top_k": map[string]interface{}{
							"type":        "integer",
							"description": "Number of results to return",
						},
					},
					"required": []string{"query"},
				}
			}
		}

		// Copy remaining fields to config (for vector_search: connectionString, tableName, etc.)
		for k, v := range toolMap {
			if k != "name" && k != "description" && k != "parameters" && k != "config" && k != "type" {
				toolConfig[k] = v
			}
		}

		tools = append(tools, &ToolDefinition{
			Name:        name,
			Description: description,
			Parameters:  parameters,
			Config:      toolConfig,
		})
	}

	return tools, nil
}

// parseToolFromFlatFields parses tool definition from UI flat field format
func parseToolFromFlatFields(config map[string]interface{}, resolver TemplateResolver) ([]*ToolDefinition, error) {
	toolType, _ := config["toolType"].(string)
	if toolType == "" {
		toolType = "vector_search" // default
	}

	toolName, _ := config["toolName"].(string)
	if toolName == "" {
		toolName = "search" // default name
	}

	toolDescription, _ := config["toolDescription"].(string)
	if toolDescription == "" {
		toolDescription = "Search for relevant information"
	}

	// Build tool config from flat fields
	toolConfig := map[string]interface{}{
		"type": toolType,
	}

	// Map UI field names to tool config field names
	fieldMappings := map[string]string{
		"toolConnectionString":  "connectionString",
		"toolTableName":         "tableName",
		"toolContentColumns":    "contentColumns",
		"toolEmbeddingProvider": "embeddingProvider",
		"toolEmbeddingModel":    "embeddingModel",
		"toolEmbeddingApiKey":   "embeddingApiKey",
		"toolTopK":              "topK",
		"toolEmbeddingColumn":   "embeddingColumn",
	}

	// Also copy searchMode, vectorColumns, aggregationMethod, distanceMetric, returnColumns directly
	directCopyFields := []string{"searchMode", "vectorColumns", "aggregationMethod", "distanceMetric", "returnColumns", "table", "connectionString"}
	for _, field := range directCopyFields {
		if v, ok := config[field]; ok && v != nil && v != "" {
			toolConfig[field] = v
		}
	}

	for uiField, configField := range fieldMappings {
		if v, ok := config[uiField]; ok && v != nil && v != "" {
			toolConfig[configField] = v
		}
	}

	// Set defaults
	if _, ok := toolConfig["embeddingColumn"]; !ok {
		toolConfig["embeddingColumn"] = "embedding"
	}

	// Build parameters for vector_search based on searchMode
	var parameters map[string]interface{}
	if toolType == "vector_search" {
		searchMode := "simple"
		if sm, ok := config["searchMode"].(string); ok && sm != "" {
			searchMode = sm
		}

		if searchMode == "advanced" {
			// Advanced mode: AI provides queries object with field-specific queries
			parameters = map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"queries": map[string]interface{}{
						"type":        "object",
						"description": "Object mapping vector column names to search queries. Must provide a query for each vector column. Example: {\"embedding_mpn\": \"PROD-123\", \"embedding_description\": \"industrial widget\"}",
						"additionalProperties": map[string]interface{}{
							"type": "string",
						},
					},
					"top_k": map[string]interface{}{
						"type":        "integer",
						"description": "Number of results to return (default: 10)",
					},
				},
				"required": []string{"queries"},
			}
		} else {
			// Simple mode: AI provides single query for all columns
			parameters = map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"query": map[string]interface{}{
						"type":        "string",
						"description": "The search query to find similar content across all vector columns",
					},
					"top_k": map[string]interface{}{
						"type":        "integer",
						"description": "Number of results to return (default: 10)",
					},
				},
				"required": []string{"query"},
			}
		}
	}

	return []*ToolDefinition{
		{
			Name:        toolName,
			Description: toolDescription,
			Parameters:  parameters,
			Config:      toolConfig,
		},
	}, nil
}

// ExecuteToolCalls executes a list of tool calls and returns results
func ExecuteToolCalls(ctx context.Context, calls []*ToolCall, tools []*ToolDefinition, resolver TemplateResolver) ([]*ToolCallResult, error) {
	registry := GetGlobalToolRegistry()

	// Build tool lookup map
	toolMap := make(map[string]*ToolDefinition)
	for _, tool := range tools {
		toolMap[tool.Name] = tool
	}

	results := make([]*ToolCallResult, len(calls))
	for i, call := range calls {
		tool, ok := toolMap[call.Name]
		if !ok {
			results[i] = &ToolCallResult{
				ToolCallID: call.ID,
				Name:       call.Name,
				Error:      fmt.Sprintf("unknown tool: %s", call.Name),
			}
			continue
		}

		executor, err := registry.CreateExecutor(tool, resolver)
		if err != nil {
			results[i] = &ToolCallResult{
				ToolCallID: call.ID,
				Name:       call.Name,
				Error:      fmt.Sprintf("failed to create executor: %v", err),
			}
			continue
		}

		result, err := executor.Execute(ctx, call.Arguments)
		if err != nil {
			results[i] = &ToolCallResult{
				ToolCallID: call.ID,
				Name:       call.Name,
				Error:      fmt.Sprintf("execution failed: %v", err),
			}
			continue
		}

		if result.Error != "" {
			results[i] = &ToolCallResult{
				ToolCallID: call.ID,
				Name:       call.Name,
				Error:      result.Error,
			}
		} else {
			results[i] = &ToolCallResult{
				ToolCallID: call.ID,
				Name:       call.Name,
				Content:    result.Result,
			}
		}
	}

	return results, nil
}

// FormatToolResultsForOpenAI formats tool results as OpenAI tool messages
func FormatToolResultsForOpenAI(results []*ToolCallResult) []map[string]interface{} {
	messages := make([]map[string]interface{}, len(results))
	for i, result := range results {
		content := ""
		if result.Error != "" {
			content = fmt.Sprintf("Error: %s", result.Error)
		} else {
			// Convert result to string
			switch v := result.Content.(type) {
			case string:
				content = v
			default:
				// JSON encode complex results
				if jsonBytes, err := jsonMarshal(v); err == nil {
					content = string(jsonBytes)
				} else {
					content = fmt.Sprintf("%v", v)
				}
			}
		}

		messages[i] = map[string]interface{}{
			"role":         "tool",
			"tool_call_id": result.ToolCallID,
			"content":      content,
		}
	}
	return messages
}

// FormatToolResultsForOpenAIResponses formats tool results for OpenAI Responses API
// The Responses API uses function_call_output items
func FormatToolResultsForOpenAIResponses(results []*ToolCallResult) []map[string]interface{} {
	items := make([]map[string]interface{}, len(results))
	for i, result := range results {
		output := ""
		if result.Error != "" {
			output = fmt.Sprintf("Error: %s", result.Error)
		} else {
			// Convert result to string
			switch v := result.Content.(type) {
			case string:
				output = v
			default:
				// JSON encode complex results
				if jsonBytes, err := jsonMarshal(v); err == nil {
					output = string(jsonBytes)
				} else {
					output = fmt.Sprintf("%v", v)
				}
			}
		}

		items[i] = map[string]interface{}{
			"type":    "function_call_output",
			"call_id": result.ToolCallID,
			"output":  output,
		}
	}
	return items
}

// FormatToolResultsForAnthropic formats tool results as Anthropic tool_result content blocks
func FormatToolResultsForAnthropic(results []*ToolCallResult) []map[string]interface{} {
	blocks := make([]map[string]interface{}, len(results))
	for i, result := range results {
		content := ""
		if result.Error != "" {
			content = fmt.Sprintf("Error: %s", result.Error)
		} else {
			switch v := result.Content.(type) {
			case string:
				content = v
			default:
				if jsonBytes, err := jsonMarshal(v); err == nil {
					content = string(jsonBytes)
				} else {
					content = fmt.Sprintf("%v", v)
				}
			}
		}

		blocks[i] = map[string]interface{}{
			"type":        "tool_result",
			"tool_use_id": result.ToolCallID,
			"content":     content,
		}
	}
	return blocks
}

// FormatToolResultsForGemini formats tool results as Gemini function response parts
func FormatToolResultsForGemini(results []*ToolCallResult) []map[string]interface{} {
	parts := make([]map[string]interface{}, len(results))
	for i, result := range results {
		response := make(map[string]interface{})
		if result.Error != "" {
			response["error"] = result.Error
		} else {
			response["result"] = result.Content
		}

		parts[i] = map[string]interface{}{
			"functionResponse": map[string]interface{}{
				"name":     result.Name,
				"response": response,
			},
		}
	}
	return parts
}

// jsonMarshal is a helper that wraps json.Marshal
func jsonMarshal(v interface{}) ([]byte, error) {
	return json.Marshal(v)
}

// jsonUnmarshal is a helper that wraps json.Unmarshal
func jsonUnmarshal(data []byte, v interface{}) error {
	return json.Unmarshal(data, v)
}

// ParseToolCallsFromPromptResponse parses tool calls from a model response (for prompt-based fallback)
func ParseToolCallsFromPromptResponse(response string) ([]*ToolCall, string, error) {
	// Look for JSON tool call format in the response
	// Format: {"tool_call": {"name": "...", "arguments": {...}}}

	response = strings.TrimSpace(response)

	// Check if response contains a tool call JSON block
	startIdx := strings.Index(response, "{\"tool_call\":")
	if startIdx == -1 {
		// Try alternate format with backticks
		if strings.Contains(response, "```json") {
			start := strings.Index(response, "```json")
			end := strings.Index(response[start+7:], "```")
			if end != -1 {
				jsonBlock := strings.TrimSpace(response[start+7 : start+7+end])
				if strings.Contains(jsonBlock, "tool_call") {
					startIdx = 0
					response = jsonBlock
				}
			}
		}
	}

	if startIdx == -1 {
		// No tool call found, return original response
		return nil, response, nil
	}

	// Find the matching closing brace
	braceCount := 0
	endIdx := -1
	inString := false
	escaped := false

	for i := startIdx; i < len(response); i++ {
		c := response[i]
		if escaped {
			escaped = false
			continue
		}
		if c == '\\' {
			escaped = true
			continue
		}
		if c == '"' {
			inString = !inString
			continue
		}
		if inString {
			continue
		}
		if c == '{' {
			braceCount++
		} else if c == '}' {
			braceCount--
			if braceCount == 0 {
				endIdx = i + 1
				break
			}
		}
	}

	if endIdx == -1 {
		return nil, response, nil
	}

	jsonStr := response[startIdx:endIdx]

	// Parse the JSON
	var wrapper struct {
		ToolCall struct {
			Name      string                 `json:"name"`
			Arguments map[string]interface{} `json:"arguments"`
		} `json:"tool_call"`
	}

	// Use the json package for parsing
	if err := jsonUnmarshal([]byte(jsonStr), &wrapper); err != nil {
		return nil, response, nil
	}

	if wrapper.ToolCall.Name == "" {
		return nil, response, nil
	}

	// Generate a unique ID for the tool call
	toolCall := &ToolCall{
		ID:        fmt.Sprintf("call_%d", time.Now().UnixNano()),
		Name:      wrapper.ToolCall.Name,
		Arguments: wrapper.ToolCall.Arguments,
	}

	// Extract remaining text (before and after the JSON)
	remainingText := strings.TrimSpace(response[:startIdx] + response[endIdx:])

	return []*ToolCall{toolCall}, remainingText, nil
}

// getWeightsMapFromConfig parses the weights configuration and returns a map of column names to weight values
func getWeightsMapFromConfig(config map[string]interface{}, key string) map[string]float64 {
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
			} else if w, ok := weight.(string); ok {
				// Handle string values from database (e.g., "0.6")
				var floatVal float64
				if _, err := fmt.Sscanf(w, "%f", &floatVal); err == nil {
					result[column] = floatVal
				}
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
