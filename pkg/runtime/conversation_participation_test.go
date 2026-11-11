package runtime

import (
	"testing"
)

type participationTestStore interface {
	ConversationStore
	RunCommandStore
}

func TestParticipationArchiveRequiresFreshOptIn(t *testing.T) {
	store := NewMemoryStore()
	service := NewConversationService(store)
	scope := Scope{Kind: "local", ID: "default"}
	seedTestSpace(t, store, scope)
	channel, _, err := service.CreateConversation(t.Context(), CreateConversationRequest{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "research"}, Title: "Research", IdempotencyKey: "archive"})
	if err != nil {
		t.Fatal(err)
	}
	enabled := true
	channel, err = service.UpdateConversation(t.Context(), UpdateConversationRequest{Scope: scope, ConversationID: channel.ID, ExpectedRevision: channel.Revision, ParticipationEnabled: &enabled})
	if err != nil {
		t.Fatal(err)
	}
	trigger := postConversationRunTestMessage(t, service, channel, ConversationParticipantUser, MessageIntentQuestion, "Review evidence", "before-archive")
	for _, status := range []ConversationStatus{ConversationStatusArchived, ConversationStatusActive} {
		channel, err = service.GetConversation(t.Context(), scope, channel.ID)
		if err != nil {
			t.Fatal(err)
		}
		channel, err = service.UpdateConversation(t.Context(), UpdateConversationRequest{Scope: scope, ConversationID: channel.ID, ExpectedRevision: channel.Revision, Status: &status})
		if err != nil {
			t.Fatal(err)
		}
		if channel.Participation.Enabled {
			t.Fatal("archive/restore re-enabled participation")
		}
	}
	channel, err = service.UpdateConversation(t.Context(), UpdateConversationRequest{Scope: scope, ConversationID: channel.ID, ExpectedRevision: channel.Revision, ParticipationEnabled: &enabled})
	if err != nil {
		t.Fatal(err)
	}
	if conversationParticipationAllows(channel, trigger) {
		t.Fatal("restored channel woke pre-archive message")
	}
}
