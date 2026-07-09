package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/axiom-studio/openseal/pkg/config"
	"github.com/axiom-studio/openseal/pkg/skill/skillmd"
)

// AIExecutor makes calls to AI/LLM APIs
//
//	Config: {
//	  "provider": "openai",  // openai, anthropic, ollama, openai-compatible, gemini
//	  "model": "gpt-4",
//	  "prompt": "{{trigger.message}}",
//	  "systemPrompt": "You are a helpful assistant.",
//	  "temperature": 0.7,
//	  "maxTokens": 1000,
//	  "apiKey": "sk-...",  // optional, falls back to env var
//	  "baseUrl": "https://api.openai.com/v1/chat/completions",  // for openai/openai-compatible (full URL including endpoint)
//	  "ollamaUrl": "http://localhost:11434",  // for ollama
//	  "tools": [...]  // optional array of tool definitions for tool calling
//	  "maxToolIterations": 10  // optional, maximum number of tool call iterations (default: 10)
//	}
//
// Note: For OpenAI and openai-compatible providers, baseUrl should include the full path to the endpoint:
//   - "/chat/completions" for standard OpenAI-compatible APIs
//   - "/v1/responses" for OpenAI's newer responses API
//   - Custom paths for other providers
type AIExecutor struct {
	client *http.Client
}

// Maximum number of tool call iterations to prevent infinite loops
const maxToolIterations = 10

func NewAIExecutor() *AIExecutor {
	return &AIExecutor{
		client: &http.Client{
			// No timeout - rely on context deadline from workflow execution
			// This allows AI nodes to run longer, especially with multiple tool calls
		},
	}
}

func (e *AIExecutor) Type() string {
	return StepTypeAI
}

func (e *AIExecutor) Execute(ctx context.Context, step *StepDefinition, resolver TemplateResolver) (*StepResult, error) {
	config := step.Config
	if config == nil {
		return nil, fmt.Errorf("ai step requires config")
	}

	// Discover connected tool nodes from the graph
	var tools []*ToolDefinition
	if gp, ok := resolver.(GraphProvider); ok {
		if graph := gp.GetGraph(); graph != nil && step.Id != "" {
			connectedTools := graph.GetConnectedTools(step.Id)
			for _, toolNode := range connectedTools {
				// Check if this is an MCP tool that needs discovery
				if toolNode.Type == "tool_mcp" {
					discoveredTools := e.discoverMCPTools(ctx, toolNode, resolver)
					tools = append(tools, discoveredTools...)
				} else {
					toolDef := e.buildToolDefinitionFromNode(toolNode, resolver)
					if toolDef != nil {
						tools = append(tools, toolDef)
					}
				}
			}
		}
	}

	// Also check for inline tools (backwards compatibility)
	inlineTools, err := ParseToolDefinitionsFromConfig(config, resolver)
	if err != nil {
		return nil, fmt.Errorf("failed to parse tools: %w", err)
	}
	tools = append(tools, inlineTools...)

	// Include tools from skills registered in the global tool registry
	skillTools := GetGlobalToolRegistry().GetAllTools()
	tools = append(tools, skillTools...)

	// If tools are present, use the tool calling loop
	if len(tools) > 0 {
		return e.executeWithTools(ctx, step, resolver, tools)
	}

	// Original execution without tools
	return e.executeSimple(ctx, step, resolver)
}

// executeSimple performs AI execution without tool calling
func (e *AIExecutor) executeSimple(ctx context.Context, step *StepDefinition, resolver TemplateResolver) (*StepResult, error) {
	config := step.Config

	// Get provider (default: openai)
	provider := "openai"
	if p, ok := config["provider"].(string); ok && p != "" {
		provider = p
	}

	// Get model
	model := ""
	if m, ok := config["model"].(string); ok {
		model = resolver.ResolveString(m)
	}

	// Get and resolve prompt
	promptTemplate, ok := config["prompt"].(string)
	if !ok || promptTemplate == "" {
		return nil, fmt.Errorf("ai step requires 'prompt'")
	}
	prompt := resolver.ResolveString(promptTemplate)

	// Get system prompt
	systemPrompt := ""
	if sp, ok := config["systemPrompt"].(string); ok {
		systemPrompt = resolver.ResolveString(sp)
	}

	// Get temperature
	temperature := 0.7
	if t, ok := config["temperature"].(float64); ok {
		temperature = t
	}

	// Get max tokens
	maxTokens := 1000
	if mt, ok := config["maxTokens"].(float64); ok {
		maxTokens = int(mt)
	}

	// Get API key from config (falls back to env var in provider methods)
	// Resolve templates in case the API key is a binding reference
	apiKey := ""
	if ak, ok := config["apiKey"].(string); ok {
		apiKey = resolver.ResolveString(ak)
	}

	// Get custom base URL for openai-compatible provider
	// Resolve templates in case the URL is a binding reference like {{bindings.openAIEndpoint}}
	// Should include the full path, e.g., "https://api.openai.com/v1/chat/completions" or "https://api.openai.com/v1/responses"
	baseUrl := ""
	if bu, ok := config["baseUrl"].(string); ok {
		baseUrl = resolver.ResolveString(bu)
	}

	// Get Ollama URL
	// Resolve templates in case the URL is a binding reference
	ollamaUrl := ""
	if ou, ok := config["ollamaUrl"].(string); ok {
		ollamaUrl = resolver.ResolveString(ou)
	}

	// Extract files from config - resolve templates first to get actual file objects
	// Files can be referenced as "file": "{{trigger.document}}" or comma-delimited "{{trigger.doc1}}, {{trigger.doc2}}"
	var files []*FileObject

	// Resolve the file config value to get actual objects
	if fileTemplate, ok := config["file"].(string); ok {
		// Check if comma-delimited (multiple files)
		fileParts := strings.Split(fileTemplate, ",")
		for i, part := range fileParts {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			// Resolve template like "{{trigger.document}}" to get the file object
			key := fmt.Sprintf("file_%d", i)
			resolvedConfig := resolver.ResolveMap(map[string]interface{}{key: part})
			if IsFileObject(resolvedConfig[key]) {
				if fileObj, err := ParseFileObject(resolvedConfig[key]); err == nil {
					files = append(files, fileObj)
				}
			}
		}
	} else if IsFileObject(config["file"]) {
		// Already a file object (not a template)
		if fileObj, err := ParseFileObject(config["file"]); err == nil {
			files = append(files, fileObj)
		}
	}

	// Execute based on provider
	var response string
	var err error
	var usage map[string]interface{}

	switch provider {
	case "openai":
		response, usage, err = e.callOpenAI(ctx, model, prompt, systemPrompt, temperature, maxTokens, apiKey, "", files)
	case "anthropic":
		response, usage, err = e.callAnthropic(ctx, model, prompt, systemPrompt, temperature, maxTokens, apiKey, files)
	case "gemini":
		response, usage, err = e.callGemini(ctx, model, prompt, systemPrompt, temperature, maxTokens, apiKey, files)
	case "ollama":
		response, usage, err = e.callOllama(ctx, model, prompt, systemPrompt, temperature, ollamaUrl, files)
	case "openai-compatible":
		if baseUrl == "" {
			return nil, fmt.Errorf("openai-compatible provider requires 'baseUrl'")
		}
		response, usage, err = e.callOpenAI(ctx, model, prompt, systemPrompt, temperature, maxTokens, apiKey, baseUrl, files)
	default:
		return nil, fmt.Errorf("unsupported AI provider: %s", provider)
	}

	if err != nil {
		return &StepResult{
			Output: map[string]interface{}{
				"error":    err.Error(),
				"provider": provider,
				"model":    model,
			},
		}, err
	}

	// Start with the full API response (which is in usage parameter)
	output := make(map[string]interface{})
	if usage != nil {
		// Copy all fields from the full API response
		for k, v := range usage {
			output[k] = v
		}
	}

	// Add our custom fields
	output["response"] = response
	output["provider"] = provider
	// model is usually already in the API response, but ensure it's set
	if _, hasModel := output["model"]; !hasModel {
		output["model"] = model
	}

	return &StepResult{
		Output: output,
	}, nil
}

