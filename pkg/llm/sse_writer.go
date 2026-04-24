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
	"net/http"
)

// SSEWriter writes SSE events to an HTTP response
type SSEWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	done    bool
}

// NewSSEWriter creates a new SSE writer
func NewSSEWriter(w http.ResponseWriter) (*SSEWriter, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, fmt.Errorf("streaming not supported")
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher.Flush()

	return &SSEWriter{w: w, flusher: flusher}, nil
}

// SendEvent sends an SSE event
func (s *SSEWriter) SendEvent(event StreamEvent) error {
	if s.done {
		return fmt.Errorf("SSE writer is closed")
	}

	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("failed to marshal event: %w", err)
	}

	_, err = fmt.Fprintf(s.w, "data: %s\n\n", string(data))
	if err != nil {
		return fmt.Errorf("failed to write event: %w", err)
	}

	s.flusher.Flush()
	return nil
}

// SendTextDelta sends a text delta event
func (s *SSEWriter) SendTextDelta(content string) error {
	return s.SendEvent(StreamEvent{
		Type:    StreamEventTextDelta,
		Content: content,
	})
}

// SendToolCall sends a tool call event
func (s *SSEWriter) SendToolCall(toolCall ToolCall) error {
	return s.SendEvent(StreamEvent{
		Type: StreamEventToolCall,
		Data: toolCall,
	})
}

// SendToolResult sends a tool result event
func (s *SSEWriter) SendToolResult(result ToolResult) error {
	return s.SendEvent(StreamEvent{
		Type: StreamEventToolResult,
		Data: result,
	})
}

// SendStateSnapshot sends a workflow state snapshot
func (s *SSEWriter) SendStateSnapshot(state WorkflowState) error {
	return s.SendEvent(StreamEvent{
		Type: StreamEventStateSnapshot,
		Data: state,
	})
}

// SendDone sends the done event
func (s *SSEWriter) SendDone() error {
	s.done = true
	return s.SendEvent(StreamEvent{
		Type: StreamEventDone,
	})
}

// SendError sends an error event
func (s *SSEWriter) SendError(message string) error {
	return s.SendEvent(StreamEvent{
		Type:    StreamEventError,
		Content: message,
	})
}
