package runtime

import (
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
)

// A Task's result is its own Agent's answer; another member's answer to the
// same message never counts as it.
func TestConversationTaskResultIsTheExpectedSpeakersOwnAnswer(t *testing.T) {
	scope := models.Scope{Kind: "tenant", ID: "roster"}
	run := &models.AgentRun{ID: "harp-run", Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "crew"}, AssignedAgentID: "harp"}
	task := &models.ConversationTask{Scope: scope, ConversationID: "chat", SourceMessageID: "ask", ThreadRootID: "ask", Owner: run.Owner, TargetAgentID: "harp", Mode: models.ConversationTaskModeContinuation}
	answer := func(sender string) *models.ChannelMessage {
		return &models.ChannelMessage{Scope: scope, ConversationID: "chat", ReplyToMessageID: "ask", ThreadRootID: "ask", Intent: models.MessageIntentAnswer, Content: "Done",
			Sender: models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: sender}, References: []models.ConversationReference{{Kind: models.ConversationReferenceRun, ID: run.ID}}}
	}
	if !conversationTaskResultMessageMatches(task, run, answer("harp"), false) {
		t.Fatal("member's own answer rejected")
	}
	if conversationTaskResultMessageMatches(task, run, answer("marlow"), false) {
		t.Fatal("another member's answer accepted as this Task's result")
	}
	task.TargetAgentID = ""
	if conversationTaskResultMessageMatches(task, run, answer("marlow"), false) || !conversationTaskResultMessageMatches(task, run, answer("harp"), false) {
		t.Fatal("routed continuation result is not its assigned member's")
	}
	own := &models.AgentRun{ID: "own-run", Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "own"}, AssignedAgentID: "own"}
	ownTask := &models.ConversationTask{Scope: scope, ConversationID: "chat", SourceMessageID: "ask", ThreadRootID: "ask", Owner: own.Owner, TargetAgentID: "own", Mode: models.ConversationTaskModeContinuation}
	ownAnswer := answer("own")
	ownAnswer.References = []models.ConversationReference{{Kind: models.ConversationReferenceRun, ID: own.ID}}
	if !conversationTaskResultMessageMatches(ownTask, own, ownAnswer, false) {
		t.Fatal("1:1 result rejected")
	}
}
