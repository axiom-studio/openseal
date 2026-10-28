package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestAgentTurnCarriesTheUsersSavedImageStyle(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "image-style"}
	service := NewConversationService(store)
	var goals []string
	agentTurns := TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*TurnRunnerBinding, error) {
		return &TurnRunnerBinding{
			DeploymentID: "agent-42", DefinitionID: "agent-definition", DefinitionVersion: "1",
			Runner: TurnRunnerFunc(func(_ context.Context, input TurnExecutionContext) (*TurnOutcome, error) {
				goals = append(goals, input.Run.Goal)
				return &TurnOutcome{NextRunStatus: AgentRunStatusCompleted, RunOutput: map[string]interface{}{"summary": "Here you go."}}, nil
			}),
		}, nil
	})
	// The host reads the style saved by the sender of the run's trigger.
	styles := map[string]string{"Draw a fox": " cartoon "}
	runner, err := NewConversationRunTurnRunner(store, conversationRunTestCoordinator(t, service), ConversationRunTurnRunnerConfig{
		AgentTurns: agentTurns,
		ImageStyle: func(_ context.Context, run *AgentRun) string {
			conversationID, _ := run.Context[conversationRunContextConversationID].(string)
			triggerID, _ := run.Context[conversationRunContextTriggerID].(string)
			message, err := service.GetChannelMessage(ctx, scope, conversationID, triggerID)
			if err != nil {
				t.Fatal(err)
			}
			return styles[message.Content]
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	turn := func(text string) (*string, string) {
		t.Helper()
		conversation, _, err := service.CreateConversation(ctx, CreateConversationRequest{
			Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent-42"}, Title: text, IdempotencyKey: "image-style-" + text,
		})
		if err != nil {
			t.Fatal(err)
		}
		trigger := postConversationRunTestMessage(t, service, conversation, ConversationParticipantUser, MessageIntentQuestion, text, "image-style-"+text)
		scheduled, _, err := mustConversationRunScheduler(t, store).ScheduleMessage(ctx, scope, conversation.ID, trigger.ID)
		if err != nil {
			t.Fatal(err)
		}
		binding, err := runner.ResolveTurnRunner(ctx, scheduled.Run)
		if err != nil {
			t.Fatal(err)
		}
		if outcome, err := binding.Runner.RunTurn(ctx, TurnExecutionContext{Run: scheduled.Run}); err != nil || outcome.NextRunStatus != AgentRunStatusCompleted {
			t.Fatalf("outcome = %#v, err=%v", outcome, err)
		}
		goal := goals[len(goals)-1]
		index := strings.Index(goal, "\n\n{")
		if index < 0 {
			t.Fatalf("goal has no context payload: %q", goal)
		}
		var payload struct {
			ImageStyle      *string `json:"imageStyle"`
			ContextGuidance string  `json:"contextGuidance"`
		}
		if err := json.Unmarshal([]byte(goal[index+2:]), &payload); err != nil {
			t.Fatal(err)
		}
		return payload.ImageStyle, payload.ContextGuidance
	}

	style, guidance := turn("Draw a fox")
	if style == nil || *style != "cartoon" || !strings.Contains(guidance, "generate_image applies it automatically. Pass style only to override it for one image when they ask.") {
		t.Fatalf("style %v, guidance %q", style, guidance)
	}
	style, guidance = turn("Draw an owl")
	if style != nil || strings.Contains(guidance, "picture style") {
		t.Fatalf("unsaved style: %v, guidance %q", style, guidance)
	}
}
