package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCPToolExecutor connects to MCP servers and calls their tools
// Config:
//   - command: The MCP server command (e.g., "npx", "uvx", "/path/to/server")
//   - args: Arguments for the command
//   - env: Environment variables
//   - tool: The specific tool name to call (set by AI node when invoking)
type MCPToolExecutor struct {
	def      *ToolDefinition
	resolver TemplateResolver

	// Connection caching
	clients   map[string]*mcp.Client
	sessions  map[string]*mcp.ClientSession
	mu        sync.RWMutex
}

// NewMCPToolExecutor creates a new MCP tool executor
func NewMCPToolExecutor(def *ToolDefinition, resolver TemplateResolver) (ToolExecutor, error) {
	return &MCPToolExecutor{
		def:      def,
		resolver: resolver,
		clients:  make(map[string]*mcp.Client),
		sessions: make(map[string]*mcp.ClientSession),
	}, nil
}

// Execute calls an MCP tool
func (e *MCPToolExecutor) Execute(ctx context.Context, args map[string]interface{}) (*ToolResult, error) {
	config := e.def.Config

	// Get MCP server config
	command, _ := config["command"].(string)
	if command == "" {
		return nil, fmt.Errorf("MCP tool requires 'command' in config")
	}

	// Auto-convert npx to bunx for faster execution
	if command == "npx" {
		command = "bunx"
	}

	// Get tool name from args (passed by AI node) or config
	toolName, _ := args["tool"].(string)
	if toolName == "" {
		toolName, _ = config["tool"].(string)
	}
	if toolName == "" {
		return nil, fmt.Errorf("MCP tool requires 'tool' name in args or config")
	}

	// Build server key for caching
	serverKey := e.buildServerKey(config)

	// Get or create session
	session, err := e.getOrCreateSession(ctx, serverKey, config)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to MCP server: %w", err)
	}

	// Build tool arguments (exclude 'tool' from args)
	toolArgs := make(map[string]interface{})
	for k, v := range args {
		if k != "tool" {
			toolArgs[k] = v
		}
	}

	// Call the MCP tool
	params := &mcp.CallToolParams{
		Name:      toolName,
		Arguments: toolArgs,
	}

	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	result, err := session.CallTool(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("MCP tool call failed: %w", err)
	}

	// Extract content from result
	var output interface{}
	if len(result.Content) == 0 {
		output = nil
	} else if len(result.Content) == 1 {
		content := result.Content[0]
		if textContent, ok := content.(*mcp.TextContent); ok {
			output = textContent.Text
		} else {
			output = result.Content[0]
		}
	} else {
		output = result.Content
	}

	return &ToolResult{
		Name:   toolName,
		Result: output,
	}, nil
}

// getOrCreateSession gets an existing session or creates a new one
func (e *MCPToolExecutor) getOrCreateSession(ctx context.Context, serverKey string, config map[string]interface{}) (*mcp.ClientSession, error) {
	e.mu.RLock()
	if session, ok := e.sessions[serverKey]; ok {
		e.mu.RUnlock()
		return session, nil
	}
	e.mu.RUnlock()

	e.mu.Lock()
	defer e.mu.Unlock()

	// Double check
	if session, ok := e.sessions[serverKey]; ok {
		return session, nil
	}

	// Create new client and session
	client := mcp.NewClient(&mcp.Implementation{
		Name:    "axiom-openseal",
		Version: "1.0.0",
	}, nil)

	// Build command
	command, _ := config["command"].(string)
	// Auto-convert npx to bunx for faster execution
	if command == "npx" {
		command = "bunx"
	}
	var cmdArgs []string
	
	// Parse args - handle multiple types
	switch a := config["args"].(type) {
	case []interface{}:
		for _, arg := range a {
			if s, ok := arg.(string); ok {
				// Skip -y flag when using bunx (it doesn't support it)
				if command == "bunx" && s == "-y" {
					continue
				}
				cmdArgs = append(cmdArgs, s)
			}
		}
	case []string:
		for _, arg := range a {
			// Skip -y flag when using bunx (it doesn't support it)
			if command == "bunx" && arg == "-y" {
				continue
			}
			cmdArgs = append(cmdArgs, arg)
		}
	case string:
		// Handle JSON-encoded string
		if strings.HasPrefix(a, "[") {
			var parsed []string
			if err := json.Unmarshal([]byte(a), &parsed); err == nil {
				for _, arg := range parsed {
					if command == "bunx" && arg == "-y" {
						continue
					}
					cmdArgs = append(cmdArgs, arg)
				}
			}
		} else {
			cmdArgs = append(cmdArgs, a)
		}
	}

	cmd := exec.Command(command, cmdArgs...)

	// Set environment
	if env, ok := config["env"].(map[string]interface{}); ok {
		envList := []string{}
		for k, v := range env {
			envList = append(envList, fmt.Sprintf("%s=%v", k, v))
		}
		cmd.Env = envList
	}

	// Capture stderr for debugging
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	// Create transport and connect
	transport := &mcp.CommandTransport{Command: cmd}

	// Use 5 minute timeout for initial connection (npx may need to download packages)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		// Include stderr in error message for debugging
		stderr := stderrBuf.String()
		if stderr != "" {
			return nil, fmt.Errorf("%w (stderr: %s)", err, stderr)
		}
		return nil, err
	}

	e.clients[serverKey] = client
	e.sessions[serverKey] = session

	return session, nil
}

