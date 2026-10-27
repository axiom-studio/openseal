package presentation

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestInteractionWave2(t *testing.T) {
	cards := deck(3)
	rules := []struct {
		component Component
		actionID  string
		want      InteractionRule
	}{
		{cards, FlashcardsMark, InteractionRule{Kind: InteractionMark, ItemKeyed: true}},
		{cards, FlashcardsDone, InteractionRule{Kind: InteractionDone, SingleUse: true, NotifyAgent: true}},
		{recipeOf(true), "", InteractionRule{Kind: InteractionCheck, ItemKeyed: true, Shared: true}},
		{draftOf(nil), DraftRespond, InteractionRule{Kind: InteractionRespond, SingleUse: true, NotifyAgent: true}},
	}
	for _, tc := range rules {
		if rule, err := Interaction(tc.component, tc.actionID); err != nil || rule != tc.want {
			t.Fatalf("%s %q: rule = %#v, %v", tc.component.Type, tc.actionID, rule, err)
		}
	}
	for name, call := range map[string]func() error{
		"flashcards without action": func() error { _, err := Interaction(cards, ""); return err },
		"flashcards flip":           func() error { _, err := Interaction(cards, "flip"); return err },
		"draft edit is client-only": func() error { _, err := Interaction(draftOf(nil), DraftEdit); return err },
		"draft without action":      func() error { _, err := Interaction(draftOf(nil), ""); return err },
		"bound draft": func() error {
			_, err := Interaction(draftOf(&Binding{Kind: BindingApproval, ID: "ap"}), DraftRespond)
			return err
		},
		"recipe not checkable": func() error { _, err := Interaction(recipeOf(false), ""); return err },
		"recipe with action":   func() error { _, err := Interaction(recipeOf(true), "check"); return err },
		"math":                 func() error { _, err := Interaction(mathOf("x"), ""); return err },
		"item": func() error {
			_, err := Interaction(Component{ID: "i", Type: "item", Item: &Item{Name: "x"}}, "")
			return err
		},
		"edit reaches no value check": func() error { return ValidateInteractionValues(draftOf(nil), DraftEdit, nil) },
	} {
		if call() == nil {
			t.Fatalf("%s: expected an error", name)
		}
	}
	items := []struct {
		component         Component
		actionID, itemKey string
		err               string
	}{
		{cards, FlashcardsMark, "c1", ""},
		{cards, FlashcardsMark, "", "need the item id"},
		{cards, FlashcardsMark, "c9", `flashcards "deck" has no item "c9"`},
		{cards, FlashcardsDone, "c1", "do not take an item key"},
		{recipeOf(true), "", "eggs", ""},
		{recipeOf(true), "", "mix", `recipe "r" has no item "mix"`},
		{draftOf(nil), DraftRespond, "x", "do not take an item key"},
	}
	for _, tc := range items {
		err := ValidateInteractionItem(tc.component, tc.actionID, tc.itemKey)
		if tc.err == "" && err != nil || tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)) {
			t.Fatalf("%s %s %q: error = %v, want %q", tc.component.Type, tc.actionID, tc.itemKey, err, tc.err)
		}
	}
	values := []struct {
		component Component
		actionID  string
		values    map[string]interface{}
		err       string
	}{
		{cards, FlashcardsMark, map[string]interface{}{"result": "got"}, ""},
		{cards, FlashcardsMark, map[string]interface{}{"result": "again"}, ""},
		{cards, FlashcardsMark, map[string]interface{}{"result": "maybe"}, "result must be one of got, again"},
		{cards, FlashcardsMark, map[string]interface{}{"result": "got", "x": 1.0}, `unexpected value "x"`},
		{cards, FlashcardsDone, nil, ""},
		{cards, FlashcardsDone, map[string]interface{}{"got": 3.0}, "done carries no values"},
		{recipeOf(true), "", map[string]interface{}{"checked": true}, ""},
		{recipeOf(true), "", map[string]interface{}{"checked": "yes"}, "checked must be true or false"},
		{draftOf(nil), DraftRespond, map[string]interface{}{"response": "looks_good"}, ""},
		{draftOf(nil), DraftRespond, map[string]interface{}{"response": "shorter"}, ""},
		{draftOf(nil), DraftRespond, map[string]interface{}{"response": "longer"}, "response must be one of looks_good, shorter"},
		{draftOf(nil), DraftRespond, nil, "response is required"},
	}
	for _, tc := range values {
		err := ValidateInteractionValues(tc.component, tc.actionID, tc.values)
		if tc.err == "" && err != nil || tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)) {
			t.Fatalf("%s %s %v: error = %v, want %q", tc.component.Type, tc.actionID, tc.values, err, tc.err)
		}
	}
}

