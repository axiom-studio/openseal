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
				if ownerType == OwnerTypeAgent {
					resolver := TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*TurnRunnerBinding, error) {
						return &TurnRunnerBinding{DeploymentID: "owner", Runner: TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
							calls++
							return &TurnOutcome{NextRunStatus: AgentRunStatusCompleted, RunOutput: map[string]interface{}{
								"summary": "Canonical answer", "responseMode": opposite,
							}}, nil
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
					execute = func() (*TurnOutcome, error) {
						return binding.Runner.RunTurn(t.Context(), TurnExecutionContext{Run: scheduled.Run})
					}
				} else {
					participants := ConversationParticipantSourceFunc(func(context.Context, ConversationParticipantQuery) ([]ConversationParticipantBinding, error) {
						return []ConversationParticipantBinding{{Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "speaker"}}}, nil
					})
					provider := ParticipationProposalProviderFunc(func(_ context.Context, input ParticipationProposalContext) (ParticipationProposal, error) {
						calls++
						if input.Trigger.ID != trigger.ID || input.Trigger.ResponseMode != mode {
							t.Fatalf("participation lost canonical trigger metadata: %#v", input.Trigger)
						}
						return ParticipationProposal{
							WantsToSpeak: true, Intent: MessageIntentAnswer, Content: "Canonical answer", Audience: ConversationAudience{Kind: ConversationAudienceChannel},
							ReplyToMessageID: input.Trigger.ID, ResolvesMessageID: input.Trigger.ID,
							Signals: ParticipationSignals{AnswersOpenQuestion: true, HasNewInformation: true, RoleRelevant: true},
						}, nil
					})
					coordinator, err := NewConversationCoordinator(service, participants, provider, ConversationCoordinatorConfig{})
					if err != nil {
						t.Fatal(err)
					}
					runner, err := NewConversationRunTurnRunner(store, coordinator, ConversationRunTurnRunnerConfig{})
					if err != nil {
						t.Fatal(err)
					}
					execute = func() (*TurnOutcome, error) {
						return runner.RunTurn(t.Context(), TurnExecutionContext{Run: scheduled.Run})
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
				if err != nil || len(replayed) != 3 || replayed[2].ID != answer.ID || replayed[2].ResponseMode != mode || calls < 1 || ownerType == OwnerTypeTeam && calls != 1 {
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

func TestGovernedTeamActionAnswerUsesCanonicalSourceModeAndLegacyReceipt(t *testing.T) {
	for _, test := range []struct {
		name   string
		mode   string
		legacy bool
	}{
		{name: "spoken", mode: "spoken"},
		{name: "typed"},
		{name: "legacy-spoken", mode: "spoken", legacy: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := NewMemoryStore()
			service := NewConversationService(store)
			conversation, _, err := service.CreateConversation(t.Context(), CreateConversationRequest{
				Scope: Scope{Kind: "tenant", ID: "governed-team-mode"}, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "team"},
				Title: "Governed Team answer", IdempotencyKey: "governed-team-mode",
			})
			if err != nil {
				t.Fatal(err)
			}
			trigger := conversationThreadStartPost(t, service, conversation, PostChannelMessageRequest{
				IdempotencyKey: "team-trigger", ResponseMode: test.mode, RequiresResponse: true,
			})
			scheduled, _, err := mustConversationRunScheduler(t, store).ScheduleMessage(t.Context(), conversation.Scope, conversation.ID, trigger.ID)
			if err != nil {
				t.Fatal(err)
			}
			spoofedMode := "spoken"
			if test.mode == "spoken" {
				spoofedMode = ""
			}
			scheduled.Run.Context["responseMode"] = spoofedMode
			participant := ConversationParticipant{Type: ConversationParticipantAgent, ID: "speaker"}
			completion := &governedConversationCompletion{Content: "Canonical governed result", References: []ConversationReference{{Kind: ConversationReferenceRun, ID: scheduled.Run.ID}}}
			var legacy *ChannelMessage
			if test.legacy {
				current, err := service.GetConversation(t.Context(), conversation.Scope, conversation.ID)
				if err != nil {
					t.Fatal(err)
				}
				key := "team-action-outcome:" + hashString(scheduled.Run.Scope.Kind+"\x00"+scheduled.Run.Scope.ID+"\x00"+scheduled.Run.ID+"\x00"+trigger.ID)
				saved, err := service.PostChannelMessage(t.Context(), PostChannelMessageRequest{
					Scope: conversation.Scope, ConversationID: conversation.ID, ExpectedRevision: current.Revision,
					Sender: participant, Intent: MessageIntentAnswer, Content: completion.Content, Audience: ConversationAudience{Kind: ConversationAudienceChannel},
					ReplyToMessageID: trigger.ID, ResolvesMessageID: trigger.ID, References: completion.References, IdempotencyKey: key,
				})
				if err != nil {
					t.Fatal(err)
				}
				legacy = cloneChannelMessage(saved.Message)
			}
			runner := &ConversationRunTurnRunner{conversations: service}
			answer, replayed, err := runner.postTeamActionOutcome(t.Context(), scheduled.Run, conversation, trigger.ID, participant, completion)
			expectedMode := test.mode
			if test.legacy {
				expectedMode = ""
			}
			if err != nil || answer == nil || replayed != test.legacy || answer.ResponseMode != expectedMode || answer.Sender != participant || answer.ReplyToMessageID != trigger.ID || answer.ResolvesMessageID != trigger.ID || answer.Content != completion.Content {
				t.Fatalf("governed answer lost canonical source presentation: answer=%#v replayed=%t expectedMode=%q err=%v", answer, replayed, expectedMode, err)
			}
			if test.legacy && !reflect.DeepEqual(legacy, answer) {
				t.Fatalf("governed spoken upgrade rewrote its legacy receipt: saved=%#v replay=%#v", legacy, answer)
			}
			before, err := service.GetConversation(t.Context(), conversation.Scope, conversation.ID)
			if err != nil {
				t.Fatal(err)
			}
			second, secondReplayed, err := runner.postTeamActionOutcome(t.Context(), scheduled.Run, conversation, trigger.ID, participant, completion)
			if err != nil || !secondReplayed || !reflect.DeepEqual(answer, second) {
				t.Fatalf("governed answer replay changed its receipt: %#v, %t, %v", second, secondReplayed, err)
			}
			changed := *completion
			changed.Content = "A conflicting governed result"
			if _, _, err := runner.postTeamActionOutcome(t.Context(), scheduled.Run, conversation, trigger.ID, participant, &changed); err == nil {
				t.Fatal("governed mode compatibility ignored a content conflict")
			}
			after, err := service.GetConversation(t.Context(), conversation.Scope, conversation.ID)
			messages, listErr := service.ListChannelMessages(t.Context(), ChannelMessageFilter{Scope: conversation.Scope, ConversationID: conversation.ID, Limit: 100})
			if err != nil || listErr != nil || len(messages) != 2 || before.Revision != after.Revision || before.LastSequence != after.LastSequence || !reflect.DeepEqual(answer, messages[1]) {
				t.Fatalf("governed replay duplicated or changed durable history: messages=%#v err=%v/%v", messages, err, listErr)
			}
		})
	}
}
