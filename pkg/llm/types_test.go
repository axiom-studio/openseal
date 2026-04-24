package llm

import (
	"encoding/json"
	"testing"
	"time"
)

func TestStreamEventTypeConstants(t *testing.T) {
	tests := []struct {
		name  string
		value StreamEventType
		want  string
	}{
		{"tool_call", StreamEventToolCall, "tool_call"},
		{"tool_result", StreamEventToolResult, "tool_result"},
		{"text_delta", StreamEventTextDelta, "text_delta"},
		{"state_snapshot", StreamEventStateSnapshot, "state_snapshot"},
		{"done", StreamEventDone, "done"},
		{"error", StreamEventError, "error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if string(tt.value) != tt.want {
				t.Errorf("got %q, want %q", tt.value, tt.want)
			}
		})
	}
}

func TestStreamEvent_MarshalUnmarshal(t *testing.T) {
	event := StreamEvent{
		Type:    StreamEventTextDelta,
		Content: "Hello",
		Data:    map[string]string{"key": "value"},
	}
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded StreamEvent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Type != StreamEventTextDelta || decoded.Content != "Hello" {
		t.Errorf("got type=%q content=%q", decoded.Type, decoded.Content)
	}
}

func TestChatRequest_MarshalUnmarshal(t *testing.T) {
	wfID := 42
	req := ChatRequest{
		ConversationID: "conv-1", Message: "add node",
		AgentLibraryID: 5, WorkflowID: &wfID,
	}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded ChatRequest
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.ConversationID != "conv-1" || decoded.Message != "add node" {
		t.Errorf("unexpected decoded values")
	}
}

func TestChatResponse_MarshalUnmarshal(t *testing.T) {
	resp := ChatResponse{
		ConversationID: "conv-1", Message: "done",
		ToolCalls: []ToolCall{{CallID: "c1", Name: "add_node", Arguments: map[string]interface{}{"type": "trigger"}}},
		Finished: true,
	}
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded ChatResponse
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(decoded.ToolCalls) != 1 || decoded.ToolCalls[0].Name != "add_node" {
		t.Errorf("unexpected tool calls")
	}
}

func TestToolCall_MarshalUnmarshal(t *testing.T) {
	call := ToolCall{
		CallID: "c1", Name: "add_node",
		Arguments: map[string]interface{}{"type": "http"},
	}
	data, err := json.Marshal(call)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded ToolCall
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.CallID != "c1" || decoded.Name != "add_node" {
		t.Errorf("got %q %q", decoded.CallID, decoded.Name)
	}
}

func TestToolResult_WithSuccess(t *testing.T) {
	result := ToolResult{
		CallID: "c1", Status: "completed",
		Result: map[string]interface{}{"nodeId": "n1"},
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded ToolResult
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Status != "completed" || decoded.Error != nil {
		t.Errorf("unexpected result")
	}
}

func TestToolResult_WithError(t *testing.T) {
	result := ToolResult{
		CallID: "c1", Status: "failed",
		Error: &ToolError{Code: "INVALID", Message: "bad config"},
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded ToolResult
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Status != "failed" || decoded.Error == nil || decoded.Error.Code != "INVALID" {
		t.Errorf("unexpected error result")
	}
}

func TestWorkflowState_MarshalUnmarshal(t *testing.T) {
	state := WorkflowState{
		Nodes: []NodeState{{ID: "n1", Type: "trigger", Name: "Webhook", PositionX: 100, PositionY: 200}},
		Edges: []EdgeState{{ID: "e1", SourceNodeID: "n1", TargetNodeID: "n2"}},
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded WorkflowState
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(decoded.Nodes) != 1 || len(decoded.Edges) != 1 {
		t.Errorf("unexpected state")
	}
}

func TestConversation_MarshalUnmarshal(t *testing.T) {
	now := time.Now()
	conv := Conversation{
		ID: "conv-1", UserID: "u1", WorkflowID: 7, AgentLibraryID: 3, Status: "active",
		CreatedAt: now, UpdatedAt: now,
	}
	data, err := json.Marshal(conv)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded Conversation
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.ID != "conv-1" || decoded.Status != "active" {
		t.Errorf("unexpected conversation")
	}
}

func TestMessage_MarshalUnmarshal(t *testing.T) {
	now := time.Now()
	msg := Message{
		ID: "msg-1", ConversationID: "conv-1", Role: "user", Content: "hello",
		InputTokens: 100, OutputTokens: 50, CreatedAt: now,
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded Message
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.ID != "msg-1" || decoded.Role != "user" {
		t.Errorf("unexpected message")
	}
}

func TestToolDefinition_MarshalUnmarshal(t *testing.T) {
	def := ToolDefinition{
		Name: "add_node", Description: "Add a node",
		Parameters: map[string]interface{}{"type": "object"},
	}
	data, err := json.Marshal(def)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded ToolDefinition
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Name != "add_node" {
		t.Errorf("got %q", decoded.Name)
	}
}

func TestNodeTypeDefinition_MarshalUnmarshal(t *testing.T) {
	nt := NodeTypeDefinition{
		Type: "http", Name: "HTTP", Category: "action", Description: "Make HTTP request",
		ConfigFields:  []ConfigFieldDefinition{{Name: "url", Type: "string", Required: true}},
		OutputHandles: []OutputHandle{{ID: "success", Label: "Success"}},
	}
	data, err := json.Marshal(nt)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded NodeTypeDefinition
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Type != "http" || len(decoded.ConfigFields) != 1 {
		t.Errorf("unexpected node type")
	}
}