// buildToolDefinitionFromNode creates a ToolDefinition from a tool node's config
func (e *AIExecutor) buildToolDefinitionFromNode(node *NodeDefinition, resolver TemplateResolver) *ToolDefinition {
	if node == nil || node.Config == nil {
		return nil
	}
	config := node.Config

	toolName, _ := config["toolName"].(string)
	if toolName == "" {
		toolName = "tool"
	}

	toolDesc, _ := config["toolDescription"].(string)

	// Determine tool type based on node type
	nodeType := node.Type
	toolType := strings.TrimPrefix(nodeType, "tool_") // "tool_pgvector" -> "pgvector"

	// Build tool config from node config
	toolConfig := map[string]interface{}{
		"type": toolType,
	}

	// Copy relevant config fields (excluding name and description)
	for k, v := range config {
		if k != "toolName" && k != "toolDescription" {
			toolConfig[k] = v
		}
	}

	// Build parameters schema based on tool type and searchMode
	var parameters map[string]interface{}
	switch toolType {
	case "pgvector":
		// Vector search tool - parameters depend on searchMode
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
						"description": "Search query to find similar content across all vector columns",
					},
				},
				"required": []string{"query"},
			}
		}
		// Map pgvector to vector_search for the tool registry
		toolConfig["type"] = "vector_search"
	case "debug":
		// Debug tool - accepts message, data, and variables
		parameters = map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"message": map[string]interface{}{
					"type":        "string",
					"description": "Debug message to log",
				},
				"data": map[string]interface{}{
					"type":        "object",
					"description": "Data object to inspect (optional)",
				},
				"variables": map[string]interface{}{
					"type":        "array",
					"description": "Array of variable names to inspect from context (optional)",
					"items": map[string]interface{}{
						"type": "string",
					},
				},
			},
		}
	case "mcp":
		// MCP tool - if no fixed tool name in config, AI must provide it
		fixedToolName, _ := config["tool"].(string)
		if fixedToolName != "" {
			// Fixed tool name - AI just provides arguments for that tool
			parameters = map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			}
		} else {
			// No fixed tool name - AI must specify which MCP tool to call
			parameters = map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"tool": map[string]interface{}{
						"type":        "string",
						"description": "Name of the MCP tool to call",
					},
				},
				"required": []string{"tool"},
			}
		}
	default:
		// Generic object schema for unknown tool types
		parameters = map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		}
	}

	return &ToolDefinition{
		Name:        toolName,
		Description: toolDesc,
		Parameters:  parameters,
		Config:      toolConfig,
	}
}

// discoverMCPTools connects to an MCP server and discovers all available tools
func (e *AIExecutor) discoverMCPTools(ctx context.Context, node *NodeDefinition, resolver TemplateResolver) []*ToolDefinition {
	if node == nil || node.Config == nil {
		return nil
	}

	config := node.Config

	// Check if a specific tool is configured
	fixedToolName, _ := config["tool"].(string)
	if fixedToolName != "" {
		// Single tool mode - just build one tool definition
		toolDef := e.buildToolDefinitionFromNode(node, resolver)
		if toolDef != nil {
			return []*ToolDefinition{toolDef}
		}
		return nil
	}

	// Discover all tools from the MCP server
	discovery := &MCPToolDiscovery{}
	discoveredTools, err := discovery.DiscoverTools(ctx, config)
	if err != nil {
		// Log error but don't fail - return a fallback tool definition
		toolName, _ := config["toolName"].(string)
		if toolName == "" {
			toolName = "mcp_tool"
		}
		toolDesc, _ := config["toolDescription"].(string)
		if toolDesc == "" {
			toolDesc = "MCP tool (discovery failed)"
		}
		return []*ToolDefinition{{
			Name:        toolName,
			Description: toolDesc,
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"tool": map[string]interface{}{
						"type":        "string",
						"description": "Name of the MCP tool to call",
					},
				},
				"required": []string{"tool"},
			},
			Config: map[string]interface{}{
				"type":    "mcp",
				"command": config["command"],
				"args":    config["args"],
				"env":     config["env"],
			},
		}}
	}

	// Convert to pointers
	result := make([]*ToolDefinition, len(discoveredTools))
	for i := range discoveredTools {
		result[i] = &discoveredTools[i]
	}

	return result
}

// executeWithTools performs AI execution with tool calling loop
func (e *AIExecutor) executeWithTools(ctx context.Context, step *StepDefinition, resolver TemplateResolver, tools []*ToolDefinition) (*StepResult, error) {
	config := step.Config

	// Get provider (default: openai)
	provider := "openai"
	if p, ok := config["provider"].(string); ok && p != "" {
		provider = p
	}

	// Get model
	model := ""
	if m, ok := config["model"].(string); ok {
		model = resolver.ResolveString(m)
	}

	// Get and resolve prompt
	promptTemplate, ok := config["prompt"].(string)
	if !ok || promptTemplate == "" {
		return nil, fmt.Errorf("ai step requires 'prompt'")
	}
	prompt := resolver.ResolveString(promptTemplate)

	// Get system prompt
	systemPrompt := ""
	if sp, ok := config["systemPrompt"].(string); ok {
		systemPrompt = resolver.ResolveString(sp)
	}

	// Get temperature
	temperature := 0.7
	if t, ok := config["temperature"].(float64); ok {
		temperature = t
	}

	// Get max tokens
	maxTokens := 1000
	if mt, ok := config["maxTokens"].(float64); ok {
		maxTokens = int(mt)
	}

	// Get API key
	apiKey := ""
	if ak, ok := config["apiKey"].(string); ok {
		apiKey = resolver.ResolveString(ak)
	}

	// Get custom base URL
	// For "openai" provider: always use Responses API (ignore any baseUrl from config)
	// For "openai-compatible" provider: use the configured baseUrl
	baseUrl := ""
	if provider == "openai-compatible" {
		if bu, ok := config["baseUrl"].(string); ok {
			baseUrl = resolver.ResolveString(bu)
		}
	}
	// Note: "openai" provider will get default URL set in executeOpenAIToolLoop

	// Get Ollama URL
	ollamaUrl := ""
	if ou, ok := config["ollamaUrl"].(string); ok {
		ollamaUrl = resolver.ResolveString(ou)
	}

	// Get max tool iterations (default: 10)
	maxToolIter := maxToolIterations
	if mti, ok := config["maxToolIterations"].(float64); ok {
		maxToolIter = int(mti)
	}

	// Determine if provider supports native tool calling
	nativeToolSupport := provider == "openai" || provider == "anthropic" || provider == "gemini" || provider == "openai-compatible"

	var response string
	var usage map[string]interface{}
	var toolCallHistory []map[string]interface{}
	var err error

	if nativeToolSupport {
		response, usage, toolCallHistory, err = e.executeNativeToolLoop(ctx, provider, model, prompt, systemPrompt, temperature, maxTokens, apiKey, baseUrl, tools, resolver, maxToolIter)
	} else {
		// Prompt-based fallback for ollama and other providers
		response, usage, toolCallHistory, err = e.executePromptToolLoop(ctx, provider, model, prompt, systemPrompt, temperature, maxTokens, apiKey, ollamaUrl, tools, resolver, maxToolIter)
	}

	if err != nil {
		return &StepResult{
			Output: map[string]interface{}{
				"error":    err.Error(),
				"provider": provider,
				"model":    model,
			},
		}, err
	}

	// Start with the full API response (which is in usage parameter)
	output := make(map[string]interface{})
	if usage != nil {
		// Copy all fields from the full API response
		for k, v := range usage {
			output[k] = v
		}
	}

	// Add our custom fields
	output["response"] = response
	output["provider"] = provider
	// model is usually already in the API response, but ensure it's set
	if _, hasModel := output["model"]; !hasModel {
		output["model"] = model
	}

	if len(toolCallHistory) > 0 {
		output["tool_calls"] = toolCallHistory
	}

	return &StepResult{
		Output: output,
	}, nil
}

// executeNativeToolLoop handles tool calling for providers with native support (OpenAI, Anthropic, Gemini)
func (e *AIExecutor) executeNativeToolLoop(ctx context.Context, provider, model, prompt, systemPrompt string, temperature float64, maxTokens int, apiKey, baseUrl string, tools []*ToolDefinition, resolver TemplateResolver, maxToolIter int) (string, map[string]interface{}, []map[string]interface{}, error) {
	switch provider {
	case "openai", "openai-compatible":
		return e.executeOpenAIToolLoop(ctx, model, prompt, systemPrompt, temperature, maxTokens, apiKey, baseUrl, tools, resolver, maxToolIter)
	case "anthropic":
		return e.executeAnthropicToolLoop(ctx, model, prompt, systemPrompt, temperature, maxTokens, apiKey, tools, resolver, maxToolIter)
	case "gemini":
		return e.executeGeminiToolLoop(ctx, model, prompt, systemPrompt, temperature, maxTokens, apiKey, tools, resolver, maxToolIter)
	default:
		return "", nil, nil, fmt.Errorf("unsupported provider for native tool calling: %s", provider)
	}
}

