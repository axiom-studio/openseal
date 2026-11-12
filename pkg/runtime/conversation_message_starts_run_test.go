package runtime

import (
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime/conversation/conversationtest"
)

func TestConversationMessageStartsRunSkipsApprovalCoordinatorProjections(t *testing.T) {
	conversation := &Conversation{
		ID: "approval-channel", Scope: Scope{Kind: "tenant", ID: "11"},
		Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "coding-agent"}, Status: ConversationStatusActive,
	}
	message := &ChannelMessage{
		ID: "approval-request", Scope: conversation.Scope, ConversationID: conversation.ID,
		Sender: ConversationParticipant{Type: ConversationParticipantService, ID: "approval-coordinator"},
		Intent: MessageIntentApprovalRequest,
	}
	if conversationMessageStartsRun(conversation, message) {
		t.Fatal("approval coordinator request recursively started an Agent Run")
	}
	message.Intent = MessageIntentUpdate
	if conversationMessageStartsRun(conversation, message) {
		t.Fatal("approval coordinator outcome recursively started an Agent Run")
	}
	message.Sender.ID = "release-coordinator"
	if !conversationMessageStartsRun(conversation, message) {
		t.Fatal("unrelated service handoff was suppressed")
	}
}

func TestTeamMessagesStartRunsOnlyForUsersAndHandoffs(t *testing.T) {
	conversation := &Conversation{ID: "chat", Scope: Scope{Kind: "tenant", ID: "one"}, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "team"}, Status: ConversationStatusActive}
	message := func(sender ConversationParticipant, mentions ...ConversationParticipant) *ChannelMessage {
		return &ChannelMessage{ID: "m", Scope: conversation.Scope, ConversationID: "chat", Sender: sender, Intent: MessageIntentAnswer, Mentions: mentions}
	}
	user := ConversationParticipant{Type: ConversationParticipantUser, ID: "kev"}
	if !conversationMessageStartsRun(conversation, message(user)) {
		t.Fatal("user message did not start a Run")
	}
	if conversationMessageStartsRun(conversation, message(conversationtest.AgentParticipant("rio"))) {
		t.Fatal("a member answer started a Run")
	}
	if conversationMessageStartsRun(conversation, message(conversationtest.AgentParticipant("rio"), conversationtest.AgentParticipant("rio"), user)) {
		t.Fatal("a self or user mention started a Run")
	}
	if !conversationMessageStartsRun(conversation, message(conversationtest.AgentParticipant("rio"), conversationtest.AgentParticipant("maya"))) {
		t.Fatal("a handoff did not start a Run")
	}
}
