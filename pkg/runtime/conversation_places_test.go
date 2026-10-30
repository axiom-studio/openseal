package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestAgentTurnListsThePlacesTheChatAlreadyFound(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "places"}
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
	found := map[string][]ConversationPlace{}
	runner, err := NewConversationRunTurnRunner(store, conversationRunTestCoordinator(t, service), ConversationRunTurnRunnerConfig{
		AgentTurns: agentTurns,
		Places: func(_ context.Context, run *AgentRun) []ConversationPlace {
			conversationID, _ := run.Context[conversationRunContextConversationID].(string)
			return found[conversationID]
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	turn := func(text string, places int) ([]ConversationPlace, string) {
		t.Helper()
		conversation, _, err := service.CreateConversation(ctx, CreateConversationRequest{
			Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent-42"}, Title: text, IdempotencyKey: "places-" + text,
		})
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < places; i++ {
			found[conversation.ID] = append(found[conversation.ID], ConversationPlace{Place: fmt.Sprintf("place:%012x", i), Name: fmt.Sprintf("Stop %d", i), Lat: 37.4, Lon: -121.9})
		}
		trigger := postConversationRunTestMessage(t, service, conversation, ConversationParticipantUser, MessageIntentQuestion, text, "places-"+text)
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
			Places          []ConversationPlace `json:"places"`
			ContextGuidance string              `json:"contextGuidance"`
		}
		if err := json.Unmarshal([]byte(goal[index+2:]), &payload); err != nil {
			t.Fatal(err)
		}
		return payload.Places, payload.ContextGuidance
	}

	places, guidance := turn("Driving directions please", MaximumConversationPlaces+5)
	if len(places) != MaximumConversationPlaces || places[0].Place != "place:000000000000" || places[0].Name != "Stop 0" || !strings.Contains(guidance, "Use their place ids for maps, near and route instead of searching for them again.") {
		t.Fatalf("places %#v, guidance %q", places, guidance)
	}
	places, guidance = turn("Hello", 0)
	if places != nil || strings.Contains(guidance, "find_place") {
		t.Fatalf("a chat without places: %#v, guidance %q", places, guidance)
	}
}
