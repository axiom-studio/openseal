/*
 * Copyright (c) 2025. Axiom Studio
 */

package executor

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMemoryTool_InMemoryStore tests the memory tool with in-memory store
func TestMemoryTool_InMemoryStore(t *testing.T) {
	os.Setenv("MEMORY_TOOL_IN_MEMORY", "true")
	defer os.Unsetenv("MEMORY_TOOL_IN_MEMORY")

	toolDef := &ToolDefinition{
		Name:        "memory",
		Description: "Memory tool for testing",
		Parameters:  map[string]interface{}{},
		Config: map[string]interface{}{
			"useInMemory": true,
			"sessionId":   "test-session-123",
			"userId":      "test-user-456",
		},
	}

	executor, err := NewMemoryToolExecutor(toolDef, &noopResolver{})
	require.NoError(t, err)
	require.NotNil(t, executor)

	ctx := context.Background()

	t.Run("Save", func(t *testing.T) {
		result, err := executor.Execute(ctx, map[string]interface{}{
			"operation": "save",
			"memory":    "The user's favorite color is blue",
			"key":       "favorite_color",
			"scope":     "session",
			"metadata": map[string]interface{}{
				"category": "preferences",
			},
		})

		require.NoError(t, err)
		assert.Equal(t, "memory_save", result.Name)

		resultMap, ok := result.Result.(map[string]interface{})
		require.True(t, ok, "Result should be a map")
		assert.Contains(t, resultMap, "id")
		assert.Equal(t, "favorite_color", resultMap["key"])
		t.Logf("Save result: %+v", resultMap)
	})

	t.Run("Recall", func(t *testing.T) {
		result, err := executor.Execute(ctx, map[string]interface{}{
			"operation": "recall",
			"memory":    "What is the user's favorite color?",
			"scope":     "session",
			"limit":     5,
		})

		require.NoError(t, err)
		assert.Equal(t, "memory_recall", result.Name)

		resultMap, ok := result.Result.(map[string]interface{})
		require.True(t, ok, "Result should be a map")
		assert.Contains(t, resultMap, "memories")

		memories, ok := resultMap["memories"].([]map[string]interface{})
		require.True(t, ok, "memories should be a slice of maps")
		require.Greater(t, len(memories), 0, "Should find at least one memory")

		found := false
		for _, m := range memories {
			if m["key"] == "favorite_color" {
				found = true
				assert.Contains(t, m["content"], "blue")
				break
			}
		}
		assert.True(t, found, "Should find the favorite_color memory")
		t.Logf("Recall result: count=%v", resultMap["count"])
	})

	t.Run("List", func(t *testing.T) {
		result, err := executor.Execute(ctx, map[string]interface{}{
			"operation": "list",
			"scope":     "session",
			"limit":     10,
		})

		require.NoError(t, err)
		assert.Equal(t, "memory_list", result.Name)

		resultMap, ok := result.Result.(map[string]interface{})
		require.True(t, ok, "Result should be a map")
		assert.Contains(t, resultMap, "memories")

		memories, ok := resultMap["memories"].([]map[string]interface{})
		require.True(t, ok, "memories should be a slice of maps")
		assert.GreaterOrEqual(t, len(memories), 1, "Should have at least one memory")
		t.Logf("List result: count=%v", resultMap["count"])
	})

	t.Run("Forget", func(t *testing.T) {
		result, err := executor.Execute(ctx, map[string]interface{}{
			"operation": "forget",
			"key":       "favorite_color",
			"scope":     "session",
		})

		require.NoError(t, err)
		assert.Equal(t, "memory_forget", result.Name)

		resultMap, ok := result.Result.(map[string]interface{})
		require.True(t, ok, "Result should be a map")
		deleted, ok := resultMap["deleted"].(int)
		require.True(t, ok, "deleted should be an int")
		assert.GreaterOrEqual(t, deleted, 1, "Should delete at least one memory")
		t.Logf("Forget result: deleted=%v", deleted)

		listResult, err := executor.Execute(ctx, map[string]interface{}{
			"operation": "list",
			"scope":     "session",
		})
		require.NoError(t, err)
		listMap, ok := listResult.Result.(map[string]interface{})
		require.True(t, ok, "Result should be a map")
		memories, ok := listMap["memories"].([]map[string]interface{})
		require.True(t, ok, "memories should be a slice of maps")
		for _, m := range memories {
			assert.NotEqual(t, "favorite_color", m["key"], "Memory should be deleted")
		}
	})
}

