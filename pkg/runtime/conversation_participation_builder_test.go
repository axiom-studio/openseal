package runtime

import "testing"

// TestAgentBuilderConversationNeedsNoParticipationOptIn: a host that leaves
// unconfigured chats inert (the OSS daemon) still answers a builder chat,
// and only a builder chat.
func TestAgentBuilderConversationNeedsNoParticipationOptIn(t *testing.T) {
	builder := ObjectiveOwner{Type: OwnerTypeAgent, ID: AgentBuilderDeploymentID}
	origin := &ConversationReference{Kind: ConversationReferenceAgentBuilder, ID: "request-1"}
	message := &ChannelMessage{Sequence: 1}
	for _, tc := range []struct {
		name         string
		conversation *Conversation
		want         bool
	}{
		{"builder chat", &Conversation{Owner: builder, Origin: origin, Status: ConversationStatusActive}, true},
		{"archived builder chat", &Conversation{Owner: builder, Origin: origin, Status: ConversationStatusArchived}, false},
		{"builder's other chat", &Conversation{Owner: builder, Status: ConversationStatusActive}, false},
		{"another agent's builder-origin chat", &Conversation{Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "helper"}, Origin: origin, Status: ConversationStatusActive}, false},
		{"opted-in chat", &Conversation{Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "team"}, Status: ConversationStatusActive, Participation: &ConversationParticipation{Enabled: true}}, true},
	} {
		if got := conversationParticipationAllows(tc.conversation, message); got != tc.want {
			t.Errorf("%s: allows = %v, want %v", tc.name, got, tc.want)
		}
	}
}
