package conversation_test

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/axiom-studio/openseal/pkg/models"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation/conversationtest"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
)

type countedConversationBackdropStore struct {
	*conversationtest.CountedPointStore
	lists   int
	filters []models.ChannelMessageFilter
}

func (s *countedConversationBackdropStore) ListChannelMessages(ctx context.Context, filter models.ChannelMessageFilter) ([]*models.ChannelMessage, error) {
	s.lists++
	s.filters = append(s.filters, filter)
	return s.ConversationStore.ListChannelMessages(ctx, filter)
}

func TestConversationThreadBackdropDoesNotSeekPastHiddenPage(t *testing.T) {
	for _, replyTo := range []bool{false, true} {
		t.Run(fmt.Sprintf("replyTo=%t", replyTo), func(t *testing.T) {
			store := kerneltest.NewStore(t)
			service := runtimeconversation.NewConversationService(store)
			conversation := conversationtest.ThreadStartConversation(t, store)
			older := conversationtest.ThreadStartPost(t, service, conversation, runtimeconversation.PostChannelMessageRequest{
				IdempotencyKey: "older-authorized", Content: "AUTHORIZED_OLDER_CONTEXT", Intent: models.MessageIntentUpdate,
			})
			parentID := ""
			if replyTo {
				parentID = older.ID
			}
			for index := 0; index < 24; index++ {
				conversationtest.ThreadStartPost(t, service, conversation, runtimeconversation.PostChannelMessageRequest{
					IdempotencyKey: fmt.Sprintf("hidden-%02d", index), Intent: models.MessageIntentUpdate,
					Content: "HIDDEN_CONTEXT_MUST_NOT_APPEAR", ReplyToMessageID: parentID,
					Audience: models.ConversationAudience{Kind: models.ConversationAudienceParticipants, Participants: []models.ConversationParticipant{{Type: models.ConversationParticipantUser, ID: "private-user"}}},
				})
			}
			root := conversationtest.ThreadStartPost(t, service, conversation, runtimeconversation.PostChannelMessageRequest{
				IdempotencyKey: "new-call-root", StartThread: true, ReplyToMessageID: parentID,
			})
			counted := &countedConversationBackdropStore{CountedPointStore: &conversationtest.CountedPointStore{ConversationStore: store}}
			viewer := models.ConversationViewer{Participant: models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: conversation.Owner.ID}}
			backdrop, err := runtimeconversation.LoadConversationThreadBackdrop(t.Context(), runtimeconversation.NewConversationService(counted), conversation, root, viewer)
			if err != nil || counted.lists != 1 || len(counted.filters) != 1 {
				t.Fatalf("backdrop sought past its first hidden page: lists=%d filters=%#v err=%v", counted.lists, counted.filters, err)
			}
			filter := counted.filters[0]
			if filter.Scope != conversation.Scope || filter.ConversationID != conversation.ID || filter.Limit != 16 || !filter.Descending || filter.BeforeSequence != root.Sequence || filter.ThreadRootID != parentID || filter.ChannelTimeline == replyTo {
				t.Fatalf("backdrop did not issue one bounded pre-start page: %#v", filter)
			}
			if replyTo {
				if len(backdrop) != 1 || backdrop[0].ID != older.ID || backdrop[0].Content != older.Content {
					t.Fatalf("exact authorized parent outside the page was lost: %#v", backdrop)
				}
			} else if len(backdrop) != 0 {
				t.Fatalf("backdrop sought older accessible history to fill a hidden page: %#v", backdrop)
			}
			for _, message := range backdrop {
				if strings.Contains(message.Content, "HIDDEN_CONTEXT_MUST_NOT_APPEAR") {
					t.Fatal("bounded raw page bypassed viewer authorization")
				}
			}
		})
	}
}

func conversationThreadBackdropFixture(t *testing.T, service *runtimeconversation.ConversationService, conversation *models.Conversation, replyTo bool) (*models.ChannelMessage, []*models.ChannelMessage) {
	t.Helper()
	otherRoot := conversationtest.ThreadStartPost(t, service, conversation, runtimeconversation.PostChannelMessageRequest{IdempotencyKey: "other-root", Intent: models.MessageIntentSystem})
	own := make([]*models.ChannelMessage, 0, 11)
	parent := ""
	for index := 1; index <= 11; index++ {
		key := fmt.Sprintf("own-context-%02d", index)
		content := "AUTHORIZED_" + key
		if index == 11 {
			content += strings.Repeat("日本語", 200)
		}
		message := conversationtest.ThreadStartPost(t, service, conversation, runtimeconversation.PostChannelMessageRequest{
			IdempotencyKey: key, Intent: models.MessageIntentUpdate, Content: content, ReplyToMessageID: parent,
		})
		own = append(own, message)
		if replyTo && parent == "" {
			parent = message.ID
		}
	}
	conversationtest.ThreadStartPost(t, service, conversation, runtimeconversation.PostChannelMessageRequest{
		IdempotencyKey: "inaccessible-private", Intent: models.MessageIntentUpdate, Content: "PRIVATE_CONTEXT_MUST_NOT_APPEAR",
		Audience: models.ConversationAudience{Kind: models.ConversationAudienceParticipants, Participants: []models.ConversationParticipant{{Type: models.ConversationParticipantUser, ID: "private-user"}}},
	})
	conversationtest.ThreadStartPost(t, service, conversation, runtimeconversation.PostChannelMessageRequest{
		IdempotencyKey: "other-thread-reply", ReplyToMessageID: otherRoot.ID, Content: "OTHER_THREAD_CONTEXT_MUST_NOT_APPEAR", Intent: models.MessageIntentUpdate,
	})
	rootReply := ""
	expected := own[len(own)-8:]
	if replyTo {
		// Pin an older exact parent while retaining recent context from its
		// thread. The parent must survive the eight-message excerpt bound.
		rootReply = own[0].ID
		expected = append([]*models.ChannelMessage{own[0]}, own[len(own)-7:]...)
	}
	root := conversationtest.ThreadStartPost(t, service, conversation, runtimeconversation.PostChannelMessageRequest{
		IdempotencyKey: "call-root", StartThread: true, ReplyToMessageID: rootReply, RequiresResponse: true, Content: "CALL_ROOT_ONLY",
	})
	return root, expected
}

