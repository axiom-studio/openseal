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
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	defaultBaseURL = "https://api.openai.com/v1"
	defaultModel   = "gpt-5.4"
	requestTimeout = 20 * time.Second
	maxRetries     = 2
)

// OpenAIClientImpl implements OpenAIClient
type OpenAIClientImpl struct {
	httpClient *http.Client
	apiKey     string
	baseURL    string
	model      string
}

// NewOpenAIClient creates a new client
func NewOpenAIClient(apiKey string, baseURL string, model string) *OpenAIClientImpl {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	if model == "" {
		model = defaultModel
	}
	return &OpenAIClientImpl{
		httpClient: &http.Client{},
		apiKey:     apiKey,
		baseURL:    baseURL,
		model:      model,
	}
}

// CreateResponse makes a non-streaming request to OpenAI Responses API
func (c *OpenAIClientImpl) CreateResponse(ctx context.Context, req OpenAIRequest) (*OpenAIResponse, error) {
	return c.createResponseWithRetry(ctx, req, false)
}

// CreateStreamingResponse makes a streaming request to OpenAI Responses API
func (c *OpenAIClientImpl) CreateStreamingResponse(ctx context.Context, req OpenAIRequest) (<-chan OpenAIStreamEvent, error) {
	req.Stream = true
	eventChan := make(chan OpenAIStreamEvent, 100)

	// Build request body
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	// Create HTTP request with timeout context
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/responses", bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)

	// Execute request
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("request failed: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		cancel()
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(bodyBytes))
	}

	// Parse SSE stream in goroutine
	go func() {
		defer cancel()
		defer close(eventChan)
		c.parseSSEStream(resp.Body, eventChan)
	}()

	return eventChan, nil
}

func (c *OpenAIClientImpl) parseSSEStream(body io.ReadCloser, eventChan chan<- OpenAIStreamEvent) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<20)
	scanner.Split(splitSSEEvents)

	var currentItem *OutputItem
	var currentArgs strings.Builder

	for scanner.Scan() {
		line := scanner.Text()

		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			if data == "[DONE]" {
				eventChan <- OpenAIStreamEvent{Type: "done", Done: true}
				return
			}

			var event map[string]interface{}
			if err := json.Unmarshal([]byte(data), &event); err != nil {
				continue
			}

			eventType, _ := event["type"].(string)

			switch eventType {
			case "response.output_text.delta":
				delta, _ := event["delta"].(string)
				eventChan <- OpenAIStreamEvent{Type: "text_delta", Delta: delta}

			case "response.output_item.added":
				itemData, _ := event["item"].(map[string]interface{})
				if itemData != nil {
					itemType, _ := itemData["type"].(string)
					if itemType == "function_call" {
						currentItem = &OutputItem{
							Type:   "function_call",
							ID:     itemData["id"].(string),
							Name:   itemData["name"].(string),
							CallID: itemData["call_id"].(string),
						}
						currentArgs.Reset()
					}
				}

			case "response.function_call_arguments.delta":
				delta, _ := event["delta"].(string)
				currentArgs.WriteString(delta)

			case "response.function_call_arguments.done":
				if currentItem != nil {
					currentItem.Arguments = currentArgs.String()
					eventChan <- OpenAIStreamEvent{Type: "function_call", Item: currentItem}
					currentItem = nil
				}

			case "response.completed", "response.done":
				eventChan <- OpenAIStreamEvent{Type: "done", Done: true}
				return
			}
		}
	}
}

// splitSSEEvents splits SSE events (data: lines separated by blank lines)
func splitSSEEvents(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}

	// Find next event boundary (blank line)
	if i := bytes.Index(data, []byte("\n\n")); i >= 0 {
		return i + 2, data[:i], nil
	}

	if atEOF {
		return len(data), data, nil
	}

	return 0, nil, nil
}

func (c *OpenAIClientImpl) createResponseWithRetry(ctx context.Context, req OpenAIRequest, streaming bool) (*OpenAIResponse, error) {
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			// Exponential backoff
			backoff := time.Duration(1<<uint(attempt-1)) * time.Second
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
		}

		resp, err := c.doRequest(ctx, req, streaming)
		if err == nil {
			return resp, nil
		}

		lastErr = err

		// Only retry on 5xx errors
		if !isRetryableError(err) {
			return nil, err
		}
	}

	return nil, fmt.Errorf("max retries exceeded: %w", lastErr)
}

func (c *OpenAIClientImpl) doRequest(ctx context.Context, req OpenAIRequest, streaming bool) (*OpenAIResponse, error) {
	req.Stream = streaming
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/responses", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var openAIResp OpenAIResponse
	if err := json.NewDecoder(resp.Body).Decode(&openAIResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &openAIResp, nil
}

func isRetryableError(err error) bool {
	// Check for 5xx errors in error message
	msg := err.Error()
	return strings.Contains(msg, "500") || strings.Contains(msg, "502") ||
		strings.Contains(msg, "503") || strings.Contains(msg, "504") ||
		strings.Contains(msg, "context deadline exceeded")
}
