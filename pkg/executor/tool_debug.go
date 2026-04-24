package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// DebugToolExecutor implements a debugging tool for inspecting variables and logging
type DebugToolExecutor struct {
	def      *ToolDefinition
	resolver TemplateResolver
}

// NewDebugToolExecutor creates a new debug tool executor
func NewDebugToolExecutor(def *ToolDefinition, resolver TemplateResolver) (ToolExecutor, error) {
	return &DebugToolExecutor{
		def:      def,
		resolver: resolver,
	}, nil
}

// DebugOutput represents the structured output of a debug operation
type DebugOutput struct {
	Timestamp string                 `json:"timestamp"`
	Message   string                 `json:"message"`
	Data      map[string]interface{} `json:"data,omitempty"`
	Level     string                 `json:"level"`
	Formatted string                 `json:"formatted"`
	RawData   interface{}            `json:"raw_data,omitempty"`
}

// Execute performs the debug operation
func (e *DebugToolExecutor) Execute(ctx context.Context, args map[string]interface{}) (*ToolResult, error) {
	config := e.def.Config

	// Get debug level from config (default: "info")
	level := "info"
	if configLevel, ok := config["level"].(string); ok && configLevel != "" {
		level = configLevel
	}

	// Get message from args
	message := ""
	if msgArg, ok := args["message"].(string); ok {
		message = msgArg
	}

	// Get data to inspect from args
	var data map[string]interface{}
	if dataArg, ok := args["data"].(map[string]interface{}); ok {
		data = dataArg
	}

	// Get variables to inspect from args
	var variables []string
	if varsArg, ok := args["variables"].([]interface{}); ok {
		for _, v := range varsArg {
			if varName, ok := v.(string); ok {
				variables = append(variables, varName)
			}
		}
	} else if varsArg, ok := args["variables"].(string); ok {
		// Support comma-separated string
		variables = strings.Split(varsArg, ",")
		for i := range variables {
			variables[i] = strings.TrimSpace(variables[i])
		}
	}

	// Resolve variables from context if specified
	resolvedData := make(map[string]interface{})
	if len(variables) > 0 {
		for _, varName := range variables {
			if varName == "" {
				continue
			}
			// Try to resolve the variable from context
			resolved := e.resolver.ResolveString(fmt.Sprintf("{{%s}}", varName))

			// Try to parse as JSON if it looks like JSON
			var parsedValue interface{}
			if strings.HasPrefix(resolved, "{") || strings.HasPrefix(resolved, "[") {
				if err := json.Unmarshal([]byte(resolved), &parsedValue); err == nil {
					resolvedData[varName] = parsedValue
				} else {
					resolvedData[varName] = resolved
				}
			} else {
				resolvedData[varName] = resolved
			}
		}
	}

	// Merge explicit data with resolved variables
	if data != nil {
		for k, v := range data {
			resolvedData[k] = v
		}
	}

	// Build formatted output
	formatted := e.formatDebugOutput(message, resolvedData, level)

	// Create debug output
	output := DebugOutput{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Message:   message,
		Data:      resolvedData,
		Level:     level,
		Formatted: formatted,
	}

	// Include raw data if requested
	if includeRaw, ok := config["includeRaw"].(bool); ok && includeRaw {
		output.RawData = args
	}

	return &ToolResult{
		Name:   e.def.Name,
		Result: output,
	}, nil
}

// formatDebugOutput creates a human-readable formatted string
func (e *DebugToolExecutor) formatDebugOutput(message string, data map[string]interface{}, level string) string {
	var builder strings.Builder

	// Header with level
	builder.WriteString(fmt.Sprintf("[%s] ", strings.ToUpper(level)))

	// Message
	if message != "" {
		builder.WriteString(message)
		builder.WriteString("\n")
	}

	// Data
	if len(data) > 0 {
		builder.WriteString("\nInspected Variables:\n")
		builder.WriteString(strings.Repeat("-", 50))
		builder.WriteString("\n")

		for key, value := range data {
			builder.WriteString(fmt.Sprintf("\n%s:\n", key))

			// Pretty print the value
			formatted := e.formatValue(value, "  ")
			builder.WriteString(formatted)
			builder.WriteString("\n")
		}

		builder.WriteString(strings.Repeat("-", 50))
	}

	return builder.String()
}

// formatValue formats a value for display with proper indentation
func (e *DebugToolExecutor) formatValue(value interface{}, indent string) string {
	switch v := value.(type) {
	case map[string]interface{}:
		return e.formatMap(v, indent)
	case []interface{}:
		return e.formatArray(v, indent)
	case string:
		return indent + fmt.Sprintf("%q", v)
	case nil:
		return indent + "null"
	default:
		// Try to marshal as JSON for complex types
		if jsonBytes, err := json.MarshalIndent(v, indent, "  "); err == nil {
			return indent + string(jsonBytes)
		}
		return indent + fmt.Sprintf("%v", v)
	}
}

// formatMap formats a map for display
func (e *DebugToolExecutor) formatMap(m map[string]interface{}, indent string) string {
	if len(m) == 0 {
		return indent + "{}"
	}

	var builder strings.Builder
	builder.WriteString(indent + "{\n")

	for key, value := range m {
		builder.WriteString(fmt.Sprintf("%s  %q: ", indent, key))

		// Format nested value
		switch v := value.(type) {
		case map[string]interface{}:
			builder.WriteString("\n")
			builder.WriteString(e.formatMap(v, indent+"    "))
		case []interface{}:
			builder.WriteString("\n")
			builder.WriteString(e.formatArray(v, indent+"    "))
		case string:
			builder.WriteString(fmt.Sprintf("%q", v))
		case nil:
			builder.WriteString("null")
		default:
			builder.WriteString(fmt.Sprintf("%v", v))
		}

		builder.WriteString("\n")
	}

	builder.WriteString(indent + "}")
	return builder.String()
}

// formatArray formats an array for display
func (e *DebugToolExecutor) formatArray(arr []interface{}, indent string) string {
	if len(arr) == 0 {
		return indent + "[]"
	}

	var builder strings.Builder
	builder.WriteString(indent + "[\n")

	for i, value := range arr {
		builder.WriteString(fmt.Sprintf("%s  [%d]: ", indent, i))

		// Format nested value
		switch v := value.(type) {
		case map[string]interface{}:
			builder.WriteString("\n")
			builder.WriteString(e.formatMap(v, indent+"    "))
		case []interface{}:
			builder.WriteString("\n")
			builder.WriteString(e.formatArray(v, indent+"    "))
		case string:
			builder.WriteString(fmt.Sprintf("%q", v))
		case nil:
			builder.WriteString("null")
		default:
			builder.WriteString(fmt.Sprintf("%v", v))
		}

		builder.WriteString("\n")
	}

	builder.WriteString(indent + "]")
	return builder.String()
}
