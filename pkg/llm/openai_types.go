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

// OpenAIRequest - request to OpenAI Responses API
type OpenAIRequest struct {
	Model            string           `json:"model"`
	Input            interface{}      `json:"input"`
	Instructions     string           `json:"instructions"`
	Tools            []ToolDefinition `json:"tools,omitempty"`
	Stream           bool             `json:"stream,omitempty"`
	Store            bool             `json:"store,omitempty"`
	PreviousResponseID string         `json:"previous_response_id,omitempty"`
}

// OpenAIResponse - response from OpenAI Responses API
type OpenAIResponse struct {
	ID     string       `json:"id"`
	Output []OutputItem `json:"output"`
	Status string       `json:"status"`
}

// OutputItem - polymorphic output item
type OutputItem struct {
	Type   string        `json:"type"`
	ID     string        `json:"id,omitempty"`
	Role   string        `json:"role,omitempty"`
	Content []ContentPart `json:"content,omitempty"`
	// For function_call type
	Name      string `json:"name,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// ContentPart represents a part of message content
type ContentPart struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// OpenAIStreamEvent - streaming event from OpenAI
type OpenAIStreamEvent struct {
	Type  string      `json:"type"`
	Delta string      `json:"delta,omitempty"`
	Item  *OutputItem `json:"item,omitempty"`
	Done  bool        `json:"done,omitempty"`
}
