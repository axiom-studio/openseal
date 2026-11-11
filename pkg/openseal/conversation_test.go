package openseal

import (
	"context"
	"testing"
	"time"
)

func TestPublicConversationFacadeKeepsTeamChannelFacts(t *testing.T) {
	t.Parallel()
	engine, err := New()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	scope := Scope{Kind: "local", ID: "channel"}
	home := seedSpace(t, engine, scope, "")
	conversation, replayed, err := engine.CreateConversation(ctx, CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "engineering"}, Title: "Release", IdempotencyKey: "release-v1",
	})
	if err != nil || replayed || conversation.SpaceID != home.ID {
		t.Fatalf("conversation = %#v, replayed = %v, err = %v", conversation, replayed, err)
	}
	question, err := engine.PostChannelMessage(ctx, PostChannelMessageRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "operator"}, Intent: MessageIntentQuestion,
		Content: "Is it ready?", Audience: ConversationAudience{Kind: ConversationAudienceChannel}, RequiresResponse: true,
		IdempotencyKey: "question-v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	agent := ConversationParticipant{Type: ConversationParticipantAgent, ID: "developer"}
	cursor, _, err := engine.AdvanceConversationCursor(ctx, AdvanceConversationCursorRequest{
		Scope: scope, ConversationID: conversation.ID, Participant: agent,
		DeliveredSequence: question.Conversation.LastSequence, ReadSequence: question.Conversation.LastSequence,
	})
	if err != nil || cursor.ReadSequence != 1 {
		t.Fatalf("cursor = %#v, err = %v", cursor, err)
	}
	presence, err := engine.SetConversationPresence(ctx, SetConversationPresenceRequest{
		Scope: scope, ConversationID: conversation.ID, Participant: agent, State: ConversationPresenceWorking,
		Summary: "Following rollout", RunID: "release-run", TTL: 30 * time.Second,
	})
	if err != nil || presence.LeaseID == "" {
		t.Fatalf("presence = %#v, err = %v", presence, err)
	}
}
