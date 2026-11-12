package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

type recordingReactionProjector struct {
	events []*models.ConversationReactionEvent
}

func (p *recordingReactionProjector) ProjectMessageReaction(_ context.Context, event *models.ConversationReactionEvent) error {
	p.events = append(p.events, event)
	return nil
}

func TestConversationReactionsAcrossStores(t *testing.T) {
	t.Parallel()
	stores := map[string]func(t *testing.T) models.ConversationStore{
		"sqlite": func(t *testing.T) models.ConversationStore {
			store, err := sqlite.NewSQLiteStore(filepath.Join(t.TempDir(), "reactions.db"))
			if err != nil {
				t.Fatal(err)
			}
			return store
		},
	}
	for name, open := range stores {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			testConversationReactions(t, open(t))
		})
	}
}

func testConversationReactions(t *testing.T, store models.ConversationStore) {
	ctx := context.Background()
	fixed := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	tick := 0
	service := NewConversationService(store, models.WithClock(func() time.Time { tick++; return fixed.Add(time.Duration(tick) * time.Second) }))
	projector := &recordingReactionProjector{}
	service.SetReactionProjector(projector)
	scope := models.Scope{Kind: "tenant", ID: "one"}
	seedTestSpace(t, store.(models.SpaceStore), scope)
	conversation, _, err := service.CreateConversation(ctx, CreateConversationRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "launch"}, Title: "Launch", IdempotencyKey: "launch",
	})
	if err != nil {
		t.Fatal(err)
	}
	user := models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "maya"}
	rio := models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: "rio"}
	posted, err := service.PostChannelMessage(ctx, PostChannelMessageRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision, Sender: user,
		Intent: models.MessageIntentQuestion, Content: "Ship it?", Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel},
		IdempotencyKey: "ship",
	})
	if err != nil {
		t.Fatal(err)
	}
	private, err := service.PostChannelMessage(ctx, PostChannelMessageRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: posted.Conversation.Revision, Sender: user,
		Intent: models.MessageIntentQuestion, Content: "Only for me",
		Audience:       models.ConversationAudience{Kind: models.ConversationAudienceParticipants, Participants: []models.ConversationParticipant{user}},
		IdempotencyKey: "private",
	})
	if err != nil {
		t.Fatal(err)
	}
	message := posted.Message
	react := func(add bool, actor models.ConversationParticipant, messageID, emoji string) (*MessageReactionResult, error) {
		request := MessageReactionRequest{Scope: scope, ConversationID: conversation.ID, MessageID: messageID, Emoji: emoji, Actor: actor}
		if add {
			return service.AddMessageReaction(ctx, request)
		}
		return service.RemoveMessageReaction(ctx, request)
	}

	first, err := react(true, rio, message.ID, "👍")
	if err != nil || !first.Changed || first.Event == nil || first.Event.Type != models.ConversationReactionAdded || first.Event.Sequence != 1 {
		t.Fatalf("first add = %+v, %v", first, err)
	}
	again, err := react(true, rio, message.ID, "👍")
	if err != nil || again.Changed || again.Event != nil || len(again.Reactions) != 1 || again.Message == nil || len(again.Message.Reactions) != 1 {
		t.Fatalf("repeated add = %+v, %v", again, err)
	}
	if _, err := react(true, user, message.ID, "👍"); err != nil {
		t.Fatal(err)
	}
	if _, err := react(true, user, message.ID, "✅"); err != nil {
		t.Fatal(err)
	}
	read, err := service.GetChannelMessage(ctx, scope, conversation.ID, message.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(read.Reactions) != 2 || read.Reactions[0].Emoji != "👍" || len(read.Reactions[0].Actors) != 2 ||
		read.Reactions[0].Actors[0] != rio || read.Reactions[1].Emoji != "✅" {
		t.Fatalf("reactions on read = %+v", read.Reactions)
	}
	listed, err := service.ListChannelMessages(ctx, models.ChannelMessageFilter{Scope: scope, ConversationID: conversation.ID, Viewer: &models.ConversationViewer{Participant: user}})
	if err != nil || len(listed) != 2 || len(listed[0].Reactions) != 2 || len(listed[1].Reactions) != 0 {
		t.Fatalf("listed = %+v, %v", listed, err)
	}
	if lines := reactionPromptLines(listed); lines[message.ID] != "👍 rio, 👍 a user, ✅ a user" {
		t.Fatalf("prompt line = %q", lines[message.ID])
	}

	removed, err := react(false, user, message.ID, "✅")
	if err != nil || !removed.Changed || removed.Event.Type != models.ConversationReactionRemoved || len(removed.Reactions) != 1 {
		t.Fatalf("remove = %+v, %v", removed, err)
	}
	absent, err := react(false, user, message.ID, "✅")
	if err != nil || absent.Changed {
		t.Fatalf("absent remove = %+v, %v", absent, err)
	}

	for _, emoji := range []string{"", "ok", "👍 ", "👍\n", "😀😀😀😀😀"} {
		if _, err := react(true, user, message.ID, emoji); !errors.Is(err, models.ErrInvalidConversation) {
			t.Fatalf("emoji %q: %v", emoji, err)
		}
	}
	for _, emoji := range []string{"1️⃣", "👩‍💻", "❤️"} {
		if _, err := react(true, user, message.ID, emoji); err != nil {
			t.Fatalf("emoji %q: %v", emoji, err)
		}
	}
	if _, err := react(true, rio, private.Message.ID, "👀"); !errors.Is(err, models.ErrChannelMessageNotFound) {
		t.Fatalf("hidden message reaction: %v", err)
	}
	if _, err := react(true, rio, "missing", "👀"); !errors.Is(err, models.ErrChannelMessageNotFound) {
		t.Fatalf("missing message reaction: %v", err)
	}

	stored, err := store.GetChannelMessage(ctx, scope, conversation.ID, message.ID)
	if err != nil || stored.Reactions != nil {
		t.Fatalf("stored message carries reactions: %+v, %v", stored, err)
	}
	if len(projector.events) != 7 {
		t.Fatalf("projected %d events, want one per change", len(projector.events))
	}
	if _, err := service.AddMessageReaction(ctx, MessageReactionRequest{
		Scope: scope, ConversationID: conversation.ID, MessageID: message.ID, Emoji: "🎉", Actor: user, FromExternal: true,
	}); err != nil || len(projector.events) != 7 {
		t.Fatalf("external reaction projected back out: %d, %v", len(projector.events), err)
	}

	changes, err := NewConversationChangeService(store, newTestKernelStore(t))
	if err != nil {
		t.Fatal(err)
	}
	page, err := changes.ListChanges(ctx, ConversationChangeRequest{Scope: scope, ConversationID: conversation.ID, Limit: 3, Viewer: &models.ConversationViewer{Participant: rio}})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.ReactionEvents) != 3 || !page.HasMore || page.ReactionEvents[0].Sequence != 1 ||
		len(page.ReactionEvents[0].Reactions) != 5 || len(page.Messages) != 1 || len(page.Messages[0].Reactions) != 5 {
		t.Fatalf("first change page = %d events, more %v, %+v", len(page.ReactionEvents), page.HasMore, page.Messages)
	}
	next, err := changes.ListChanges(ctx, ConversationChangeRequest{Scope: scope, ConversationID: conversation.ID, Cursor: page.Cursor, Limit: 100, Viewer: &models.ConversationViewer{Participant: rio}})
	if err != nil {
		t.Fatal(err)
	}
	if len(next.ReactionEvents) != 5 || next.ReactionEvents[0].Sequence != 4 || next.ReactionEvents[4].Emoji != "🎉" || !next.HasChanges {
		t.Fatalf("next change page = %+v", next.ReactionEvents)
	}
	idle, err := changes.ListChanges(ctx, ConversationChangeRequest{Scope: scope, ConversationID: conversation.ID, Cursor: next.Cursor, Viewer: &models.ConversationViewer{Participant: rio}})
	if err != nil || len(idle.ReactionEvents) != 0 {
		t.Fatalf("idle page = %+v, %v", idle, err)
	}
}

func TestAdmitTurnReactions(t *testing.T) {
	got := admitTurnReactions([]TurnReaction{
		{MessageID: "m1", Emoji: "👍"}, {MessageID: "m1", Emoji: "👍"}, {MessageID: "", Emoji: "👍"},
		{MessageID: "m2", Emoji: "nope"}, {MessageID: "m2", Emoji: "✅"}, {MessageID: "m3", Emoji: "👀"}, {MessageID: "m4", Emoji: "🎉"},
	})
	if len(got) != MaximumTurnReactions || got[0].MessageID != "m1" || got[1].Emoji != "✅" || got[2].MessageID != "m3" {
		t.Fatalf("admitted = %+v", got)
	}
}

func TestReactionEchoIsNotAnAnswer(t *testing.T) {
	thumbs := []TurnReaction{{MessageID: "m", Emoji: "👍"}}
	for content, want := range map[string]bool{"": true, "👍": true, " 👍\n": true, "👍️": true, "👍 Done.": false, "Sure": false} {
		if got := reactionEcho(content, thumbs); got != want {
			t.Fatalf("reactionEcho(%q) = %v", content, got)
		}
	}
	if reactionEcho("👍", nil) {
		t.Fatal("an emoji with no reaction is an answer")
	}
}