// TestMemoryTool_ScopeIsolation tests that different scopes are isolated
func TestMemoryTool_ScopeIsolation(t *testing.T) {
	os.Setenv("MEMORY_TOOL_IN_MEMORY", "true")
	defer os.Unsetenv("MEMORY_TOOL_IN_MEMORY")

	toolDef := &ToolDefinition{
		Name:        "memory",
		Description: "Memory tool for testing",
		Parameters:  map[string]interface{}{},
		Config: map[string]interface{}{
			"useInMemory": true,
			"sessionId":   "scope-test-session",
			"userId":      "scope-test-user",
		},
	}

	executor, err := NewMemoryToolExecutor(toolDef, &noopResolver{})
	require.NoError(t, err)

	ctx := context.Background()

	_, err = executor.Execute(ctx, map[string]interface{}{
		"operation": "save",
		"memory":    "Session memory",
		"key":       "session_mem",
		"scope":     "session",
	})
	require.NoError(t, err)

	_, err = executor.Execute(ctx, map[string]interface{}{
		"operation": "save",
		"memory":    "Global memory",
		"key":       "global_mem",
		"scope":     "global",
	})
	require.NoError(t, err)

	result, err := executor.Execute(ctx, map[string]interface{}{
		"operation": "list",
		"scope":     "session",
	})
	require.NoError(t, err)
	resultMap, ok := result.Result.(map[string]interface{})
	require.True(t, ok)
	memories, ok := resultMap["memories"].([]map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, 1, len(memories), "Should only see session memory")
	assert.Equal(t, "session_mem", memories[0]["key"])

	result, err = executor.Execute(ctx, map[string]interface{}{
		"operation": "list",
		"scope":     "global",
	})
	require.NoError(t, err)
	resultMap, ok = result.Result.(map[string]interface{})
	require.True(t, ok)
	memories, ok = resultMap["memories"].([]map[string]interface{})
	require.True(t, ok)
	assert.GreaterOrEqual(t, len(memories), 1, "Should see global memory")
}

// TestMemoryTool_MultipleSaves tests saving multiple memories
func TestMemoryTool_MultipleSaves(t *testing.T) {
	os.Setenv("MEMORY_TOOL_IN_MEMORY", "true")
	defer os.Unsetenv("MEMORY_TOOL_IN_MEMORY")

	toolDef := &ToolDefinition{
		Name:        "memory",
		Description: "Memory tool for testing",
		Parameters:  map[string]interface{}{},
		Config: map[string]interface{}{
			"useInMemory": true,
			"sessionId":   "multi-save-session",
		},
	}

	executor, err := NewMemoryToolExecutor(toolDef, &noopResolver{})
	require.NoError(t, err)

	ctx := context.Background()

	memories := []struct {
		key     string
		content string
	}{
		{"pref_color", "User likes blue color"},
		{"pref_food", "User likes pizza"},
		{"pref_music", "User likes jazz music"},
	}

	for _, m := range memories {
		_, err := executor.Execute(ctx, map[string]interface{}{
			"operation": "save",
			"memory":    m.content,
			"key":       m.key,
			"scope":     "session",
		})
		require.NoError(t, err)
	}

	result, err := executor.Execute(ctx, map[string]interface{}{
		"operation": "list",
		"scope":     "session",
	})
	require.NoError(t, err)
	resultMap, ok := result.Result.(map[string]interface{})
	require.True(t, ok)
	list, ok := resultMap["memories"].([]map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, 3, len(list), "Should have 3 memories")

	result, err = executor.Execute(ctx, map[string]interface{}{
		"operation": "recall",
		"memory":    "What music does the user enjoy?",
		"scope":     "session",
		"limit":     3,
	})
	require.NoError(t, err)
	resultMap, ok = result.Result.(map[string]interface{})
	require.True(t, ok)
	recalled, ok := resultMap["memories"].([]map[string]interface{})
	require.True(t, ok)
	assert.Greater(t, len(recalled), 0, "Should find memories")
}

// TestMemoryTool_UpdateMemory tests updating an existing memory
func TestMemoryTool_UpdateMemory(t *testing.T) {
	os.Setenv("MEMORY_TOOL_IN_MEMORY", "true")
	defer os.Unsetenv("MEMORY_TOOL_IN_MEMORY")

	toolDef := &ToolDefinition{
		Name:        "memory",
		Description: "Memory tool for testing",
		Parameters:  map[string]interface{}{},
		Config: map[string]interface{}{
			"useInMemory": true,
			"sessionId":   "update-test-session",
		},
	}

	executor, err := NewMemoryToolExecutor(toolDef, &noopResolver{})
	require.NoError(t, err)

	ctx := context.Background()

	_, err = executor.Execute(ctx, map[string]interface{}{
		"operation": "save",
		"memory":    "User's favorite color is blue",
		"key":       "fav_color",
		"scope":     "session",
	})
	require.NoError(t, err)

	_, err = executor.Execute(ctx, map[string]interface{}{
		"operation": "save",
		"memory":    "User's favorite color changed to green",
		"key":       "fav_color",
		"scope":     "session",
	})
	require.NoError(t, err)

	result, err := executor.Execute(ctx, map[string]interface{}{
		"operation": "list",
		"scope":     "session",
	})
	require.NoError(t, err)
	resultMap, ok := result.Result.(map[string]interface{})
	require.True(t, ok)
	memories, ok := resultMap["memories"].([]map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, 1, len(memories), "Should have only one memory (updated)")
	assert.Contains(t, memories[0]["content"], "green")
}

// noopResolver is a simple resolver for testing
type noopResolver struct{}

func (r *noopResolver) ResolveString(s string) string {
	return s
}

func (r *noopResolver) ResolveMap(input map[string]interface{}) map[string]interface{} {
	return input
}

func (r *noopResolver) EvaluateCondition(condition string) bool {
	return true
}

func (r *noopResolver) SetVariable(name string, value interface{}) {}

func (r *noopResolver) GetStepOutput(stepName string) interface{} {
	return nil
}

func (r *noopResolver) SetStepOutput(stepName string, output interface{}) {}
