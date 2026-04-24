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

package instruction

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

func TestNewInstructionHandler(t *testing.T) {
	logger := zap.NewNop().Sugar()
	handler := NewInstructionHandler(logger)

	if handler == nil {
		t.Fatal("expected handler to be created")
	}
	if handler.logger == nil {
		t.Fatal("expected logger to be set")
	}
}

func TestNewSSEWriter(t *testing.T) {
	tests := []struct {
		name       string
		expectFail bool
	}{
		{
			name:       "creates SSE writer successfully",
			expectFail: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			logger := zap.NewNop().Sugar()

			sseWriter, err := NewSSEWriter(w, logger)

			if tt.expectFail {
				if err == nil {
					t.Fatal("expected error but got none")
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if sseWriter == nil {
				t.Fatal("expected SSE writer to be created")
			}

			// Verify headers
			headers := w.Header()
			if headers.Get("Content-Type") != "text/event-stream; charset=utf-8" {
				t.Errorf("expected Content-Type header to be 'text/event-stream; charset=utf-8', got '%s'", headers.Get("Content-Type"))
			}
			if headers.Get("Cache-Control") != "no-cache" {
				t.Errorf("expected Cache-Control header to be 'no-cache', got '%s'", headers.Get("Cache-Control"))
			}
			if headers.Get("Connection") != "keep-alive" {
				t.Errorf("expected Connection header to be 'keep-alive', got '%s'", headers.Get("Connection"))
			}
			if headers.Get("X-Accel-Buffering") != "no" {
				t.Errorf("expected X-Accel-Buffering header to be 'no', got '%s'", headers.Get("X-Accel-Buffering"))
			}

			// Verify CORS headers
			if headers.Get("Access-Control-Allow-Origin") != "*" {
				t.Errorf("expected Access-Control-Allow-Origin header to be '*', got '%s'", headers.Get("Access-Control-Allow-Origin"))
			}
			if headers.Get("Access-Control-Allow-Methods") != "GET, OPTIONS" {
				t.Errorf("expected Access-Control-Allow-Methods header to be 'GET, OPTIONS', got '%s'", headers.Get("Access-Control-Allow-Methods"))
			}

			// Verify status code
			if w.Code != http.StatusOK {
				t.Errorf("expected status code to be %d, got %d", http.StatusOK, w.Code)
			}
		})
	}
}

func TestSSEWriter_SendInstruction(t *testing.T) {
	w := httptest.NewRecorder()
	logger := zap.NewNop().Sugar()
	sseWriter, err := NewSSEWriter(w, logger)
	if err != nil {
		t.Fatalf("failed to create SSE writer: %v", err)
	}

	// Create test instruction
	instruction := NewInstruction(AddNode, &AddNodePayload{
		Node: NodeConfig{
			ID:        uuid.New().String(),
			Type:      "webhook",
			Name:      "Webhook Trigger",
			PositionX: 100,
			PositionY: 100,
		},
	})

	// Send instruction
	err = sseWriter.SendInstruction(instruction)
	if err != nil {
		t.Fatalf("failed to send instruction: %v", err)
	}

	// Verify output
	body := w.Body.String()
	if !strings.Contains(body, "event: instruction") {
		t.Error("expected event to be 'instruction'")
	}
	if !strings.Contains(body, "data:") {
		t.Error("expected data field")
	}
	if !strings.Contains(body, "add_node") {
		t.Error("expected instruction type in data")
	}

	// Verify JSON structure
	lines := strings.Split(body, "\n")
	var dataLine string
	for _, line := range lines {
		if strings.HasPrefix(line, "data:") {
			dataLine = strings.TrimPrefix(line, "data: ")
			break
		}
	}

	if dataLine != "" {
		var receivedInstruction Instruction
		err = json.Unmarshal([]byte(dataLine), &receivedInstruction)
		if err != nil {
			t.Fatalf("failed to unmarshal instruction: %v", err)
		}
		if receivedInstruction.Type != AddNode {
			t.Errorf("expected instruction type to be %s, got %s", AddNode, receivedInstruction.Type)
		}
	}
}

func TestSSEWriter_SendHeartbeat(t *testing.T) {
	w := httptest.NewRecorder()
	logger := zap.NewNop().Sugar()
	sseWriter, err := NewSSEWriter(w, logger)
	if err != nil {
		t.Fatalf("failed to create SSE writer: %v", err)
	}

	// Send heartbeat
	err = sseWriter.SendHeartbeat()
	if err != nil {
		t.Fatalf("failed to send heartbeat: %v", err)
	}

	// Verify output
	body := w.Body.String()
	if !strings.Contains(body, ": heartbeat") {
		t.Error("expected heartbeat comment")
	}
}

func TestSSEWriter_SendError(t *testing.T) {
	w := httptest.NewRecorder()
	logger := zap.NewNop().Sugar()
	sseWriter, err := NewSSEWriter(w, logger)
	if err != nil {
		t.Fatalf("failed to create SSE writer: %v", err)
	}

	// Send error
	err = sseWriter.SendError("test error message", "test_error_code")
	if err != nil {
		t.Fatalf("failed to send error: %v", err)
	}

	// Verify output
	body := w.Body.String()
	if !strings.Contains(body, "event: error") {
		t.Error("expected event to be 'error'")
	}
	if !strings.Contains(body, "test error message") {
		t.Error("expected error message in data")
	}
	if !strings.Contains(body, "test_error_code") {
		t.Error("expected error code in data")
	}
}

func TestHandleInstructions_OptionsRequest(t *testing.T) {
	logger := zap.NewNop().Sugar()
	handler := NewInstructionHandler(logger)

	req := httptest.NewRequest("OPTIONS", "/orchestrator/agent/instructions", nil)
	w := httptest.NewRecorder()

	handler.HandleInstructions(w, req)

	// Verify CORS headers
	headers := w.Header()
	if headers.Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("expected Access-Control-Allow-Origin header to be '*', got '%s'", headers.Get("Access-Control-Allow-Origin"))
	}
	if headers.Get("Access-Control-Allow-Methods") != "GET, OPTIONS" {
		t.Errorf("expected Access-Control-Allow-Methods header to be 'GET, OPTIONS', got '%s'", headers.Get("Access-Control-Allow-Methods"))
	}

	// Verify status code
	if w.Code != http.StatusOK {
		t.Errorf("expected status code to be %d, got %d", http.StatusOK, w.Code)
	}
}

