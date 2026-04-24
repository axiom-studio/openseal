package executor

import (
	"context"
	"fmt"
	"sync"
)

// ToolDefinition defines a tool that can be called by an AI model
type ToolDefinition struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Parameters  map[string]interface{} `json:"parameters"` // JSON Schema
	Config      map[string]interface{} `json:"config"`     // Tool-specific config
}

// ToolResult represents the result of a tool execution
type ToolResult struct {
	Name   string      `json:"name"`
	Result interface{} `json:"result"`
	Error  string      `json:"error,omitempty"`
}

// ToolExecutor interface for executing tools
type ToolExecutor interface {
	Execute(ctx context.Context, args map[string]interface{}) (*ToolResult, error)
}

// ToolFactory creates a ToolExecutor from a ToolDefinition
type ToolFactory func(def *ToolDefinition, resolver TemplateResolver) (ToolExecutor, error)

// ToolRegistry manages tool factories by type
type ToolRegistry struct {
	factories map[string]ToolFactory
	tools     map[string]*ToolDefinition // Registered tool definitions from skills
	mu        sync.RWMutex
}

// NewToolRegistry creates a new tool registry with default tool types
func NewToolRegistry() *ToolRegistry {
	r := &ToolRegistry{
		factories: make(map[string]ToolFactory),
		tools:     make(map[string]*ToolDefinition),
	}

	// Register built-in tool types
	r.Register("vector_search", NewVectorSearchToolExecutor)
	r.Register("debug", NewDebugToolExecutor)
	r.Register("memory", NewMemoryToolExecutor)
	r.Register("mcp", NewMCPToolExecutor)
	r.Register("openclaw-skill", NewOpenClawSkillToolExecutor)

	return r
}

// Register adds a tool factory to the registry
func (r *ToolRegistry) Register(toolType string, factory ToolFactory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.factories[toolType] = factory
}

// CreateExecutor creates a ToolExecutor for the given tool definition
func (r *ToolRegistry) CreateExecutor(def *ToolDefinition, resolver TemplateResolver) (ToolExecutor, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// Determine tool type from config
	toolType := ""
	if t, ok := def.Config["type"].(string); ok {
		toolType = t
	}
	if toolType == "" {
		return nil, fmt.Errorf("tool definition requires 'type' in config")
	}

	factory, ok := r.factories[toolType]
	if !ok {
		return nil, fmt.Errorf("no factory registered for tool type: %s", toolType)
	}

	return factory(def, resolver)
}

// HasToolType checks if a tool type is registered
func (r *ToolRegistry) HasToolType(toolType string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.factories[toolType]
	return ok
}

// RegisterTool registers a tool definition from a skill
// The tool can then be discovered and used by AI agents
func (r *ToolRegistry) RegisterTool(tool *ToolDefinition) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[tool.Name] = tool
}

// RegisterTools registers multiple tool definitions from a skill
func (r *ToolRegistry) RegisterTools(tools []*ToolDefinition) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, tool := range tools {
		r.tools[tool.Name] = tool
	}
}

// UnregisterTool removes a tool definition by name
func (r *ToolRegistry) UnregisterTool(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.tools, name)
}

// GetTool returns a tool definition by name
func (r *ToolRegistry) GetTool(name string) *ToolDefinition {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.tools[name]
}

// GetAllTools returns all registered tool definitions
func (r *ToolRegistry) GetAllTools() []*ToolDefinition {
	r.mu.RLock()
	defer r.mu.RUnlock()
	tools := make([]*ToolDefinition, 0, len(r.tools))
	for _, tool := range r.tools {
		tools = append(tools, tool)
	}
	return tools
}

// GetToolsByPrefix returns tools whose names start with the given prefix
// Useful for getting all tools from a specific skill (e.g., "web_" prefix)
func (r *ToolRegistry) GetToolsByPrefix(prefix string) []*ToolDefinition {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var tools []*ToolDefinition
	for name, tool := range r.tools {
		if len(name) >= len(prefix) && name[:len(prefix)] == prefix {
			tools = append(tools, tool)
		}
	}
	return tools
}

// Global tool registry singleton
var globalToolRegistry = NewToolRegistry()

// GetGlobalToolRegistry returns the global tool registry
func GetGlobalToolRegistry() *ToolRegistry {
	return globalToolRegistry
}

// ToolCall represents a tool call request from an AI model
type ToolCall struct {
	ID        string                 `json:"id"`
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments"`
}

