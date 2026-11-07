package runtime

import (
	"context"
	"encoding/base64"
	"fmt"
	"testing"
	"time"
)

type quotedPhotoHost struct{ calls int }

func (h *quotedPhotoHost) ReadExternalConversationAttachment(_ context.Context, req ExternalConversationAttachmentHostRequest) (*ExternalConversationAttachmentContent, error) {
	if err := req.Event.Validate(); err != nil {
		return nil, err
	}
	h.calls++
	data, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aN1sAAAAASUVORK5CYII=")
	return &ExternalConversationAttachmentContent{ID: req.Attachment.ID, Name: "photo.png", MediaType: "image/png", Data: data, Status: "supplied"}, nil
}

func TestQuotedPhotoHistoryImportsMediaIntoModelContext(t *testing.T) {
	store, catalog, endpoint := fileConversationFixture(t)
	content := &externalFileContentStore{}
	photo := &quotedPhotoHost{}
	now := time.Now().UTC().Truncate(time.Millisecond)
	item := receiveContextTestEvent(t, store, endpoint, "photo-reply", "reply", "parent", now)
	history := &recordingContextHost{result: &ExternalConversationContextResult{Status: ExternalConversationContextPartial, Messages: []ExternalConversationContextMessage{{
		ExternalConversationID: item.Event.ExternalConversationID, ExternalThreadID: "parent", ExternalMessageID: "parent", ExternalParticipantID: "Kev", ParticipantDisplayName: "Kev", OccurredAt: now.Add(-time.Second),
		Attachments: []ExternalConversationAttachment{{ID: "photo", Name: "photo.png", MediaType: "image/png", SizeBytes: 68}},
	}}}}
	worker, err := NewExternalConversationInboxWorker(store, replyAliasDispatcher{}, ExternalConversationInboxWorkerConfig{WorkerID: "photo-worker", ContextHost: history, ContextCatalog: contextHistoryResolver{catalog}, AttachmentHost: photo, AttachmentContent: content})
	if err != nil {
		t.Fatal(err)
	}
	applied, err := worker.ProcessOne(t.Context(), endpoint.Scope)
	if err != nil || applied == nil || applied.Status != ExternalConversationInboxApplied {
		t.Fatalf("apply: %#v %v", applied, err)
	}
	conversation, _ := store.GetConversation(t.Context(), endpoint.Scope, applied.ConversationID)
	messages, _ := store.ListChannelMessages(t.Context(), ChannelMessageFilter{Scope: endpoint.Scope, ConversationID: conversation.ID, Limit: 10})
	if len(messages) != 2 || !messages[0].Historical || messages[1].ReplyToMessageID != messages[0].ID {
		t.Fatalf("messages: %#v", messages)
	}
	runner := &ConversationRunTurnRunner{artifacts: store, config: ConversationRunTurnRunnerConfig{AttachmentContent: content}}
	attachments := runner.conversationAttachments(t.Context(), conversation, messages[1], messages)
	if len(attachments) != 1 || attachments[0].media == nil || attachments[0].Status != "image_context" || attachments[0].MessageID != messages[0].ID || photo.calls != 1 {
		t.Fatalf("media: %#v calls=%d", attachments, photo.calls)
	}
}

func TestTelegramPrivateChatKeepsContextAcrossReplyRoots(t *testing.T) {
	conversation := &Conversation{Origin: &ConversationReference{Kind: ConversationReferenceExternalSource, ID: "telegram-endpoint"}}
	trigger := &ChannelMessage{ID: "new-message", ThreadRootID: "quoted-message", ExternalSource: &ExternalMessageSource{Provider: "telegram", ChannelType: "private", ThreadID: "reply:7"}}
	if got := externalConversationThreadRoot(conversation, trigger); got != "" {
		t.Fatalf("DM context restricted to %s", got)
	}
	for _, topic := range []string{"topic:7", "direct-topic:7"} {
		trigger.ExternalSource.ThreadID = topic
		if got := externalConversationThreadRoot(conversation, trigger); got != "quoted-message" {
			t.Fatalf("topic lost isolation: %s", got)
		}
	}
	trigger.ExternalSource.ThreadID = "reply:7"
	trigger.ExternalSource.ChannelType = "supergroup"
	if externalConversationThreadRoot(conversation, trigger) != "quoted-message" {
		t.Fatal("group lost isolation")
	}
	trigger.ExternalSource.Provider = "slack"
	trigger.ExternalSource.ChannelType = "private"
	if externalConversationThreadRoot(conversation, trigger) != "quoted-message" {
		t.Fatal("Slack lost isolation")
	}
}

func TestQuotedPhotoContextRejectsInvalidOriginAndUnboundedFiles(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	event := NormalizedExternalConversationEvent{ExternalConversationID: "chat", ExternalThreadID: "reply:7", ExternalMessageID: "chat:8", ReplyToExternalMessageID: "chat:7", OccurredAt: now}
	parent := ExternalConversationContextMessage{ExternalConversationID: "chat", ExternalThreadID: "reply:7", ExternalMessageID: "chat:7", ExternalParticipantID: "user", OccurredAt: now, Attachments: []ExternalConversationAttachment{{ID: "photo", Name: "photo.jpg", MediaType: "image/jpeg", SizeBytes: 20}}}
	result := &ExternalConversationContextResult{Status: ExternalConversationContextPartial, Messages: []ExternalConversationContextMessage{parent}}
	messages, _, ok := normalizeExternalConversationContext(result, event)
	if !ok || len(messages) != 1 {
		t.Fatal("same-second quoted photo lost")
	}
	result.Messages[0].ExternalConversationID = "another-chat"
	if _, _, ok := normalizeExternalConversationContext(result, event); ok {
		t.Fatal("cross-chat parent accepted")
	}
	result.Messages[0] = parent
	result.Messages[0].Attachments = []ExternalConversationAttachment{{ID: "photo", Name: "bad\nname"}}
	if _, _, ok := normalizeExternalConversationContext(result, event); ok {
		t.Fatal("invalid descriptor accepted")
	}
	result.Messages = nil
	for i := 0; i < 9; i++ {
		p := parent
		p.ExternalMessageID = fmt.Sprint(i)
		p.OccurredAt = now.Add(-time.Second)
		result.Messages = append(result.Messages, p)
	}
	if _, _, ok := normalizeExternalConversationContext(result, event); ok {
		t.Fatal("unbounded historical attachments accepted")
	}
}