// OpenAIProvider defines the interface for OpenAI API implementations
type OpenAIProvider interface {
	ExecuteToolLoop(ctx context.Context, model, prompt, systemPrompt string, temperature float64, maxTokens int, apiKey, baseUrl string, tools []*ToolDefinition, resolver TemplateResolver, maxToolIter int) (string, map[string]interface{}, []map[string]interface{}, error)
}

// ChatCompletionsProvider implements OpenAI Chat Completions API
type ChatCompletionsProvider struct {
	client *http.Client
}

// ResponsesProvider implements OpenAI Responses API
type ResponsesProvider struct {
	client *http.Client
}

// executeOpenAIToolLoop routes to the appropriate OpenAI API implementation
func (e *AIExecutor) executeOpenAIToolLoop(ctx context.Context, model, prompt, systemPrompt string, temperature float64, maxTokens int, configApiKey, customBaseUrl string, tools []*ToolDefinition, resolver TemplateResolver, maxToolIter int) (string, map[string]interface{}, []map[string]interface{}, error) {
	apiKey := configApiKey
	if apiKey == "" {
		apiKey = os.Getenv("OPENAI_API_KEY")
	}
	if apiKey == "" {
		return "", nil, nil, fmt.Errorf("API key not provided and OPENAI_API_KEY not set")
	}

	if model == "" {
		model = "gpt-4o-mini"
	}

	baseUrl := customBaseUrl
	if baseUrl == "" {
		// Default to Responses API for "openai" provider
		baseUrl = "https://api.openai.com/v1/responses"
	}

	// Select appropriate provider implementation
	var provider OpenAIProvider
	if strings.Contains(baseUrl, "/responses") {
		provider = &ResponsesProvider{client: e.client}
	} else {
		provider = &ChatCompletionsProvider{client: e.client}
	}

	return provider.ExecuteToolLoop(ctx, model, prompt, systemPrompt, temperature, maxTokens, apiKey, baseUrl, tools, resolver, maxToolIter)
}

// ExecuteToolLoop implements tool calling for OpenAI Chat Completions API
func (p *ChatCompletionsProvider) ExecuteToolLoop(ctx context.Context, model, prompt, systemPrompt string, temperature float64, maxTokens int, apiKey, baseUrl string, tools []*ToolDefinition, resolver TemplateResolver, maxToolIter int) (string, map[string]interface{}, []map[string]interface{}, error) {
	// Build initial messages
	var messages []interface{}
	if systemPrompt != "" {
		messages = append(messages, map[string]string{
			"role":    "system",
			"content": systemPrompt,
		})
	}
	messages = append(messages, map[string]string{
		"role":    "user",
		"content": prompt,
	})

	// Convert tools to Chat Completions format
	openAITools := ConvertToolsToOpenAI(tools)

	var toolCallHistory []map[string]interface{}
	var lastFullResponse map[string]interface{}

	for iteration := 0; iteration < maxToolIter; iteration++ {
		select {
		case <-ctx.Done():
			return "", nil, toolCallHistory, ctx.Err()
		default:
		}

		reqBody := map[string]interface{}{
			"model":                 model,
			"messages":              messages,
			"temperature":           temperature,
			"max_completion_tokens": maxTokens,
			"tools":                 openAITools,
		}

		bodyBytes, err := json.Marshal(reqBody)
		if err != nil {
			return "", nil, toolCallHistory, err
		}

		req, err := http.NewRequestWithContext(ctx, "POST", baseUrl, bytes.NewReader(bodyBytes))
		if err != nil {
			return "", nil, toolCallHistory, err
		}

		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+apiKey)

		resp, err := p.client.Do(req)
		if err != nil {
			return "", nil, toolCallHistory, err
		}

		respBody, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return "", nil, toolCallHistory, err
		}

		if resp.StatusCode != http.StatusOK {
			return "", nil, toolCallHistory, fmt.Errorf("API error: %s", string(respBody))
		}

		var respMap map[string]interface{}
		if err := json.Unmarshal(respBody, &respMap); err != nil {
			return "", nil, toolCallHistory, err
		}

		// Parse Chat Completions response format
		choices, ok := respMap["choices"].([]interface{})
		if !ok || len(choices) == 0 {
			return "", nil, toolCallHistory, fmt.Errorf("no response from API")
		}

		choice, ok := choices[0].(map[string]interface{})
		if !ok {
			return "", nil, toolCallHistory, fmt.Errorf("invalid choice format")
		}

		message, ok := choice["message"].(map[string]interface{})
		if !ok {
			return "", nil, toolCallHistory, fmt.Errorf("invalid message format")
		}

		var content string
		if c, ok := message["content"].(string); ok {
			content = c
		}

		var rawToolCalls []interface{}
		if tc, ok := message["tool_calls"].([]interface{}); ok {
			rawToolCalls = tc
		}

		lastFullResponse = respMap

		if len(rawToolCalls) == 0 {
			return content, lastFullResponse, toolCallHistory, nil
		}

		// Add assistant message with tool calls to conversation
		messages = append(messages, map[string]interface{}{
			"role":       "assistant",
			"content":    content,
			"tool_calls": rawToolCalls,
		})

		// Parse tool calls
		var toolCalls []*ToolCall
		for _, tc := range rawToolCalls {
			tcMap, ok := tc.(map[string]interface{})
			if !ok {
				continue
			}

			function, ok := tcMap["function"].(map[string]interface{})
			if !ok {
				continue
			}

			toolCall := &ToolCall{
				ID:   tcMap["id"].(string),
				Name: function["name"].(string),
			}

			if argsStr, ok := function["arguments"].(string); ok {
				var args map[string]interface{}
				if err := json.Unmarshal([]byte(argsStr), &args); err == nil {
					toolCall.Arguments = args
				}
			}

			toolCalls = append(toolCalls, toolCall)
		}

		results, err := ExecuteToolCalls(ctx, toolCalls, tools, resolver)
		if err != nil {
			return "", nil, toolCallHistory, fmt.Errorf("failed to execute tool calls: %w", err)
		}

		// Record tool call history
		for i, tc := range toolCalls {
			historyEntry := map[string]interface{}{
				"name":      tc.Name,
				"arguments": tc.Arguments,
			}
			if i < len(results) {
				if results[i].Error != "" {
					historyEntry["error"] = results[i].Error
				} else {
					historyEntry["result"] = results[i].Content
				}
			}
			toolCallHistory = append(toolCallHistory, historyEntry)
		}

		// Add tool results to messages array
		toolMessages := FormatToolResultsForOpenAI(results)
		for _, tm := range toolMessages {
			messages = append(messages, tm)
		}
	}

	return "", nil, toolCallHistory, fmt.Errorf("exceeded maximum tool call iterations (%d)", maxToolIter)
}

