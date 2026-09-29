package runtime

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestConversationHistoryForwardPagesPreserveCorrectionsAndVisibility(t *testing.T) {
	service := NewConversationService(NewMemoryStore())
	scope := Scope{Kind: "tenant", ID: "history-forward"}
	owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}
	viewer := ConversationViewer{Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: owner.ID}}
	conversation, _, err := service.CreateConversation(t.Context(), CreateConversationRequest{Scope: scope, Owner: owner, Title: "history", IdempotencyKey: "history"})
	if err != nil {
		t.Fatal(err)
	}
	var expected []int64
	for seq := int64(1); seq <= 26; seq++ {
		audience := ConversationAudience{Kind: ConversationAudienceChannel}
		if seq == 11 || seq == 21 {
			audience = ConversationAudience{Kind: ConversationAudienceParticipants, Participants: []ConversationParticipant{{Type: ConversationParticipantUser, ID: "private"}}}
		} else {
			expected = append(expected, seq)
		}
		content := fmt.Sprintf("message %d", seq)
		if seq == 22 {
			content = "Correction: the deadline is Friday, not Thursday."
		}
		_, err := service.PostChannelMessage(t.Context(), PostChannelMessageRequest{Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision, Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "sender"}, Intent: MessageIntentUpdate, Content: content, Audience: audience, IdempotencyKey: fmt.Sprint(seq)})
		if err != nil {
			t.Fatal(err)
		}
		conversation, err = service.GetConversation(t.Context(), scope, conversation.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	var seen []int64
	for after, pages := int64(0), 0; ; pages++ {
		if pages > 3 {
			t.Fatal("cursor failed to advance")
		}
		page, err := ReadConversationHistory(t.Context(), service, scope, owner, conversation.ID, viewer, ConversationHistoryReadRequest{AfterSequence: &after, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if page.NextBeforeSequence != 0 || len(page.Messages) > 10 {
			t.Fatalf("bad forward page: %+v", page)
		}
		for _, message := range page.Messages {
			seen = append(seen, message.Sequence)
		}
		if page.NextAfterSequence == 0 {
			break
		}
		if page.NextAfterSequence <= after || page.NextAfterSequence == 11 || page.NextAfterSequence == 21 {
			t.Fatal("invalid/private cursor")
		}
		after = page.NextAfterSequence
	}
	if !reflect.DeepEqual(seen, expected) {
		t.Fatalf("history beyond ten lost: %v", seen)
	}
	after := int64(20)
	page, err := ReadConversationHistory(t.Context(), service, scope, owner, conversation.ID, viewer, ConversationHistoryReadRequest{AfterSequence: &after, Limit: 1})
	if err != nil || len(page.Messages) != 1 || page.Messages[0].Sequence != 22 || !strings.HasPrefix(page.Messages[0].Content, "Correction:") {
		t.Fatalf("correction lost: %+v %v", page, err)
	}
	negative := int64(-1)
	for _, request := range []ConversationHistoryReadRequest{{AfterSequence: &negative}, {AfterSequence: &after, BeforeSequence: 25}, {AfterSequence: &after, MessageID: page.Messages[0].ID}, {AfterSequence: &after, OffsetBytes: 1}} {
		if _, err := ReadConversationHistory(t.Context(), service, scope, owner, conversation.ID, viewer, request); err == nil {
			t.Fatalf("ambiguous request accepted: %+v", request)
		}
	}
}

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
