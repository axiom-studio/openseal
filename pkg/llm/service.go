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
	"time"
)

type LLMServiceImpl struct {
	openAIClient     OpenAIClient
	conversationRepo ConversationRepository
	toolBridge       *ToolBridge
	promptBuilder    PromptBuilder
	contextManager   ContextManager
	metricsCollector *MetricsCollector
	nodeTypes        []NodeTypeDefinition
}

func NewLLMService(
	openAIClient OpenAIClient,
	conversationRepo ConversationRepository,
	toolBridge *ToolBridge,
	promptBuilder PromptBuilder,
	contextManager ContextManager,
	metricsCollector *MetricsCollector,
	nodeTypes []NodeTypeDefinition,
) *LLMServiceImpl {
	return &LLMServiceImpl{
		openAIClient:     openAIClient,
		conversationRepo: conversationRepo,
		toolBridge:       toolBridge,
		promptBuilder:    promptBuilder,
		contextManager:   contextManager,
		metricsCollector: metricsCollector,
		nodeTypes:        nodeTypes,
	}
}

func (s *LLMServiceImpl) StreamChatMessage(ctx context.Context, req ChatRequest, userID string, eventChan chan<- StreamEvent) {
	defer close(eventChan)

	startTime := time.Now()
	s.metricsCollector.RecordAttempt()

	convID := req.ConversationID
	if convID == "" {
		convID = fmt.Sprintf("conv-%d-%d", req.AgentLibraryID, time.Now().Unix())
	}

	nodeTypeMap := make(map[string]*NodeTypeDefinition, len(s.nodeTypes))
	for i := range s.nodeTypes {
		nodeTypeMap[s.nodeTypes[i].Type] = &s.nodeTypes[i]
	}
	schemaVersion := ComputeSchemaVersion(nodeTypeMap)

	systemPrompt := s.promptBuilder.BuildSystemPrompt(s.nodeTypes, req.CurrentWorkflow, schemaVersion)

	userMsg := s.promptBuilder.BuildUserMessage(req.Message, req.CurrentWorkflow)

	openAIReq := OpenAIRequest{
		Model:        "gpt-5.4",
		Input:        userMsg,
		Instructions: systemPrompt,
		Tools:        s.buildToolDefinitions(),
		Stream:       true,
	}

	s.streamOpenAIResponse(ctx, openAIReq, eventChan)

	s.metricsCollector.RecordSuccess()
	s.metricsCollector.RecordConversationLatency(time.Since(startTime).Milliseconds())
}

func (s *LLMServiceImpl) ProcessChatMessage(ctx context.Context, req ChatRequest, userID string) (ChatResponse, error) {
	return ChatResponse{}, fmt.Errorf("non-streaming not implemented")
}

func (s *LLMServiceImpl) buildToolDefinitions() []ToolDefinition {
	return []ToolDefinition{
		{Name: "add_node", Description: "Add a new node to the workflow canvas", Parameters: map[string]interface{}{"type": "object"}},
		{Name: "remove_node", Description: "Remove a node from the workflow canvas", Parameters: map[string]interface{}{"type": "object"}},
		{Name: "update_node_config", Description: "Update the configuration of an existing node", Parameters: map[string]interface{}{"type": "object"}},
		{Name: "add_edge", Description: "Connect two nodes with an edge", Parameters: map[string]interface{}{"type": "object"}},
		{Name: "remove_edge", Description: "Remove an edge between nodes", Parameters: map[string]interface{}{"type": "object"}},
		{Name: "get_workflow", Description: "Get current workflow state", Parameters: map[string]interface{}{"type": "object"}},
		{Name: "get_node_types", Description: "Get all available node types", Parameters: map[string]interface{}{"type": "object"}},
		{Name: "get_node_schema", Description: "Get detailed schema for a specific node type", Parameters: map[string]interface{}{"type": "object"}},
		{Name: "clear_workflow", Description: "Remove all nodes and edges", Parameters: map[string]interface{}{"type": "object"}},
		{Name: "suggest_layout", Description: "Calculate non-overlapping positions for nodes", Parameters: map[string]interface{}{"type": "object"}},
		{Name: "create_agent", Description: "Create a new agent library with workflow", Parameters: map[string]interface{}{"type": "object"}},
	}
}

func (s *LLMServiceImpl) streamOpenAIResponse(ctx context.Context, req OpenAIRequest, eventChan chan<- StreamEvent) {
	eventStream, err := s.openAIClient.CreateStreamingResponse(ctx, req)
	if err != nil {
		s.metricsCollector.RecordFailure()
		s.metricsCollector.RecordError(ErrorCategoryLLM)
		eventChan <- StreamEvent{
			Type:    StreamEventError,
			Content: fmt.Sprintf("Failed to create streaming response: %v", err),
		}
		eventChan <- StreamEvent{Type: StreamEventDone}
		return
	}

	var pendingToolCalls []ToolCall

	for {
		select {
		case <-ctx.Done():
			eventChan <- StreamEvent{
				Type:    StreamEventError,
				Content: "Request cancelled",
			}
			eventChan <- StreamEvent{Type: StreamEventDone}
			return
		case event, ok := <-eventStream:
			if !ok {
				if len(pendingToolCalls) > 0 {
					s.handlePendingToolCalls(ctx, pendingToolCalls, eventChan)
				}
				eventChan <- StreamEvent{Type: StreamEventDone}
				return
			}

			switch event.Type {
			case "text_delta":
				eventChan <- StreamEvent{
					Type:    StreamEventTextDelta,
					Content: event.Delta,
				}

			case "function_call":
				if event.Item != nil {
					var args map[string]interface{}
					if err := json.Unmarshal([]byte(event.Item.Arguments), &args); err != nil {
						args = map[string]interface{}{}
					}

					toolCall := ToolCall{
						CallID:    event.Item.CallID,
						Name:      event.Item.Name,
						Arguments: args,
					}
					pendingToolCalls = append(pendingToolCalls, toolCall)

					eventChan <- StreamEvent{
						Type: StreamEventToolCall,
						Data: toolCall,
					}
				}

			case "done":
				if len(pendingToolCalls) > 0 {
					s.handlePendingToolCalls(ctx, pendingToolCalls, eventChan)
				}
				eventChan <- StreamEvent{Type: StreamEventDone}
				return
			}
		}
	}
}

func (s *LLMServiceImpl) handlePendingToolCalls(ctx context.Context, toolCalls []ToolCall, eventChan chan<- StreamEvent) {
	for _, tc := range toolCalls {
		toolStart := time.Now()

		result, err := s.toolBridge.WaitForResult(tc.CallID, 30*time.Second)
		if err != nil {
			result = ToolResult{
				CallID: tc.CallID,
				Status: "failed",
				Error: &ToolError{
					Code:    "TIMEOUT",
					Message: fmt.Sprintf("Tool result not received within timeout: %v", err),
				},
			}
		}

		latencyMs := time.Since(toolStart).Milliseconds()
		success := result.Status == "success"
		s.metricsCollector.RecordToolCall(tc.Name, success, latencyMs)

		eventChan <- StreamEvent{
			Type: StreamEventToolResult,
			Data: result,
		}
	}
}
