package openseal

import (
	"context"
	"testing"
	"time"
)

func TestPublicConversationFacadeCoordinatesNaturalTeamChannel(t *testing.T) {
	t.Parallel()
	engine, err := New()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	scope := Scope{Kind: "local", ID: "channel"}
	conversation, replayed, err := engine.CreateConversation(ctx, CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "engineering"}, Title: "Release", IdempotencyKey: "release-v1",
	})
	if err != nil || replayed {
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
	round, err := engine.CoordinateParticipation(ctx, CoordinateParticipationRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: question.Conversation.Revision,
		TriggerMessageID: question.Message.ID, IdempotencyKey: "round-v1",
		Proposals: []ParticipationProposal{
			{
				Participant: agent, WantsToSpeak: true, Intent: MessageIntentAnswer,
				Content: "It is ready and the evidence passed.", Audience: ConversationAudience{Kind: ConversationAudienceChannel},
				ReplyToMessageID: question.Message.ID, ResolvesMessageID: question.Message.ID,
				Signals: ParticipationSignals{AnswersOpenQuestion: true, HasNewInformation: true, RoleRelevant: true, HasEvidence: true},
			},
			{Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "quiet"}, WantsToSpeak: false},
		},
	})
	if err != nil || len(round.Messages) != 1 || len(round.Round.Arbitration.Decisions) != 2 {
		t.Fatalf("round = %#v, err = %v", round, err)
	}
	loaded, err := engine.GetParticipationRound(ctx, scope, conversation.ID, round.Round.ID)
	if err != nil || len(loaded.Messages) != 1 {
		t.Fatalf("loaded round = %#v, err = %v", loaded, err)
	}
	cursor, _, err := engine.AdvanceConversationCursor(ctx, AdvanceConversationCursorRequest{
		Scope: scope, ConversationID: conversation.ID, Participant: agent,
		DeliveredSequence: round.Conversation.LastSequence, ReadSequence: round.Conversation.LastSequence,
	})
	if err != nil || cursor.ReadSequence != 2 {
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
