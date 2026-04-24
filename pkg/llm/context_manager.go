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

const (
	defaultMaxMessages = 20
	charsPerToken      = 4
	defaultMaxTokens   = 128000
)

// ContextManagerImpl implements ContextManager
type ContextManagerImpl struct {
	maxMessages int
	maxTokens   int
}

// NewContextManager creates a new context manager
func NewContextManager(maxMessages int, maxTokens int) *ContextManagerImpl {
	if maxMessages <= 0 {
		maxMessages = defaultMaxMessages
	}
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}
	return &ContextManagerImpl{
		maxMessages: maxMessages,
		maxTokens:   maxTokens,
	}
}

// BuildContextWindow assembles the final context for the LLM
func (m *ContextManagerImpl) BuildContextWindow(messages []*Message, maxMessages int) []*Message {
	if maxMessages <= 0 {
		maxMessages = m.maxMessages
	}

	if len(messages) <= maxMessages {
		return messages
	}

	// Keep the system message (first message)
	var systemMsg *Message
	if len(messages) > 0 && messages[0].Role == "system" {
		systemMsg = messages[0]
		messages = messages[1:]
	}

	// Summarize older messages
	olderMessages := messages[:len(messages)-maxMessages+1]
	recentMessages := messages[len(messages)-maxMessages+1:]

	summary := m.SummarizeMessages(olderMessages)

	// Create summary message
	summaryMsg := &Message{
		Role:    "system",
		Content: fmt.Sprintf("Summary of earlier conversation:\n%s", summary),
	}

	// Build final context: system + summary + recent
	result := make([]*Message, 0, maxMessages+1)
	if systemMsg != nil {
		result = append(result, systemMsg)
	}
	result = append(result, summaryMsg)
	result = append(result, recentMessages...)

	return result
}

// SummarizeMessages condenses messages into a summary
func (m *ContextManagerImpl) SummarizeMessages(messages []*Message) string {
	var sb strings.Builder
	sb.WriteString("Conversation summary:\n\n")

	for i, msg := range messages {
		if msg.Role == "system" {
			continue // Skip system messages in summary
		}

		role := "User"
		if msg.Role == "assistant" {
			role = "Assistant"
		} else if msg.Role == "tool" {
			role = "Tool"
		}

		// Truncate long messages
		content := msg.Content
		if len(content) > 200 {
			content = content[:200] + "..."
		}

		sb.WriteString(fmt.Sprintf("%d. %s: %s\n", i+1, role, content))

		if len(msg.ToolCalls) > 0 {
			for _, tc := range msg.ToolCalls {
				sb.WriteString(fmt.Sprintf("   - Tool call: %s\n", tc.Name))
			}
		}

		if len(msg.ToolResults) > 0 {
			for _, tr := range msg.ToolResults {
				status := "success"
				if tr.Status == "failed" {
					status = "failed"
				}
				sb.WriteString(fmt.Sprintf("   - Tool result: %s (%s)\n", tr.CallID, status))
			}
		}
	}

	return sb.String()
}

// EstimateTokenCount estimates token count (4 chars ≈ 1 token)
func (m *ContextManagerImpl) EstimateTokenCount(text string) int {
	return len(text) / charsPerToken
}

// IsWithinTokenLimit checks if messages fit within token limit
func (m *ContextManagerImpl) IsWithinTokenLimit(messages []*Message) bool {
	totalTokens := 0
	for _, msg := range messages {
		totalTokens += m.EstimateTokenCount(msg.Content)
		for _, tc := range msg.ToolCalls {
			data, _ := json.Marshal(tc)
			totalTokens += m.EstimateTokenCount(string(data))
		}
		for _, tr := range msg.ToolResults {
			if tr.Result != nil {
				switch v := tr.Result.(type) {
				case string:
					totalTokens += m.EstimateTokenCount(v)
				default:
					data, _ := json.Marshal(tr.Result)
					totalTokens += m.EstimateTokenCount(string(data))
				}
			}
			if tr.Error != nil {
				data, _ := json.Marshal(tr.Error)
				totalTokens += m.EstimateTokenCount(string(data))
			}
		}
	}
	return totalTokens <= m.maxTokens
}
