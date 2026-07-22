package runtime

import (
	"errors"
	"testing"
	"time"
)

func TestConversationFactsValidateStructuredChannelState(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	scope := Scope{Kind: "tenant", ID: "one"}
	conversation := &Conversation{
		ID: "release-channel", Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "product"},
		Title: "Release coordination", Status: ConversationStatusActive, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := conversation.Validate(); err != nil {
		t.Fatal(err)
	}
	message := &ChannelMessage{
		ID: "message-1", Scope: scope, ConversationID: conversation.ID, Sequence: 1,
		Sender: ConversationParticipant{Type: ConversationParticipantAgent, ID: "developer"},
		Intent: MessageIntentQuestion, Content: "@reviewer, is the launch evidence sufficient?",
		Audience: ConversationAudience{Kind: ConversationAudienceParticipants, Participants: []ConversationParticipant{{Type: ConversationParticipantAgent, ID: "reviewer"}}},
		Mentions: []ConversationParticipant{{Type: ConversationParticipantAgent, ID: "reviewer"}},
		References: []ConversationReference{
			{Kind: ConversationReferenceRun, ID: "run-42"},
			{Kind: ConversationReferenceInitiative, ID: "initiative-launch", Version: 2},
			{Kind: ConversationReferenceArtifact, ID: "launch-evidence", Version: 3},
		},
		RequiresResponse: true, IdempotencyKey: "release-question-v1", CreatedAt: now,
	}
	if err := message.Validate(); err != nil {
		t.Fatal(err)
	}
	cursor := &ConversationCursor{
		Scope: scope, ConversationID: conversation.ID, Participant: ConversationParticipant{Type: ConversationParticipantUser, ID: "operator"},
		DeliveredSequence: 1, ReadSequence: 1, Revision: 1, UpdatedAt: now,
	}
	if err := cursor.Validate(); err != nil {
		t.Fatal(err)
	}
	presence := &ConversationPresence{
		Scope: scope, ConversationID: conversation.ID, Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "reviewer"},
		State: ConversationPresenceWorking, Summary: "Reviewing launch evidence", RunID: "run-43", LeaseID: "presence-lease-1",
		Revision: 1, UpdatedAt: now, ExpiresAt: now.Add(30 * time.Second),
	}
	if err := presence.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestConversationFactsRejectInventedOrUntruthfulState(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	scope := Scope{Kind: "tenant", ID: "one"}
	invalidCursor := &ConversationCursor{
		Scope: scope, ConversationID: "release", Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "reviewer"},
		DeliveredSequence: 2, ReadSequence: 3, Revision: 1, UpdatedAt: now,
	}
	if err := invalidCursor.Validate(); !errors.Is(err, ErrInvalidConversation) {
		t.Fatalf("cursor error = %v", err)
	}
	expiredPresence := &ConversationPresence{
		Scope: scope, ConversationID: "release", Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "reviewer"},
		State: ConversationPresenceTyping, LeaseID: "lease", Revision: 1, UpdatedAt: now, ExpiresAt: now,
	}
	if err := expiredPresence.Validate(); !errors.Is(err, ErrInvalidConversation) {
		t.Fatalf("presence error = %v", err)
	}
	invalidMessage := &ChannelMessage{
		ID: "message", Scope: scope, ConversationID: "release", Sequence: 1,
		Sender: ConversationParticipant{Type: ConversationParticipantAgent, ID: "reviewer"}, Intent: MessageIntentUpdate,
		Content: "Update", Audience: ConversationAudience{Kind: ConversationAudienceChannel, Participants: []ConversationParticipant{{Type: ConversationParticipantAgent, ID: "other"}}}, CreatedAt: now,
	}
	if err := invalidMessage.Validate(); !errors.Is(err, ErrInvalidConversation) {
		t.Fatalf("message error = %v", err)
	}
}
