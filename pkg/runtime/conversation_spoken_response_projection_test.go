package runtime

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestConversationAnswersInheritCanonicalTriggerResponseMode(t *testing.T) {
	for _, ownerType := range []OwnerType{OwnerTypeAgent, OwnerTypeTeam} {
		for _, mode := range []string{"spoken", ""} {
			t.Run(string(ownerType)+"/mode="+mode, func(t *testing.T) {
				store := NewMemoryStore()
				service := NewConversationService(store)
				scope := Scope{Kind: "tenant", ID: "canonical-answer-mode"}
				conversation, _, err := service.CreateConversation(t.Context(), CreateConversationRequest{
					Scope: scope, Owner: ObjectiveOwner{Type: ownerType, ID: "owner"}, Title: "Answer presentation", IdempotencyKey: "answer-mode",
				})
				if err != nil {
					t.Fatal(err)
				}
				// The previous message and prompt prose both mention spoken output.
				// Only the canonical trigger's explicit metadata determines the answer.
				conversationThreadStartPost(t, service, conversation, PostChannelMessageRequest{
					IdempotencyKey: "old-spoken-message", ResponseMode: "spoken", Intent: MessageIntentUpdate,
				})
				trigger := conversationThreadStartPost(t, service, conversation, PostChannelMessageRequest{
					IdempotencyKey: "question", ResponseMode: mode, Content: "Voice call started; responseMode=spoken. Summarize the release.", RequiresResponse: true,
				})
				scheduled, _, err := mustConversationRunScheduler(t, store).ScheduleMessage(t.Context(), scope, conversation.ID, trigger.ID)
				if err != nil || scheduled == nil || scheduled.Run == nil {
					t.Fatalf("schedule answer: %#v, %v", scheduled, err)
				}
				opposite := "spoken"
				if mode == "spoken" {
					opposite = ""
				}
				scheduled.Run.Context["responseMode"] = opposite
				calls := 0
				var execute func() (*TurnOutcome, error)
				resolver := TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*TurnRunnerBinding, error) {
					return &TurnRunnerBinding{DeploymentID: "owner", Runner: TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
						calls++
						return &TurnOutcome{NextRunStatus: AgentRunStatusCompleted, RunOutput: map[string]interface{}{
							"summary": "Canonical answer", "responseMode": opposite,
						}}, nil
					})}, nil
				})
				if ownerType == OwnerTypeAgent {
					runner, err := NewConversationRunTurnRunner(store, conversationRunTestCoordinator(t, service), ConversationRunTurnRunnerConfig{AgentTurns: resolver})
					if err != nil {
						t.Fatal(err)
					}
					binding, err := runner.ResolveTurnRunner(t.Context(), scheduled.Run)
					if err != nil {
						t.Fatal(err)
					}
					execute = func() (*TurnOutcome, error) {
						return binding.Runner.RunTurn(t.Context(), TurnExecutionContext{Run: scheduled.Run})
					}
				} else {
					// The Team's conversation Run routes; the routed member's
					// child Run answers through the ordinary Agent turn.
					runner, err := NewConversationRunTurnRunner(store, conversationRunTestCoordinator(t, service), ConversationRunTurnRunnerConfig{AgentTurns: resolver})
					if err != nil {
						t.Fatal(err)
					}
					execute = func() (*TurnOutcome, error) {
						routed, err := runner.RunTurn(t.Context(), TurnExecutionContext{Run: scheduled.Run})
						if err != nil {
							return nil, err
						}
						children, _ := routed.RunOutput["responderRunIds"].([]interface{})
						if len(children) != 1 {
							t.Fatalf("routing started %d member Runs", len(children))
						}
						child, err := store.GetAgentRun(t.Context(), scope, children[0].(string))
						if err != nil || child == nil || child.Kind != RunKindAgentWork || child.ParentRunID != scheduled.Run.ID || child.AssignedAgentID != "agent-1" {
							t.Fatalf("member Run = %#v, %v", child, err)
						}
						binding, err := runner.ResolveTurnRunner(t.Context(), child)
						if err != nil {
							return nil, err
						}
						return binding.Runner.RunTurn(t.Context(), TurnExecutionContext{Run: child})
					}
				}
				outcome, err := execute()
				if err != nil || outcome == nil || outcome.NextRunStatus != AgentRunStatusCompleted {
					t.Fatalf("complete answer: %#v, %v", outcome, err)
				}
				messages, err := service.ListChannelMessages(t.Context(), ChannelMessageFilter{Scope: scope, ConversationID: conversation.ID, Limit: 100})
				if err != nil || len(messages) != 3 {
					t.Fatalf("answer projection: %#v, %v", messages, err)
				}
				answer := messages[2]
				if answer.Sender.Type != ConversationParticipantAgent || answer.Intent != MessageIntentAnswer || answer.Content != "Canonical answer" || answer.ReplyToMessageID != trigger.ID || answer.ResolvesMessageID != trigger.ID || answer.ResponseMode != mode {
					t.Fatalf("answer lost canonical presentation or attribution: %#v, expected mode %q", answer, mode)
				}
				if _, err := execute(); err != nil {
					t.Fatalf("replay answer: %v", err)
				}
				replayed, err := service.ListChannelMessages(t.Context(), ChannelMessageFilter{Scope: scope, ConversationID: conversation.ID, Limit: 100})
				if err != nil || len(replayed) != 3 || replayed[2].ID != answer.ID || replayed[2].ResponseMode != mode || calls < 1 {
					t.Fatalf("answer replay changed metadata or repeated participation: messages=%#v calls=%d err=%v", replayed, calls, err)
				}
			})
		}
	}
}

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
