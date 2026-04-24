package executor

import (
	"context"
	"testing"
	"time"
)

func TestToolExecutionContextStore_StoreAndGet(t *testing.T) {
	store := NewToolExecutionContextStore()

	ctx := &ToolExecutionContext{
		RunID:  "run-123",
		NodeID: "node-456",
		Tools: []*ToolDefinition{
			{
				Name:        "test_tool",
				Description: "Test tool",
			},
		},
	}

	err := store.Store(ctx)
	if err != nil {
		t.Fatalf("Store() failed: %v", err)
	}

	retrieved, err := store.Get("run-123", "node-456")
	if err != nil {
		t.Fatalf("Get() failed: %v", err)
	}

	if retrieved.RunID != ctx.RunID {
		t.Errorf("Get() runID = %v, want %v", retrieved.RunID, ctx.RunID)
	}
	if retrieved.NodeID != ctx.NodeID {
		t.Errorf("Get() nodeID = %v, want %v", retrieved.NodeID, ctx.NodeID)
	}
	if len(retrieved.Tools) != len(ctx.Tools) {
		t.Errorf("Get() tools length = %v, want %v", len(retrieved.Tools), len(ctx.Tools))
	}
}

func TestToolExecutionContextStore_GetNonExistent(t *testing.T) {
	store := NewToolExecutionContextStore()

	_, err := store.Get("nonexistent-run", "nonexistent-node")
	if err == nil {
		t.Error("Get() should return error for non-existent context")
	}
}

func TestToolExecutionContextStore_StoreEmptyIDs(t *testing.T) {
	store := NewToolExecutionContextStore()

	tests := []struct {
		name   string
		runID  string
		nodeID string
	}{
		{"empty runID", "", "node-456"},
		{"empty nodeID", "run-123", ""},
		{"both empty", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := &ToolExecutionContext{
				RunID:  tt.runID,
				NodeID: tt.nodeID,
			}

			err := store.Store(ctx)
			if err == nil {
				t.Error("Store() should return error for empty IDs")
			}
		})
	}
}

func TestToolExecutionContextStore_Delete(t *testing.T) {
	store := NewToolExecutionContextStore()

	ctx := &ToolExecutionContext{
		RunID:  "run-123",
		NodeID: "node-456",
	}

	_ = store.Store(ctx)

	store.Delete("run-123", "node-456")

	_, err := store.Get("run-123", "node-456")
	if err == nil {
		t.Error("Get() should return error after Delete()")
	}
}

func TestToolExecutionContext_VerifyToolAccess(t *testing.T) {
	ctx := &ToolExecutionContext{
		RunID:  "run-123",
		NodeID: "node-456",
		Tools: []*ToolDefinition{
			{
				Name:        "vector_search",
				Description: "Search vectors",
			},
			{
				Name:        "slack_send",
				Description: "Send Slack message",
			},
		},
	}

	tests := []struct {
		name     string
		toolName string
		wantErr  bool
	}{
		{
			name:     "existing tool",
			toolName: "vector_search",
			wantErr:  false,
		},
		{
			name:     "another existing tool",
			toolName: "slack_send",
			wantErr:  false,
		},
		{
			name:     "non-existent tool",
			toolName: "nonexistent_tool",
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tool, err := ctx.VerifyToolAccess(tt.toolName)

			if (err != nil) != tt.wantErr {
				t.Errorf("VerifyToolAccess() error = %v, wantErr %v", err, tt.wantErr)
			}

			if !tt.wantErr && tool.Name != tt.toolName {
				t.Errorf("VerifyToolAccess() tool name = %v, want %v", tool.Name, tt.toolName)
			}
		})
	}
}

func TestToolExecutionContextStore_Cleanup(t *testing.T) {
	store := NewToolExecutionContextStore()
	store.maxAge = 100 * time.Millisecond

	ctx := &ToolExecutionContext{
		RunID:  "run-123",
		NodeID: "node-456",
	}
	_ = store.Store(ctx)

	_, err := store.Get("run-123", "node-456")
	if err != nil {
		t.Fatal("Context should exist immediately after Store()")
	}

	time.Sleep(150 * time.Millisecond)

	store.cleanup()

	_, err = store.Get("run-123", "node-456")
	if err == nil {
		t.Error("Context should be cleaned up after maxAge")
	}
}

func TestToolExecutionContext_ExecuteTool_NoRegistry(t *testing.T) {
	ctx := &ToolExecutionContext{
		RunID:  "run-123",
		NodeID: "node-456",
		Tools: []*ToolDefinition{
			{
				Name:   "test_tool",
				Config: map[string]interface{}{"type": "test"},
			},
		},
		ToolRegistry: nil,
	}

	_, err := ctx.ExecuteTool(context.Background(), "test_tool", nil)
	if err == nil {
		t.Error("ExecuteTool() should return error when ToolRegistry is nil")
	}
}
