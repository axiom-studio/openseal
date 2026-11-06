package runtime

import (
	"testing"
	"time"
)

// Work a Team member hands to a teammate in the chat is answered in that same
// chat: an ordinary chat Run in the teammate's own lane with the chat's ids.
func TestCollaborationChildAnswersInTheSameChat(t *testing.T) {
	now := time.Now().UTC()
	scope := Scope{Kind: "tenant", ID: "handoff"}
	source := &AgentRun{ID: "marlow-run", RootRunID: "marlow-run", Kind: RunKindConversation, Scope: scope,
		Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "gtm"}, AssignedAgentID: "marlow",
		ConcurrencyKey: conversationRunLane("chat", "root", "marlow"), Source: RunSourceChat,
		Context: map[string]interface{}{conversationRunContextConversationID: "chat", conversationRunContextTriggerID: "ask", "threadRootMessageId": "root"}}
	for _, kind := range []AgentRequestKind{AgentRequestKindRequest, AgentRequestKindHandoff} {
		request := &AgentRequest{ID: "request", Kind: kind, Goal: "Suggest one launch step",
			Requester: CollaborationParty{Type: OwnerTypeTeam, ID: "gtm"}, Recipient: CollaborationParty{Type: OwnerTypeAgent, ID: "harp"}, AssignedAgentID: "harp"}
		child := buildCollaborationChildRun(source, request, now, "harp-run")
		if kind == AgentRequestKindHandoff {
			// A handoff gives the work to its recipient; it still answers in the chat.
			child.Owner = source.Owner
		}
		if child.Kind != RunKindConversation || child.ConcurrencyKey != conversationRunLane("chat", "root", "harp") {
			t.Fatalf("%s child = %s %q", kind, child.Kind, child.ConcurrencyKey)
		}
		if err := validateConversationRun(child); err != nil {
			t.Fatalf("%s child is not a valid chat Run: %v", kind, err)
		}
		if child.Context[conversationRunContextTriggerID] != "ask" || child.Context["threadRootMessageId"] != "root" {
			t.Fatalf("%s child context = %#v", kind, child.Context)
		}
		if assignment := conversationRunAssignment(child); assignment == nil || assignment.From != "marlow" || assignment.Work != "Suggest one launch step" {
			t.Fatalf("%s assignment = %#v", kind, assignment)
		}
	}
	if conversationRunAssignment(source) != nil {
		t.Fatal("a chat message is not handed-over work")
	}
}

// An Agent's 1:1 chat belongs to that Agent alone, so work it hands to another
// Agent runs as ordinary Agent work rather than as an invalid chat Run.
func TestCollaborationChildFromOneToOneChatIsAgentWork(t *testing.T) {
	source := &AgentRun{ID: "own-run", Kind: RunKindConversation, Scope: Scope{Kind: "tenant", ID: "one"},
		Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "own"}, AssignedAgentID: "own", ConcurrencyKey: conversationRunLane("chat", "", "own"),
		Context: map[string]interface{}{conversationRunContextConversationID: "chat", conversationRunContextTriggerID: "ask"}}
	request := &AgentRequest{ID: "request", Kind: AgentRequestKindRequest, Goal: "Review",
		Requester: CollaborationParty{Type: OwnerTypeAgent, ID: "own"}, Recipient: CollaborationParty{Type: OwnerTypeAgent, ID: "other"}, AssignedAgentID: "other"}
	if child := buildCollaborationChildRun(source, request, time.Now().UTC(), "other-run"); child.Kind != RunKindAgentWork {
		t.Fatalf("child kind = %s", child.Kind)
	}
}
