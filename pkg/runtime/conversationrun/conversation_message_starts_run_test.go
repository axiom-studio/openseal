package conversationrun

import (
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation/conversationtest"
)

func TestConversationMessageStartsRunSkipsApprovalCoordinatorProjections(t *testing.T) {
	conversation := &models.Conversation{
		ID: "approval-channel", Scope: models.Scope{Kind: "tenant", ID: "11"},
		Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "coding-agent"}, Status: models.ConversationStatusActive,
	}
	message := &models.ChannelMessage{
		ID: "approval-request", Scope: conversation.Scope, ConversationID: conversation.ID,
		Sender: models.ConversationParticipant{Type: models.ConversationParticipantService, ID: "approval-coordinator"},
		Intent: models.MessageIntentApprovalRequest,
	}
	if conversationMessageStartsRun(conversation, message) {
		t.Fatal("approval coordinator request recursively started an Agent Run")
	}
	message.Intent = models.MessageIntentUpdate
	if conversationMessageStartsRun(conversation, message) {
		t.Fatal("approval coordinator outcome recursively started an Agent Run")
	}
	message.Sender.ID = "release-coordinator"
	if !conversationMessageStartsRun(conversation, message) {
		t.Fatal("unrelated service handoff was suppressed")
	}
}

func TestTeamMessagesStartRunsOnlyForUsersAndHandoffs(t *testing.T) {
	conversation := &models.Conversation{ID: "chat", Scope: models.Scope{Kind: "tenant", ID: "one"}, Owner: models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "team"}, Status: models.ConversationStatusActive}
	message := func(sender models.ConversationParticipant, mentions ...models.ConversationParticipant) *models.ChannelMessage {
		return &models.ChannelMessage{ID: "m", Scope: conversation.Scope, ConversationID: "chat", Sender: sender, Intent: models.MessageIntentAnswer, Mentions: mentions}
	}
	user := models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "kev"}
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
