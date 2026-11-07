package runtime

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestConversationSpokenAnswerUpgradeReplaysLegacyReceiptStrictly(t *testing.T) {
	for _, conflict := range []string{"", "content", "sender", "references"} {
		t.Run("conflict="+conflict, func(t *testing.T) {
			store := NewMemoryStore()
			service := NewConversationService(store)
			conversation := conversationThreadStartConversation(t, store)
			trigger := conversationThreadStartPost(t, service, conversation, PostChannelMessageRequest{
				IdempotencyKey: "spoken-trigger", ResponseMode: "spoken", RequiresResponse: true,
			})
			scheduled, _, err := mustConversationRunScheduler(t, store).ScheduleMessage(t.Context(), conversation.Scope, conversation.ID, trigger.ID)
			if err != nil {
				t.Fatal(err)
			}
			current, err := service.GetConversation(t.Context(), conversation.Scope, conversation.ID)
			if err != nil {
				t.Fatal(err)
			}
			key := "agent-channel-response:" + hashString(scheduled.Run.Scope.Kind+"\x00"+scheduled.Run.Scope.ID+"\x00"+scheduled.Run.ID+"\x00"+trigger.ID)
			legacyRequest := PostChannelMessageRequest{
				Scope: conversation.Scope, ConversationID: conversation.ID, ExpectedRevision: current.Revision,
				Sender: ConversationParticipant{Type: ConversationParticipantAgent, ID: conversation.Owner.ID},
				Intent: MessageIntentAnswer, Content: "Canonical answer", Audience: ConversationAudience{Kind: ConversationAudienceChannel},
				ReplyToMessageID: trigger.ID, ResolvesMessageID: trigger.ID,
				References: []ConversationReference{{Kind: ConversationReferenceRun, ID: scheduled.Run.ID}}, IdempotencyKey: key,
			}
			switch conflict {
			case "content":
				legacyRequest.Content = "Conflicting saved answer"
			case "sender":
				legacyRequest.Sender.ID = "other-agent"
			case "references":
				legacyRequest.References[0].ID = "different-run"
			}
			saved, err := service.PostChannelMessage(t.Context(), legacyRequest)
			if err != nil {
				t.Fatal(err)
			}
			before := cloneChannelMessage(saved.Message)
			beforeConversation, err := service.GetConversation(t.Context(), conversation.Scope, conversation.ID)
			if err != nil {
				t.Fatal(err)
			}
			modeChanged := legacyRequest
			modeChanged.ResponseMode = "spoken"
			if _, err := service.PostChannelMessage(t.Context(), modeChanged); !errors.Is(err, ErrMessageConflict) {
				t.Fatalf("general message replay loosened its presentation contract: %v", err)
			}
			resolver := TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*TurnRunnerBinding, error) {
				return &TurnRunnerBinding{DeploymentID: conversation.Owner.ID, Runner: TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
					return &TurnOutcome{NextRunStatus: AgentRunStatusCompleted, RunOutput: map[string]interface{}{"summary": "Canonical answer"}}, nil
				})}, nil
			})
			runner, err := NewConversationRunTurnRunner(store, conversationRunTestCoordinator(t, service), ConversationRunTurnRunnerConfig{AgentTurns: resolver})
			if err != nil {
				t.Fatal(err)
			}
			binding, err := runner.ResolveTurnRunner(t.Context(), scheduled.Run)
			if err != nil {
				t.Fatal(err)
			}
			outcome, err := binding.Runner.RunTurn(t.Context(), TurnExecutionContext{Run: scheduled.Run})
			if conflict == "" {
				if err != nil || outcome == nil || outcome.NextRunStatus != AgentRunStatusCompleted || outcome.RunOutput["replayed"] != true || outcome.RunOutput["messageId"] != before.ID {
					t.Fatalf("spoken metadata upgrade rejected the exact legacy answer receipt: %#v, %v", outcome, err)
				}
			} else if err == nil {
				t.Fatalf("spoken replay ignored a non-mode %s conflict: %#v", conflict, outcome)
			}
			after, readErr := service.GetChannelMessage(t.Context(), conversation.Scope, conversation.ID, before.ID)
			afterConversation, conversationErr := service.GetConversation(t.Context(), conversation.Scope, conversation.ID)
			messages, listErr := service.ListChannelMessages(t.Context(), ChannelMessageFilter{Scope: conversation.Scope, ConversationID: conversation.ID, Limit: 100})
			if readErr != nil || conversationErr != nil || listErr != nil || !reflect.DeepEqual(before, after) || len(messages) != 2 || after.ResponseMode != "" || beforeConversation.Revision != afterConversation.Revision || beforeConversation.LastSequence != afterConversation.LastSequence {
				t.Fatalf("upgrade replay rewrote or duplicated its durable receipt: before=%#v after=%#v messages=%d errors=%v/%v/%v", before, after, len(messages), readErr, conversationErr, listErr)
			}
		})
	}
}
