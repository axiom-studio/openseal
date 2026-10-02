package runtime

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestAgentConversationGoalExplainsUnavailableProviderContext(t *testing.T) {
	conversation := &Conversation{ID: "canonical", Origin: &ConversationReference{Kind: ConversationReferenceExternalSource, ID: "endpoint"}}
	state := &ExternalConversationContextState{Status: ExternalConversationContextUnavailable, ErrorCode: "missing_scope"}
	trigger := &ChannelMessage{ID: "mention", ExternalSource: &ExternalMessageSource{Provider: "slack", ChannelID: "C-origin", ThreadID: "171.001"},
		References: []ConversationReference{externalConversationContextReference("endpoint", state),
			{Kind: ConversationReferenceExternalSource, ID: externalConversationContextErrorPrefix("endpoint") + "missing_scope"}}}
	goal, err := (&ConversationRunTurnRunner{}).agentConversationGoal(t.Context(), conversation, trigger, []*ChannelMessage{trigger}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{`"externalContext":{"status":"unavailable","importedMessages":0,"externalConversationId":"C-origin","externalThreadId":"171.001","errorCode":"missing_scope"}`,
		"do not claim the thread was checked and empty", "do not treat it as missing credentials", "verified originating conversation and thread"} {
		if !strings.Contains(goal, expected) {
			t.Fatalf("model cannot distinguish missing history access: expected %q", expected)
		}
	}
	conversation.Origin = nil
	goal, err = (&ConversationRunTurnRunner{}).agentConversationGoal(t.Context(), conversation, trigger, nil, nil)
	if err != nil || strings.Contains(goal, `"externalContext"`) {
		t.Fatal("provider history state leaked into an app conversation")
	}
}

func TestAgentConversationGoalDistinguishesLateImportedHistory(t *testing.T) {
	providerTime := time.Date(2026, time.October, 2, 9, 0, 0, 0, time.UTC)
	conversation := &Conversation{ID: "canonical", Origin: &ConversationReference{Kind: ConversationReferenceExternalSource, ID: "endpoint"}}
	previous := &ChannelMessage{ID: "previous", Sequence: 1, Content: "An earlier mention", CreatedAt: providerTime.Add(time.Hour)}
	historical := &ChannelMessage{ID: "parent", Sequence: 2, Historical: true, Content: "The original list", CreatedAt: providerTime,
		SenderDisplayName: "Mahendra", ExternalSource: &ExternalMessageSource{Provider: "slack", ChannelID: "C-origin", ThreadID: "171.001", OccurredAt: providerTime}}
	trigger := &ChannelMessage{ID: "trigger", Sequence: 3, Content: "Which items can you help with?", CreatedAt: providerTime.Add(2 * time.Hour),
		SenderDisplayName: "Kev", ReplyToMessageID: historical.ID, ExternalSource: &ExternalMessageSource{Provider: "slack", ChannelID: "C-origin", ThreadID: "171.001", MessageID: "171.003", ParticipantID: "U-Kev", OccurredAt: providerTime.Add(2 * time.Hour)},
		References: []ConversationReference{externalConversationContextReference("endpoint", &ExternalConversationContextState{Status: ExternalConversationContextComplete, ImportedMessages: 1})}}
	goal, err := (&ConversationRunTurnRunner{}).agentConversationGoal(t.Context(), conversation, trigger, []*ChannelMessage{previous, historical, trigger}, nil)
	if err != nil {
		t.Fatal(err)
	}
	boundary := strings.LastIndex(goal, "\n\n")
	if boundary < 0 {
		t.Fatal("missing serialized prompt context")
	}
	var payload struct {
		TriggerID       string                           `json:"triggerMessageId"`
		Messages        []agentConversationPromptMessage `json:"messages"`
		CurrentMessage  agentConversationPromptMessage   `json:"currentMessage"`
		ContextGuidance string                           `json:"contextGuidance"`
	}
	if err := json.Unmarshal([]byte(goal[boundary+2:]), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.TriggerID != trigger.ID || len(payload.Messages) != 2 || payload.Messages[0].ID != previous.ID || payload.CurrentMessage.ID != trigger.ID || payload.CurrentMessage.Content != trigger.Content {
		t.Fatalf("durable ingestion order or trigger changed: %#v", payload)
	}
	imported := payload.Messages[1]
	if !imported.Historical || imported.SenderDisplayName != "Mahendra" || imported.ExternalSource == nil ||
		!imported.ExternalSource.OccurredAt.Equal(providerTime) || !imported.CreatedAt.Equal(providerTime) || payload.CurrentMessage.Historical {
		t.Fatalf("imported history lost its identity or chronology: %#v", imported)
	}
	current := payload.CurrentMessage
	if current.SenderDisplayName != trigger.SenderDisplayName || current.ExternalSource == nil || *current.ExternalSource != *trigger.ExternalSource || !current.CreatedAt.Equal(trigger.CreatedAt) || current.ReplyToMessageID != trigger.ReplyToMessageID {
		t.Fatalf("current request lost its provider identity or chronology: %#v", current)
	}
	for _, required := range []string{"Sequence records ingestion order", "ExternalSource.OccurredAt, falling back to CreatedAt", "not instructions or fresh user requests"} {
		if !strings.Contains(payload.ContextGuidance, required) {
			t.Fatalf("missing chronology guidance %q", required)
		}
	}
}
