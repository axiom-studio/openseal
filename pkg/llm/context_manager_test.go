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
	"time"
)

func TestContextManager_FewerThanMaxMessages(t *testing.T) {
	mgr := NewContextManager(20, 128000)
	messages := []*Message{
		{Role: "user", Content: "Hello", CreatedAt: time.Now()},
		{Role: "assistant", Content: "Hi there", CreatedAt: time.Now()},
	}

	result := mgr.BuildContextWindow(messages, 20)

	if len(result) != 2 {
		t.Errorf("expected 2 messages, got %d", len(result))
	}
}

func TestContextManager_SummarizesOlderMessages(t *testing.T) {
	mgr := NewContextManager(5, 128000)
	var messages []*Message
	for i := 0; i < 10; i++ {
		messages = append(messages, &Message{
			Role:      "user",
			Content:   "Message " + string(rune('0'+i)),
			CreatedAt: time.Now(),
		})
	}

	result := mgr.BuildContextWindow(messages, 5)

	// Should have: summary message + last 4 messages (5-1 for summary)
	if len(result) < 4 {
		t.Errorf("expected at least 4 messages, got %d", len(result))
	}

	// First non-system message should be summary
	hasSummary := false
	for _, msg := range result {
		if strings.Contains(msg.Content, "Summary of earlier conversation") {
			hasSummary = true
			break
		}
	}
	if !hasSummary {
		t.Error("expected summary message in context window")
	}
}

func TestContextManager_SummarizeMessages(t *testing.T) {
	mgr := NewContextManager(20, 128000)
	messages := []*Message{
		{Role: "user", Content: "Add a webhook node", CreatedAt: time.Now()},
		{Role: "assistant", Content: "I'll add it now", CreatedAt: time.Now()},
	}

	summary := mgr.SummarizeMessages(messages)

	if !strings.Contains(summary, "Conversation summary") {
		t.Error("summary should contain header")
	}
	if !strings.Contains(summary, "User: Add a webhook node") {
		t.Error("summary should contain user message")
	}
	if !strings.Contains(summary, "Assistant: I'll add it now") {
		t.Error("summary should contain assistant message")
	}
}

func TestContextManager_EstimateTokenCount(t *testing.T) {
	mgr := NewContextManager(20, 128000)

	// 40 chars should be approximately 10 tokens (4 chars per token)
	text := strings.Repeat("a", 40)
	count := mgr.EstimateTokenCount(text)

	if count != 10 {
		t.Errorf("expected ~10 tokens, got %d", count)
	}
}

func TestContextManager_IsWithinTokenLimit(t *testing.T) {
	mgr := NewContextManager(20, 100) // Small limit for testing

	messages := []*Message{
		{Role: "user", Content: "Short message", CreatedAt: time.Now()},
	}

	if !mgr.IsWithinTokenLimit(messages) {
		t.Error("short message should be within limit")
	}

	// Very long message should exceed limit
	longMsg := strings.Repeat("a", 500)
	messages = append(messages, &Message{Role: "assistant", Content: longMsg, CreatedAt: time.Now()})

	if mgr.IsWithinTokenLimit(messages) {
		t.Error("long message should exceed limit")
	}
}
