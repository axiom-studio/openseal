package runtime

import (
	"fmt"
	"strings"
	"testing"
	"time"

	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
)

// Work a Team member hands to a teammate in the chat is answered in that same
// chat: an ordinary chat Run in the teammate's own lane with the chat's ids.
func TestCollaborationChildAnswersInTheSameChat(t *testing.T) {
	now := time.Now().UTC()
	scope := Scope{Kind: "tenant", ID: "handoff"}
	source := &AgentRun{ID: "marlow-run", RootRunID: "marlow-run", Kind: RunKindConversation, Scope: scope,
		Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "gtm"}, AssignedAgentID: "marlow",
		ConcurrencyKey: ConversationRunLane("chat", "root", "marlow"), Source: RunSourceChat,
		Context: map[string]interface{}{conversationRunContextConversationID: "chat", conversationRunContextTriggerID: "ask", "threadRootMessageId": "root"}}
	for _, kind := range []AgentRequestKind{AgentRequestKindRequest, AgentRequestKindHandoff} {
		request := &AgentRequest{ID: "request", Kind: kind, Goal: "Suggest one launch step",
			Requester: CollaborationParty{Type: OwnerTypeTeam, ID: "gtm"}, Recipient: CollaborationParty{Type: OwnerTypeAgent, ID: "harp"}, AssignedAgentID: "harp"}
		child := buildCollaborationChildRun(source, request, now, "harp-run")
		// A hand-off gives the work to its recipient member; the chat keeps it.
		if child.Owner != source.Owner || child.AssignedAgentID != "harp" {
			t.Fatalf("%s child owner = %v, Agent %q", kind, child.Owner, child.AssignedAgentID)
		}
		if child.Kind != RunKindConversation || child.ConcurrencyKey != ConversationRunLane("chat", "root", "harp") {
			t.Fatalf("%s child = %s %q", kind, child.Kind, child.ConcurrencyKey)
		}
		if err := validateConversationRun(child); err != nil {
			t.Fatalf("%s child is not a valid chat Run: %v", kind, err)
		}
		if child.Context[conversationRunContextTriggerID] != "ask" || child.Context["threadRootMessageId"] != "root" {
			t.Fatalf("%s child context = %#v", kind, child.Context)
		}
		// The chat tells the teammate who handed the work over and what it is.
		want := fmt.Sprintf("Work handed to you by marlow (%s): Suggest one launch step", child.Source)
		if child.Source != collaborationChildSource[kind] {
			t.Fatalf("%s child source = %q", kind, child.Source)
		}
		if note := handOffRespondTo(source, child); !strings.Contains(note, want) {
			t.Fatalf("%s respond-to note = %q; want %q", kind, note, want)
		}
	}
	if note := handOffRespondTo(source, source); strings.Contains(note, "Work handed to you") {
		t.Fatalf("a chat message is not handed-over work: %q", note)
	}
}

var collaborationChildSource = map[AgentRequestKind]RunSource{AgentRequestKindRequest: RunSourceRequest, AgentRequestKindHandoff: RunSourceHandoff}

// handOffRespondTo is the respond-to section of run's chat context note,
// answering the chat message that started source.
func handOffRespondTo(source, run *AgentRun) string {
	trigger := &ChannelMessage{ID: "ask", ConversationID: "chat", Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "ana"}}
	conversation := &Conversation{ID: "chat", Scope: source.Scope, Owner: source.Owner}
	return runtimeconversation.RunContextRespondTo(runtimeconversation.ConversationContextInput{Run: run, Conversation: conversation, Trigger: trigger})
}
