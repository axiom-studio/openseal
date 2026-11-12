package runtime

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

func TestChannelTimelineStoresPaginateRootsAndBroadcastsBeforeLimiting(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		store, err := sqlite.NewSQLiteStore(filepath.Join(t.TempDir(), "channel-timeline.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		testChannelTimelineStore(t, store)
	})
}

// A busy thread must not consume a channel page. Broadcasts still require
// visibility of their own audience, root and immediate reply target.
func testChannelTimelineStore(t *testing.T, store models.ConversationStore) {
	t.Helper()
	service := runtimeconversation.NewConversationService(store)
	scope := models.Scope{Kind: "tenant", ID: "channel-timeline"}
	seedTestSpace(t, store.(models.SpaceStore), scope)
	conversation, _, err := service.CreateConversation(t.Context(), runtimeconversation.CreateConversationRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "team"},
		Title: "Channel timeline", IdempotencyKey: "timeline",
	})
	if err != nil {
		t.Fatal(err)
	}
	viewer := models.ConversationViewer{Participant: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "viewer"}}
	sender := models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: "agent"}
	channelAudience := models.ConversationAudience{Kind: models.ConversationAudienceChannel}
	privateAudience := models.ConversationAudience{Kind: models.ConversationAudienceParticipants, Participants: []models.ConversationParticipant{{Type: models.ConversationParticipantUser, ID: "someone-else"}}}
	post := func(content, reply string, broadcast bool, audience models.ConversationAudience) *models.ChannelMessage {
		t.Helper()
		result, err := service.PostChannelMessage(t.Context(), runtimeconversation.PostChannelMessageRequest{
			Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
			Sender: sender, Intent: models.MessageIntentUpdate, Content: content,
			ReplyToMessageID: reply, BroadcastToChannel: broadcast, Audience: audience,
			IdempotencyKey: fmt.Sprintf("timeline-%d", conversation.Revision),
		})
		if err != nil {
			t.Fatal(err)
		}
		conversation = result.Conversation
		return result.Message
	}
	root := post("Older channel root", "", false, channelAudience)
	broadcast := post("Visible reply also sent to channel", root.ID, true, channelAudience)
	for i := 0; i < 60; i++ {
		post(fmt.Sprintf("Thread-only reply %d", i), root.ID, false, channelAudience)
	}
	newRoot := post("Newer channel root", "", false, channelAudience)
	privateRoot := post("Private root", "", false, privateAudience)
	post("Channel-looking broadcast with a private root", privateRoot.ID, true, channelAudience)
	post("Private broadcast with a visible root", root.ID, true, privateAudience)
	privateReply := post("Private reply target", root.ID, false, privateAudience)
	post("Channel-looking broadcast with a private reply target", privateReply.ID, true, channelAudience)

	filter := models.ChannelMessageFilter{Scope: scope, ConversationID: conversation.ID, ChannelTimeline: true, Descending: true, Limit: 2, Viewer: &viewer}
	assertMessages := func(got []*models.ChannelMessage, err error, want ...*models.ChannelMessage) {
		t.Helper()
		if err != nil || len(got) != len(want) {
			t.Fatalf("messages count = %d, want %d, err = %v", len(got), len(want), err)
		}
		for i := range want {
			if got[i].ID != want[i].ID {
				t.Fatalf("message[%d] = %q (%q), want %q", i, got[i].ID, got[i].Content, want[i].ID)
			}
		}
	}
	page, err := service.ListChannelMessages(t.Context(), filter)
	assertMessages(page, err, newRoot, broadcast)
	filter.BeforeSequence = page[len(page)-1].Sequence
	page, err = service.ListChannelMessages(t.Context(), filter)
	assertMessages(page, err, root)
	filter.BeforeSequence, filter.AfterSequence, filter.Descending = 0, root.Sequence, false
	page, err = service.ListChannelMessages(t.Context(), filter)
	assertMessages(page, err, broadcast, newRoot)

	// Without the opt-in, existing chronological conversation reads stay intact.
	all, err := service.ListChannelMessages(t.Context(), models.ChannelMessageFilter{Scope: scope, ConversationID: conversation.ID, Limit: 100})
	if err != nil || len(all) != int(conversation.LastSequence) {
		t.Fatalf("ordinary history count = %d, last sequence = %d, err = %v", len(all), conversation.LastSequence, err)
	}
	// Thread reads keep the root plus replies, including the broadcast.
	thread, err := service.ListChannelMessages(t.Context(), models.ChannelMessageFilter{Scope: scope, ConversationID: conversation.ID, ThreadRootID: root.ID, Limit: 2})
	assertMessages(thread, err, root, broadcast)
	filter.ThreadRootID = root.ID
	if _, err := service.ListChannelMessages(t.Context(), filter); !errors.Is(err, models.ErrInvalidConversation) {
		t.Fatalf("combined service filter error = %v", err)
	}
	if _, err := store.ListChannelMessages(t.Context(), filter); !errors.Is(err, models.ErrInvalidConversation) {
		t.Fatalf("combined store filter error = %v", err)
	}
	filter.ThreadRootID, filter.Scope.ID = "", "foreign-tenant"
	if _, err := service.ListChannelMessages(t.Context(), filter); !errors.Is(err, models.ErrConversationNotFound) {
		t.Fatalf("foreign tenant filter error = %v", err)
	}
}