func TestConversationThreadBackdropIsBoundedAuthorizedAndStable(t *testing.T) {
	for _, replyTo := range []bool{false, true} {
		t.Run(fmt.Sprintf("replyTo=%t", replyTo), func(t *testing.T) {
			conversationtest.ForThreadStartStores(t, func(t *testing.T, store conversationtest.ThreadStartStore, _ func(*testing.T) conversationtest.ThreadStartStore) {
				conversation := conversationtest.ThreadStartConversation(t, store)
				service := runtimeconversation.NewConversationService(store)
				root, expected := conversationThreadBackdropFixture(t, service, conversation, replyTo)
				viewer := models.ConversationViewer{Participant: models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: conversation.Owner.ID}}
				backdrop, err := runtimeconversation.LoadConversationThreadBackdrop(t.Context(), service, conversation, root, viewer)
				if err != nil || !reflect.DeepEqual(conversationtest.ThreadStartMessageIDs(backdrop), conversationtest.ThreadStartMessageIDs(expected)) {
					t.Fatalf("backdrop did not select its latest eight authorized originals: ids=%v expected=%v err=%v", conversationtest.ThreadStartMessageIDs(backdrop), conversationtest.ThreadStartMessageIDs(expected), err)
				}
				for _, message := range backdrop {
					if message.Sequence >= root.Sequence || len(message.Content) > 1024 || !utf8.ValidString(message.Content) {
						t.Fatalf("backdrop was not bounded pre-start UTF-8 context: %#v", message)
					}
				}
				longOriginal, err := service.GetChannelMessage(t.Context(), conversation.Scope, conversation.ID, expected[len(expected)-1].ID)
				if err != nil || len(longOriginal.Content) <= 1024 || !strings.HasPrefix(longOriginal.Content, backdrop[len(backdrop)-1].Content) {
					t.Fatalf("prompt excerpt rewrote the durable original: %#v, %v", longOriginal, err)
				}
				conversationtest.ThreadStartPost(t, service, conversation, runtimeconversation.PostChannelMessageRequest{IdempotencyKey: "later-main", Content: "LATER_MAIN_MUST_NOT_APPEAR", Intent: models.MessageIntentUpdate})
				utterance := conversationtest.ThreadStartPost(t, service, conversation, runtimeconversation.PostChannelMessageRequest{IdempotencyKey: "future-call-utterance", ReplyToMessageID: root.ID, RequiresResponse: true})
				later, err := runtimeconversation.LoadConversationThreadBackdrop(t.Context(), service, conversation, utterance, viewer)
				if err != nil || !reflect.DeepEqual(backdrop, later) {
					t.Fatalf("future call utterance moved the canonical pre-start cutoff: initial=%#v later=%#v err=%v", backdrop, later, err)
				}
				backdrop[0].Content = "changed returned excerpt"
				original, err := service.GetChannelMessage(t.Context(), conversation.Scope, conversation.ID, expected[0].ID)
				if err != nil || original.Content != expected[0].Content {
					t.Fatalf("returned backdrop shared mutable persistence state: %#v, %v", original, err)
				}
			})
		})
	}
}

func TestOrdinaryConversationHasNoThreadBackdropReads(t *testing.T) {
	store := kerneltest.NewStore(t)
	service := runtimeconversation.NewConversationService(store)
	conversation := conversationtest.ThreadStartConversation(t, store)
	root := conversationtest.ThreadStartPost(t, service, conversation, runtimeconversation.PostChannelMessageRequest{IdempotencyKey: "ordinary-root"})
	reply := conversationtest.ThreadStartPost(t, service, conversation, runtimeconversation.PostChannelMessageRequest{IdempotencyKey: "ordinary-reply", ReplyToMessageID: root.ID})
	counted := &countedConversationBackdropStore{CountedPointStore: &conversationtest.CountedPointStore{ConversationStore: store}}
	reader := runtimeconversation.NewConversationService(counted)
	viewer := models.ConversationViewer{Participant: models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: conversation.Owner.ID}}
	backdrop, err := runtimeconversation.LoadConversationThreadBackdrop(t.Context(), reader, conversation, root, viewer)
	if err != nil || len(backdrop) != 0 || counted.Points != 0 || counted.lists != 0 {
		t.Fatalf("ordinary root acquired prompt backdrop reads: backdrop=%#v points=%d lists=%d err=%v", backdrop, counted.Points, counted.lists, err)
	}
	// A canonical ordinary reply can require its root point read to determine
	// whether it belongs to a StartsThread root; it must never page old history.
	backdrop, err = runtimeconversation.LoadConversationThreadBackdrop(t.Context(), reader, conversation, reply, viewer)
	if err != nil || len(backdrop) != 0 || counted.lists != 0 || counted.Points > 1 {
		t.Fatalf("ordinary reply paged old conversation backdrop: backdrop=%#v points=%d lists=%d err=%v", backdrop, counted.Points, counted.lists, err)
	}
}