// ToolCallResult represents the result of executing a tool call
type ToolCallResult struct {
	ToolCallID string      `json:"tool_call_id"`
	Name       string      `json:"name"`
	Content    interface{} `json:"content"`
	Error      string      `json:"error,omitempty"`
}

// ConvertToolsToOpenAI converts tool definitions to OpenAI Chat Completions function format
func ConvertToolsToOpenAI(tools []*ToolDefinition) []map[string]interface{} {
	result := make([]map[string]interface{}, len(tools))
	for i, tool := range tools {
		result[i] = map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        tool.Name,
				"description": tool.Description,
				"parameters":  tool.Parameters,
			},
		}
	}
	return result
}

// ConvertToolsToOpenAIResponses converts tool definitions to OpenAI Responses API tool format
// The Responses API uses a flatter structure without the nested "function" object
func ConvertToolsToOpenAIResponses(tools []*ToolDefinition) []map[string]interface{} {
	result := make([]map[string]interface{}, len(tools))
	for i, tool := range tools {
		result[i] = map[string]interface{}{
			"type":        "function",
			"name":        tool.Name,
			"description": tool.Description,
			"parameters":  tool.Parameters,
		}
	}
	return result
}

// ConvertToolsToAnthropic converts tool definitions to Anthropic tool format
func ConvertToolsToAnthropic(tools []*ToolDefinition) []map[string]interface{} {
	result := make([]map[string]interface{}, len(tools))
	for i, tool := range tools {
		result[i] = map[string]interface{}{
			"name":         tool.Name,
			"description":  tool.Description,
			"input_schema": tool.Parameters,
		}
	}
	return result
}

// sanitizeParametersForGemini removes fields that Gemini doesn't support
func sanitizeParametersForGemini(params map[string]interface{}) map[string]interface{} {
	result := make(map[string]interface{})
	for key, value := range params {
		// Skip unsupported fields
		if key == "additionalProperties" {
			continue
		}

		// Recursively sanitize nested maps
		if mapVal, ok := value.(map[string]interface{}); ok {
			result[key] = sanitizeParametersForGemini(mapVal)
		} else {
			result[key] = value
		}
	}
	return result
}

// ConvertToolsToGemini converts tool definitions to Gemini tool format
func ConvertToolsToGemini(tools []*ToolDefinition) map[string]interface{} {
	functions := make([]map[string]interface{}, len(tools))
	for i, tool := range tools {
		// Sanitize parameters to remove Gemini-unsupported fields
		sanitizedParams := sanitizeParametersForGemini(tool.Parameters)

		functions[i] = map[string]interface{}{
			"name":        tool.Name,
			"description": tool.Description,
			"parameters":  sanitizedParams,
		}
	}
	return map[string]interface{}{
		"function_declarations": functions,
	}
}

// GenerateToolPrompt generates a system prompt section describing available tools for non-native providers
func GenerateToolPrompt(tools []*ToolDefinition) string {
	if len(tools) == 0 {
		return ""
	}

	prompt := "\n\nYou have access to the following tools. To use a tool, respond with a JSON object in this exact format:\n"
	prompt += "```json\n{\"tool_call\": {\"name\": \"tool_name\", \"arguments\": {...}}}\n```\n\n"
	prompt += "Available tools:\n"

	for _, tool := range tools {
		prompt += fmt.Sprintf("\n### %s\n%s\n", tool.Name, tool.Description)
		if len(tool.Parameters) > 0 {
			prompt += "Parameters:\n"
			if props, ok := tool.Parameters["properties"].(map[string]interface{}); ok {
				required := make(map[string]bool)
				if reqList, ok := tool.Parameters["required"].([]interface{}); ok {
					for _, r := range reqList {
						if s, ok := r.(string); ok {
							required[s] = true
						}
					}
				}
				for name, propVal := range props {
					if prop, ok := propVal.(map[string]interface{}); ok {
						desc := ""
						if d, ok := prop["description"].(string); ok {
							desc = d
						}
						propType := "any"
						if t, ok := prop["type"].(string); ok {
							propType = t
						}
						reqStr := ""
						if required[name] {
							reqStr = " (required)"
						}
						prompt += fmt.Sprintf("- %s (%s)%s: %s\n", name, propType, reqStr, desc)
					}
				}
			}
		}
	}

	prompt += "\nAfter receiving tool results, continue your response. If no tool is needed, respond normally without the JSON format.\n"
	return prompt
}