// ExecuteToolLoop implements tool calling for OpenAI Responses API
func (p *ResponsesProvider) ExecuteToolLoop(ctx context.Context, model, prompt, systemPrompt string, temperature float64, maxTokens int, apiKey, baseUrl string, tools []*ToolDefinition, resolver TemplateResolver, maxToolIter int) (string, map[string]interface{}, []map[string]interface{}, error) {
	// Convert tools to Responses API format
	openAITools := ConvertToolsToOpenAIResponses(tools)

	var toolCallHistory []map[string]interface{}
	var lastFullResponse map[string]interface{}
	var previousResponseID string
	var pendingToolResults []map[string]interface{}

	for iteration := 0; iteration < maxToolIter; iteration++ {
		select {
		case <-ctx.Done():
			return "", nil, toolCallHistory, ctx.Err()
		default:
		}

		var reqBody map[string]interface{}

		if iteration == 0 {
			// First request: use input text
			inputText := prompt
			if systemPrompt != "" {
				inputText = systemPrompt + "\n\n" + prompt
			}

			reqBody = map[string]interface{}{
				"model":             model,
				"input":             inputText,
				"temperature":       temperature,
				"max_output_tokens": maxTokens,
			}
		} else {
			// Subsequent requests: use previous_response_id for conversation continuity
			reqBody = map[string]interface{}{
				"model":                model,
				"previous_response_id": previousResponseID,
				"input":                pendingToolResults,
				"temperature":          temperature,
				"max_output_tokens":    maxTokens,
			}
		}

		// Add tools
		if len(openAITools) > 0 {
			reqBody["tools"] = openAITools
		}

		bodyBytes, err := json.Marshal(reqBody)
		if err != nil {
			return "", nil, toolCallHistory, err
		}

		req, err := http.NewRequestWithContext(ctx, "POST", baseUrl, bytes.NewReader(bodyBytes))
		if err != nil {
			return "", nil, toolCallHistory, err
		}

		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+apiKey)

		resp, err := p.client.Do(req)
		if err != nil {
			return "", nil, toolCallHistory, err
		}

		respBody, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return "", nil, toolCallHistory, err
		}

		if resp.StatusCode != http.StatusOK {
			return "", nil, toolCallHistory, fmt.Errorf("API error: %s", string(respBody))
		}

		var respMap map[string]interface{}
		if err := json.Unmarshal(respBody, &respMap); err != nil {
			return "", nil, toolCallHistory, err
		}

		// Parse Responses API format
		outputArray, ok := respMap["output"].([]interface{})
		if !ok || len(outputArray) == 0 {
			return "", nil, toolCallHistory, fmt.Errorf("no output from Responses API")
		}

		var content string
		var rawToolCalls []interface{}

		// Loop through output items
		for _, outputItem := range outputArray {
			item, ok := outputItem.(map[string]interface{})
			if !ok {
				continue
			}

			itemType, _ := item["type"].(string)

			// Handle function_call items directly in output array
			if itemType == "function_call" || itemType == "tool_call" {
				rawToolCalls = append(rawToolCalls, item)
				continue
			}

			// Handle message items with nested content
			if itemType == "message" {
				contentArray, ok := item["content"].([]interface{})
				if !ok {
					continue
				}

				for _, contentItem := range contentArray {
					contentObj, ok := contentItem.(map[string]interface{})
					if !ok {
						continue
					}

					// Extract text content
					if contentObj["type"] == "output_text" {
						if text, ok := contentObj["text"].(string); ok {
							content = text
						}
					}

					// Extract tool/function calls from message content
					if contentObj["type"] == "tool_call" || contentObj["type"] == "function_call" {
						rawToolCalls = append(rawToolCalls, contentObj)
					}
				}
			}
		}

		lastFullResponse = respMap

		// Capture response ID for next iteration
		if respID, ok := respMap["id"].(string); ok {
			previousResponseID = respID
		}

		if len(rawToolCalls) == 0 {
			return content, lastFullResponse, toolCallHistory, nil
		}

		// Parse tool calls
		var toolCalls []*ToolCall
		for _, tc := range rawToolCalls {
			tcMap, ok := tc.(map[string]interface{})
			if !ok {
				continue
			}

			id := ""
			if callID, ok := tcMap["call_id"].(string); ok {
				id = callID
			} else if idVal, ok := tcMap["id"].(string); ok {
				id = idVal
			}

			toolCall := &ToolCall{
				ID:   id,
				Name: tcMap["name"].(string),
			}

			// Arguments can be either an object or a JSON string
			if args, ok := tcMap["arguments"].(map[string]interface{}); ok {
				toolCall.Arguments = args
			} else if argsStr, ok := tcMap["arguments"].(string); ok {
				var args map[string]interface{}
				if err := json.Unmarshal([]byte(argsStr), &args); err == nil {
					toolCall.Arguments = args
				}
			}

			toolCalls = append(toolCalls, toolCall)
		}

		results, err := ExecuteToolCalls(ctx, toolCalls, tools, resolver)
		if err != nil {
			return "", nil, toolCallHistory, fmt.Errorf("failed to execute tool calls: %w", err)
		}

		// Record tool call history
		for i, tc := range toolCalls {
			historyEntry := map[string]interface{}{
				"name":      tc.Name,
				"arguments": tc.Arguments,
			}
			if i < len(results) {
				if results[i].Error != "" {
					historyEntry["error"] = results[i].Error
				} else {
					historyEntry["result"] = results[i].Content
				}
			}
			toolCallHistory = append(toolCallHistory, historyEntry)
		}

		// Format tool results as function_call_output items
		pendingToolResults = FormatToolResultsForOpenAIResponses(results)
	}

	return "", nil, toolCallHistory, fmt.Errorf("exceeded maximum tool call iterations (%d)", maxToolIter)
}

// executeAnthropicToolLoop implements the tool calling loop for Anthropic
func (e *AIExecutor) executeAnthropicToolLoop(ctx context.Context, model, prompt, systemPrompt string, temperature float64, maxTokens int, configApiKey string, tools []*ToolDefinition, resolver TemplateResolver, maxToolIter int) (string, map[string]interface{}, []map[string]interface{}, error) {
	apiKey := configApiKey
	if apiKey == "" {
		apiKey = os.Getenv("ANTHROPIC_API_KEY")
	}
	if apiKey == "" {
		return "", nil, nil, fmt.Errorf("API key not provided and ANTHROPIC_API_KEY not set")
	}

	if model == "" {
		model = "claude-3-5-sonnet-latest"
	}

	// Build initial messages
	var messages []interface{}
	messages = append(messages, map[string]string{
		"role":    "user",
		"content": prompt,
	})

	// Convert tools to Anthropic format
	anthropicTools := ConvertToolsToAnthropic(tools)

	var toolCallHistory []map[string]interface{}
	var lastFullResponse map[string]interface{}

	for iteration := 0; iteration < maxToolIter; iteration++ {
		select {
		case <-ctx.Done():
			return "", nil, toolCallHistory, ctx.Err()
		default:
		}

		reqBody := map[string]interface{}{
			"model":      model,
			"messages":   messages,
			"max_tokens": maxTokens,
			"tools":      anthropicTools,
		}

		if systemPrompt != "" {
			reqBody["system"] = systemPrompt
		}

		bodyBytes, err := json.Marshal(reqBody)
		if err != nil {
			return "", nil, toolCallHistory, err
		}

		req, err := http.NewRequestWithContext(ctx, "POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(bodyBytes))
		if err != nil {
			return "", nil, toolCallHistory, err
		}

		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("anthropic-version", "2023-06-01")

		resp, err := e.client.Do(req)
		if err != nil {
			return "", nil, toolCallHistory, err
		}

		respBody, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return "", nil, toolCallHistory, err
		}

		if resp.StatusCode != http.StatusOK {
			return "", nil, toolCallHistory, fmt.Errorf("Anthropic API error: %s", string(respBody))
		}

		// Parse full response first
		var fullResp map[string]interface{}
		if err := json.Unmarshal(respBody, &fullResp); err != nil {
			return "", nil, toolCallHistory, err
		}
		lastFullResponse = fullResp

		// Also parse structured format for easier access
		var result struct {
			Content []struct {
				Type  string                 `json:"type"`
				Text  string                 `json:"text,omitempty"`
				ID    string                 `json:"id,omitempty"`
				Name  string                 `json:"name,omitempty"`
				Input map[string]interface{} `json:"input,omitempty"`
			} `json:"content"`
			StopReason string                 `json:"stop_reason"`
			Usage      map[string]interface{} `json:"usage"`
		}

		if err := json.Unmarshal(respBody, &result); err != nil {
			return "", nil, toolCallHistory, err
		}

		// Check for tool use blocks
		var toolCalls []*ToolCall
		var textContent strings.Builder

		for _, block := range result.Content {
			if block.Type == "text" {
				textContent.WriteString(block.Text)
			} else if block.Type == "tool_use" {
				toolCalls = append(toolCalls, &ToolCall{
					ID:        block.ID,
					Name:      block.Name,
					Arguments: block.Input,
				})
			}
		}

		// If no tool calls, return the final response
		if len(toolCalls) == 0 {
			return textContent.String(), lastFullResponse, toolCallHistory, nil
		}

		// Add assistant message with tool use to conversation
		// Must ensure "input" field is always present for tool_use blocks (Anthropic requirement)
		assistantContent := make([]map[string]interface{}, len(result.Content))
		for i, block := range result.Content {
			assistantContent[i] = map[string]interface{}{
				"type": block.Type,
			}
			if block.Type == "text" {
				assistantContent[i]["text"] = block.Text
			} else if block.Type == "tool_use" {
				assistantContent[i]["id"] = block.ID
				assistantContent[i]["name"] = block.Name
				// Ensure input is always present, even if empty
				if block.Input != nil {
					assistantContent[i]["input"] = block.Input
				} else {
					assistantContent[i]["input"] = map[string]interface{}{}
				}
			}
		}
		messages = append(messages, map[string]interface{}{
			"role":    "assistant",
			"content": assistantContent,
		})

		// Execute tool calls
		results, err := ExecuteToolCalls(ctx, toolCalls, tools, resolver)
		if err != nil {
			return "", nil, toolCallHistory, fmt.Errorf("failed to execute tool calls: %w", err)
		}

		// Record tool call history
		for i, tc := range toolCalls {
			historyEntry := map[string]interface{}{
				"name":      tc.Name,
				"arguments": tc.Arguments,
			}
			if i < len(results) {
				if results[i].Error != "" {
					historyEntry["error"] = results[i].Error
				} else {
					historyEntry["result"] = results[i].Content
				}
			}
			toolCallHistory = append(toolCallHistory, historyEntry)
		}

		// Add tool results as user message
		toolResultBlocks := FormatToolResultsForAnthropic(results)
		messages = append(messages, map[string]interface{}{
			"role":    "user",
			"content": toolResultBlocks,
		})
	}

	return "", nil, toolCallHistory, fmt.Errorf("exceeded maximum tool call iterations (%d)", maxToolIter)
}

