package presentation

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
)

// InteractionKind names a stored card interaction.
type InteractionKind string

const (
	InteractionSubmit  InteractionKind = "submit"  // form submit
	InteractionRespond InteractionKind = "respond" // actions respond button, choices chip
	InteractionPick    InteractionKind = "pick"    // comparison option
	InteractionCheck   InteractionKind = "check"   // checklist item, checkable step
	InteractionAnswer  InteractionKind = "answer"  // quiz answer
	InteractionMark    InteractionKind = "mark"    // flashcard got/again
	InteractionDone    InteractionKind = "done"    // flashcards finished
	InteractionVote    InteractionKind = "vote"    // poll vote
	InteractionShare   InteractionKind = "share"   // explorer result shared
)

// Components with more than one interaction name it in the action id:
// flashcards take FlashcardsMark or FlashcardsDone, drafts take DraftRespond,
// polls PollVoteAction and explorers ExplorerShareAction. DraftEdit opens the
// composer on the client and is never stored.
const (
	FlashcardsMark      = "mark"
	FlashcardsDone      = "done"
	DraftRespond        = "respond"
	DraftEdit           = "edit"
	PollVoteAction      = "vote"
	ExplorerShareAction = "share"
)

// Flashcard mark results (values {"result": ...}) and draft responses
// (values {"response": ...}).
const (
	FlashcardGot     = "got"
	FlashcardAgain   = "again"
	DraftLooksGood   = "looks_good"
	DraftMakeShorter = "shorter"
)

// DraftResponses maps each draft response to its button label.
var DraftResponses = map[string]string{DraftLooksGood: "Looks good", DraftMakeShorter: "Make it shorter"}

// InteractionRule describes how the host stores one interaction.
type InteractionRule struct {
	Kind InteractionKind
	// SingleUse interactions are unique per (surface, version, component,
	// actor); others are keyed by idempotency key.
	SingleUse bool
	// NotifyAgent interactions post an answer that wakes the agent. Others
	// reach it as card state on its next turn.
	NotifyAgent bool
	// ItemKeyed interactions name one item of the component (its item id) in
	// the interaction's item key; state is the last write per item.
	ItemKeyed bool
	// Shared state is read across actors: everyone who can see the card sees
	// and changes the same ticks.
	Shared bool
	// WakeWhenEveryoneVoted (poll votes) wakes the agent once per visual
	// version, the first time EveryoneVoted reports that every human
	// participant has a vote stored; other votes do not wake it.
	WakeWhenEveryoneVoted bool
}

var checkRule = InteractionRule{Kind: InteractionCheck, ItemKeyed: true, Shared: true}

