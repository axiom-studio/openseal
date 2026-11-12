package externalconversation

import (
	"context"
	"fmt"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation/conversationtest"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

type countingFinalReplyStore struct {
	*sqlite.Store
	pages int
}

func (s *countingFinalReplyStore) ListChannelMessages(ctx context.Context, filter models.ChannelMessageFilter) ([]*models.ChannelMessage, error) {
	s.pages++
	return s.Store.ListChannelMessages(ctx, filter)
}

func TestExternalFinalFailureAnswerSurvivesBusyChannel(t *testing.T) {
	ctx := t.Context()
	store := &countingFinalReplyStore{Store: kerneltest.NewStore(t)}
	service := runtimeconversation.NewService(store)
	scope := models.Scope{Kind: "tenant", ID: "17"}
	conversationtest.SeedSpace(t, store, scope)
	conversation, _, err := service.CreateConversation(ctx, runtimeconversation.CreateConversationRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "seal"}, Title: "Busy channel", IdempotencyKey: "busy-final-channel",
	})
	if err != nil {
		t.Fatal(err)
	}
	var trigger *models.ChannelMessage
	for i := 0; i < 151; i++ {
		current, err := service.GetConversation(ctx, scope, conversation.ID)
		if err != nil {
			t.Fatal(err)
		}
		posted, err := service.PostChannelMessage(ctx, runtimeconversation.PostChannelMessageRequest{
			Scope: scope, ConversationID: conversation.ID, ExpectedRevision: current.Revision,
			Sender: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "participant"},
			Intent: models.MessageIntentUpdate, Content: fmt.Sprintf("Message %d", i), Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel},
			IdempotencyKey: fmt.Sprintf("busy-message-%d", i),
		})
		if err != nil {
			t.Fatal(err)
		}
		trigger = posted.Message
	}
	current, _ := service.GetConversation(ctx, scope, conversation.ID)
	posted, err := service.PostChannelMessage(ctx, runtimeconversation.PostChannelMessageRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: current.Revision,
		Sender: models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: "seal"}, Intent: models.MessageIntentAnswer,
		Content:  "The image request failed, so I stopped. Your original image is still available.",
		Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel}, ReplyToMessageID: trigger.ID,
		References: []models.ConversationReference{{Kind: models.ConversationReferenceRun, ID: "failed-run"}}, IdempotencyKey: "busy-final-answer",
	})
	if err != nil {
		t.Fatal(err)
	}
	worker := &ReplyWorker{store: store, conversations: service}
	item := &models.ExternalConversationInboxItem{Scope: scope, ConversationID: conversation.ID, ChannelMessageID: trigger.ID}
	run := &models.AgentRun{ID: "failed-run", Scope: scope, Status: models.AgentRunStatusFailed,
		FailureExplanation: &models.RunFailureExplanation{Kind: "execution", Code: "execution_failed", Message: "Image generation failed", Explained: true},
		Output:             map[string]interface{}{"messageId": posted.Message.ID}}
	// Posting the answer read the channel once; count only the lookup.
	store.pages = 0
	message, err := worker.findCanonicalReply(ctx, item, run)
	if err != nil || message == nil || message.ID != posted.Message.ID || store.pages != 0 {
		t.Fatalf("trusted final answer not found directly: message=%#v pages=%d err=%v", message, store.pages, err)
	}
	for _, messageID := range []string{"", trigger.ID} {
		store.pages = 0
		run.Output["messageId"] = messageID
		message, err := worker.findCanonicalReply(ctx, item, run)
		if err != nil || message == nil || message.ID != posted.Message.ID || store.pages < 2 {
			t.Fatalf("paged final answer missing: message=%#v pages=%d err=%v", message, store.pages, err)
		}
	}
	for _, mutation := range []string{"scope", "conversation", "trigger", "user", "historical", "run"} {
		t.Run(mutation, func(t *testing.T) {
			candidate := models.CloneChannelMessage(posted.Message)
			switch mutation {
			case "scope":
				candidate.Scope.ID = "18"
			case "conversation":
				candidate.ConversationID = "another-conversation"
			case "trigger":
				candidate.ReplyToMessageID = "another-trigger"
			case "user":
				candidate.Sender.Type = models.ConversationParticipantUser
			case "historical":
				candidate.Historical = true
			case "run":
				candidate.References = []models.ConversationReference{{Kind: models.ConversationReferenceRun, ID: "other-run"}}
			}
			if canonicalExternalRunReply(candidate, item, run) {
				t.Fatal("unrelated or untrusted answer was accepted")
			}
		})
	}
}

func TestExternalReplyUsesExplainedSummary(t *testing.T) {
	ctx := t.Context()
	run := &models.AgentRun{Status: models.AgentRunStatusFailed,
		FailureExplanation: &models.RunFailureExplanation{Kind: "execution", Code: "execution_failed", Message: "Image generation failed"},
		Output:             map[string]interface{}{"summary": "The image request failed, so I stopped."}}
	if reply, ok := externalConversationRunReply(ctx, nil, nil, run); !ok || reply != runtimeconversation.TerminalFailureReply("execution_failed") {
		t.Fatalf("unexplained failure reused model output: %q", reply)
	}
	run.FailureExplanation.Explained = true
	if reply, ok := externalConversationRunReply(ctx, nil, nil, run); !ok || reply != run.Output["summary"] {
		t.Fatalf("model answer replaced: %q, %v", reply, ok)
	}
	run.Output["summary"] = "   "
	if reply, ok := externalConversationRunReply(ctx, nil, nil, run); !ok || reply != runtimeconversation.TerminalFailureReply("execution_failed") {
		t.Fatalf("empty explanation was posted: %q", reply)
	}
	run.FailureExplanation, run.Output["summary"] = nil, "model text"
	if reply, ok := externalConversationRunReply(ctx, nil, nil, run); !ok || reply == "model text" {
		t.Fatal("unmarked failed attempt trusted model-authored output")
	}
	if _, ok := externalConversationRunReply(ctx, nil, nil, &models.AgentRun{Status: models.AgentRunStatusRunning}); ok {
		t.Fatal("a running Run has no reply")
	}
}