// executeGeminiToolLoop implements the tool calling loop for Gemini
func (e *AIExecutor) executeGeminiToolLoop(ctx context.Context, model, prompt, systemPrompt string, temperature float64, maxTokens int, configApiKey string, tools []*ToolDefinition, resolver TemplateResolver, maxToolIter int) (string, map[string]interface{}, []map[string]interface{}, error) {
	apiKey := configApiKey
	if apiKey == "" {
		apiKey = os.Getenv("GEMINI_API_KEY")
	}
	if apiKey == "" {
		return "", nil, nil, fmt.Errorf("API key not provided and GEMINI_API_KEY not set")
	}

	if model == "" {
		model = "gemini-1.5-flash"
	}

	// Build initial contents
	contents := []map[string]interface{}{
		{
			"role": "user",
			"parts": []map[string]interface{}{
				{"text": prompt},
			},
		},
	}

	// Convert tools to Gemini format
	geminiTools := ConvertToolsToGemini(tools)

	var toolCallHistory []map[string]interface{}
	var lastFullResponse map[string]interface{}

	for iteration := 0; iteration < maxToolIter; iteration++ {
		select {
		case <-ctx.Done():
			return "", nil, toolCallHistory, ctx.Err()
		default:
		}

		reqBody := map[string]interface{}{
			"contents": contents,
			"tools":    []interface{}{geminiTools},
			"generationConfig": map[string]interface{}{
				"temperature":     temperature,
				"maxOutputTokens": maxTokens,
			},
		}

		if systemPrompt != "" {
			reqBody["system_instruction"] = map[string]interface{}{
				"parts": []map[string]string{
					{"text": systemPrompt},
				},
			}
		}

		bodyBytes, err := json.Marshal(reqBody)
		if err != nil {
			return "", nil, toolCallHistory, err
		}

		url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent?key=%s", model, apiKey)
		req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(bodyBytes))
		if err != nil {
			return "", nil, toolCallHistory, err
		}

		req.Header.Set("Content-Type", "application/json")

		resp, err := e.client.Do(req)
		if err != nil {
			return "", nil, toolCallHistory, err
		}

		respBody, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return "", nil, toolCallHistory, err
		}

		if resp.StatusCode != http.StatusOK {
			return "", nil, toolCallHistory, fmt.Errorf("Gemini API error: %s", string(respBody))
		}

		// Parse full response first
		var fullResp map[string]interface{}
		if err := json.Unmarshal(respBody, &fullResp); err != nil {
			return "", nil, toolCallHistory, err
		}
		lastFullResponse = fullResp

		// Also parse structured format for easier access
		var result struct {
			Candidates []struct {
				Content struct {
					Role  string `json:"role"`
					Parts []struct {
						Text             string `json:"text,omitempty"`
						ThoughtSignature string `json:"thoughtSignature,omitempty"`
						FunctionCall     *struct {
							Name string                 `json:"name"`
							Args map[string]interface{} `json:"args"`
						} `json:"functionCall,omitempty"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"candidates"`
			UsageMetadata map[string]interface{} `json:"usageMetadata"`
		}

		if err := json.Unmarshal(respBody, &result); err != nil {
			return "", nil, toolCallHistory, err
		}

		if len(result.Candidates) == 0 {
			return "", nil, toolCallHistory, fmt.Errorf("no response from Gemini")
		}
		candidate := result.Candidates[0]

		// Check for function calls
		var toolCalls []*ToolCall
		var textParts []string

		for _, part := range candidate.Content.Parts {
			// Check for text
			if part.Text != "" {
				textParts = append(textParts, part.Text)
			}
			// Check for function call
			if part.FunctionCall != nil {
				toolCalls = append(toolCalls, &ToolCall{
					ID:        fmt.Sprintf("call_%d", len(toolCalls)),
					Name:      part.FunctionCall.Name,
					Arguments: part.FunctionCall.Args,
				})
			}
		}

		// If no function calls, return the final response
		if len(toolCalls) == 0 {
			return strings.Join(textParts, ""), lastFullResponse, toolCallHistory, nil
		}

		// Add model response to contents
		contents = append(contents, map[string]interface{}{
			"role":  "model",
			"parts": candidate.Content.Parts,
		})

		// Execute tool calls
		results, err := ExecuteToolCalls(ctx, toolCalls, tools, resolver)
		if err != nil {
			return "", nil, toolCallHistory, fmt.Errorf("failed to execute tool calls: %w", err)
		}

		// Record tool call history
		for i, tc := range toolCalls {
			historyEntry := map[string]interface{}{
				"name":      tc.Name,
				"arguments": tc.Arguments,
			}
			if i < len(results) {
				if results[i].Error != "" {
					historyEntry["error"] = results[i].Error
				} else {
					historyEntry["result"] = results[i].Content
				}
			}
			toolCallHistory = append(toolCallHistory, historyEntry)
		}

		// Add function response to contents
		functionResponseParts := FormatToolResultsForGemini(results)
		contents = append(contents, map[string]interface{}{
			"role":  "function",
			"parts": functionResponseParts,
		})
	}

	return "", nil, toolCallHistory, fmt.Errorf("exceeded maximum tool call iterations (%d)", maxToolIter)
}

// executePromptToolLoop handles tool calling for providers without native support (Ollama, etc.)
func (e *AIExecutor) executePromptToolLoop(ctx context.Context, provider, model, prompt, systemPrompt string, temperature float64, maxTokens int, apiKey, ollamaUrl string, tools []*ToolDefinition, resolver TemplateResolver, maxToolIter int) (string, map[string]interface{}, []map[string]interface{}, error) {
	// Inject tool descriptions into system prompt
	toolPrompt := GenerateToolPrompt(tools)
	augmentedSystemPrompt := systemPrompt + toolPrompt

	var toolCallHistory []map[string]interface{}
	currentPrompt := prompt

	for iteration := 0; iteration < maxToolIter; iteration++ {
		select {
		case <-ctx.Done():
			return "", nil, toolCallHistory, ctx.Err()
		default:
		}

		// Call the provider
		var response string
		var err error

		switch provider {
		case "ollama":
			response, _, err = e.callOllama(ctx, model, currentPrompt, augmentedSystemPrompt, temperature, ollamaUrl, nil)
		default:
			return "", nil, nil, fmt.Errorf("unsupported provider for prompt-based tool calling: %s", provider)
		}

		if err != nil {
			return "", nil, toolCallHistory, err
		}

		// Parse tool calls from response
		toolCalls, remainingText, err := ParseToolCallsFromPromptResponse(response)
		if err != nil {
			return "", nil, toolCallHistory, err
		}

		// If no tool calls, return the response
		if len(toolCalls) == 0 {
			return response, nil, toolCallHistory, nil
		}

		// Execute tool calls
		results, err := ExecuteToolCalls(ctx, toolCalls, tools, resolver)
		if err != nil {
			return "", nil, toolCallHistory, fmt.Errorf("failed to execute tool calls: %w", err)
		}

		// Record tool call history
		for i, tc := range toolCalls {
			historyEntry := map[string]interface{}{
				"name":      tc.Name,
				"arguments": tc.Arguments,
			}
			if i < len(results) {
				if results[i].Error != "" {
					historyEntry["error"] = results[i].Error
				} else {
					historyEntry["result"] = results[i].Content
				}
			}
			toolCallHistory = append(toolCallHistory, historyEntry)
		}

		// Build follow-up prompt with tool results
		var resultText strings.Builder
		resultText.WriteString(remainingText)
		resultText.WriteString("\n\nTool Results:\n")
		for _, result := range results {
			if result.Error != "" {
				resultText.WriteString(fmt.Sprintf("- %s: Error - %s\n", result.Name, result.Error))
			} else {
				resultBytes, _ := json.Marshal(result.Content)
				resultText.WriteString(fmt.Sprintf("- %s: %s\n", result.Name, string(resultBytes)))
			}
		}
		resultText.WriteString("\nPlease continue your response based on these results.")

		currentPrompt = resultText.String()
	}

	return "", nil, toolCallHistory, fmt.Errorf("exceeded maximum tool call iterations (%d)", maxToolIter)
}

func (e *AIExecutor) callOpenAI(ctx context.Context, model, prompt, systemPrompt string, temperature float64, maxTokens int, configApiKey, customBaseUrl string, files []*FileObject) (string, map[string]interface{}, error) {
	apiKey := configApiKey
	if apiKey == "" {
		apiKey = os.Getenv("OPENAI_API_KEY")
	}
	if apiKey == "" {
		return "", nil, fmt.Errorf("API key not provided and OPENAI_API_KEY not set")
	}

	if model == "" {
		model = "gpt-4o-mini"
	}

	baseUrl := customBaseUrl
	if baseUrl == "" {
		baseUrl = "https://api.openai.com/v1/chat/completions"
	}

	// Build messages with optional file attachments
	var messages []interface{}
	if systemPrompt != "" {
		messages = append(messages, map[string]string{
			"role":    "system",
			"content": systemPrompt,
		})
	}

	// Build user message content - can be string or array with text + images
	if len(files) > 0 {
		// Multi-modal message with images
		var content []map[string]interface{}

		// Add text prompt
		content = append(content, map[string]interface{}{
			"type": "text",
			"text": prompt,
		})

		// Add image files
		for _, file := range files {
			if isImageMimeType(file.MimeType) {
				base64Data, err := FetchFileBase64(file)
				if err != nil {
					continue // Skip files that fail to fetch
				}
				content = append(content, map[string]interface{}{
					"type": "image_url",
					"image_url": map[string]string{
						"url": fmt.Sprintf("data:%s;base64,%s", file.MimeType, base64Data),
					},
				})
			}
		}

		messages = append(messages, map[string]interface{}{
			"role":    "user",
			"content": content,
		})
	} else {
		// Simple text message
		messages = append(messages, map[string]string{
			"role":    "user",
			"content": prompt,
		})
	}

	// Detect if using /responses API (different format than /chat/completions)
	isResponsesAPI := strings.Contains(baseUrl, "/responses")

	var reqBody map[string]interface{}

	if isResponsesAPI {
		// Responses API format: uses "input" instead of "messages"
		// For now, convert messages to simple text input (Responses API may not support complex messages)
		inputText := prompt
		if systemPrompt != "" {
			inputText = systemPrompt + "\n\n" + prompt
		}

		reqBody = map[string]interface{}{
			"model":             model,
			"input":             inputText,
			"temperature":       temperature,
			"max_output_tokens": maxTokens,
		}
	} else {
		// Chat Completions API format
		reqBody = map[string]interface{}{
			"model":                 model,
			"messages":              messages,
			"temperature":           temperature,
			"max_completion_tokens": maxTokens,
		}
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", baseUrl, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := e.client.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("API error: %s", string(respBody))
	}

	// Parse full response to preserve all fields from the API
	var fullResponse map[string]interface{}
	if err := json.Unmarshal(respBody, &fullResponse); err != nil {
		return "", nil, fmt.Errorf("failed to parse OpenAI response: %w\nRaw response: %s", err, string(respBody))
	}

	var content string

	if isResponsesAPI {
		// Responses API format: output[].content[].text
		output, ok := fullResponse["output"].([]interface{})
		if !ok || len(output) == 0 {
			return "", nil, fmt.Errorf("API returned no output\nRaw response: %s", string(respBody))
		}

		// Find first message with output_text
		for _, item := range output {
			msg, ok := item.(map[string]interface{})
			if !ok || msg["type"] != "message" {
				continue
			}

			contentArray, ok := msg["content"].([]interface{})
			if !ok {
				continue
			}

			for _, contentItem := range contentArray {
				contentObj, ok := contentItem.(map[string]interface{})
				if !ok {
					continue
				}

				if contentObj["type"] == "output_text" {
					if text, ok := contentObj["text"].(string); ok {
						content = text
						break
					}
				}
			}

			if content != "" {
				break
			}
		}

		if content == "" {
			return "", nil, fmt.Errorf("API returned no text content\nRaw response: %s", string(respBody))
		}
	} else {
		// Chat Completions API format: choices[].message.content
		choices, ok := fullResponse["choices"].([]interface{})
		if !ok || len(choices) == 0 {
			return "", nil, fmt.Errorf("API returned no choices\nRaw response: %s", string(respBody))
		}

		choice, ok := choices[0].(map[string]interface{})
		if !ok {
			return "", nil, fmt.Errorf("invalid choice format\nRaw response: %s", string(respBody))
		}

		message, ok := choice["message"].(map[string]interface{})
		if !ok {
			return "", nil, fmt.Errorf("invalid message format\nRaw response: %s", string(respBody))
		}

		content, _ = message["content"].(string)

		if content == "" {
			return "", nil, fmt.Errorf("API returned empty content\nRaw response: %s", string(respBody))
		}
	}

	// Return full response so all fields (id, created, system_fingerprint, time_info, etc.) are available
	return content, fullResponse, nil
}

func (e *AIExecutor) callAnthropic(ctx context.Context, model, prompt, systemPrompt string, temperature float64, maxTokens int, configApiKey string, files []*FileObject) (string, map[string]interface{}, error) {
	apiKey := configApiKey
	if apiKey == "" {
		apiKey = os.Getenv("ANTHROPIC_API_KEY")
	}
	if apiKey == "" {
		return "", nil, fmt.Errorf("API key not provided and ANTHROPIC_API_KEY not set")
	}

	if model == "" {
		model = "claude-3-5-sonnet-latest"
	}

	// Build message content - can include images and documents
	var content []map[string]interface{}

	// Add file attachments first (images, PDFs, etc.)
	for _, file := range files {
		base64Data, err := FetchFileBase64(file)
		if err != nil {
			continue // Skip files that fail to fetch
		}

		if isImageMimeType(file.MimeType) {
			// Image attachment
			content = append(content, map[string]interface{}{
				"type": "image",
				"source": map[string]string{
					"type":       "base64",
					"media_type": file.MimeType,
					"data":       base64Data,
				},
			})
		} else if file.MimeType == "application/pdf" {
			// PDF document attachment (Claude supports PDFs)
			content = append(content, map[string]interface{}{
				"type": "document",
				"source": map[string]string{
					"type":       "base64",
					"media_type": file.MimeType,
					"data":       base64Data,
				},
			})
		}
	}

	// Add text prompt
	content = append(content, map[string]interface{}{
		"type": "text",
		"text": prompt,
	})

	var messages []interface{}
	if len(files) > 0 {
		// Multi-content message
		messages = append(messages, map[string]interface{}{
			"role":    "user",
			"content": content,
		})
	} else {
		// Simple text message
		messages = append(messages, map[string]string{
			"role":    "user",
			"content": prompt,
		})
	}

	reqBody := map[string]interface{}{
		"model":      model,
		"messages":   messages,
		"max_tokens": maxTokens,
	}

	if systemPrompt != "" {
		reqBody["system"] = systemPrompt
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(bodyBytes))
	if err != nil {
		return "", nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := e.client.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("Anthropic API error: %s", string(respBody))
	}

	var result struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		Usage map[string]interface{} `json:"usage"`
	}

	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", nil, err
	}

	if len(result.Content) == 0 {
		return "", nil, fmt.Errorf("no response from Anthropic")
	}

	return result.Content[0].Text, result.Usage, nil
}

