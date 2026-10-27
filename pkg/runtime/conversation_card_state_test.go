package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

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

func TestTeamParticipantsSeeCardTicksSinceTheirLastMessage(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "team-card-state"}
	service := NewConversationService(store)
	conversation, _, err := service.CreateConversation(ctx, CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "family"}, Title: "Trip", IdempotencyKey: "team-card-state-channel",
	})
	if err != nil {
		t.Fatal(err)
	}
	participants := ConversationParticipantSourceFunc(func(context.Context, ConversationParticipantQuery) ([]ConversationParticipantBinding, error) {
		return []ConversationParticipantBinding{
			{Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "planner"}, SemanticRoles: []string{"planner"}},
			{Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "quiet"}, SemanticRoles: []string{"helper"}},
		}, nil
	})
	seen := map[string][][]string{}
	proposals := ParticipationProposalProviderFunc(func(_ context.Context, input ParticipationProposalContext) (ParticipationProposal, error) {
		seen[input.Participant.ID] = append(seen[input.Participant.ID], append([]string(nil), input.CardState...))
		if input.Participant.ID == "quiet" {
			return ParticipationProposal{}, nil
		}
		proposal := ParticipationProposal{
			WantsToSpeak: true, Intent: MessageIntentAnswer, Content: fmt.Sprintf("Reply %d.", len(seen["planner"])),
			Audience: ConversationAudience{Kind: ConversationAudienceChannel},
			Signals:  ParticipationSignals{AnswersOpenQuestion: true, HasNewInformation: true, RoleRelevant: true},
		}
		if len(seen["planner"]) == 1 {
			proposal.Visual = cardStateTestVisual()
		}
		return proposal, nil
	})
	coordinator, err := NewConversationCoordinator(service, participants, proposals, DefaultConversationCoordinatorConfig())
	if err != nil {
		t.Fatal(err)
	}
	runner, err := NewConversationRunTurnRunner(store, coordinator, ConversationRunTurnRunnerConfig{AttachmentContent: &answerVisualContent{}})
	if err != nil {
		t.Fatal(err)
	}
	turn := func(text string) {
		t.Helper()
		current, err := service.GetConversation(ctx, scope, conversation.ID)
		if err != nil {
			t.Fatal(err)
		}
		trigger := postConversationRunTestMessage(t, service, current, ConversationParticipantUser, MessageIntentQuestion, text, "team-card-state-"+text)
		scheduled, _, err := mustConversationRunScheduler(t, store).ScheduleMessage(ctx, scope, conversation.ID, trigger.ID)
		if err != nil {
			t.Fatal(err)
		}
		if outcome, err := runner.RunTurn(ctx, TurnExecutionContext{Run: scheduled.Run}); err != nil || outcome.NextRunStatus != AgentRunStatusCompleted {
			t.Fatalf("outcome = %#v, err=%v", outcome, err)
		}
	}

	turn("Packing list please")
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
	if _, _, err := service.RecordSurfaceInteraction(ctx, RecordSurfaceInteractionRequest{
		Scope: scope, ConversationID: conversation.ID, SurfaceArtifactID: artifactID, SurfaceID: artifactID, SurfaceVersion: 1,
		MessageID: card.ID, ComponentID: "k0", ItemKey: "a", Actor: ConversationParticipant{Type: ConversationParticipantUser, ID: "sender-user"},
		Repeatable: true, Values: json.RawMessage(`{"checked":true}`), IdempotencyKey: "tick",
	}); err != nil {
		t.Fatal(err)
	}
	turn("Anything missing?")
	turn("Thanks")
	want := "Packing list: 1/3 ticked (unticked: Charger, Towel)"
	planner, quiet := seen["planner"], seen["quiet"]
	if len(planner) != 3 || len(quiet) != 3 {
		t.Fatalf("proposals planner=%q quiet=%q", planner, quiet)
	}
	if len(planner[0]) != 0 || strings.Join(planner[1], "\n") != want || len(planner[2]) != 0 {
		t.Fatalf("planner card state = %q", planner)
	}
	// The quiet member never spoke, so the tick is news to it on every round.
	if strings.Join(quiet[1], "\n") != want || strings.Join(quiet[2], "\n") != want {
		t.Fatalf("quiet card state = %q", quiet)
	}
}

func cardStateWave2Visual() *presentation.Visual {
	cards := []presentation.Flashcard{{ID: "pain", Front: "le pain", Back: "bread"}, {ID: "eau", Front: "l'eau", Back: "water"}, {ID: "pomme", Front: "la pomme", Back: "apple"}}
	qty := 2.0
	return &presentation.Visual{Title: "Learn and cook", Components: []presentation.Component{
		{ID: "deck", Type: "flashcards", Title: "French food words", Summary: "Three words.", Flashcards: &presentation.Flashcards{Cards: cards}},
		{ID: "pancakes", Type: "recipe", Title: "Pancakes", Summary: "Pancakes for 2.", Recipe: &presentation.Recipe{
			Servings: 2, Checkable: true, Steps: []presentation.RecipeStep{{ID: "mix", Text: "Whisk into a batter and fry."}},
			Ingredients: []presentation.RecipeIngredient{{ID: "flour", Name: "flour"}, {ID: "eggs", Qty: &qty, Name: "eggs"}},
		}},
	}}
}