// Interaction reports which stored interaction a component accepts. actionID
// selects the button of an actions component, the interaction of flashcards
// (mark or done), of a draft (respond), of a poll (vote) and of an explorer
// (share), and must be empty otherwise.
// Approval and entity buttons use their existing endpoints and edit buttons
// are client-only, so none of them is a card interaction; neither are the
// buttons of a draft bound to an approval.
func Interaction(component Component, actionID string) (InteractionRule, error) {
	if !contains([]string{"actions", "flashcards", "draft", "poll", "explorer"}, component.Type) && actionID != "" {
		return InteractionRule{}, fmt.Errorf("%s interactions do not take an action id", component.Type)
	}
	switch component.Type {
	case "form":
		return InteractionRule{Kind: InteractionSubmit, SingleUse: true, NotifyAgent: true}, nil
	case "actions":
		for _, action := range component.Actions {
			if action.ID != actionID {
				continue
			}
			if action.Kind != ActionRespond {
				return InteractionRule{}, fmt.Errorf("action %q is a %s action and is not recorded as a card interaction", actionID, action.Kind)
			}
			return InteractionRule{Kind: InteractionRespond, SingleUse: true, NotifyAgent: true}, nil
		}
		return InteractionRule{}, fmt.Errorf("component %q has no action %q", component.ID, actionID)
	case "comparison":
		if component.Comparison == nil || !component.Comparison.Pickable {
			return InteractionRule{}, fmt.Errorf("comparison %q is not pickable", component.ID)
		}
		return InteractionRule{Kind: InteractionPick, SingleUse: true, NotifyAgent: true}, nil
	case "choices":
		// Every tap is its own reply, deduplicated by idempotency key.
		return InteractionRule{Kind: InteractionRespond, NotifyAgent: true}, nil
	case "steps":
		if component.Steps == nil || !component.Steps.Checkable {
			return InteractionRule{}, fmt.Errorf("steps %q are not checkable", component.ID)
		}
		return checkRule, nil
	case "checklist":
		return checkRule, nil
	case "recipe":
		if component.Recipe == nil || !component.Recipe.Checkable {
			return InteractionRule{}, fmt.Errorf("recipe %q is not checkable", component.ID)
		}
		return checkRule, nil
	case "flashcards":
		switch actionID {
		case FlashcardsMark:
			// Each person's own progress, last write per card; no wake.
			return InteractionRule{Kind: InteractionMark, ItemKeyed: true}, nil
		case FlashcardsDone:
			return InteractionRule{Kind: InteractionDone, SingleUse: true, NotifyAgent: true}, nil
		}
		return InteractionRule{}, fmt.Errorf("flashcards interactions take the action id %q or %q (got %q)", FlashcardsMark, FlashcardsDone, actionID)
	case "draft":
		if component.Binding != nil {
			return InteractionRule{}, fmt.Errorf("draft %q is bound to an approval; its buttons decide that approval and are not card interactions", component.ID)
		}
		switch actionID {
		case DraftRespond:
			return InteractionRule{Kind: InteractionRespond, SingleUse: true, NotifyAgent: true}, nil
		case DraftEdit:
			return InteractionRule{}, fmt.Errorf("draft edit opens the composer and is not recorded as a card interaction")
		}
		return InteractionRule{}, fmt.Errorf("draft interactions take the action id %q (got %q)", DraftRespond, actionID)
	case "quiz":
		return InteractionRule{Kind: InteractionAnswer, SingleUse: true, NotifyAgent: true}, nil
	case "poll":
		if actionID != PollVoteAction {
			return InteractionRule{}, fmt.Errorf("poll interactions take the action id %q (got %q)", PollVoteAction, actionID)
		}
		// Changeable: the latest vote per person counts.
		return InteractionRule{Kind: InteractionVote, Shared: true, WakeWhenEveryoneVoted: true}, nil
	case "explorer":
		if actionID != ExplorerShareAction {
			return InteractionRule{}, fmt.Errorf("explorer interactions take the action id %q (got %q)", ExplorerShareAction, actionID)
		}
		return InteractionRule{Kind: InteractionShare, NotifyAgent: true}, nil
	default:
		return InteractionRule{}, fmt.Errorf("component %q (%s) does not accept interactions", component.ID, component.Type)
	}
}

// ValidateInteractionItem checks the item key of an interaction: required
// and naming an item of the component for item-keyed interactions, empty
// otherwise.
func ValidateInteractionItem(component Component, actionID, itemKey string) error {
	rule, err := Interaction(component, actionID)
	if err != nil {
		return err
	}
	if !rule.ItemKeyed {
		if itemKey != "" {
			return fmt.Errorf("%s interactions do not take an item key", component.Type)
		}
		return nil
	}
	if itemKey == "" {
		return fmt.Errorf("%s interactions need the item id as item key", component.Type)
	}
	if _, ok := itemLabel(component, itemKey); !ok {
		return fmt.Errorf("%s %q has no item %q", component.Type, component.ID, itemKey)
	}
	return nil
}

// itemLabel names an item of an item-keyed component: a tickable item or
// the front of a flashcard.
func itemLabel(component Component, id string) (string, bool) {
	if component.Type == "flashcards" && component.Flashcards != nil {
		for _, card := range component.Flashcards.Cards {
			if card.ID == id {
				return card.Front, true
			}
		}
		return "", false
	}
	return checkItemLabel(component, id)
}

// checkItemLabel returns the label of a checklist item, the title of a
// step or the name of a recipe ingredient.
func checkItemLabel(component Component, id string) (string, bool) {
	for _, item := range checkItems(component) {
		if item.id == id {
			return item.label, true
		}
	}
	return "", false
}

type checkItem struct{ id, label string }

func checkItems(component Component) []checkItem {
	var items []checkItem
	switch {
	case component.Type == "checklist" && component.Checklist != nil:
		for _, item := range component.Checklist.Items {
			items = append(items, checkItem{item.ID, item.Label})
		}
	case component.Type == "steps" && component.Steps != nil:
		for _, step := range component.Steps.Items {
			items = append(items, checkItem{step.ID, step.Title})
		}
	case component.Type == "recipe" && component.Recipe != nil:
		for _, ingredient := range component.Recipe.Ingredients {
			items = append(items, checkItem{ingredient.ID, ingredient.Name})
		}
	}
	return items
}