func (e *AIExecutor) callOllama(ctx context.Context, model, prompt, systemPrompt string, temperature float64, configOllamaUrl string, files []*FileObject) (string, map[string]interface{}, error) {
	// Note: Ollama supports images via the "images" field for vision models
	// For now, we'll include base64 images if present
	ollamaURL := configOllamaUrl
	if ollamaURL == "" {
		ollamaURL = os.Getenv("OLLAMA_URL")
	}
	if ollamaURL == "" {
		ollamaURL = "http://localhost:11434"
	}

	if model == "" {
		model = "llama3.2"
	}

	reqBody := map[string]interface{}{
		"model":  model,
		"prompt": prompt,
		"stream": false,
		"options": map[string]interface{}{
			"temperature": temperature,
		},
	}

	if systemPrompt != "" {
		reqBody["system"] = systemPrompt
	}

	// Add images for vision models (llava, etc.)
	if len(files) > 0 {
		var images []string
		for _, file := range files {
			if isImageMimeType(file.MimeType) {
				base64Data, err := FetchFileBase64(file)
				if err != nil {
					continue
				}
				images = append(images, base64Data)
			}
		}
		if len(images) > 0 {
			reqBody["images"] = images
		}
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", ollamaURL+"/api/generate", bytes.NewReader(bodyBytes))
	if err != nil {
		return "", nil, err
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("Ollama API error: %s", string(respBody))
	}

	var result struct {
		Response string `json:"response"`
	}

	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", nil, err
	}

	return result.Response, nil, nil
}

