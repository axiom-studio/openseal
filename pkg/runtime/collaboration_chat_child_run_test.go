package runtime

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
)

// Work a Team member hands to a teammate in the chat is answered in that same
// chat: an ordinary chat Run in the teammate's own lane with the chat's ids.
func TestCollaborationChildAnswersInTheSameChat(t *testing.T) {
	now := time.Now().UTC()
	scope := models.Scope{Kind: "tenant", ID: "handoff"}
	source := &models.AgentRun{ID: "marlow-run", RootRunID: "marlow-run", Kind: models.RunKindConversation, Scope: scope,
		Owner: models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "gtm"}, AssignedAgentID: "marlow",
		ConcurrencyKey: models.ConversationRunLane("chat", "root", "marlow"), Source: models.RunSourceChat,
		Context: map[string]interface{}{models.ConversationRunConversationIDContextKey: "chat", models.ConversationRunTriggerIDContextKey: "ask", "threadRootMessageId": "root"}}
	for _, kind := range []models.AgentRequestKind{models.AgentRequestKindRequest, models.AgentRequestKindHandoff} {
		request := &models.AgentRequest{ID: "request", Kind: kind, Goal: "Suggest one launch step",
			Requester: models.CollaborationParty{Type: models.OwnerTypeTeam, ID: "gtm"}, Recipient: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "harp"}, AssignedAgentID: "harp"}
		child := buildCollaborationChildRun(source, request, now, "harp-run")
		// A hand-off gives the work to its recipient member; the chat keeps it.
		if child.Owner != source.Owner || child.AssignedAgentID != "harp" {
			t.Fatalf("%s child owner = %v, Agent %q", kind, child.Owner, child.AssignedAgentID)
		}
		if child.Kind != models.RunKindConversation || child.ConcurrencyKey != models.ConversationRunLane("chat", "root", "harp") {
			t.Fatalf("%s child = %s %q", kind, child.Kind, child.ConcurrencyKey)
		}
		if err := models.ValidateConversationRun(child); err != nil {
			t.Fatalf("%s child is not a valid chat Run: %v", kind, err)
		}
		if child.Context[models.ConversationRunTriggerIDContextKey] != "ask" || child.Context["threadRootMessageId"] != "root" {
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

var collaborationChildSource = map[models.AgentRequestKind]models.RunSource{models.AgentRequestKindRequest: models.RunSourceRequest, models.AgentRequestKindHandoff: models.RunSourceHandoff}

// handOffRespondTo is the respond-to section of run's chat context note,
// answering the chat message that started source.
func handOffRespondTo(source, run *models.AgentRun) string {
	trigger := &models.ChannelMessage{ID: "ask", ConversationID: "chat", Sender: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "ana"}}
	conversation := &models.Conversation{ID: "chat", Scope: source.Scope, Owner: source.Owner}
	return runtimeconversation.RunContextRespondTo(runtimeconversation.ConversationContextInput{Run: run, Conversation: conversation, Trigger: trigger})
}
