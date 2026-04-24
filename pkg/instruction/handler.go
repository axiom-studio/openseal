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
	"fmt"
	"net/http"
	"sync"
	"time"

	"go.uber.org/zap"
)

// InstructionHandler handles SSE streaming of agent instructions
type InstructionHandler struct {
	logger *zap.SugaredLogger
	mu     sync.Mutex
}

// NewInstructionHandler creates a new instruction handler
func NewInstructionHandler(logger *zap.SugaredLogger) *InstructionHandler {
	return &InstructionHandler{
		logger: logger,
	}
}

// SSEWriter writes Server-Sent Events to an HTTP response
type SSEWriter struct {
	mu      sync.Mutex
	w       http.ResponseWriter
	flusher http.Flusher
	logger  *zap.SugaredLogger
}

// NewSSEWriter creates an SSE writer and sets appropriate headers
func NewSSEWriter(w http.ResponseWriter, logger *zap.SugaredLogger) (*SSEWriter, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, fmt.Errorf("streaming not supported")
	}

	// Set SSE headers
	headers := w.Header()
	headers.Set("Content-Type", "text/event-stream; charset=utf-8")
	headers.Set("Cache-Control", "no-cache")
	headers.Set("Connection", "keep-alive")
	headers.Set("X-Accel-Buffering", "no") // Disable nginx buffering

	// Set CORS headers for cross-origin requests
	headers.Set("Access-Control-Allow-Origin", "*")
	headers.Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	headers.Set("Access-Control-Allow-Headers", "Accept, Content-Type, Authorization")
	headers.Set("Access-Control-Max-Age", "86400") // 24 hours

	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	return &SSEWriter{w: w, flusher: flusher, logger: logger}, nil
}

// writeEvent writes an SSE event to the response
func (s *SSEWriter) writeEvent(event string, data interface{}) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	jsonData, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("failed to marshal data: %w", err)
	}

	// SSE format: "event: <event>\ndata: <data>\n\n"
	fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, string(jsonData))
	s.flusher.Flush()

	return nil
}

// SendInstruction sends an instruction event
func (s *SSEWriter) SendInstruction(instruction *Instruction) error {
	return s.writeEvent("instruction", instruction)
}

// SendHeartbeat sends a heartbeat comment to keep connection alive
func (s *SSEWriter) SendHeartbeat() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// SSE comment format: ": <comment>\n"
	fmt.Fprintf(s.w, ": heartbeat\n\n")
	s.flusher.Flush()

	return nil
}

// SendError sends an error event
func (s *SSEWriter) SendError(message string, code string) error {
	return s.writeEvent("error", map[string]string{
		"message": message,
		"code":    code,
	})
}

