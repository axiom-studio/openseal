package executor

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// ToolExecutionContext stores information about a code execution's available tools
type ToolExecutionContext struct {
	RunID            string
	NodeID           string
	Tools            []*ToolDefinition
	ToolRegistry     *ToolRegistry
	TemplateResolver TemplateResolver
	CreatedAt        time.Time
}

// ToolExecutionContextStore manages execution contexts in memory
type ToolExecutionContextStore struct {
	mu       sync.RWMutex
	contexts map[string]*ToolExecutionContext // key: runID:nodeID
	// Cleanup old contexts periodically
	cleanupInterval time.Duration
	maxAge          time.Duration
}

// NewToolExecutionContextStore creates a new context store
func NewToolExecutionContextStore() *ToolExecutionContextStore {
	store := &ToolExecutionContextStore{
		contexts:        make(map[string]*ToolExecutionContext),
		cleanupInterval: 10 * time.Minute,
		maxAge:          2 * time.Hour, // Keep contexts for 2 hours max
	}

	// Start cleanup goroutine
	go store.cleanupLoop()

	return store
}

// Store saves an execution context
func (s *ToolExecutionContextStore) Store(ctx *ToolExecutionContext) error {
	if ctx.RunID == "" || ctx.NodeID == "" {
		return fmt.Errorf("runID and nodeID are required")
	}

	key := makeContextKey(ctx.RunID, ctx.NodeID)

	s.mu.Lock()
	defer s.mu.Unlock()

	ctx.CreatedAt = time.Now()
	s.contexts[key] = ctx

	return nil
}

// Get retrieves an execution context
func (s *ToolExecutionContextStore) Get(runID, nodeID string) (*ToolExecutionContext, error) {
	key := makeContextKey(runID, nodeID)

	s.mu.RLock()
	defer s.mu.RUnlock()

	ctx, ok := s.contexts[key]
	if !ok {
		return nil, fmt.Errorf("execution context not found for runID=%s nodeID=%s", runID, nodeID)
	}

	return ctx, nil
}

// Delete removes an execution context
func (s *ToolExecutionContextStore) Delete(runID, nodeID string) {
	key := makeContextKey(runID, nodeID)

	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.contexts, key)
}

// cleanupLoop periodically removes old contexts
func (s *ToolExecutionContextStore) cleanupLoop() {
	ticker := time.NewTicker(s.cleanupInterval)
	defer ticker.Stop()

	for range ticker.C {
		s.cleanup()
	}
}

// cleanup removes contexts older than maxAge
func (s *ToolExecutionContextStore) cleanup() {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	for key, ctx := range s.contexts {
		if now.Sub(ctx.CreatedAt) > s.maxAge {
			delete(s.contexts, key)
		}
	}
}

// makeContextKey creates a unique key for a context
func makeContextKey(runID, nodeID string) string {
	return fmt.Sprintf("%s:%s", runID, nodeID)
}

// VerifyToolAccess checks if a tool is available in the execution context
func (ctx *ToolExecutionContext) VerifyToolAccess(toolName string) (*ToolDefinition, error) {
	for _, tool := range ctx.Tools {
		if tool.Name == toolName {
			return tool, nil
		}
	}
	return nil, fmt.Errorf("tool '%s' not available in this execution context", toolName)
}

// ExecuteTool executes a tool with the given arguments
func (ctx *ToolExecutionContext) ExecuteTool(ctxGo context.Context, toolName string, arguments map[string]interface{}) (interface{}, error) {
	// Verify tool access
	toolDef, err := ctx.VerifyToolAccess(toolName)
	if err != nil {
		return nil, err
	}

	// Get tool executor from registry
	if ctx.ToolRegistry == nil {
		return nil, fmt.Errorf("tool registry not available")
	}

	executor, err := ctx.ToolRegistry.CreateExecutor(toolDef, ctx.TemplateResolver)
	if err != nil {
		return nil, fmt.Errorf("failed to create tool executor: %w", err)
	}

	// Execute tool
	result, err := executor.Execute(ctxGo, arguments)
	if err != nil {
		return nil, err
	}

	// Return the result content
	if result != nil {
		return result.Result, nil
	}

	return nil, nil
}