func TestHandleInstructions_MethodNotAllowed(t *testing.T) {
	logger := zap.NewNop().Sugar()
	handler := NewInstructionHandler(logger)

	req := httptest.NewRequest("POST", "/orchestrator/agent/instructions", nil)
	w := httptest.NewRecorder()

	handler.HandleInstructions(w, req)

	// Verify status code
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected status code to be %d, got %d", http.StatusMethodNotAllowed, w.Code)
	}
}

func TestHandleInstructions_SSEStream(t *testing.T) {
	logger := zap.NewNop().Sugar()
	handler := NewInstructionHandler(logger)

	req := httptest.NewRequest("GET", "/orchestrator/agent/instructions", nil)
	w := httptest.NewRecorder()

	// Create context with timeout to prevent test hanging
	// Use 6 second timeout to ensure at least one heartbeat (5 second interval)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	req = req.WithContext(ctx)

	// Start handler in goroutine
	done := make(chan bool)
	go func() {
		handler.HandleInstructions(w, req)
		done <- true
	}()

	// Wait for handler to complete or timeout
	select {
	case <-done:
		// Handler completed (context cancelled)
	case <-time.After(7 * time.Second):
		t.Fatal("handler did not complete within timeout")
	}

	// Verify SSE headers were set
	headers := w.Header()
	if headers.Get("Content-Type") != "text/event-stream; charset=utf-8" {
		t.Errorf("expected Content-Type header to be 'text/event-stream; charset=utf-8', got '%s'", headers.Get("Content-Type"))
	}

	// Verify at least one heartbeat was sent
	body := w.Body.String()
	if !strings.Contains(body, ": heartbeat") {
		t.Error("expected at least one heartbeat to be sent")
	}
}

func TestInstructionStreamHandler(t *testing.T) {
	logger := zap.NewNop().Sugar()
	handler := NewInstructionHandler(logger)

	// Create instruction channel
	instructionChan := make(chan *Instruction, 1)

	// Create test instruction
	instruction := NewInstruction(AddNode, &AddNodePayload{
		Node: NodeConfig{
			ID:        uuid.New().String(),
			Type:      "webhook",
			Name:      "Test Node",
			PositionX: 100,
			PositionY: 100,
		},
	})

	// Create handler function
	handlerFunc := handler.InstructionStreamHandler(instructionChan)

	req := httptest.NewRequest("GET", "/orchestrator/agent/instructions", nil)
	w := httptest.NewRecorder()

	// Create context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req = req.WithContext(ctx)

	// Start handler in goroutine
	done := make(chan bool)
	go func() {
		handlerFunc(w, req)
		done <- true
	}()

	// Send instruction
	instructionChan <- instruction

	// Close channel after short delay
	time.Sleep(100 * time.Millisecond)
	close(instructionChan)

	// Wait for handler to complete
	select {
	case <-done:
		// Handler completed
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not complete within timeout")
	}

	// Verify instruction was sent
	body := w.Body.String()
	if !strings.Contains(body, "event: instruction") {
		t.Error("expected instruction event")
	}
	if !strings.Contains(body, "add_node") {
		t.Error("expected instruction type in data")
	}
}

