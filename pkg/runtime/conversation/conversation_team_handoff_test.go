package conversation

import (
	"context"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
)

type handoffMessages map[string]*models.ChannelMessage

func (m handoffMessages) GetChannelMessage(_ context.Context, _ models.Scope, _ string, id string) (*models.ChannelMessage, error) {
	return m[id], nil
}

// A teammate's hand-off can answer a service message a person started, such
// as the "connection completed" update after they signed in: the hand-off
// acts for that person, as a voice call's start does.
func TestResolveTeamHandoffRequestFollowsTheServiceMessageInitiator(t *testing.T) {
	scope := models.Scope{Kind: "tenant", ID: "2"}
	const conversationID = "conversation-1"
	key := "agent-channel-response:sha256:cove"
	handoffID := models.StableConversationID(scope, key, "message")
	run := &models.AgentRun{
		ID: "miso-run", Scope: scope, Kind: models.RunKindConversation, AssignedAgentID: "miso",
		Owner:   models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "team-1"},
		Context: map[string]interface{}{models.ConversationRunConversationIDContextKey: conversationID, models.ConversationRunTriggerIDContextKey: handoffID},
	}
	person := models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "5"}
	signedIn := &models.ChannelMessage{
		ID: "connected", Scope: scope, ConversationID: conversationID, Sequence: 8,
		Sender:    models.ConversationParticipant{Type: models.ConversationParticipantService, ID: "skill-setup-coordinator"},
		Initiator: &person, Intent: models.MessageIntentUpdate,
	}
	handoff := &models.ChannelMessage{
		ID: handoffID, Scope: scope, ConversationID: conversationID, Sequence: 9, IdempotencyKey: key,
		Sender: models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: "cove"},
		Intent: models.MessageIntentAnswer, ReplyToMessageID: signedIn.ID,
		Mentions: []models.ConversationParticipant{{Type: models.ConversationParticipantAgent, ID: "miso"}},
	}

	request, err := ResolveTeamHandoffRequest(context.Background(), handoffMessages{signedIn.ID: signedIn}, run, handoff)
	if err != nil || request == nil || request.ID != signedIn.ID || models.MessageActor(request) != person {
		t.Fatalf("hand-off to a sign-in update = %#v, %v; want it to act for user 5", request, err)
	}

	// A service message no person started still has no one behind it.
	signedIn.Initiator = nil
	if request, err := ResolveTeamHandoffRequest(context.Background(), handoffMessages{signedIn.ID: signedIn}, run, handoff); err == nil || request != nil {
		t.Fatalf("hand-off to a service message without an initiator = %#v, %v; want it refused", request, err)
	}
}