func (e *AIExecutor) callGemini(ctx context.Context, model, prompt, systemPrompt string, temperature float64, maxTokens int, configApiKey string, files []*FileObject) (string, map[string]interface{}, error) {
	apiKey := configApiKey
	if apiKey == "" {
		apiKey = os.Getenv("GEMINI_API_KEY")
	}
	if apiKey == "" {
		return "", nil, fmt.Errorf("API key not provided and GEMINI_API_KEY not set")
	}

	if model == "" {
		model = "gemini-1.5-flash"
	}

	// Build parts array with optional file attachments
	var parts []map[string]interface{}

	// Add file attachments first
	for _, file := range files {
		base64Data, err := FetchFileBase64(file)
		if err != nil {
			continue
		}
		parts = append(parts, map[string]interface{}{
			"inline_data": map[string]string{
				"mime_type": file.MimeType,
				"data":      base64Data,
			},
		})
	}

	// Add text prompt
	parts = append(parts, map[string]interface{}{
		"text": prompt,
	})

	contents := []map[string]interface{}{
		{
			"role":  "user",
			"parts": parts,
		},
	}

	// Handle system prompt (Gemini uses system_instruction)
	var systemInstruction map[string]interface{}
	if systemPrompt != "" {
		systemInstruction = map[string]interface{}{
			"parts": []map[string]string{
				{"text": systemPrompt},
			},
		}
	}

	reqBody := map[string]interface{}{
		"contents": contents,
		"generationConfig": map[string]interface{}{
			"temperature":     temperature,
			"maxOutputTokens": maxTokens,
		},
	}

	if systemInstruction != nil {
		reqBody["system_instruction"] = systemInstruction
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", nil, err
	}

	url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent?key=%s", model, apiKey)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", nil, err
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("Gemini API error: %s", string(respBody))
	}

	var result struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
		UsageMetadata map[string]interface{} `json:"usageMetadata"`
	}

	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", nil, err
	}

	if len(result.Candidates) == 0 || len(result.Candidates[0].Content.Parts) == 0 {
		return "", nil, fmt.Errorf("no response from Gemini")
	}

	var textParts []string
	for _, part := range result.Candidates[0].Content.Parts {
		if part.Text != "" {
			textParts = append(textParts, part.Text)
		}
	}

	return strings.Join(textParts, ""), result.UsageMetadata, nil
}

// isImageMimeType checks if a MIME type is an image
func isImageMimeType(mimeType string) bool {
	switch mimeType {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return true
	default:
		return false
	}
}

// SupportsStreaming returns true if the AI executor can stream with the given config
func (e *AIExecutor) SupportsStreaming(config map[string]interface{}) bool {
	// Streaming is disabled for AI nodes for now
	return false
}

// ExecuteStreaming runs the AI step with streaming, calling onStream for each token chunk
func (e *AIExecutor) ExecuteStreaming(ctx context.Context, step *StepDefinition, resolver TemplateResolver, onStream StreamCallback) (*StepResult, error) {
	config := step.Config
	if config == nil {
		return nil, fmt.Errorf("ai step requires config")
	}

	// Get provider (default: openai)
	provider := "openai"
	if p, ok := config["provider"].(string); ok && p != "" {
		provider = p
	}

	// Get model
	model := ""
	if m, ok := config["model"].(string); ok {
		model = resolver.ResolveString(m)
	}

	// Get and resolve prompt
	promptTemplate, ok := config["prompt"].(string)
	if !ok || promptTemplate == "" {
		return nil, fmt.Errorf("ai step requires 'prompt'")
	}
	prompt := resolver.ResolveString(promptTemplate)

	// Get system prompt
	systemPrompt := ""
	if sp, ok := config["systemPrompt"].(string); ok {
		systemPrompt = resolver.ResolveString(sp)
	}

	// Get temperature
	temperature := 0.7
	if t, ok := config["temperature"].(float64); ok {
		temperature = t
	}

	// Get max tokens
	maxTokens := 1000
	if mt, ok := config["maxTokens"].(float64); ok {
		maxTokens = int(mt)
	}

	// Get API key
	apiKey := ""
	if ak, ok := config["apiKey"].(string); ok {
		apiKey = resolver.ResolveString(ak)
	}

	// Get custom base URL
	baseUrl := ""
	if bu, ok := config["baseUrl"].(string); ok {
		baseUrl = resolver.ResolveString(bu)
	}

	// Execute streaming based on provider
	var fullResponse strings.Builder
	var usage map[string]interface{}
	var err error

	switch provider {
	case "openai", "openai-compatible":
		fullResponse, usage, err = e.callOpenAIStreaming(ctx, model, prompt, systemPrompt, temperature, maxTokens, apiKey, baseUrl, onStream)
	case "anthropic":
		fullResponse, usage, err = e.callAnthropicStreaming(ctx, model, prompt, systemPrompt, temperature, maxTokens, apiKey, onStream)
	default:
		// Fall back to non-streaming for unsupported providers
		return e.Execute(ctx, step, resolver)
	}

	if err != nil {
		return &StepResult{
			Output: map[string]interface{}{
				"error":    err.Error(),
				"provider": provider,
				"model":    model,
			},
		}, err
	}

	output := map[string]interface{}{
		"response": fullResponse.String(),
		"provider": provider,
		"model":    model,
	}
	if usage != nil {
		output["usage"] = usage
	}

	return &StepResult{
		Output: output,
	}, nil
}

