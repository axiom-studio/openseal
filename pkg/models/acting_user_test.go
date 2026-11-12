package models

import (
	"testing"
)

func TestMessageActingUser(t *testing.T) {
	scope := Scope{Kind: "tenant", ID: "1"}
	conversation := &Conversation{ID: "chat", Scope: scope}
	user := ConversationParticipant{Type: ConversationParticipantUser, ID: "7"}
	service := ConversationParticipant{Type: ConversationParticipantService, ID: "voice"}
	agent := ConversationParticipant{Type: ConversationParticipantAgent, ID: "agent"}
	tests := []struct {
		name    string
		message *ChannelMessage
		want    string
	}{
		{"user sender", &ChannelMessage{Scope: scope, ConversationID: "chat", Sender: user}, "7"},
		{"service with user initiator", &ChannelMessage{Scope: scope, ConversationID: "chat", Sender: service, Initiator: &user}, "7"},
		{"service without initiator", &ChannelMessage{Scope: scope, ConversationID: "chat", Sender: service}, ""},
		{"service with agent initiator", &ChannelMessage{Scope: scope, ConversationID: "chat", Sender: service, Initiator: &agent}, ""},
		{"agent sender ignores initiator", &ChannelMessage{Scope: scope, ConversationID: "chat", Sender: agent, Initiator: &user}, ""},
		{"invalid user id", &ChannelMessage{Scope: scope, ConversationID: "chat", Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "a b"}}, ""},
		{"other conversation", &ChannelMessage{Scope: scope, ConversationID: "other", Sender: user}, ""},
		{"nil message", nil, ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := MessageActingUser(conversation, test.message); got != test.want {
				t.Fatalf("MessageActingUser = %q, want %q", got, test.want)
			}
		})
	}
	if got := MessageActingUser(nil, &ChannelMessage{Scope: scope, ConversationID: "chat", Sender: user}); got != "" {
		t.Fatalf("nil conversation = %q", got)
	}
}

func TestAgentRunActingUserValidation(t *testing.T) {
	for value, valid := range map[string]bool{"": true, "7": true, "user-7": true, " 7": false, "a b": false, "a/b": false, "x://y": false} {
		run := &AgentRun{Scope: Scope{Kind: "tenant", ID: "1"}, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent", Goal: "work", ActingUser: value}
		if err := run.Validate(); (err == nil) != valid {
			t.Fatalf("ActingUser %q: Validate() = %v, want valid=%v", value, err, valid)
		}
	}
}