func TestFlashcardsOutcome(t *testing.T) {
	cards := deck(5)
	tally, err := FlashcardsOutcome(cards, map[string]string{"c0": "got", "c1": "got", "c2": "again", "c9": "got", "c3": "maybe"})
	if err != nil || tally != (FlashcardsTally{Got: 2, Again: 1, Unseen: 2}) {
		t.Fatalf("tally = %#v, %v", tally, err)
	}
	if tally, err := FlashcardsOutcome(cards, nil); err != nil || tally != (FlashcardsTally{Unseen: 5}) {
		t.Fatalf("empty tally = %#v, %v", tally, err)
	}
	if _, err := FlashcardsOutcome(mathOf("x"), nil); err == nil {
		t.Fatal("tallied a math component")
	}
	for raw, want := range map[string]string{`{"result":"got"}`: "got", `{"result":"again"}`: "again", `{"result":"meh"}`: "", `{}`: "", `nope`: ""} {
		if got, ok := FlashcardMark(json.RawMessage(raw)); got != want || ok != (want != "") {
			t.Fatalf("FlashcardMark(%s) = %q, %v", raw, got, ok)
		}
	}
	encoded, _ := json.Marshal(tally)
	if string(encoded) != `{"got":2,"again":1,"unseen":2}` {
		t.Fatalf("outcome JSON = %s", encoded)
	}
}

func TestCardResponseTextWave2(t *testing.T) {
	raw := func(value string) json.RawMessage { return json.RawMessage(value) }
	untitled := draftOf(nil)
	untitled.Title = ""
	cases := []struct {
		component         Component
		actionID, itemKey string
		values, outcome   string
		want              string
	}{
		{deck(15), FlashcardsDone, "", `{}`, `{"got":12,"again":3,"unseen":0}`, `Card response (flashcards "French food words"): 12 got, 3 again`},
		{deck(20), FlashcardsDone, "", `{}`, `{"got":12,"again":3,"unseen":5}`, `Card response (flashcards "French food words"): 12 got, 3 again, 5 unseen`},
		{deck(3), FlashcardsMark, "c1", `{"result":"again"}`, ``, `Card response (flashcards "French food words"): marked "front 1" again`},
		{draftOf(nil), DraftRespond, "", `{"response":"looks_good"}`, ``, `Card response (draft "Reply to Sam"): "Looks good"`},
		{untitled, DraftRespond, "", `{"response":"shorter"}`, ``, `Card response (draft "reply"): "Make it shorter"`},
		{recipeOf(true), "", "eggs", `{"checked":true}`, ``, `Card response (recipe "Pancakes"): ticked "eggs"`},
	}
	for _, tc := range cases {
		if got := CardResponseText(tc.component, tc.actionID, tc.itemKey, raw(tc.values), raw(tc.outcome)); got != tc.want {
			t.Fatalf("CardResponseText = %s, want %s", got, tc.want)
		}
	}
}

func TestCardStateLinesWave2(t *testing.T) {
	if line, ok := CardStateLine(recipeOf(true), map[string]bool{"flour": true, "eggs": true}); !ok || line != `Recipe "Pancakes": 2/3 ingredients ticked` {
		t.Fatalf("recipe line = %q, %v", line, ok)
	}
	if line, ok := FlashcardsStateLine(deck(4), map[string]string{"c0": "got", "c1": "again"}); !ok || line != `Flashcards "French food words": 1 got, 1 again, 2 unseen` {
		t.Fatalf("flashcards line = %q, %v", line, ok)
	}
	if _, ok := FlashcardsStateLine(recipeOf(true), nil); ok {
		t.Fatal("flashcards line for a recipe")
	}
	if _, ok := CardStateLine(deck(2), nil); ok {
		t.Fatal("tick line for flashcards")
	}
}