func TestStreamInstructions(t *testing.T) {
	w := httptest.NewRecorder()
	logger := zap.NewNop().Sugar()
	sseWriter, err := NewSSEWriter(w, logger)
	if err != nil {
		t.Fatalf("failed to create SSE writer: %v", err)
	}

	// Create instruction channel
	instructionChan := make(chan *Instruction, 2)

	// Create test instructions
	instruction1 := NewInstruction(AddNode, &AddNodePayload{
		Node: NodeConfig{
			ID:        uuid.New().String(),
			Type:      "webhook",
			Name:      "Node 1",
			PositionX: 100,
			PositionY: 100,
		},
	})

	instruction2 := NewInstruction(AddEdge, &AddEdgePayload{
		Edge: EdgeConfig{
			ID:           uuid.New().String(),
			SourceNodeID: uuid.New().String(),
			TargetNodeID: uuid.New().String(),
		},
	})

	// Create context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Start streaming in goroutine
	done := make(chan error)
	go func() {
		err := StreamInstructions(ctx, sseWriter, instructionChan, logger)
		done <- err
	}()

	// Send instructions
	instructionChan <- instruction1
	time.Sleep(50 * time.Millisecond)
	instructionChan <- instruction2
	time.Sleep(50 * time.Millisecond)

	// Close channel
	close(instructionChan)

	// Wait for streaming to complete
	select {
	case err := <-done:
		if err != nil && err != context.DeadlineExceeded {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("streaming did not complete within timeout")
	}

	// Verify instructions were sent
	body := w.Body.String()
	if !strings.Contains(body, "add_node") {
		t.Error("expected first instruction type in output")
	}
	if !strings.Contains(body, "add_edge") {
		t.Error("expected second instruction type in output")
	}
}

func TestStreamInstructions_InvalidInstruction(t *testing.T) {
	w := httptest.NewRecorder()
	logger := zap.NewNop().Sugar()
	sseWriter, err := NewSSEWriter(w, logger)
	if err != nil {
		t.Fatalf("failed to create SSE writer: %v", err)
	}

	// Create instruction channel
	instructionChan := make(chan *Instruction, 1)

	// Create invalid instruction (missing required fields)
	invalidInstruction := &Instruction{
		Type: AddNode,
		Payload: &AddNodePayload{
			Node: NodeConfig{
				ID:        uuid.New().String(),
				Type:      "", // Missing required field
				Name:      "Test",
				PositionX: 100,
				PositionY: 100,
			},
		},
		Metadata: NewInstructionMetadata(),
	}

	// Create context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Start streaming in goroutine
	done := make(chan error)
	go func() {
		err := StreamInstructions(ctx, sseWriter, instructionChan, logger)
		done <- err
	}()

	// Send invalid instruction
	instructionChan <- invalidInstruction
	time.Sleep(100 * time.Millisecond)

	// Close channel
	close(instructionChan)

	// Wait for streaming to complete
	select {
	case err := <-done:
		if err != nil && err != context.DeadlineExceeded {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("streaming did not complete within timeout")
	}

	// Verify error was sent
	body := w.Body.String()
	if !strings.Contains(body, "event: error") {
		t.Error("expected error event for invalid instruction")
	}
	if !strings.Contains(body, "validation_error") {
		t.Error("expected validation error code")
	}
}

func TestHandleInstructions_ClientDisconnect(t *testing.T) {
	logger := zap.NewNop().Sugar()
	handler := NewInstructionHandler(logger)

	req := httptest.NewRequest("GET", "/orchestrator/agent/instructions", nil)
	w := httptest.NewRecorder()

	// Create context that we can cancel immediately
	ctx, cancel := context.WithCancel(context.Background())
	req = req.WithContext(ctx)

	// Start handler in goroutine
	done := make(chan bool)
	go func() {
		handler.HandleInstructions(w, req)
		done <- true
	}()

	// Cancel context immediately to simulate client disconnect
	cancel()

	// Wait for handler to complete
	select {
	case <-done:
		// Handler completed (client disconnected)
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not handle disconnect within timeout")
	}

	// Verify SSE headers were set before disconnect
	headers := w.Header()
	if headers.Get("Content-Type") != "text/event-stream; charset=utf-8" {
		t.Errorf("expected Content-Type header to be set")
	}
}

func TestSSEWriter_ConcurrentWrites(t *testing.T) {
	w := httptest.NewRecorder()
	logger := zap.NewNop().Sugar()
	sseWriter, err := NewSSEWriter(w, logger)
	if err != nil {
		t.Fatalf("failed to create SSE writer: %v", err)
	}

	// Create multiple instructions
	instructions := make([]*Instruction, 10)
	for i := 0; i < 10; i++ {
		instructions[i] = NewInstruction(AddNode, &AddNodePayload{
			Node: NodeConfig{
				ID:        uuid.New().String(),
				Type:      "webhook",
				Name:      "Node " + string(rune(i)),
				PositionX: float64(i * 100),
				PositionY: float64(i * 100),
			},
		})
	}

	// Send instructions concurrently
	done := make(chan error, 10)
	for i := 0; i < 10; i++ {
		go func(idx int) {
			err := sseWriter.SendInstruction(instructions[idx])
			done <- err
		}(i)
	}

	// Wait for all sends to complete
	for i := 0; i < 10; i++ {
		err := <-done
		if err != nil {
			t.Errorf("concurrent send failed: %v", err)
		}
	}

	// Verify all instructions were sent
	body := w.Body.String()
	count := strings.Count(body, "event: instruction")
	if count != 10 {
		t.Errorf("expected 10 instruction events, got %d", count)
	}
}
