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
	"encoding/json"
	"fmt"
	"strings"
)

// PromptBuilderImpl implements PromptBuilder
type PromptBuilderImpl struct{}

// NewPromptBuilder creates a new prompt builder
func NewPromptBuilder() *PromptBuilderImpl {
	return &PromptBuilderImpl{}
}

func (b *PromptBuilderImpl) BuildSystemPrompt(nodeTypes []NodeTypeDefinition, workflowState *WorkflowState, schemaVersion string) string {
	var sb strings.Builder

	sb.WriteString("You are an AI workflow builder assistant. Your job is to help users build workflows by adding nodes, connecting them with edges, and configuring them.\n\n")

	if schemaVersion != "" {
		sb.WriteString(fmt.Sprintf("## Schema Version: %s\n\n", schemaVersion))
	}

	// Available node types
	sb.WriteString("## Available Node Types\n\n")
	for _, nt := range nodeTypes {
		sb.WriteString(fmt.Sprintf("### %s (%s)\n", nt.Name, nt.Type))
		sb.WriteString(fmt.Sprintf("Category: %s\n", nt.Category))
		sb.WriteString(fmt.Sprintf("Description: %s\n", nt.Description))
		if len(nt.ConfigFields) > 0 {
			sb.WriteString("Config fields:\n")
			for _, f := range nt.ConfigFields {
				req := ""
				if f.Required {
					req = " (required)"
				}
				sb.WriteString(fmt.Sprintf("  - %s: %s%s - %s\n", f.Name, f.Type, req, f.Description))
			}
		}
		if len(nt.OutputHandles) > 0 {
			sb.WriteString("Output handles:\n")
			for _, h := range nt.OutputHandles {
				sb.WriteString(fmt.Sprintf("  - %s: %s - %s\n", h.ID, h.Label, h.Description))
			}
		}
		sb.WriteString("\n")
	}

	// Edge creation rules
	sb.WriteString("## Edge Creation Rules\n\n")
	sb.WriteString("1. Tool nodes (type starting with 'tool_') can ONLY connect to AI nodes or Code nodes\n")
	sb.WriteString("2. The 'tools-target' handle only accepts connections from tool nodes\n")
	sb.WriteString("3. No duplicate edges (same source + target + handles)\n")
	sb.WriteString("4. No self-connections (source !== target)\n")
	sb.WriteString("5. Always connect nodes with edges after adding them\n\n")

	// Positioning guidance
	sb.WriteString("## Node Positioning\n\n")
	if workflowState != nil && len(workflowState.Nodes) > 0 {
		sb.WriteString("Current node positions (use these to avoid overlap):\n")
		for _, n := range workflowState.Nodes {
			sb.WriteString(fmt.Sprintf("  - %s (%s): x=%.0f, y=%.0f\n", n.Name, n.Type, n.PositionX, n.PositionY))
		}
		sb.WriteString("\nPosition new nodes at least 300px apart horizontally and 150px vertically.\n\n")
	} else {
		sb.WriteString("Start the first node at position (100, 100). Position subsequent nodes 300px apart horizontally.\n\n")
	}

	// Tool usage instructions
	sb.WriteString("## Tool Usage\n\n")
	sb.WriteString("1. Call tools ONE AT A TIME (sequential execution)\n")
	sb.WriteString("2. Always call get_node_schema before adding a new node type to understand its config\n")
	sb.WriteString("3. After adding nodes, connect them with edges\n")
	sb.WriteString("4. Include complete configuration for each node (use defaults from schema if unsure)\n")
	sb.WriteString("5. If a tool call fails, read the error and retry with corrected parameters\n")
	sb.WriteString("6. Maximum 10 tool calls per turn - after that, respond with text only\n\n")

	// Current workflow state
	if workflowState != nil {
		sb.WriteString("## Current Workflow State\n\n")
		stateJSON, _ := json.MarshalIndent(workflowState, "", "  ")
		sb.WriteString(fmt.Sprintf("```json\n%s\n```\n\n", string(stateJSON)))
	}

	return sb.String()
}

func (b *PromptBuilderImpl) BuildUserMessage(userInput string, workflowState *WorkflowState) string {
	if workflowState == nil {
		return userInput
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("User request: %s\n\n", userInput))
	sb.WriteString("Current workflow state:\n")
	stateJSON, _ := json.MarshalIndent(workflowState, "", "  ")
	sb.WriteString(fmt.Sprintf("```json\n%s\n```", string(stateJSON)))
	return sb.String()
}

func (b *PromptBuilderImpl) BuildToolResultMessage(toolResults []ToolResult) string {
	var sb strings.Builder
	sb.WriteString("Tool execution results:\n\n")
	for _, r := range toolResults {
		if r.Status == "completed" {
			resultJSON, _ := json.Marshal(r.Result)
			sb.WriteString(fmt.Sprintf("- %s: SUCCESS\n  Result: %s\n\n", r.CallID, string(resultJSON)))
		} else {
			errMsg := "unknown error"
			if r.Error != nil {
				errMsg = r.Error.Message
			}
			sb.WriteString(fmt.Sprintf("- %s: FAILED\n  Error: %s\n", r.CallID, errMsg))
			if r.Error != nil && r.Error.Suggestion != "" {
				sb.WriteString(fmt.Sprintf("  Suggestion: %s\n\n", r.Error.Suggestion))
			}
		}
	}
	return sb.String()
}
