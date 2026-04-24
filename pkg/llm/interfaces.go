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

import "context"

// LLMServiceInterface - main service interface
type LLMServiceInterface interface {
	StreamChatMessage(ctx context.Context, req ChatRequest, userID string, eventChan chan<- StreamEvent)
	ProcessChatMessage(ctx context.Context, req ChatRequest, userID string) (ChatResponse, error)
}

// ConversationRepository - DB persistence
type ConversationRepository interface {
	CreateConversation(ctx context.Context, conv *Conversation) error
	GetConversation(ctx context.Context, id string) (*Conversation, error)
	AddMessage(ctx context.Context, msg *Message) error
	GetMessages(ctx context.Context, conversationID string, limit int) ([]*Message, error)
	ArchiveConversation(ctx context.Context, id string) error
	ResumeConversation(ctx context.Context, id string) error
	AutoArchiveInactive(ctx context.Context, inactiveDays int) (int, error)
	ListConversations(ctx context.Context, userID string, workflowID int) ([]*Conversation, error)
}

// OpenAIClient - OpenAI Responses API client
type OpenAIClient interface {
	CreateStreamingResponse(ctx context.Context, req OpenAIRequest) (<-chan OpenAIStreamEvent, error)
	CreateResponse(ctx context.Context, req OpenAIRequest) (*OpenAIResponse, error)
}

// ToolExecutor - routes tool calls to frontend
type ToolExecutor interface {
	ExecuteToolCall(ctx context.Context, call ToolCall) (ToolResult, error)
}

// PromptBuilder - builds system prompts with node schemas
type PromptBuilder interface {
	BuildSystemPrompt(nodeTypes []NodeTypeDefinition, workflowState *WorkflowState, schemaVersion string) string
	BuildUserMessage(userInput string, workflowState *WorkflowState) string
	BuildToolResultMessage(toolResults []ToolResult) string
}

// ContextManager - manages conversation context window
type ContextManager interface {
	BuildContextWindow(messages []*Message, maxMessages int) []*Message
	SummarizeMessages(messages []*Message) string
	EstimateTokenCount(text string) int
}