// callOpenAIStreaming makes a streaming call to OpenAI API
func (e *AIExecutor) callOpenAIStreaming(ctx context.Context, model, prompt, systemPrompt string, temperature float64, maxTokens int, configApiKey, customBaseUrl string, onStream StreamCallback) (strings.Builder, map[string]interface{}, error) {
	var fullResponse strings.Builder

	apiKey := configApiKey
	if apiKey == "" {
		apiKey = os.Getenv("OPENAI_API_KEY")
	}
	if apiKey == "" {
		return fullResponse, nil, fmt.Errorf("API key not provided and OPENAI_API_KEY not set")
	}

	if model == "" {
		model = "gpt-4o-mini"
	}

	baseUrl := customBaseUrl
	if baseUrl == "" {
		baseUrl = "https://api.openai.com/v1/chat/completions"
	}

	// Detect if using /responses API (different format than /chat/completions)
	isResponsesAPI := strings.Contains(baseUrl, "/responses")

	var reqBody map[string]interface{}

	if isResponsesAPI {
		// Responses API format: uses "input" instead of "messages"
		inputText := prompt
		if systemPrompt != "" {
			inputText = systemPrompt + "\n\n" + prompt
		}
		reqBody = map[string]interface{}{
			"model":             model,
			"input":             inputText,
			"temperature":       temperature,
			"max_output_tokens": maxTokens,
			"stream":            true,
		}
	} else {
		// Chat Completions API format
		var messages []interface{}
		if systemPrompt != "" {
			messages = append(messages, map[string]string{
				"role":    "system",
				"content": systemPrompt,
			})
		}
		messages = append(messages, map[string]string{
			"role":    "user",
			"content": prompt,
		})

		reqBody = map[string]interface{}{
			"model":                 model,
			"messages":              messages,
			"temperature":           temperature,
			"max_completion_tokens": maxTokens,
			"stream":                true,
		}
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return fullResponse, nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", baseUrl, bytes.NewReader(bodyBytes))
	if err != nil {
		return fullResponse, nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	// Use a client without timeout for streaming
	streamClient := &http.Client{}
	resp, err := streamClient.Do(req)
	if err != nil {
		return fullResponse, nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fullResponse, nil, fmt.Errorf("API error: %s", string(respBody))
	}

	// Parse SSE stream
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return fullResponse, nil, ctx.Err()
		default:
		}

		line := scanner.Text()

		if !strings.HasPrefix(line, "data:") {
			continue
		}

		data := strings.TrimPrefix(line, "data:")
		data = strings.TrimSpace(data)

		if data == "[DONE]" {
			break
		}

		// Parse as generic map to handle both API formats
		var chunkMap map[string]interface{}
		if err := json.Unmarshal([]byte(data), &chunkMap); err != nil {
			continue
		}

		var content string

		if isResponsesAPI {
			// Responses API streaming format: delta.content or output[].content[].delta
			// Try direct delta.content first (simpler format)
			if delta, ok := chunkMap["delta"].(map[string]interface{}); ok {
				if c, ok := delta["content"].(string); ok {
					content = c
				}
			}
			// Or look in output array
			if content == "" {
				if output, ok := chunkMap["output"].([]interface{}); ok && len(output) > 0 {
					if msg, ok := output[0].(map[string]interface{}); ok {
						if contentArray, ok := msg["content"].([]interface{}); ok && len(contentArray) > 0 {
							if contentItem, ok := contentArray[0].(map[string]interface{}); ok {
								if delta, ok := contentItem["delta"].(map[string]interface{}); ok {
									if c, ok := delta["text"].(string); ok {
										content = c
									}
								}
							}
						}
					}
				}
			}
		} else {
			// Chat Completions streaming format: choices[].delta.content
			if choices, ok := chunkMap["choices"].([]interface{}); ok && len(choices) > 0 {
				if choice, ok := choices[0].(map[string]interface{}); ok {
					if delta, ok := choice["delta"].(map[string]interface{}); ok {
						if c, ok := delta["content"].(string); ok {
							content = c
						}
					}
				}
			}
		}

		if content != "" {
			fullResponse.WriteString(content)

			// Send streaming update
			if onStream != nil {
				onStream(&StreamUpdate{
					Partial: content,
				})
			}
		}
	}

	return fullResponse, nil, nil
}

// callAnthropicStreaming makes a streaming call to Anthropic API
func (e *AIExecutor) callAnthropicStreaming(ctx context.Context, model, prompt, systemPrompt string, temperature float64, maxTokens int, configApiKey string, onStream StreamCallback) (strings.Builder, map[string]interface{}, error) {
	var fullResponse strings.Builder

	apiKey := configApiKey
	if apiKey == "" {
		apiKey = os.Getenv("ANTHROPIC_API_KEY")
	}
	if apiKey == "" {
		return fullResponse, nil, fmt.Errorf("API key not provided and ANTHROPIC_API_KEY not set")
	}

	if model == "" {
		model = "claude-3-5-sonnet-latest"
	}

	messages := []interface{}{
		map[string]string{
			"role":    "user",
			"content": prompt,
		},
	}

	reqBody := map[string]interface{}{
		"model":      model,
		"messages":   messages,
		"max_tokens": maxTokens,
		"stream":     true,
	}

	if systemPrompt != "" {
		reqBody["system"] = systemPrompt
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return fullResponse, nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(bodyBytes))
	if err != nil {
		return fullResponse, nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	// Use a client without timeout for streaming
	streamClient := &http.Client{}
	resp, err := streamClient.Do(req)
	if err != nil {
		return fullResponse, nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fullResponse, nil, fmt.Errorf("Anthropic API error: %s", string(respBody))
	}

	// Parse SSE stream from Anthropic
	// Anthropic sends events like: event: content_block_delta\ndata: {...}
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return fullResponse, nil, ctx.Err()
		default:
		}

		line := scanner.Text()

		if !strings.HasPrefix(line, "data:") {
			continue
		}

		data := strings.TrimPrefix(line, "data:")
		data = strings.TrimSpace(data)

		var event struct {
			Type  string `json:"type"`
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		}

		if err := json.Unmarshal([]byte(data), &event); err != nil {
			continue
		}

		if event.Type == "content_block_delta" && event.Delta.Text != "" {
			fullResponse.WriteString(event.Delta.Text)

			// Send streaming update
			if onStream != nil {
				onStream(&StreamUpdate{
					Partial: event.Delta.Text,
				})
			}
		}
	}

	return fullResponse, nil, nil
}

func (e *AIExecutor) buildSystemPromptWithSkills(originalPrompt string, enabledSkills []string, gatingConfig map[string]interface{}) (string, []string) {
	if len(enabledSkills) == 0 {
		return originalPrompt, nil
	}

	skillsDir := os.Getenv(config.AXIOM_OPENCLAW_SKILLS_DIR_ENV)
	if skillsDir == "" {
		skillsDir = config.AXIOM_OPENCLAW_SKILLS_DIR_DEFAULT
	}

	maxSizeKB := 20
	if envVal := os.Getenv("SKILL_PROMPT_MAX_SIZE_KB"); envVal != "" {
		if v, err := strconv.Atoi(envVal); err == nil && v > 0 {
			maxSizeKB = v
		}
	}
	maxSizeBytes := maxSizeKB * 1024
	hardLimitBytes := maxSizeBytes + (maxSizeBytes / 2)

	type skillEntry struct {
		name string
		body string
	}
	var skills []skillEntry
	var warnings []string

	for _, slug := range enabledSkills {
		skillPath := filepath.Join(skillsDir, slug, "SKILL.md")
		content, err := os.ReadFile(skillPath)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("skill %q: failed to read SKILL.md: %v", slug, err))
			continue
		}

		parsed, err := skillmd.ParseSkillMD(content)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("skill %q: failed to parse: %v", slug, err))
			continue
		}

		avail := skillmd.CheckAvailability(parsed, gatingConfig)
		if !avail.IsAvailable {
			warnings = append(warnings, fmt.Sprintf("skill %q: unavailable: %s", parsed.Name, strings.Join(avail.Reasons, "; ")))
			continue
		}

		skills = append(skills, skillEntry{name: parsed.Name, body: parsed.Body})
	}

	sort.Slice(skills, func(i, j int) bool {
		return skills[i].name < skills[j].name
	})

	var sb strings.Builder
	totalSize := 0
	softLimitWarned := false

	for _, s := range skills {
		injection := fmt.Sprintf("--- SKILL: %s ---\n%s\n--- END SKILL ---\n", s.name, s.body)
		projectedSize := totalSize + len(injection)

		// Hard limit: stop injecting skills beyond this point
		if projectedSize > hardLimitBytes {
			warnings = append(warnings, fmt.Sprintf("Skill content truncated: %dKB exceeds limit of %dKB", projectedSize/1024, maxSizeKB))
			break
		}

		// Soft limit: warn when approaching or exceeding
		if !softLimitWarned && projectedSize > maxSizeBytes {
			warnings = append(warnings, fmt.Sprintf("Skill prompt approaching limit: %dKB of %dKB budget", projectedSize/1024, maxSizeKB))
			softLimitWarned = true
		}

		sb.WriteString(injection)
		totalSize = projectedSize
	}

	if sb.Len() > 0 {
		sb.WriteString("\n")
	}
	sb.WriteString(originalPrompt)

	return sb.String(), warnings
}

// GetSkillPromptBudget returns the configured soft and hard limits in bytes.
func GetSkillPromptBudget() (softLimitBytes, hardLimitBytes int) {
	maxSizeKB := 20
	if envVal := os.Getenv("SKILL_PROMPT_MAX_SIZE_KB"); envVal != "" {
		if v, err := strconv.Atoi(envVal); err == nil && v > 0 {
			maxSizeKB = v
		}
	}
	maxSizeBytes := maxSizeKB * 1024
	return maxSizeBytes, maxSizeBytes + (maxSizeBytes / 2)
}

// CalculateSkillInjectionSize returns the size in bytes of a skill's prompt injection.
func CalculateSkillInjectionSize(skillName, skillBody string) int {
	return len(fmt.Sprintf("--- SKILL: %s ---\n%s\n--- END SKILL ---\n", skillName, skillBody))
}
