package runtime

import (
	"context"
	"fmt"
	"testing"
)

type countingFinalReplyStore struct {
	*MemoryStore
	pages int
}

func (s *countingFinalReplyStore) ListChannelMessages(ctx context.Context, filter ChannelMessageFilter) ([]*ChannelMessage, error) {
	s.pages++
	return s.MemoryStore.ListChannelMessages(ctx, filter)
}

func TestExternalFinalFailureAnswerSurvivesBusyChannel(t *testing.T) {
	ctx := t.Context()
	store := &countingFinalReplyStore{MemoryStore: NewMemoryStore()}
	service := NewConversationService(store)
	scope := Scope{Kind: "tenant", ID: "17"}
	conversation, _, err := service.CreateConversation(ctx, CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "seal"}, Title: "Busy channel", IdempotencyKey: "busy-final-channel",
	})
	if err != nil {
		t.Fatal(err)
	}
	var trigger *ChannelMessage
	for i := 0; i < 151; i++ {
		current, err := service.GetConversation(ctx, scope, conversation.ID)
		if err != nil {
			t.Fatal(err)
		}
		posted, err := service.PostChannelMessage(ctx, PostChannelMessageRequest{
			Scope: scope, ConversationID: conversation.ID, ExpectedRevision: current.Revision,
			Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "participant"},
			Intent: MessageIntentUpdate, Content: fmt.Sprintf("Message %d", i), Audience: ConversationAudience{Kind: ConversationAudienceChannel},
			IdempotencyKey: fmt.Sprintf("busy-message-%d", i),
		})
		if err != nil {
			t.Fatal(err)
		}
		trigger = posted.Message
	}
	current, _ := service.GetConversation(ctx, scope, conversation.ID)
	posted, err := service.PostChannelMessage(ctx, PostChannelMessageRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: current.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantAgent, ID: "seal"}, Intent: MessageIntentAnswer,
		Content:  "The image request failed, so I stopped. Your original image is still available.",
		Audience: ConversationAudience{Kind: ConversationAudienceChannel}, ReplyToMessageID: trigger.ID,
		References: []ConversationReference{{Kind: ConversationReferenceRun, ID: "failed-run"}}, IdempotencyKey: "busy-final-answer",
	})
	if err != nil {
		t.Fatal(err)
	}
	worker := &ExternalConversationReplyWorker{store: store, conversations: service}
	item := &ExternalConversationInboxItem{Scope: scope, ConversationID: conversation.ID, ChannelMessageID: trigger.ID}
	run := &AgentRun{ID: "failed-run", Scope: scope, Status: AgentRunStatusFailed,
		Checkpoint: checkpointFinalFailureExplanation(nil, "action", "Image generation failed"), Output: map[string]interface{}{"messageId": posted.Message.ID}}
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
			candidate := cloneChannelMessage(posted.Message)
			switch mutation {
			case "scope":
				candidate.Scope.ID = "18"
			case "conversation":
				candidate.ConversationID = "another-conversation"
			case "trigger":
				candidate.ReplyToMessageID = "another-trigger"
			case "user":
				candidate.Sender.Type = ConversationParticipantUser
			case "historical":
				candidate.Historical = true
			case "run":
				candidate.References = []ConversationReference{{Kind: ConversationReferenceRun, ID: "other-run"}}
			}
			if canonicalExternalRunReply(candidate, item, run) {
				t.Fatal("unrelated or untrusted answer was accepted")
			}
		})
	}
}

func TestExternalFailedAttemptUsesRecordedModelAnswer(t *testing.T) {
	run := &AgentRun{Status: AgentRunStatusFailed, Checkpoint: checkpointFinalFailureExplanation(nil, "action", "Image generation failed"),
		Output: map[string]interface{}{"summary": "The image request failed, so I stopped."}}
	if reply, _ := externalConversationRunReply(run); reply == run.Output["summary"] {
		t.Fatal("unfinished explanation reused stale output")
	}
	run.Checkpoint[FinalFailureExplanationCheckpointKey].(map[string]interface{})["explained"] = true
	if reply, ok := externalConversationRunReply(run); !ok || reply != run.Output["summary"] {
		t.Fatalf("model answer replaced: %q, %v", reply, ok)
	}
	run.Checkpoint = nil
	if reply, ok := externalConversationRunReply(run); !ok || reply == run.Output["summary"] {
		t.Fatal("unmarked failed attempt trusted model-authored output")
	}
}
