package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestSpokenResponseModePersistsAndIsPartOfIdempotency(t *testing.T) {
	store := NewMemoryStore()
	service := NewConversationService(store)
	scope := Scope{Kind: "tenant", ID: "speech"}
	c, _, err := service.CreateConversation(t.Context(), CreateConversationRequest{Scope: scope,
		Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, Title: "Voice", IdempotencyKey: "chat"})
	if err != nil {
		t.Fatal(err)
	}
	req := PostChannelMessageRequest{Scope: scope, ConversationID: c.ID, ExpectedRevision: c.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "external-speaker"},
		Intent: MessageIntentQuestion, Content: "Hello", Audience: ConversationAudience{Kind: ConversationAudienceChannel},
		RequiresResponse: true, ResponseMode: "spoken", IdempotencyKey: "hello"}
	posted, err := service.PostChannelMessage(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := store.GetChannelMessage(t.Context(), scope, c.ID, posted.Message.ID)
	if err != nil || stored.ResponseMode != "spoken" {
		t.Fatalf("stored=%+v err=%v", stored, err)
	}
	run := conversationAgentRunRequest(c, stored)
	if run.Context["responseMode"] != "spoken" {
		t.Fatal("mode lost before model execution")
	}
	req.ResponseMode = ""
	if _, err := service.PostChannelMessage(t.Context(), req); !errors.Is(err, ErrMessageConflict) {
		t.Fatalf("changed idempotent mode: %v", err)
	}
	stored.ResponseMode = "unrecognized"
	if !errors.Is(stored.Validate(), ErrInvalidConversation) {
		t.Fatal("accepted unknown response mode")
	}
}

func TestSpokenResponseInstructionsDoNotAuthorizeActions(t *testing.T) {
	for _, mode := range []string{"", "spoken", "unknown"} {
		r := HostedTurnRequest{InputContext: map[string]interface{}{"responseMode": mode}, SystemInstructions: []string{"original policy"}}
		appendResponseChannelInstructions(&r)
		if (len(r.SystemInstructions) == 2) != (mode == "spoken") {
			t.Fatalf("mode=%q instructions=%v", mode, r.SystemInstructions)
		}
		if mode == "spoken" && (!strings.Contains(r.SystemInstructions[1], "not authenticated approval") || !strings.Contains(r.SystemInstructions[1], "runOutput.silent=true")) {
			t.Fatal("spoken contract missing silence or authority boundary")
		}
	}
}

func TestSpokenSilenceCompletesWithoutPublishingAnAudibleStatus(t *testing.T) {
	for _, mode := range []string{"", "spoken"} {
		t.Run(mode, func(t *testing.T) {
			store := NewMemoryStore()
			service := NewConversationService(store)
			scope := Scope{Kind: "tenant", ID: "speech"}
			c, _, err := service.CreateConversation(t.Context(), CreateConversationRequest{Scope: scope,
				Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, Title: "Voice", IdempotencyKey: "chat"})
			if err != nil {
				t.Fatal(err)
			}
			posted, err := service.PostChannelMessage(t.Context(), PostChannelMessageRequest{Scope: scope, ConversationID: c.ID, ExpectedRevision: c.Revision,
				Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "external"}, Intent: MessageIntentQuestion,
				Content: "Ambient conversation", Audience: ConversationAudience{Kind: ConversationAudienceChannel},
				RequiresResponse: true, ResponseMode: mode, IdempotencyKey: "utterance"})
			if err != nil {
				t.Fatal(err)
			}
			scheduler, err := NewConversationRunScheduler(store, store, ConversationRunSchedulerConfig{})
			if err != nil {
				t.Fatal(err)
			}
			scheduled, _, err := scheduler.ScheduleMessage(t.Context(), scope, c.ID, posted.Message.ID)
			if err != nil {
				t.Fatal(err)
			}
			resolver := TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*TurnRunnerBinding, error) {
				return &TurnRunnerBinding{DeploymentID: "agent", Runner: TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
					return &TurnOutcome{NextRunStatus: AgentRunStatusCompleted, OutputSummary: "No response needed", RunOutput: map[string]interface{}{"silent": true}}, nil
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
			if mode == "spoken" {
				if err != nil || outcome.RunOutput["silent"] != true {
					t.Fatalf("outcome=%+v err=%v", outcome, err)
				}
			} else if err == nil {
				t.Fatal("ordinary chat silently completed")
			}
			messages, err := service.ListChannelMessages(t.Context(), ChannelMessageFilter{Scope: scope, ConversationID: c.ID})
			if err != nil || len(messages) != 1 {
				t.Fatalf("published a status as speech: messages=%d err=%v", len(messages), err)
			}
		})
	}
}
