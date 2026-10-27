package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/presentation"
)

func cardStateTestVisual() *presentation.Visual {
	visual := &presentation.Visual{Title: "Trip"}
	for index := 0; index < 7; index++ {
		title := fmt.Sprintf("List %d", index)
		if index == 0 {
			title = "Packing list"
		}
		visual.Components = append(visual.Components, presentation.Component{
			ID: fmt.Sprintf("k%d", index), Type: "checklist", Title: title, Summary: "Things to pack.",
			Checklist: &presentation.Checklist{Items: []presentation.ChecklistItem{{ID: "a", Label: "Passport"}, {ID: "b", Label: "Charger"}, {ID: "c", Label: "Towel"}}},
		})
	}
	visual.Components = append(visual.Components, presentation.Component{ID: "q", Type: "quiz", Summary: "One question.", Quiz: &presentation.Quiz{
		Question: "Capital of Australia?", Options: []presentation.QuizOption{{ID: "syd", Label: "Sydney"}, {ID: "cbr", Label: "Canberra"}},
		AnswerKey: &presentation.QuizAnswerKey{Correct: []string{"cbr"}, Explanation: "Canberra."},
	}})
	return visual
}

func cardStateOfGoal(t *testing.T, goal string) []string {
	t.Helper()
	index := strings.Index(goal, "\n\n{")
	if index < 0 {
		t.Fatalf("goal has no context payload: %q", goal)
	}
	var payload struct {
		CardState       []string `json:"cardState"`
		ContextGuidance string   `json:"contextGuidance"`
	}
	if err := json.Unmarshal([]byte(goal[index+2:]), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.CardState) > 0 && !strings.Contains(payload.ContextGuidance, "CardState is what people ticked") {
		t.Fatalf("card state without guidance: %q", payload.ContextGuidance)
	}
	return payload.CardState
}

func TestAgentTurnSeesCardTicksSinceItsLastMessage(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "card-state"}
	service := NewConversationService(store)
	conversation, _, err := service.CreateConversation(ctx, CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent-42"}, Title: "Trip", IdempotencyKey: "card-state-channel",
	})
	if err != nil {
		t.Fatal(err)
	}
	var goals []string
	agentTurns := TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*TurnRunnerBinding, error) {
		return &TurnRunnerBinding{
			DeploymentID: "agent-42", DefinitionID: "agent-definition", DefinitionVersion: "1",
			Runner: TurnRunnerFunc(func(_ context.Context, input TurnExecutionContext) (*TurnOutcome, error) {
				goals = append(goals, input.Run.Goal)
				outcome := &TurnOutcome{NextRunStatus: AgentRunStatusCompleted, RunOutput: map[string]interface{}{"summary": "Here you go."}}
				if len(goals) == 1 {
					outcome.Visual = cardStateTestVisual()
				}
				return outcome, nil
			}),
		}, nil
	})
	content := &answerVisualContent{}
	runner, err := NewConversationRunTurnRunner(store, conversationRunTestCoordinator(t, service), ConversationRunTurnRunnerConfig{AgentTurns: agentTurns, AttachmentContent: content})
	if err != nil {
		t.Fatal(err)
	}
	turn := func(text string) {
		t.Helper()
		current, err := service.GetConversation(ctx, scope, conversation.ID)
		if err != nil {
			t.Fatal(err)
		}
		trigger := postConversationRunTestMessage(t, service, current, ConversationParticipantUser, MessageIntentQuestion, text, "card-state-"+text)
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
	}

	turn("Packing list please")
	if state := cardStateOfGoal(t, goals[0]); len(state) != 0 {
		t.Fatalf("first turn card state = %q", state)
	}
	messages, err := service.ListChannelMessages(ctx, ChannelMessageFilter{Scope: scope, ConversationID: conversation.ID})
	if err != nil || len(messages) != 2 {
		t.Fatalf("messages = %#v, err=%v", messages, err)
	}
	card := messages[1]
	var artifactID string
	for _, reference := range card.References {
		if reference.Kind == ConversationReferenceArtifact {
			artifactID = reference.ID
		}
	}
	record := func(actor, componentID, itemKey, values, key string, repeatable bool) {
		t.Helper()
		if _, _, err := service.RecordSurfaceInteraction(ctx, RecordSurfaceInteractionRequest{
			Scope: scope, ConversationID: conversation.ID, SurfaceArtifactID: artifactID, SurfaceID: artifactID, SurfaceVersion: 1,
			MessageID: card.ID, ComponentID: componentID, ItemKey: itemKey, Actor: ConversationParticipant{Type: ConversationParticipantUser, ID: actor},
			Repeatable: repeatable, Values: json.RawMessage(values), IdempotencyKey: key,
		}); err != nil {
			t.Fatal(err)
		}
	}
	for index := 0; index < 7; index++ {
		record("sender-user", fmt.Sprintf("k%d", index), "a", `{"checked":true}`, fmt.Sprintf("tick-%d", index), true)
	}
	record("partner", "k0", "b", `{"checked":true}`, "tick-b", true)
	record("sender-user", "k0", "b", `{"checked":false}`, "untick-b", true)
	record("sender-user", "q", "", `{"selected":["cbr"]}`, "quiz", false)

	turn("Anything missing?")
	want := []string{"Packing list: 1/3 ticked (unticked: Charger, Towel)"}
	for index := 1; index < 6; index++ {
		want = append(want, fmt.Sprintf("List %d: 1/3 ticked (unticked: Charger, Towel)", index))
	}
	if state := cardStateOfGoal(t, goals[1]); strings.Join(state, "\n") != strings.Join(want, "\n") {
		t.Fatalf("card state = %q, want %q", state, want)
	}

	turn("Thanks")
	if state := cardStateOfGoal(t, goals[2]); len(state) != 0 {
		t.Fatalf("unchanged cards reported again: %q", state)
	}
}
