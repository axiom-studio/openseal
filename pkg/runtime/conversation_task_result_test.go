package runtime

import "testing"

// A Task's result is its own Agent's answer; another member's answer to the
// same message never counts as it.
func TestConversationTaskResultIsTheExpectedSpeakersOwnAnswer(t *testing.T) {
	scope := Scope{Kind: "tenant", ID: "roster"}
	run := &AgentRun{ID: "harp-run", Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "crew"}, AssignedAgentID: "harp"}
	task := &ConversationTask{Scope: scope, ConversationID: "chat", SourceMessageID: "ask", ThreadRootID: "ask", Owner: run.Owner, TargetAgentID: "harp", Mode: ConversationTaskModeContinuation}
	answer := func(sender string) *ChannelMessage {
		return &ChannelMessage{Scope: scope, ConversationID: "chat", ReplyToMessageID: "ask", ThreadRootID: "ask", Intent: MessageIntentAnswer, Content: "Done",
			Sender: ConversationParticipant{Type: ConversationParticipantAgent, ID: sender}, References: []ConversationReference{{Kind: ConversationReferenceRun, ID: run.ID}}}
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
	own := &AgentRun{ID: "own-run", Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "own"}, AssignedAgentID: "own"}
	ownTask := &ConversationTask{Scope: scope, ConversationID: "chat", SourceMessageID: "ask", ThreadRootID: "ask", Owner: own.Owner, TargetAgentID: "own", Mode: ConversationTaskModeContinuation}
	ownAnswer := answer("own")
	ownAnswer.References = []ConversationReference{{Kind: ConversationReferenceRun, ID: own.ID}}
	if !conversationTaskResultMessageMatches(ownTask, own, ownAnswer, false) {
		t.Fatal("1:1 result rejected")
	}
}
