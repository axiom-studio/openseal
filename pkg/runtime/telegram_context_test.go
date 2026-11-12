package runtime

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
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
	history := &recordingContextHost{result: &ExternalConversationContextResult{Status: runtimeconversation.ExternalConversationContextPartial, Messages: []ExternalConversationContextMessage{{
		ExternalConversationID: item.Event.ExternalConversationID, ExternalThreadID: "parent", ExternalMessageID: "parent", ExternalParticipantID: "Kev", ParticipantDisplayName: "Kev", OccurredAt: now.Add(-time.Second),
		Attachments: []models.ExternalConversationAttachment{{ID: "photo", Name: "photo.png", MediaType: "image/png", SizeBytes: 68}},
	}}}}
	worker, err := NewExternalConversationInboxWorker(store, replyAliasDispatcher{}, ExternalConversationInboxWorkerConfig{WorkerID: "photo-worker", ContextHost: history, ContextCatalog: contextHistoryResolver{catalog}, AttachmentHost: photo, AttachmentContent: content})
	if err != nil {
		t.Fatal(err)
	}
	applied, err := worker.ProcessOne(t.Context(), endpoint.Scope)
	if err != nil || applied == nil || applied.Status != models.ExternalConversationInboxApplied {
		t.Fatalf("apply: %#v %v", applied, err)
	}
	conversation, _ := store.GetConversation(t.Context(), endpoint.Scope, applied.ConversationID)
	messages, _ := store.ListChannelMessages(t.Context(), models.ChannelMessageFilter{Scope: endpoint.Scope, ConversationID: conversation.ID, Limit: 10})
	if len(messages) != 2 || !messages[0].Historical || messages[1].ReplyToMessageID != messages[0].ID {
		t.Fatalf("messages: %#v", messages)
	}
	// The quoted parent reaches a chat session as a user entry whose image
	// part the step renders: the first step of the reply's chat Run, through
	// the conversation turn runner with the same attachment content.
	entry := quotedPhotoStepEntry(t, store, content, conversation, messages[1], messages[0].ID)
	var images []models.ContentPart
	for _, part := range entry.User.Content {
		if part.Type == models.ContentPartImage {
			images = append(images, part)
		}
	}
	if len(images) != 1 || images[0].MediaType != "image/png" || images[0].ArtifactID == "" || entry.User.MessageID != messages[0].ID || photo.calls != 1 {
		t.Fatalf("quoted photo entry: %#v calls=%d", entry.User.Content, photo.calls)
	}
	if text := entry.User.Content[0].Text; !strings.Contains(text, "historical") || !strings.Contains(text, "Attached: photo.png") {
		t.Fatalf("quoted photo header: %q", text)
	}
}

// quotedPhotoStepEntry is the user entry of messageID in the first step
// context of the chat Run that answers trigger.
func quotedPhotoStepEntry(t *testing.T, store *sqlite.Store, content models.ArtifactContentStore, conversation *models.Conversation, trigger *models.ChannelMessage, messageID string) *models.SessionEntry {
	t.Helper()
	ctx := t.Context()
	created, err := agentrun.NewRunCommandService(store).CreateAgentRun(ctx, runtimeconversation.ConversationAgentRunRequest(conversation, trigger))
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.ClaimNextAgentRun(ctx, models.AgentRunClaim{Scope: conversation.Scope, WorkerID: "photo-step", Now: time.Now().UTC(), LeaseDuration: time.Hour, AgingInterval: time.Minute})
	if err != nil || run == nil || run.ID != created.Run.ID {
		t.Fatalf("claim = %#v, %v", run, err)
	}
	agent := chatLoopAgentTurns{run.ID: {
		DeploymentID: run.AssignedAgentID, DefinitionID: "agent-definition", DefinitionVersion: "1",
		ModelProvider: string(models.ModelStepProviderAnthropic), Model: "chat-model", Producer: &chatLoopProducer{},
		Authority: agentrun.ControlAuthority{AgentID: run.AssignedAgentID, CanAskUser: true, CanReact: true},
	}}
	runner, err := conversationrun.NewConversationRunTurnRunner(store, conversationRunTestCoordinator(t, runtimeconversation.NewConversationService(store)), conversationrun.ConversationRunTurnRunnerConfig{
		AttachmentContent: content, AgentTurns: agent,
	})
	if err != nil {
		t.Fatal(err)
	}
	binding, err := runner.ResolveTurnRunner(ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	chat := binding.Preparers[len(binding.Preparers)-1]
	part, err := chat.BeforeStep(ctx, agentrun.StepInput{Run: run, Session: binding.Session, Binding: binding, WorkerID: "photo-step", Step: &models.AgentTurn{ID: run.ID + "-step-1", Sequence: 1}}, agentrun.SteeringPlan{})
	if err != nil || part == nil {
		t.Fatalf("first step context = %#v, %v", part, err)
	}
	for _, entry := range part.Entries {
		if entry.User != nil && entry.User.MessageID == messageID {
			return entry
		}
	}
	t.Fatalf("message %s is not in the first step context", messageID)
	return nil
}

func TestQuotedPhotoContextRejectsInvalidOriginAndUnboundedFiles(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	event := models.NormalizedExternalConversationEvent{ExternalConversationID: "chat", ExternalThreadID: "reply:7", ExternalMessageID: "chat:8", ReplyToExternalMessageID: "chat:7", OccurredAt: now}
	parent := ExternalConversationContextMessage{ExternalConversationID: "chat", ExternalThreadID: "reply:7", ExternalMessageID: "chat:7", ExternalParticipantID: "user", OccurredAt: now, Attachments: []models.ExternalConversationAttachment{{ID: "photo", Name: "photo.jpg", MediaType: "image/jpeg", SizeBytes: 20}}}
	result := &ExternalConversationContextResult{Status: runtimeconversation.ExternalConversationContextPartial, Messages: []ExternalConversationContextMessage{parent}}
	messages, _, ok := normalizeExternalConversationContext(result, event)
	if !ok || len(messages) != 1 {
		t.Fatal("same-second quoted photo lost")
	}
	result.Messages[0].ExternalConversationID = "another-chat"
	if _, _, ok := normalizeExternalConversationContext(result, event); ok {
		t.Fatal("cross-chat parent accepted")
	}
	result.Messages[0] = parent
	result.Messages[0].Attachments = []models.ExternalConversationAttachment{{ID: "photo", Name: "bad\nname"}}
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
