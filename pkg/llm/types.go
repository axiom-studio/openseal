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

import "time"

type StreamEventType string

const (
	StreamEventToolCall      StreamEventType = "tool_call"
	StreamEventToolResult    StreamEventType = "tool_result"
	StreamEventTextDelta     StreamEventType = "text_delta"
	StreamEventStateSnapshot StreamEventType = "state_snapshot"
	StreamEventDone          StreamEventType = "done"
	StreamEventError         StreamEventType = "error"
)

type StreamEvent struct {
	Type    StreamEventType `json:"type"`
	Content string          `json:"content,omitempty"`
	Data    interface{}     `json:"data,omitempty"`
}

type ChatRequest struct {
	ConversationID  string                 `json:"conversationId,omitempty"`
	Message         string                 `json:"message"`
	CurrentWorkflow *WorkflowState         `json:"currentWorkflow,omitempty"`
	Context         map[string]interface{} `json:"context,omitempty"`
	AgentLibraryID  int                    `json:"agentLibraryId,omitempty"`
	WorkflowID      *int                   `json:"workflowId,omitempty"`
}

type ChatResponse struct {
	ConversationID string         `json:"conversationId"`
	Message        string         `json:"message"`
	ToolCalls      []ToolCall     `json:"toolCalls,omitempty"`
	WorkflowState  *WorkflowState `json:"workflowState,omitempty"`
	Finished       bool           `json:"finished"`
	Error          string         `json:"error,omitempty"`
}

type ToolCall struct {
	CallID    string                 `json:"callId"`
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments"`
}

type ToolResult struct {
	CallID string      `json:"callId"`
	Status string      `json:"status"`
	Result interface{} `json:"result,omitempty"`
	Error  *ToolError  `json:"error,omitempty"`
}

type ToolError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Suggestion string `json:"suggestion,omitempty"`
}

type WorkflowState struct {
	Nodes []NodeState `json:"nodes"`
	Edges []EdgeState `json:"edges"`
}

type NodeState struct {
	ID          string                 `json:"id"`
	Type        string                 `json:"type"`
	Name        string                 `json:"name"`
	PositionX   float64                `json:"positionX"`
	PositionY   float64                `json:"positionY"`
	Config      map[string]interface{} `json:"config,omitempty"`
	Description string                 `json:"description,omitempty"`
	WorkflowID  *int                   `json:"workflowId,omitempty"`
}

type EdgeState struct {
	ID           string `json:"id"`
	SourceNodeID string `json:"sourceNodeId"`
	TargetNodeID string `json:"targetNodeId"`
	SourceHandle string `json:"sourceHandle,omitempty"`
	TargetHandle string `json:"targetHandle,omitempty"`
	Label        string `json:"label,omitempty"`
}

type Conversation struct {
	ID             string    `json:"id"`
	UserID         string    `json:"userId"`
	WorkflowID     int       `json:"workflowId"`
	AgentLibraryID int       `json:"agentLibraryId"`
	Status         string    `json:"status"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

type Message struct {
	ID             string       `json:"id"`
	ConversationID string       `json:"conversationId"`
	Role           string       `json:"role"`
	Content        string       `json:"content"`
	ToolCalls      []ToolCall   `json:"toolCalls,omitempty"`
	ToolResults    []ToolResult `json:"toolResults,omitempty"`
	InputTokens    int          `json:"inputTokens"`
	OutputTokens   int          `json:"outputTokens"`
	CreatedAt      time.Time    `json:"createdAt"`
}

type ToolDefinition struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Parameters  interface{} `json:"parameters"`
}

type NodeTypeDefinition struct {
	Type          string                  `json:"type"`
	Name          string                  `json:"name"`
	Category      string                  `json:"category"`
	Description   string                  `json:"description"`
	Icon          string                  `json:"icon,omitempty"`
	DefaultConfig map[string]interface{}  `json:"defaultConfig,omitempty"`
	ConfigFields  []ConfigFieldDefinition `json:"configFields,omitempty"`
	OutputHandles []OutputHandle          `json:"outputHandles,omitempty"`
}

type ConfigFieldDefinition struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Required    bool   `json:"required"`
	Default     string `json:"default,omitempty"`
	Description string `json:"description,omitempty"`
}

type OutputHandle struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}