// buildServerKey creates a unique key for caching MCP server connections
func (e *MCPToolExecutor) buildServerKey(config map[string]interface{}) string {
	command, _ := config["command"].(string)
	args, _ := config["args"].([]interface{})
	return fmt.Sprintf("%s-%v", command, args)
}

// Close closes all MCP server connections
func (e *MCPToolExecutor) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	for _, session := range e.sessions {
		session.Close()
	}

	e.clients = make(map[string]*mcp.Client)
	e.sessions = make(map[string]*mcp.ClientSession)
	return nil
}

// MCPToolDiscovery discovers tools from an MCP server
type MCPToolDiscovery struct{}

// DiscoverTools connects to an MCP server and returns available tools
func (d *MCPToolDiscovery) DiscoverTools(ctx context.Context, config map[string]interface{}) ([]ToolDefinition, error) {
	// Build command
	command, _ := config["command"].(string)
	if command == "" {
		return nil, fmt.Errorf("MCP tool requires 'command' in config")
	}

	// Auto-convert npx to bunx for faster execution
	if command == "npx" {
		command = "bunx"
	}

	var cmdArgs []string
	// Parse args - handle multiple types
	switch a := config["args"].(type) {
	case []interface{}:
		for _, arg := range a {
			if s, ok := arg.(string); ok {
				// Skip -y flag when using bunx (it doesn't support it)
				if command == "bunx" && s == "-y" {
					continue
				}
				cmdArgs = append(cmdArgs, s)
			}
		}
	case []string:
		for _, arg := range a {
			// Skip -y flag when using bunx (it doesn't support it)
			if command == "bunx" && arg == "-y" {
				continue
			}
			cmdArgs = append(cmdArgs, arg)
		}
	case string:
		// Handle JSON-encoded string
		if strings.HasPrefix(a, "[") {
			var parsed []string
			if err := json.Unmarshal([]byte(a), &parsed); err == nil {
				for _, arg := range parsed {
					if command == "bunx" && arg == "-y" {
						continue
					}
					cmdArgs = append(cmdArgs, arg)
				}
			}
		} else {
			cmdArgs = append(cmdArgs, a)
		}
	}

	cmd := exec.Command(command, cmdArgs...)

	// Set environment
	if env, ok := config["env"].(map[string]interface{}); ok {
		envList := []string{}
		for k, v := range env {
			envList = append(envList, fmt.Sprintf("%s=%v", k, v))
		}
		cmd.Env = envList
	}

	// Capture stderr for debugging
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	// Create client and connect
	client := mcp.NewClient(&mcp.Implementation{
		Name:    "axiom-openseal-tool-discovery",
		Version: "1.0.0",
	}, nil)

	transport := &mcp.CommandTransport{Command: cmd}

	// Use 5 minute timeout for discovery (npx may need to download packages)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		stderr := stderrBuf.String()
		if stderr != "" {
			return nil, fmt.Errorf("failed to connect to MCP server: %w (stderr: %s)", err, stderr)
		}
		return nil, fmt.Errorf("failed to connect to MCP server: %w", err)
	}
	defer session.Close()

	// List tools
	result, err := session.ListTools(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to list tools: %w", err)
	}

	// Convert to ToolDefinitions
	tools := make([]ToolDefinition, 0, len(result.Tools))
	for _, tool := range result.Tools {
		td := ToolDefinition{
			Name:        tool.Name,
			Description: tool.Description,
			Parameters:  make(map[string]interface{}),
			Config: map[string]interface{}{
				"type":    "mcp",
				"command": command,
				"args":    cmdArgs,
				"tool":    tool.Name,
			},
		}

		// Convert input schema
		if tool.InputSchema != nil {
			if m, ok := tool.InputSchema.(map[string]interface{}); ok {
				td.Parameters = m
			}
		}

		tools = append(tools, td)
	}

	return tools, nil
}