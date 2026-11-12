package conversationrun

import (
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation/conversationtest"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
)

type participationTestStore interface {
	models.ConversationStore
	models.RunCommandStore
}

func TestParticipationArchiveRequiresFreshOptIn(t *testing.T) {
	store := kerneltest.NewStore(t)
	service := conversation.NewService(store)
	scope := models.Scope{Kind: "local", ID: "default"}
	conversationtest.SeedSpace(t, store, scope)
	channel, _, err := service.CreateConversation(t.Context(), conversation.CreateConversationRequest{Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "research"}, Title: "Research", IdempotencyKey: "archive"})
	if err != nil {
		t.Fatal(err)
	}
	enabled := true
	channel, err = service.UpdateConversation(t.Context(), conversation.UpdateConversationRequest{Scope: scope, ConversationID: channel.ID, ExpectedRevision: channel.Revision, ParticipationEnabled: &enabled})
	if err != nil {
		t.Fatal(err)
	}
	trigger := postTestMessage(t, service, channel, models.ConversationParticipantUser, models.MessageIntentQuestion, "Review evidence", "before-archive")
	for _, status := range []models.ConversationStatus{models.ConversationStatusArchived, models.ConversationStatusActive} {
		channel, err = service.GetConversation(t.Context(), scope, channel.ID)
		if err != nil {
			t.Fatal(err)
		}
		channel, err = service.UpdateConversation(t.Context(), conversation.UpdateConversationRequest{Scope: scope, ConversationID: channel.ID, ExpectedRevision: channel.Revision, Status: &status})
		if err != nil {
			t.Fatal(err)
		}
		if channel.Participation.Enabled {
			t.Fatal("archive/restore re-enabled participation")
		}
	}
	channel, err = service.UpdateConversation(t.Context(), conversation.UpdateConversationRequest{Scope: scope, ConversationID: channel.ID, ExpectedRevision: channel.Revision, ParticipationEnabled: &enabled})
	if err != nil {
		t.Fatal(err)
	}
	if conversationParticipationAllows(channel, trigger) {
		t.Fatal("restored channel woke pre-archive message")
	}
}
