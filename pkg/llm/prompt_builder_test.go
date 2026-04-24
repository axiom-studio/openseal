/*
 * Copyright (c) 2025. Axiom Studio
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package llm

import (
	"strings"
	"testing"
)

func TestPromptBuilder_SystemPromptContainsNodeTypes(t *testing.T) {
	builder := NewPromptBuilder()
	nodeTypes := []NodeTypeDefinition{
		{Type: "webhook", Name: "Webhook", Category: "trigger", Description: "HTTP webhook trigger"},
		{Type: "http", Name: "HTTP Request", Category: "action", Description: "Make HTTP requests"},
		{Type: "code", Name: "Code", Category: "action", Description: "Execute code"},
	}

	prompt := builder.BuildSystemPrompt(nodeTypes, nil, "")

	for _, nt := range nodeTypes {
		if !strings.Contains(prompt, nt.Name) {
			t.Errorf("prompt should contain node type name %q", nt.Name)
		}
		if !strings.Contains(prompt, nt.Type) {
			t.Errorf("prompt should contain node type %q", nt.Type)
		}
	}
}

func TestPromptBuilder_SystemPromptContainsEdgeRules(t *testing.T) {
	builder := NewPromptBuilder()
	prompt := builder.BuildSystemPrompt(nil, nil, "")

	expectedRules := []string{
		"Tool nodes",
		"tools-target",
		"duplicate edges",
		"self-connections",
	}

	for _, rule := range expectedRules {
		if !strings.Contains(prompt, rule) {
			t.Errorf("prompt should contain edge rule %q", rule)
		}
	}
}

func TestPromptBuilder_SystemPromptIncludesWorkflowState(t *testing.T) {
	builder := NewPromptBuilder()
	workflowState := &WorkflowState{
		Nodes: []NodeState{
			{ID: "node-1", Type: "webhook", Name: "Webhook", PositionX: 100, PositionY: 100},
		},
		Edges: []EdgeState{},
	}

	prompt := builder.BuildSystemPrompt(nil, workflowState, "")

	if !strings.Contains(prompt, "Current Workflow State") {
		t.Error("prompt should contain 'Current Workflow State' section")
	}
	if !strings.Contains(prompt, "node-1") {
		t.Error("prompt should contain node ID 'node-1'")
	}
}

func TestPromptBuilder_UserMessageIncludesWorkflowState(t *testing.T) {
	builder := NewPromptBuilder()
	workflowState := &WorkflowState{
		Nodes: []NodeState{
			{ID: "node-1", Type: "webhook", Name: "Webhook", PositionX: 100, PositionY: 100},
		},
	}

	msg := builder.BuildUserMessage("Add an HTTP node", workflowState)

	if !strings.Contains(msg, "User request: Add an HTTP node") {
		t.Error("user message should contain the user input")
	}
	if !strings.Contains(msg, "Current workflow state") {
		t.Error("user message should contain workflow state")
	}
}

func TestPromptBuilder_ToolResultMessageFormats(t *testing.T) {
	builder := NewPromptBuilder()
	toolResults := []ToolResult{
		{CallID: "c1", Status: "completed", Result: map[string]string{"nodeId": "node-1"}},
		{CallID: "c2", Status: "failed", Error: &ToolError{Code: "INVALID", Message: "Bad type", Suggestion: "Try webhook"}},
	}

	msg := builder.BuildToolResultMessage(toolResults)

	if !strings.Contains(msg, "c1: SUCCESS") {
		t.Error("message should contain success for c1")
	}
	if !strings.Contains(msg, "c2: FAILED") {
		t.Error("message should contain failure for c2")
	}
	if !strings.Contains(msg, "Bad type") {
		t.Error("message should contain error message")
	}
	if !strings.Contains(msg, "Try webhook") {
		t.Error("message should contain suggestion")
	}
}

func TestPromptBuilder_SystemPromptIncludesSchemaVersion(t *testing.T) {
	builder := NewPromptBuilder()
	nodeTypes := []NodeTypeDefinition{
		{Type: "webhook", Name: "Webhook", Category: "trigger", Description: "HTTP webhook trigger"},
	}

	prompt := builder.BuildSystemPrompt(nodeTypes, nil, "abc12345")

	if !strings.Contains(prompt, "Schema Version: abc12345") {
		t.Error("prompt should contain schema version")
	}
}

func TestPromptBuilder_SystemPromptWithoutSchemaVersion(t *testing.T) {
	builder := NewPromptBuilder()
	nodeTypes := []NodeTypeDefinition{
		{Type: "webhook", Name: "Webhook", Category: "trigger", Description: "HTTP webhook trigger"},
	}

	prompt := builder.BuildSystemPrompt(nodeTypes, nil, "")

	if strings.Contains(prompt, "Schema Version:") {
		t.Error("prompt should not contain schema version header when empty")
	}
}