var isoDate = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// ValidateInteractionValues checks the values of an interaction against the
// component:
//
//   - form submit: keyed by field id; strings for text, textarea, date
//     (YYYY-MM-DD), select and radio (an option value); numbers for number;
//     booleans for checkbox.
//   - actions respond: no values.
//   - choices respond: {"chipId": id}.
//   - comparison pick: {"optionId": id}.
//   - check: {"checked": bool}.
//   - quiz answer: {"selected": [id, ...]}, exactly one id unless the quiz
//     allows multiple.
//   - flashcards mark: {"result": "got" | "again"}; done: no values.
//   - draft respond: {"response": "looks_good" | "shorter"}.
//   - poll vote: {"optionIds": [id, ...]}, exactly one id unless the poll
//     allows multiple, then distinct ids. Hosts also call PollOpen.
//   - explorer share: {"inputs": {inputId: number, ...}} with every input,
//     each within its range and on a step (toggles 0 or 1).
func ValidateInteractionValues(component Component, actionID string, values map[string]interface{}) error {
	rule, err := Interaction(component, actionID)
	if err != nil {
		return err
	}
	switch rule.Kind {
	case InteractionRespond:
		switch component.Type {
		case "actions":
			if len(values) > 0 {
				return errors.New("respond interactions carry no values")
			}
			return nil
		case "draft":
			return idValue(values, "response", []string{DraftLooksGood, DraftMakeShorter})
		}
		ids := make([]string, 0, len(component.Choices.Chips))
		for _, chip := range component.Choices.Chips {
			ids = append(ids, chip.ID)
		}
		return idValue(values, "chipId", ids)
	case InteractionPick:
		ids := make([]string, 0, len(component.Comparison.Options))
		for _, option := range component.Comparison.Options {
			ids = append(ids, option.ID)
		}
		return idValue(values, "optionId", ids)
	case InteractionCheck:
		if err := onlyKeys(values, "checked"); err != nil {
			return err
		}
		if _, ok := values["checked"].(bool); !ok {
			return errors.New("checked must be true or false")
		}
		return nil
	case InteractionMark:
		return idValue(values, "result", []string{FlashcardGot, FlashcardAgain})
	case InteractionDone:
		if len(values) > 0 {
			return errors.New("done carries no values; the tally comes from the marks")
		}
		return nil
	case InteractionVote:
		if err := onlyKeys(values, "optionIds"); err != nil {
			return err
		}
		ids, err := stringList(values["optionIds"], "optionIds")
		if err != nil {
			return err
		}
		return validatePollVote(component.Poll, ids)
	case InteractionShare:
		if err := onlyKeys(values, "inputs"); err != nil {
			return err
		}
		inputs, err := explorerInputValues(values["inputs"])
		if err != nil {
			return err
		}
		_, err = ExplorerShare(component, inputs)
		return err
	case InteractionAnswer:
		if err := onlyKeys(values, "selected"); err != nil {
			return err
		}
		selected, err := QuizSelection(values)
		if err != nil {
			return err
		}
		return validateSelection(component.Quiz, selected)
	}
	return validateFormValues(component.Form, values)
}

func onlyKeys(values map[string]interface{}, key string) error {
	for name := range values {
		if name != key {
			return fmt.Errorf("unexpected value %q; only %q is accepted", name, key)
		}
	}
	if _, ok := values[key]; !ok {
		return fmt.Errorf("%s is required", key)
	}
	return nil
}

func idValue(values map[string]interface{}, key string, ids []string) error {
	if err := onlyKeys(values, key); err != nil {
		return err
	}
	id, ok := values[key].(string)
	if !ok || !contains(ids, id) {
		return fmt.Errorf("%s must be one of %s", key, strings.Join(ids, ", "))
	}
	return nil
}

// QuizSelection reads the selected option ids from quiz answer values.
func QuizSelection(values map[string]interface{}) ([]string, error) {
	raw, ok := values["selected"]
	if !ok {
		return nil, errors.New("selected is required")
	}
	var selected []string
	switch list := raw.(type) {
	case []string:
		selected = append(selected, list...)
	case []interface{}:
		for _, item := range list {
			id, ok := item.(string)
			if !ok {
				return nil, errors.New("selected must be a list of option ids")
			}
			selected = append(selected, id)
		}
	default:
		return nil, errors.New("selected must be a list of option ids")
	}
	return selected, nil
}

