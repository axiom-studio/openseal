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
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestOpenAIClient_CreateResponse_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Fatalf("unexpected auth header: %s", r.Header.Get("Authorization"))
		}

		resp := OpenAIResponse{
			ID:     "resp_123",
			Status: "completed",
			Output: []OutputItem{
				{
					Type: "message",
					Content: []ContentPart{
						{Type: "output_text", Text: "Hello from OpenAI!"},
					},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := NewOpenAIClient("test-key", server.URL, "gpt-5.4")

	req := OpenAIRequest{
		Input: "Say hello",
	}

	result, err := client.CreateResponse(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.ID != "resp_123" {
		t.Fatalf("expected ID resp_123, got %s", result.ID)
	}
	if result.Status != "completed" {
		t.Fatalf("expected status completed, got %s", result.Status)
	}
	if len(result.Output) != 1 {
		t.Fatalf("expected 1 output item, got %d", len(result.Output))
	}
}

func TestOpenAIClient_CreateStreamingResponse_TextAndToolCalls(t *testing.T) {
	var mu sync.Mutex
	callCount := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		callCount++
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		flusher := w.(http.Flusher)

		events := []string{
			`data: {"type": "response.output_text.delta", "delta": "Hello"}`,
			``,
			`data: {"type": "response.output_text.delta", "delta": " world!"}`,
			``,
			`data: {"type": "response.output_item.added", "item": {"type": "function_call", "id": "item_1", "name": "get_weather", "call_id": "call_abc"}}`,
			``,
			`data: {"type": "response.function_call_arguments.delta", "delta": "{\"location\": \"NYC\"}"}`,
			``,
			`data: {"type": "response.function_call_arguments.done"}`,
			``,
			`data: {"type": "response.completed"}`,
			``,
		}

		for _, event := range events {
			fmt.Fprintln(w, event)
			flusher.Flush()
		}
	}))
	defer server.Close()

	client := NewOpenAIClient("test-key", server.URL, "gpt-5.4")

	req := OpenAIRequest{
		Input: "What's the weather?",
	}

	eventChan, err := client.CreateStreamingResponse(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var textParts []string
	var funcCalls []*OutputItem
	var doneReceived bool

	for event := range eventChan {
		switch event.Type {
		case "text_delta":
			textParts = append(textParts, event.Delta)
		case "function_call":
			funcCalls = append(funcCalls, event.Item)
		case "done":
			doneReceived = true
		}
	}

	if !doneReceived {
		t.Fatal("expected done event")
	}

	text := strings.Join(textParts, "")
	if text != "Hello world!" {
		t.Fatalf("expected 'Hello world!', got '%s'", text)
	}

	if len(funcCalls) != 1 {
		t.Fatalf("expected 1 function call, got %d", len(funcCalls))
	}

	fc := funcCalls[0]
	if fc.Name != "get_weather" {
		t.Fatalf("expected function name get_weather, got %s", fc.Name)
	}
	if fc.CallID != "call_abc" {
		t.Fatalf("expected call_id call_abc, got %s", fc.CallID)
	}
	if fc.Arguments != `{"location": "NYC"}` {
		t.Fatalf("expected arguments {\"location\": \"NYC\"}, got %s", fc.Arguments)
	}
}

func TestOpenAIClient_Timeout(t *testing.T) {
	stopCh := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(25 * time.Second):
			w.WriteHeader(http.StatusOK)
		case <-stopCh:
		}
	}))
	defer func() {
		close(stopCh)
		server.Close()
	}()

	client := NewOpenAIClient("test-key", server.URL, "gpt-5.4")

	req := OpenAIRequest{
		Input: "This should timeout",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	start := time.Now()
	_, err := client.CreateResponse(ctx, req)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}

	if elapsed > 5*time.Second {
		t.Fatalf("timeout took too long: %v", elapsed)
	}

	if elapsed < 500*time.Millisecond {
		t.Fatalf("timeout was too short: %v", elapsed)
	}
}

func TestOpenAIClient_RetryOn5xx(t *testing.T) {
	var mu sync.Mutex
	callCount := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		callCount++
		current := callCount
		mu.Unlock()

		if current == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"error": "internal server error"}`))
			return
		}

		resp := OpenAIResponse{
			ID:     "resp_retry",
			Status: "completed",
			Output: []OutputItem{
				{Type: "message", Content: []ContentPart{{Type: "output_text", Text: "Success after retry"}}},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := NewOpenAIClient("test-key", server.URL, "gpt-5.4")

	req := OpenAIRequest{
		Input: "Retry me",
	}

	result, err := client.CreateResponse(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.ID != "resp_retry" {
		t.Fatalf("expected ID resp_retry, got %s", result.ID)
	}

	mu.Lock()
	count := callCount
	mu.Unlock()

	if count != 2 {
		t.Fatalf("expected 2 calls (1 fail + 1 success), got %d", count)
	}
}

func TestOpenAIClient_NoRetryOn4xx(t *testing.T) {
	var mu sync.Mutex
	callCount := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		callCount++
		mu.Unlock()

		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error": "bad request"}`))
	}))
	defer server.Close()

	client := NewOpenAIClient("test-key", server.URL, "gpt-5.4")

	req := OpenAIRequest{
		Input: "Bad request",
	}

	_, err := client.CreateResponse(context.Background(), req)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if !strings.Contains(err.Error(), "400") {
		t.Fatalf("expected 400 in error, got: %v", err)
	}

	mu.Lock()
	count := callCount
	mu.Unlock()

	if count != 1 {
		t.Fatalf("expected exactly 1 call (no retry on 4xx), got %d", count)
	}
}
