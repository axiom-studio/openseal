package runtime

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestConversationHistoryReadsVisibleOriginals(t *testing.T) {
	service := NewConversationService(NewMemoryStore())
	scope := Scope{Kind: "tenant", ID: "one"}
	owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}
	viewer := ConversationViewer{Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: owner.ID}}
	conversation, _, err := service.CreateConversation(t.Context(), CreateConversationRequest{Scope: scope, Owner: owner, Title: "history", IdempotencyKey: "history"})
	if err != nil {
		t.Fatal(err)
	}
	var longID, privateID string
	original := strings.Repeat("a", 4095) + strings.Repeat("日本語", 1000)
	for i := 0; i < 4; i++ {
		content := fmt.Sprintf("message %d", i)
		audience := ConversationAudience{Kind: ConversationAudienceChannel}
		if i == 1 {
			content = original
		}
		if i == 2 {
			audience = ConversationAudience{Kind: ConversationAudienceParticipants, Participants: []ConversationParticipant{{Type: ConversationParticipantUser, ID: "private-user"}}}
		}
		result, err := service.PostChannelMessage(t.Context(), PostChannelMessageRequest{Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision, Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "sender"}, Intent: MessageIntentUpdate, Content: content, Audience: audience, IdempotencyKey: fmt.Sprint(i)})
		if err != nil {
			t.Fatal(err)
		}
		if i == 1 {
			longID = result.Message.ID
		}
		if i == 2 {
			privateID = result.Message.ID
		}
		conversation, err = service.GetConversation(t.Context(), scope, conversation.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	page, err := ReadConversationHistory(t.Context(), service, scope, owner, conversation.ID, viewer, ConversationHistoryReadRequest{Limit: 2})
	if err != nil || len(page.Messages) != 2 || page.Messages[1].ID != longID || page.NextBeforeSequence != 2 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	older, err := ReadConversationHistory(t.Context(), service, scope, owner, conversation.ID, viewer, ConversationHistoryReadRequest{BeforeSequence: page.NextBeforeSequence, Limit: 2})
	if err != nil || len(older.Messages) != 1 || older.Messages[0].Sequence != 1 {
		t.Fatalf("older=%+v err=%v", older, err)
	}
	var reconstructed strings.Builder
	for offset := 0; ; {
		page, err = ReadConversationHistory(t.Context(), service, scope, owner, conversation.ID, viewer, ConversationHistoryReadRequest{MessageID: longID, OffsetBytes: offset})
		if err != nil {
			t.Fatal(err)
		}
		excerpt := page.Messages[0]
		if !utf8.ValidString(excerpt.Content) || len(excerpt.Content) > 4096 {
			t.Fatal("invalid bounded UTF-8 excerpt")
		}
		reconstructed.WriteString(excerpt.Content)
		if excerpt.NextOffsetBytes == nil {
			break
		}
		offset = *excerpt.NextOffsetBytes
	}
	if reconstructed.String() != original {
		t.Fatal("could not recover exact original")
	}
	for _, request := range []ConversationHistoryReadRequest{{MessageID: privateID}, {MessageID: longID, OffsetBytes: 4096}, {OffsetBytes: 1}, {Limit: 11}} {
		if _, err := ReadConversationHistory(t.Context(), service, scope, owner, conversation.ID, viewer, request); err == nil {
			t.Fatalf("invalid/private read accepted: %+v", request)
		}
	}
	owner.ID = "other"
	if _, err := ReadConversationHistory(t.Context(), service, scope, owner, conversation.ID, viewer, ConversationHistoryReadRequest{}); err == nil {
		t.Fatal("foreign owner accepted")
	}
	scope.ID = "other"
	if _, err := ReadConversationHistory(t.Context(), service, scope, conversation.Owner, conversation.ID, viewer, ConversationHistoryReadRequest{}); err == nil {
		t.Fatal("foreign tenant accepted")
	}
}