func validateSelection(quiz *Quiz, selected []string) error {
	if quiz == nil {
		return errors.New("quiz payload is required")
	}
	if len(selected) == 0 {
		return errors.New("selected needs at least one option id")
	}
	if !quiz.AllowMultiple && len(selected) != 1 {
		return fmt.Errorf("selected needs exactly one option id (got %d)", len(selected))
	}
	seen := map[string]bool{}
	for _, id := range selected {
		if seen[id] {
			return fmt.Errorf("option %q is selected twice", id)
		}
		seen[id] = true
		found := false
		for _, option := range quiz.Options {
			found = found || option.ID == id
		}
		if !found {
			return fmt.Errorf("quiz has no option %q", id)
		}
	}
	return nil
}

// QuizOutcome is the server's grade of a quiz answer. Hosts store it on the
// interaction and show it to the person who answered.
type QuizOutcome struct {
	Correct     bool     `json:"correct"`
	CorrectIDs  []string `json:"correctIds"`
	Explanation string   `json:"explanation"`
}

// GradeQuiz grades selected option ids against a quiz component's answer
// key. The answer is correct when it selects exactly the correct options.
func GradeQuiz(component Component, selected []string) (QuizOutcome, error) {
	if component.Type != "quiz" || component.Quiz == nil {
		return QuizOutcome{}, fmt.Errorf("component %q is not a quiz", component.ID)
	}
	key := component.Quiz.AnswerKey
	if key == nil {
		return QuizOutcome{}, fmt.Errorf("quiz %q has no answer key", component.ID)
	}
	if err := validateSelection(component.Quiz, selected); err != nil {
		return QuizOutcome{}, err
	}
	correct := len(selected) == len(key.Correct)
	chosen := map[string]bool{}
	for _, id := range selected {
		chosen[id] = true
	}
	for _, id := range key.Correct {
		correct = correct && chosen[id]
	}
	return QuizOutcome{Correct: correct, CorrectIDs: append([]string(nil), key.Correct...), Explanation: key.Explanation}, nil
}

// StripAnswerKeys returns a copy of surface without quiz answer keys, for
// every reader except the agent that wrote it. The input is not modified.
// A stripped surface is for display; it no longer passes Validate.
func StripAnswerKeys(surface Surface) Surface {
	surface.Components = stripAnswerKeys(surface.Components)
	return surface
}

func stripAnswerKeys(components []Component) []Component {
	if components == nil {
		return nil
	}
	out := make([]Component, len(components))
	for index, component := range components {
		if component.Quiz != nil {
			quiz := *component.Quiz
			quiz.AnswerKey = nil
			component.Quiz = &quiz
		}
		component.Children = stripAnswerKeys(component.Children)
		if component.Tabs != nil {
			tabs := make([]Tab, len(component.Tabs))
			for tab := range component.Tabs {
				tabs[tab] = Tab{Label: component.Tabs[tab].Label, Children: stripAnswerKeys(component.Tabs[tab].Children)}
			}
			component.Tabs = tabs
		}
		out[index] = component
	}
	return out
}

// HasAnswerKeys reports whether any quiz in surface carries an answer key.
func HasAnswerKeys(surface Surface) bool {
	found := false
	walk(surface.Components, func(component *Component, _ string, _ int) bool {
		found = component.Quiz != nil && component.Quiz.AnswerKey != nil
		return !found
	})
	return found
}

// StripAnswerKeysJSON strips quiz answer keys from encoded surface bytes. It
// returns raw unchanged when there is nothing to strip.
func StripAnswerKeysJSON(raw []byte) ([]byte, error) {
	if !bytes.Contains(raw, []byte(`"answerKey"`)) {
		return raw, nil
	}
	var surface Surface
	if err := json.Unmarshal(raw, &surface); err != nil {
		return nil, fmt.Errorf("decode visual: %w", err)
	}
	if !HasAnswerKeys(surface) {
		return raw, nil
	}
	return json.Marshal(StripAnswerKeys(surface))
}

