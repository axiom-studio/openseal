package runtime

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
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

func TestFlashcardStateLinesArePerPerson(t *testing.T) {
	component := cardStateWave2Visual().Components[0]
	since := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	mark := func(actor, card, result string, at time.Time) *models.SurfaceInteraction {
		return &models.SurfaceInteraction{ComponentID: "deck", ActionID: "mark", ItemKey: card, ActorType: models.ConversationParticipantUser, ActorID: actor,
			Repeatable: true, Values: json.RawMessage(`{"result":"` + result + `"}`), CreatedAt: at}
	}
	names := map[models.ConversationParticipant]string{{Type: models.ConversationParticipantUser, ID: "ann"}: "Ann"}
	latest := []*models.SurfaceInteraction{
		mark("ann", "pain", "got", since.Add(time.Minute)), mark("ann", "eau", "got", since.Add(-time.Hour)),
		mark("bob", "pain", "again", since), mark("cat", "pomme", "got", since.Add(-time.Minute)),
		{ComponentID: "pancakes", ItemKey: "flour", ActorType: models.ConversationParticipantUser, ActorID: "dan", Repeatable: true, Values: json.RawMessage(`{"checked":true}`), CreatedAt: since},
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

func TestPollVotesKeepEachPersonsLatestVote(t *testing.T) {
	latest := []*models.SurfaceInteraction{
		{ComponentID: "vote", ActionID: presentation.PollVoteAction, ActorType: models.ConversationParticipantUser, ActorID: "ana", Values: json.RawMessage(`{"optionIds":["thai"]}`)},
		{ComponentID: "vote", ActionID: presentation.PollVoteAction, ActorType: models.ConversationParticipantUser, ActorID: "ben", Values: json.RawMessage(`{"optionIds":["pub","thai"]}`)},
		{ComponentID: "other", ActionID: presentation.PollVoteAction, ActorType: models.ConversationParticipantUser, ActorID: "cy", Values: json.RawMessage(`{"optionIds":["thai"]}`)},
		{ComponentID: "vote", ActionID: "share", ActorType: models.ConversationParticipantUser, ActorID: "dee", Values: json.RawMessage(`{"optionIds":["thai"]}`)},
		nil,
	}
	votes := PollVotes("vote", latest)
	if len(votes) != 2 || strings.Join(votes["user:ana"], ",") != "thai" || strings.Join(votes["user:ben"], ",") != "pub,thai" {
		t.Fatalf("votes = %v", votes)
	}
	if key := PollVoterKey(models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "ana"}); key != "user:ana" {
		t.Fatalf("key = %q", key)
	}
}