func TestAgentTurnSeesFlashcardMarksAndRecipeTicks(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "card-state-wave2"}
	service := NewConversationService(store)
	conversation, _, err := service.CreateConversation(ctx, CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent-42"}, Title: "Learn", IdempotencyKey: "card-state-wave2-channel",
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
					outcome.Visual = cardStateWave2Visual()
				}
				return outcome, nil
			}),
		}, nil
	})
	runner, err := NewConversationRunTurnRunner(store, conversationRunTestCoordinator(t, service), ConversationRunTurnRunnerConfig{AgentTurns: agentTurns, AttachmentContent: &answerVisualContent{}})
	if err != nil {
		t.Fatal(err)
	}
	turn := func(text string) {
		t.Helper()
		current, err := service.GetConversation(ctx, scope, conversation.ID)
		if err != nil {
			t.Fatal(err)
		}
		trigger := postConversationRunTestMessage(t, service, current, ConversationParticipantUser, MessageIntentQuestion, text, "card-state-wave2-"+text)
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

	turn("Teach me and feed me")
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
	record := func(actor, componentID, actionID, itemKey, values, key string) {
		t.Helper()
		if _, _, err := service.RecordSurfaceInteraction(ctx, RecordSurfaceInteractionRequest{
			Scope: scope, ConversationID: conversation.ID, SurfaceArtifactID: artifactID, SurfaceID: artifactID, SurfaceVersion: 1,
			MessageID: card.ID, ComponentID: componentID, ActionID: actionID, ItemKey: itemKey, Actor: ConversationParticipant{Type: ConversationParticipantUser, ID: actor},
			Repeatable: true, Values: json.RawMessage(values), IdempotencyKey: key,
		}); err != nil {
			t.Fatal(err)
		}
	}
	record("sender-user", "deck", "mark", "pain", `{"result":"again"}`, "mark-1")
	record("sender-user", "deck", "mark", "pain", `{"result":"got"}`, "mark-2")
	record("sender-user", "deck", "mark", "eau", `{"result":"again"}`, "mark-3")
	record("sender-user", "pancakes", "", "flour", `{"checked":true}`, "tick-1")

	turn("How am I doing?")
	want := []string{`Flashcards "French food words": 1 got, 1 again, 1 unseen`, `Recipe "Pancakes": 1/2 ingredients ticked`}
	if state := cardStateOfGoal(t, goals[1]); strings.Join(state, "\n") != strings.Join(want, "\n") {
		t.Fatalf("card state = %q, want %q", state, want)
	}
	turn("Thanks")
	if state := cardStateOfGoal(t, goals[2]); len(state) != 0 {
		t.Fatalf("unchanged cards reported again: %q", state)
	}
}

func TestFlashcardStateLinesArePerPerson(t *testing.T) {
	component := cardStateWave2Visual().Components[0]
	since := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	mark := func(actor, card, result string, at time.Time) *SurfaceInteraction {
		return &SurfaceInteraction{ComponentID: "deck", ActionID: "mark", ItemKey: card, ActorType: ConversationParticipantUser, ActorID: actor,
			Repeatable: true, Values: json.RawMessage(`{"result":"` + result + `"}`), CreatedAt: at}
	}
	names := map[ConversationParticipant]string{{Type: ConversationParticipantUser, ID: "ann"}: "Ann"}
	latest := []*SurfaceInteraction{
		mark("ann", "pain", "got", since.Add(time.Minute)), mark("ann", "eau", "got", since.Add(-time.Hour)),
		mark("bob", "pain", "again", since), mark("cat", "pomme", "got", since.Add(-time.Minute)),
		{ComponentID: "pancakes", ItemKey: "flour", ActorType: ConversationParticipantUser, ActorID: "dan", Repeatable: true, Values: json.RawMessage(`{"checked":true}`), CreatedAt: since},
	}
	got := flashcardStateLines(component, latest, since, names)
	want := []string{`Ann: Flashcards "French food words": 2 got, 0 again, 1 unseen`, `Someone: Flashcards "French food words": 0 got, 1 again, 2 unseen`}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("lines = %q, want %q", got, want)
	}
	if got := flashcardStateLines(component, latest[:2], since, names); len(got) != 1 || strings.HasPrefix(got[0], "Ann") {
		t.Fatalf("single person lines = %q", got)
	}
}