// CardResponseText is the compact text the agent reads for an interaction,
// e.g. `Card response (quiz "Cells"): picked "Mitochondria" (correct)`.
// values and outcome are the stored JSON of the interaction.
func CardResponseText(component Component, actionID, itemKey string, values, outcome json.RawMessage) string {
	name := strings.TrimSpace(component.Title)
	if name == "" {
		name = component.ID
	}
	var decoded map[string]interface{}
	_ = json.Unmarshal(values, &decoded)
	switch component.Type {
	case "actions":
		for _, action := range component.Actions {
			if action.ID != actionID {
				continue
			}
			value := action.Value
			if value == "" {
				value = action.Label
			}
			reply, _ := json.Marshal(value)
			return fmt.Sprintf("Card response (button %q): %s", action.Label, reply)
		}
	case "choices":
		if id, _ := decoded["chipId"].(string); component.Choices != nil {
			for _, chip := range component.Choices.Chips {
				if chip.ID == id {
					return fmt.Sprintf("Card response (choices): %q", chip.Label)
				}
			}
		}
	case "comparison":
		if id, _ := decoded["optionId"].(string); component.Comparison != nil {
			for _, option := range component.Comparison.Options {
				if option.ID == id {
					return fmt.Sprintf("Card response (comparison %q): picked %q", name, option.Name)
				}
			}
		}
	case "quiz":
		if selected, err := QuizSelection(decoded); err == nil && component.Quiz != nil {
			text := fmt.Sprintf("Card response (quiz %q): picked %s", name, quoteAll(component.Quiz.optionLabels(selected)))
			var grade QuizOutcome
			if len(outcome) > 0 && json.Unmarshal(outcome, &grade) == nil {
				if grade.Correct {
					text += " (correct)"
				} else {
					text += " (incorrect)"
				}
			}
			return text
		}
	case "flashcards":
		if actionID == FlashcardsDone {
			var tally FlashcardsTally
			if len(outcome) > 0 && json.Unmarshal(outcome, &tally) == nil {
				return fmt.Sprintf("Card response (flashcards %q): %s", name, tally.text())
			}
		} else if result, _ := decoded["result"].(string); result != "" {
			if front, found := itemLabel(component, itemKey); found {
				return fmt.Sprintf("Card response (flashcards %q): marked %q %s", name, front, result)
			}
		}
	case "poll":
		if text, ok := pollResponseText(component, values, outcome); ok {
			return fmt.Sprintf("Card response (poll %q): %s", name, text)
		}
	case "explorer":
		if text, ok := explorerResponseText(component, outcome); ok {
			return fmt.Sprintf("Card response (explorer %q): %s", name, text)
		}
	case "draft":
		if label, ok := DraftResponses[fmt.Sprint(decoded["response"])]; ok {
			return fmt.Sprintf("Card response (draft %q): %q", name, label)
		}
	case "checklist", "steps", "recipe":
		checked, ok := decoded["checked"].(bool)
		if label, found := checkItemLabel(component, itemKey); ok && found {
			verb := "ticked"
			if !checked {
				verb = "unticked"
			}
			return fmt.Sprintf("Card response (%s %q): %s %q", component.Type, name, verb, label)
		}
	}
	return fmt.Sprintf("Card response (%s %q): %s", component.Type, name, values)
}

// MaxCardStateLabels bounds the item labels CardStateLine names.
const MaxCardStateLabels = 5

// CardStateLine summarises the shared ticks of a checklist, checkable steps
// or a checkable recipe for the agent, e.g. `Packing list: 9/14 ticked
// (unticked: passport, charger)`, `Steps "Set up the router": 3/5 done` or
// `Recipe "Pancakes": 4/6 ingredients ticked`. checked holds the
// latest checked value per item id; ids that are not items are ignored. It
// returns false for components without ticks.
func CardStateLine(component Component, checked map[string]bool) (string, bool) {
	items := checkItems(component)
	if len(items) == 0 {
		return "", false
	}
	done := 0
	var open []string
	for _, item := range items {
		if checked[item.id] {
			done++
		} else {
			open = append(open, item.label)
		}
	}
	name := strings.TrimSpace(component.Title)
	if component.Type == "steps" || component.Type == "recipe" {
		if name == "" {
			name = component.ID
		}
		if component.Type == "recipe" {
			return fmt.Sprintf("Recipe %q: %d/%d ingredients ticked", name, done, len(items)), true
		}
		return fmt.Sprintf("Steps %q: %d/%d done", name, done, len(items)), true
	}
	if name == "" {
		name = "Checklist " + component.ID
	}
	line := fmt.Sprintf("%s: %d/%d ticked", name, done, len(items))
	if len(open) > 0 && len(open) < len(items) {
		shown := open
		if len(shown) > MaxCardStateLabels {
			shown = shown[:MaxCardStateLabels]
		}
		line += " (unticked: " + strings.Join(shown, ", ")
		if more := len(open) - len(shown); more > 0 {
			line += fmt.Sprintf(" and %d more", more)
		}
		line += ")"
	}
	return line, true
}

