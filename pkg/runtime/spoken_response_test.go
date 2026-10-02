package runtime

import (
	"context"
	"strings"
	"testing"
)

func TestSilentConversationCompletionRequiresSpokenChannel(t *testing.T) {
	for _, mode := range []string{"spoken", ""} {
		t.Run("mode="+mode, func(t *testing.T) {
			ctx := t.Context()
			store := NewMemoryStore()
			service := NewConversationService(store)
			scope := Scope{Kind: "tenant", ID: "spoken"}
			conversation, _, err := service.CreateConversation(ctx, CreateConversationRequest{
				Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"},
				Title: "Conversation", IdempotencyKey: "spoken-conversation",
			})
			if err != nil {
				t.Fatal(err)
			}
			posted, err := service.PostChannelMessage(ctx, PostChannelMessageRequest{
				Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
				Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "user"},
				Intent: MessageIntentQuestion, Content: "Ambient speech", ResponseMode: mode,
				Audience: ConversationAudience{Kind: ConversationAudienceChannel}, RequiresResponse: true,
				IdempotencyKey: "ambient-message",
			})
			if err != nil {
				t.Fatal(err)
			}
			scheduled, _, err := mustConversationRunScheduler(t, store).ScheduleMessage(ctx, scope, conversation.ID, posted.Message.ID)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "spoken" && scheduled.Run.Context["responseMode"] != mode {
				t.Fatal("spoken presentation was lost when scheduling the reply")
			}
			resolver := TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*TurnRunnerBinding, error) {
				return &TurnRunnerBinding{DeploymentID: "agent", DefinitionID: "definition", DefinitionVersion: "1",
					Runner: TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
						return &TurnOutcome{NextRunStatus: AgentRunStatusCompleted, RunOutput: map[string]interface{}{"silent": true}}, nil
					})}, nil
			})
			runner, err := NewConversationRunTurnRunner(store, conversationRunTestCoordinator(t, service), ConversationRunTurnRunnerConfig{AgentTurns: resolver})
			if err != nil {
				t.Fatal(err)
			}
			binding, err := runner.ResolveTurnRunner(ctx, scheduled.Run)
			if err != nil {
				t.Fatal(err)
			}
			outcome, err := binding.Runner.RunTurn(ctx, TurnExecutionContext{Run: scheduled.Run})
			if mode == "spoken" {
				if err != nil || outcome == nil || outcome.NextRunStatus != AgentRunStatusCompleted || outcome.RunOutput["silent"] != true {
					t.Fatalf("ambient spoken completion: %#v, %v", outcome, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "spoken response channel") {
				t.Fatalf("text reply was silently discarded: %#v, %v", outcome, err)
			}
			messages, err := service.ListChannelMessages(ctx, ChannelMessageFilter{Scope: scope, ConversationID: conversation.ID})
			if err != nil || len(messages) != 1 {
				t.Fatalf("silent completion posted a reply: %#v, %v", messages, err)
			}
		})
	}
}
