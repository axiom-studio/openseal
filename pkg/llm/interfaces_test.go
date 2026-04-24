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
	"context"
	"testing"
)

// Compile-time interface checks
var _ ConversationRepository = (*MockConversationRepo)(nil)
var _ PromptBuilder = (*MockPromptBuilder)(nil)
var _ ContextManager = (*MockContextManager)(nil)

// Mock implementations for interface verification
type MockConversationRepo struct{}

func (m *MockConversationRepo) CreateConversation(ctx context.Context, conv *Conversation) error {
	return nil
}
func (m *MockConversationRepo) GetConversation(ctx context.Context, id string) (*Conversation, error) {
	return nil, nil
}
func (m *MockConversationRepo) AddMessage(ctx context.Context, msg *Message) error {
	return nil
}
func (m *MockConversationRepo) GetMessages(ctx context.Context, conversationID string, limit int) ([]*Message, error) {
	return nil, nil
}
func (m *MockConversationRepo) ArchiveConversation(ctx context.Context, id string) error {
	return nil
}
func (m *MockConversationRepo) ListConversations(ctx context.Context, userID string, workflowID int) ([]*Conversation, error) {
	return nil, nil
}
func (m *MockConversationRepo) ResumeConversation(ctx context.Context, id string) error {
	return nil
}
func (m *MockConversationRepo) AutoArchiveInactive(ctx context.Context, inactiveDays int) (int, error) {
	return 0, nil
}

type MockPromptBuilder struct{}

func (m *MockPromptBuilder) BuildSystemPrompt(nodeTypes []NodeTypeDefinition, workflowState *WorkflowState, schemaVersion string) string {
	return ""
}
func (m *MockPromptBuilder) BuildUserMessage(userInput string, workflowState *WorkflowState) string {
	return ""
}
func (m *MockPromptBuilder) BuildToolResultMessage(toolResults []ToolResult) string {
	return ""
}

type MockContextManager struct{}

func (m *MockContextManager) BuildContextWindow(messages []*Message, maxMessages int) []*Message {
	return nil
}
func (m *MockContextManager) SummarizeMessages(messages []*Message) string {
	return ""
}
func (m *MockContextManager) EstimateTokenCount(text string) int {
	return 0
}

func TestInterfacesCompile(t *testing.T) {
	// If this compiles, interfaces are properly defined
	var _ ConversationRepository = &MockConversationRepo{}
	var _ PromptBuilder = &MockPromptBuilder{}
	var _ ContextManager = &MockContextManager{}
}