func validateFormValues(form *Form, values map[string]interface{}) error {
	if form == nil {
		return errors.New("form payload is required")
	}
	fields := map[string]FormField{}
	for _, field := range form.Fields {
		fields[field.ID] = field
	}
	names := make([]string, 0, len(values))
	for id := range values {
		names = append(names, id)
	}
	sort.Strings(names)
	for _, id := range names {
		if _, ok := fields[id]; !ok {
			return fmt.Errorf("form has no field %q", id)
		}
	}
	for _, field := range form.Fields {
		value, present := values[field.ID]
		if !present || value == nil {
			if field.Required {
				return fmt.Errorf("field %q is required", field.ID)
			}
			continue
		}
		if err := validateFieldValue(field, value); err != nil {
			return fmt.Errorf("field %q: %w", field.ID, err)
		}
	}
	return nil
}

func validateFieldValue(field FormField, value interface{}) error {
	switch field.Type {
	case "checkbox":
		checked, ok := value.(bool)
		if !ok {
			return errors.New("must be true or false")
		}
		if field.Required && !checked {
			return errors.New("must be checked")
		}
	case "number":
		number, ok := value.(float64)
		if !ok || math.IsNaN(number) || math.IsInf(number, 0) {
			return errors.New("must be a number")
		}
		if field.Min != nil && number < *field.Min || field.Max != nil && number > *field.Max {
			return errors.New("is out of range")
		}
	default:
		text, ok := value.(string)
		if !ok {
			return errors.New("must be a string")
		}
		if field.Required && strings.TrimSpace(text) == "" {
			return errors.New("is required")
		}
		limit := 2000
		if field.Type == "textarea" {
			limit = 20000
		}
		if len([]rune(text)) > limit {
			return fmt.Errorf("must be at most %d characters", limit)
		}
		switch field.Type {
		case "date":
			if text != "" && !isoDate.MatchString(text) {
				return errors.New("must be a date as YYYY-MM-DD")
			}
		case "select", "radio":
			if text == "" {
				return nil
			}
			for _, option := range field.Options {
				if option.Value == text {
					return nil
				}
			}
			return fmt.Errorf("%q is not one of the field's options", text)
		}
	}
	return nil
}

// FlashcardsTally is the outcome of finishing a flashcards deck: how many
// cards the person last marked got or again, and how many they never
// marked. Hosts store it on the done interaction.
type FlashcardsTally struct {
	Got    int `json:"got"`
	Again  int `json:"again"`
	Unseen int `json:"unseen"`
}

func (t FlashcardsTally) text() string {
	text := fmt.Sprintf("%d got, %d again", t.Got, t.Again)
	if t.Unseen > 0 {
		text += fmt.Sprintf(", %d unseen", t.Unseen)
	}
	return text
}

// FlashcardMark reads the result ("got" or "again") of stored mark values.
func FlashcardMark(values json.RawMessage) (string, bool) {
	var decoded struct {
		Result string `json:"result"`
	}
	if json.Unmarshal(values, &decoded) != nil || decoded.Result != FlashcardGot && decoded.Result != FlashcardAgain {
		return "", false
	}
	return decoded.Result, true
}

// FlashcardsOutcome tallies one person's latest marks on a flashcards
// component. marks maps card ids to "got" or "again" (read them with
// FlashcardMark from the person's latest mark per card); ids that are not
// cards of this deck and unknown results are ignored, and cards without a
// mark count as unseen.
func FlashcardsOutcome(component Component, marks map[string]string) (FlashcardsTally, error) {
	if component.Type != "flashcards" || component.Flashcards == nil {
		return FlashcardsTally{}, fmt.Errorf("component %q is not flashcards", component.ID)
	}
	var tally FlashcardsTally
	for _, card := range component.Flashcards.Cards {
		switch marks[card.ID] {
		case FlashcardGot:
			tally.Got++
		case FlashcardAgain:
			tally.Again++
		default:
			tally.Unseen++
		}
	}
	return tally, nil
}

// FlashcardsStateLine summarises one person's flashcard marks for the agent,
// e.g. `Flashcards "French food words": 3 got, 1 again, 1 unseen`. It
// returns false for components that are not flashcards.
func FlashcardsStateLine(component Component, marks map[string]string) (string, bool) {
	tally, err := FlashcardsOutcome(component, marks)
	if err != nil {
		return "", false
	}
	name := strings.TrimSpace(component.Title)
	if name == "" {
		name = component.ID
	}
	return fmt.Sprintf("Flashcards %q: %s", name, tally.text()), true
}
