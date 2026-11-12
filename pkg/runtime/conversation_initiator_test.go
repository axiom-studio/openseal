package runtime

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

func TestServiceMessageInitiatorSurvivesPersistenceAndReplay(t *testing.T) {
	for _, backend := range []string{"sqlite"} {
		t.Run(backend, func(t *testing.T) {
			ctx := t.Context()
			var store models.ConversationStore = kerneltest.NewStore(t)
			var disk *sqlite.Store
			path := filepath.Join(t.TempDir(), "initiator.db")
			if backend == "sqlite" {
				var err error
				disk, err = sqlite.New(path)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = disk.Close() })
				store = disk
			}
			service := runtimeconversation.NewConversationService(store)
			scope := models.Scope{Kind: "tenant", ID: "initiator"}
			seedTestSpace(t, store.(models.SpaceStore), scope)
			conversation, _, err := service.CreateConversation(ctx, runtimeconversation.CreateConversationRequest{
				Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"},
				Title: "Conversation", IdempotencyKey: "conversation",
			})
			if err != nil {
				t.Fatal(err)
			}
			initiator := &models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "42"}
			request := runtimeconversation.PostChannelMessageRequest{
				Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
				Sender:    models.ConversationParticipant{Type: models.ConversationParticipantService, ID: "service"},
				Initiator: initiator, Intent: models.MessageIntentUpdate, Content: "Service started.",
				Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel}, IdempotencyKey: "service-event",
			}
			posted, err := service.PostChannelMessage(ctx, request)
			if err != nil || posted.Message.Initiator == nil || *posted.Message.Initiator != *initiator {
				t.Fatalf("service event initiator = %#v, %v", posted, err)
			}
			initiator.ID = "99"
			posted.Message.Initiator.ID = "101"
			if backend == "sqlite" {
				if err := disk.Close(); err != nil {
					t.Fatal(err)
				}
				disk, err = sqlite.New(path)
				if err != nil {
					t.Fatal(err)
				}
				service = runtimeconversation.NewConversationService(disk)
			}
			loaded, err := service.GetChannelMessage(ctx, scope, conversation.ID, posted.Message.ID)
			if err != nil || loaded.Initiator == nil || loaded.Initiator.Type != models.ConversationParticipantUser || loaded.Initiator.ID != "42" {
				t.Fatalf("canonical initiator changed or was lost after restart: %#v, %v", loaded, err)
			}
			loaded.Initiator.ID = "102"
			loaded, err = service.GetChannelMessage(ctx, scope, conversation.ID, posted.Message.ID)
			if err != nil || loaded.Initiator.ID != "42" {
				t.Fatalf("caller mutated persisted initiator: %#v, %v", loaded, err)
			}
			// Reusing an event key cannot move its sponsor to another user or
			// remove the verified identity from the canonical event.
			if _, err := service.PostChannelMessage(ctx, request); !errors.Is(err, models.ErrMessageConflict) {
				t.Fatalf("changed initiator replay = %v", err)
			}
			request.Initiator = nil
			if _, err := service.PostChannelMessage(ctx, request); !errors.Is(err, models.ErrMessageConflict) {
				t.Fatalf("removed initiator replay = %v", err)
			}
			request.Initiator = &models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "42"}
			replayed, err := service.PostChannelMessage(ctx, request)
			if err != nil || !replayed.Replayed || replayed.Message.ID != posted.Message.ID || replayed.Message.Initiator.ID != "42" {
				t.Fatalf("same verified initiator replay = %#v, %v", replayed, err)
			}
		})
	}
}

func TestChannelMessagesRejectUntrustedInitiator(t *testing.T) {
	for _, test := range []struct {
		name      string
		sender    models.ConversationParticipantType
		initiator models.ConversationParticipant
	}{
		{name: "user message", sender: models.ConversationParticipantUser, initiator: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "42"}},
		{name: "agent message", sender: models.ConversationParticipantAgent, initiator: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "42"}},
		{name: "team message", sender: models.ConversationParticipantTeam, initiator: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "42"}},
		{name: "service initiator", sender: models.ConversationParticipantService, initiator: models.ConversationParticipant{Type: models.ConversationParticipantService, ID: "42"}},
		{name: "agent initiator", sender: models.ConversationParticipantService, initiator: models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: "42"}},
		{name: "missing user identity", sender: models.ConversationParticipantService, initiator: models.ConversationParticipant{Type: models.ConversationParticipantUser}},
		{name: "invalid user identity", sender: models.ConversationParticipantService, initiator: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "user/42"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := t.Context()
			store := kerneltest.NewStore(t)
			service := runtimeconversation.NewConversationService(store)
			scope := models.Scope{Kind: "tenant", ID: "initiator"}
			seedTestSpace(t, store, scope)
			conversation, _, err := service.CreateConversation(ctx, runtimeconversation.CreateConversationRequest{
				Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"},
				Title: "Conversation", IdempotencyKey: "conversation",
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = service.PostChannelMessage(ctx, runtimeconversation.PostChannelMessageRequest{
				Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
				Sender: models.ConversationParticipant{Type: test.sender, ID: "sender"}, Initiator: &test.initiator,
				Intent: models.MessageIntentUpdate, Content: "An event", Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel},
				IdempotencyKey: "event",
			})
			if !errors.Is(err, models.ErrInvalidConversation) {
				t.Fatalf("accepted invalid initiator: %v", err)
			}
			messages, err := service.ListChannelMessages(ctx, models.ChannelMessageFilter{Scope: scope, ConversationID: conversation.ID})
			if err != nil || len(messages) != 0 {
				t.Fatalf("invalid initiator persisted: %#v, %v", messages, err)
			}
		})
	}
}
