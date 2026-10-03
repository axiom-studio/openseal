package executor

import (
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPErrorResultFailsWithoutLeakingServerContent(t *testing.T) {
	result, err := mcpToolResult("test_action", &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "secret-token-and-internal-diagnostic"}}})
	if result != nil || err == nil {
		t.Fatalf("result=%+v error=%v; expected failure", result, err)
	}
	if strings.Contains(err.Error(), "secret-token") {
		t.Fatal("server error content leaked")
	}
}

func TestMCPSuccessResultPreservesContent(t *testing.T) {
	result, err := mcpToolResult("test_action", &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "done"}}})
	if err != nil || result == nil || result.Result != "done" {
		t.Fatalf("result=%+v error=%v", result, err)
	}
}
