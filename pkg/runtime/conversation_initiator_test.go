package runtime

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestServiceMessageInitiatorSurvivesPersistenceAndReplay(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			ctx := t.Context()
			var store ConversationStore = NewMemoryStore()
			var disk *SQLiteStore
			path := filepath.Join(t.TempDir(), "initiator.db")
			if backend == "sqlite" {
				var err error
				disk, err = NewSQLiteStore(path)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = disk.Close() })
				store = disk
			}
			service := NewConversationService(store)
			scope := Scope{Kind: "tenant", ID: "initiator"}
			conversation, _, err := service.CreateConversation(ctx, CreateConversationRequest{
				Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"},
				Title: "Conversation", IdempotencyKey: "conversation",
			})
			if err != nil {
				t.Fatal(err)
			}
			initiator := &ConversationParticipant{Type: ConversationParticipantUser, ID: "42"}
			request := PostChannelMessageRequest{
				Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
				Sender:    ConversationParticipant{Type: ConversationParticipantService, ID: "service"},
				Initiator: initiator, Intent: MessageIntentUpdate, Content: "Service started.",
				Audience: ConversationAudience{Kind: ConversationAudienceChannel}, IdempotencyKey: "service-event",
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
				disk, err = NewSQLiteStore(path)
				if err != nil {
					t.Fatal(err)
				}
				service = NewConversationService(disk)
			}
			loaded, err := service.GetChannelMessage(ctx, scope, conversation.ID, posted.Message.ID)
			if err != nil || loaded.Initiator == nil || loaded.Initiator.Type != ConversationParticipantUser || loaded.Initiator.ID != "42" {
				t.Fatalf("canonical initiator changed or was lost after restart: %#v, %v", loaded, err)
			}
			loaded.Initiator.ID = "102"
			loaded, err = service.GetChannelMessage(ctx, scope, conversation.ID, posted.Message.ID)
			if err != nil || loaded.Initiator.ID != "42" {
				t.Fatalf("caller mutated persisted initiator: %#v, %v", loaded, err)
			}
			// Reusing an event key cannot move its sponsor to another user or
			// remove the verified identity from the canonical event.
			if _, err := service.PostChannelMessage(ctx, request); !errors.Is(err, ErrMessageConflict) {
				t.Fatalf("changed initiator replay = %v", err)
			}
			request.Initiator = nil
			if _, err := service.PostChannelMessage(ctx, request); !errors.Is(err, ErrMessageConflict) {
				t.Fatalf("removed initiator replay = %v", err)
			}
			request.Initiator = &ConversationParticipant{Type: ConversationParticipantUser, ID: "42"}
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
		sender    ConversationParticipantType
		initiator ConversationParticipant
	}{
		{name: "user message", sender: ConversationParticipantUser, initiator: ConversationParticipant{Type: ConversationParticipantUser, ID: "42"}},
		{name: "agent message", sender: ConversationParticipantAgent, initiator: ConversationParticipant{Type: ConversationParticipantUser, ID: "42"}},
		{name: "team message", sender: ConversationParticipantTeam, initiator: ConversationParticipant{Type: ConversationParticipantUser, ID: "42"}},
		{name: "service initiator", sender: ConversationParticipantService, initiator: ConversationParticipant{Type: ConversationParticipantService, ID: "42"}},
		{name: "agent initiator", sender: ConversationParticipantService, initiator: ConversationParticipant{Type: ConversationParticipantAgent, ID: "42"}},
		{name: "missing user identity", sender: ConversationParticipantService, initiator: ConversationParticipant{Type: ConversationParticipantUser}},
		{name: "invalid user identity", sender: ConversationParticipantService, initiator: ConversationParticipant{Type: ConversationParticipantUser, ID: "user/42"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := t.Context()
			service := NewConversationService(NewMemoryStore())
			scope := Scope{Kind: "tenant", ID: "initiator"}
			conversation, _, err := service.CreateConversation(ctx, CreateConversationRequest{
				Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"},
				Title: "Conversation", IdempotencyKey: "conversation",
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = service.PostChannelMessage(ctx, PostChannelMessageRequest{
				Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
				Sender: ConversationParticipant{Type: test.sender, ID: "sender"}, Initiator: &test.initiator,
				Intent: MessageIntentUpdate, Content: "An event", Audience: ConversationAudience{Kind: ConversationAudienceChannel},
				IdempotencyKey: "event",
			})
			if !errors.Is(err, ErrInvalidConversation) {
				t.Fatalf("accepted invalid initiator: %v", err)
			}
			messages, err := service.ListChannelMessages(ctx, ChannelMessageFilter{Scope: scope, ConversationID: conversation.ID})
			if err != nil || len(messages) != 0 {
				t.Fatalf("invalid initiator persisted: %#v, %v", messages, err)
			}
		})
	}
}