// HandleInstructions handles GET /orchestrator/agent/instructions (SSE streaming)
func (h *InstructionHandler) HandleInstructions(w http.ResponseWriter, r *http.Request) {
	h.logger.Infow("HandleInstructions START", "method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr)

	// Custom panic recovery for SSE - don't try to write headers after they're sent
	defer func() {
		if err := recover(); err != nil {
			// Log the panic but don't try to write response - headers already sent
			h.logger.Errorw("SSE handler panic recovered", "error", err, "path", r.URL.Path)
		}
	}()

	h.logger.Infow("HandleInstructions called", "method", r.Method, "path", r.URL.Path)
	// Handle OPTIONS request for CORS preflight
	if r.Method == "OPTIONS" {
		h.handleOptions(w, r)
		return
	}

	// Only allow GET requests
	if r.Method != "GET" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	h.logger.Infow("Creating SSE writer...")
	// Create SSE writer
	sseWriter, err := NewSSEWriter(w, h.logger)
	if err != nil {
		h.logger.Errorw("failed to create SSE writer", "error", err)
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	h.logger.Infow("SSE writer created successfully")

	// Handle client disconnect
	ctx := r.Context()

	// Start heartbeat ticker (every 5 seconds)
	heartbeatTicker := time.NewTicker(5 * time.Second)
	defer heartbeatTicker.Stop()

	// Start instruction generation ticker (for demo/testing)
	// In production, this would be replaced with actual agent comprehension logic
	instructionTicker := time.NewTicker(10 * time.Second)
	defer instructionTicker.Stop()

	h.logger.Infow("SSE instruction stream started", "remote_addr", r.RemoteAddr)

	// Stream instructions until client disconnects
	for {
		select {
		case <-ctx.Done():
			// Client disconnected
			h.logger.Infow("client disconnected", "remote_addr", r.RemoteAddr)
			return

		case <-heartbeatTicker.C:
			// Send heartbeat to keep connection alive
			if err := sseWriter.SendHeartbeat(); err != nil {
				h.logger.Errorw("failed to send heartbeat", "error", err)
				return
			}

		case <-instructionTicker.C:
			// Generate and send instruction (demo implementation)
			// In production, this would be replaced with actual agent comprehension logic
			instruction := h.generateDemoInstruction()
			if instruction != nil {
				if err := sseWriter.SendInstruction(instruction); err != nil {
					h.logger.Errorw("failed to send instruction", "error", err)
					return
				}
				h.logger.Infow("instruction sent", "type", instruction.Type, "id", instruction.Metadata.ID)
			}
		}
	}
}

// handleOptions handles OPTIONS request for CORS preflight
func (h *InstructionHandler) handleOptions(w http.ResponseWriter, r *http.Request) {
	headers := w.Header()
	headers.Set("Access-Control-Allow-Origin", "*")
	headers.Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	headers.Set("Access-Control-Allow-Headers", "Accept, Content-Type, Authorization")
	headers.Set("Access-Control-Max-Age", "86400") // 24 hours
	w.WriteHeader(http.StatusOK)
}

// generateDemoInstruction generates a demo instruction for testing
// In production, this would be replaced with actual agent comprehension logic
func (h *InstructionHandler) generateDemoInstruction() *Instruction {
	// This is a placeholder implementation for testing
	// In production, this would:
	// 1. Receive user chat input
	// 2. Process with AI/agent comprehension
	// 3. Generate appropriate instructions based on user intent
	// 4. Return instructions for frontend execution

	// For now, return nil to avoid sending demo instructions
	// This allows the endpoint to be tested without generating unwanted instructions
	return nil
}

// InstructionStreamHandler handles streaming instructions from a channel
// This is used when instructions are generated externally (e.g., from agent comprehension)
func (h *InstructionHandler) InstructionStreamHandler(instructionChan <-chan *Instruction) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				h.logger.Errorw("SSE stream handler panic recovered", "error", err, "path", r.URL.Path)
			}
		}()

		// Handle OPTIONS request for CORS preflight
		if r.Method == "OPTIONS" {
			h.handleOptions(w, r)
			return
		}

		// Only allow GET requests
		if r.Method != "GET" {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Create SSE writer
		sseWriter, err := NewSSEWriter(w, h.logger)
		if err != nil {
			h.logger.Errorw("failed to create SSE writer", "error", err)
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}

		// Handle client disconnect
		ctx := r.Context()

		// Start heartbeat ticker (every 5 seconds)
		heartbeatTicker := time.NewTicker(5 * time.Second)
		defer heartbeatTicker.Stop()

		h.logger.Infow("SSE instruction stream started", "remote_addr", r.RemoteAddr)

		// Stream instructions until client disconnects
		for {
			select {
			case <-ctx.Done():
				// Client disconnected
				h.logger.Infow("client disconnected", "remote_addr", r.RemoteAddr)
				return

			case <-heartbeatTicker.C:
				// Send heartbeat to keep connection alive
				if err := sseWriter.SendHeartbeat(); err != nil {
					h.logger.Errorw("failed to send heartbeat", "error", err)
					return
				}

			case instruction, ok := <-instructionChan:
				if !ok {
					// Instruction channel closed
					h.logger.Infow("instruction channel closed", "remote_addr", r.RemoteAddr)
					return
				}

				// Send instruction
				if err := sseWriter.SendInstruction(instruction); err != nil {
					h.logger.Errorw("failed to send instruction", "error", err)
					return
				}
				h.logger.Infow("instruction sent", "type", instruction.Type, "id", instruction.Metadata.ID)
			}
		}
	}
}

// StreamInstructions streams instructions to an SSE writer from a context
// This is a helper function for custom streaming implementations
func StreamInstructions(ctx context.Context, sseWriter *SSEWriter, instructionChan <-chan *Instruction, logger *zap.SugaredLogger) error {
	// Start heartbeat ticker (every 5 seconds)
	heartbeatTicker := time.NewTicker(5 * time.Second)
	defer heartbeatTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Context cancelled (client disconnect or timeout)
			logger.Infow("instruction stream cancelled")
			return ctx.Err()

		case <-heartbeatTicker.C:
			// Send heartbeat to keep connection alive
			if err := sseWriter.SendHeartbeat(); err != nil {
				logger.Errorw("failed to send heartbeat", "error", err)
				return err
			}

		case instruction, ok := <-instructionChan:
			if !ok {
				// Instruction channel closed
				logger.Infow("instruction channel closed")
				return nil
			}

			// Validate instruction before sending
			if err := instruction.Validate(); err != nil {
				logger.Errorw("invalid instruction", "error", err, "type", instruction.Type)
				// Send error to client
				if sendErr := sseWriter.SendError(err.Error(), "validation_error"); sendErr != nil {
					return sendErr
				}
				continue
			}

			// Send instruction
			if err := sseWriter.SendInstruction(instruction); err != nil {
				logger.Errorw("failed to send instruction", "error", err)
				return err
			}
			logger.Infow("instruction sent", "type", instruction.Type, "id", instruction.Metadata.ID)
		}
	}
}
