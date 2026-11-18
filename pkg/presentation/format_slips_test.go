package presentation

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// The tests in this file take the shapes of visuals the model sent in prod
// that the format used to reject for a small slip, and check the meaning
// the format now gives each slip on every path: DecodeVisual (the model's
// visual), Validate with and without host checks, and Decode of the stored
// surface.

// acceptVisual decodes raw as the model's visual and checks it on every
// path. It returns the visual as stored.
func acceptVisual(t *testing.T, raw string) *Surface {
	t.Helper()
	visual, err := DecodeVisual([]byte(raw))
	if err != nil {
		t.Fatalf("DecodeVisual: %v", err)
	}
	surface := visual.Surface(StateReady)
	if _, err := Validate(context.Background(), surface, ValidateOptions{SkipHostChecks: true}); err != nil {
		t.Fatalf("Validate without host checks: %v", err)
	}
	hosts := ValidateOptions{Bindings: BindingResolverFunc(allow)}
	if _, err := Validate(context.Background(), surface, hosts); err != nil {
		t.Fatalf("Validate with host checks: %v", err)
	}
	encoded, err := Encode(surface)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	stored, err := Decode(encoded)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	return stored
}

func visualJSON(components ...string) string {
	return `{"title":"Answer","components":[` + strings.Join(components, ",") + `]}`
}

const agentComponent = `{"id":"ag","type":"agent","summary":"Meet Pip.","binding":{"kind":"agent","id":"agent-1"},"agent":{"headline":"Plans your week","points":["a","b","c","d","e"]}}`

// Rule 1: summary is optional; the title, or else the first sentence of
// the main text, stands in.
func TestSummaryIsOptional(t *testing.T) {
	stored := acceptVisual(t, visualJSON(
		`{"id":"tip","type":"callout","tone":"info","callout":{"body":"Book before Friday. Prices rise after that."}}`,
		`{"id":"warn","type":"callout","title":"Check the visa","callout":{"body":"You need one."}}`,
	))
	if got := stored.Components[0].AccessibleSummary(); got != "Book before Friday." {
		t.Fatalf("summary from body = %q", got)
	}
	if got := stored.Components[1].AccessibleSummary(); got != "Check the visa" {
		t.Fatalf("summary from title = %q", got)
	}
	given := Component{Type: "callout", Summary: "Own words.", Title: "T", Callout: &Callout{Body: "B."}}
	if got := given.AccessibleSummary(); got != "Own words." {
		t.Fatalf("given summary = %q", got)
	}
	for text, want := range map[string]string{"One line no stop": "One line no stop", "Costs 1.5 GBP. Then": "Costs 1.5 GBP.", "Why?\nBecause.": "Why?"} {
		if got := firstSentence(text); got != want {
			t.Fatalf("firstSentence(%q) = %q, want %q", text, got, want)
		}
	}
}

// Rule 2: prose has no maximum length and is never cut; empty is still an
// error where it would leave nothing to show.
func TestLongProseIsKept(t *testing.T) {
	body := strings.Repeat("b", 363)
	text := strings.Repeat("t", 365)
	stored := acceptVisual(t, visualJSON(
		`{"id":"c","type":"callout","summary":"s","callout":{"body":"`+body+`"}}`,
		`{"id":"t","type":"text","text":"`+text+`"}`,
	))
	if stored.Components[0].Callout.Body != body || stored.Components[1].Text != text {
		t.Fatal("long prose was changed")
	}
	if _, err := DecodeVisual([]byte(visualJSON(`{"id":"c","type":"callout","summary":"s","callout":{"body":" "}}`))); err == nil || !strings.Contains(err.Error(), "callout.body must not be blank") {
		t.Fatalf("blank body: %v", err)
	}
}

// Rule 3: a display list longer than it shows is kept whole; only its
// first items are shown.
func TestDisplayListsShowTheirFirstItems(t *testing.T) {
	stored := acceptVisual(t, visualJSON(agentComponent))
	if len(stored.Components[0].Agent.Points) != 5 {
		t.Fatal("points were cut")
	}
	points := agentCardSchema()["properties"].(map[string]interface{})["points"].(map[string]interface{})
	if points["maxItems"] != nil || !strings.Contains(points["description"].(string), fmt.Sprintf("Only the first %d are shown.", maxAgentPoints)) {
		t.Fatalf("points schema = %v", points)
	}
	if !strings.Contains(VisualFormat(), "points?:[str1..]0.. (Only the first 4 are shown.)") && !strings.Contains(VisualFormat(), "points?:[str1..] (Only the first 4 are shown.)") {
		t.Fatal("format does not say only the first 4 points are shown")
	}
	// A step past the shown ones cannot be ticked.
	steps := Component{ID: "s", Type: "steps", Summary: "x", Steps: &Steps{Items: stepsOf(maxSteps + 1), Checkable: true}}
	if err := ValidateInteractionItem(steps, "", "s9"); err != nil {
		t.Fatalf("shown step: %v", err)
	}
	if err := ValidateInteractionItem(steps, "", fmt.Sprintf("s%d", maxSteps)); err == nil {
		t.Fatal("a step that is not shown was ticked")
	}
}

// Rule 4: a column without a kind, or with an empty one, is text.
func TestTableColumnKindDefaultsToText(t *testing.T) {
	stored := acceptVisual(t, visualJSON(`{"id":"t","type":"table","summary":"s","table":{"columns":[{"key":"name","label":"Name","kind":""},{"key":"note","label":"Note"}],"rows":[{"name":"a","note":"b"}]}}`))
	for _, column := range stored.Components[0].Table.Columns {
		if column.ColumnKind() != "text" {
			t.Fatalf("column %s kind = %q", column.Key, column.ColumnKind())
		}
	}
	if _, err := DecodeVisual([]byte(visualJSON(`{"id":"t","type":"table","summary":"s","table":{"columns":[{"key":"n","label":"N","kind":"money"}]}}`))); err == nil {
		t.Fatal("an unknown kind was accepted")
	}
}

// Rule 5: a currency without format currency is ignored.
func TestCurrencyWithoutCurrencyFormatIsIgnored(t *testing.T) {
	acceptVisual(t, visualJSON(`{"id":"c","type":"chart","summary":"s","chart":{"kind":"bar","currency":"EUR","series":[{"name":"S","points":[{"label":"a","value":1}]}]}}`))
	visual, err := DecodeVisual([]byte(visualJSON(`{"id":"c","type":"chart","summary":"s","chart":{"kind":"bar","format":"currency","series":[{"name":"S","points":[{"label":"a","value":1}]}]}}`)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Validate(context.Background(), visual.Surface(StateReady), ValidateOptions{SkipHostChecks: true}); err == nil {
		t.Fatal("format currency without a currency was accepted")
	}
}

// Rule 6: options on a field that is not select or radio are ignored.
func TestFormOptionsOnOtherFieldsAreIgnored(t *testing.T) {
	acceptVisual(t, visualJSON(`{"id":"f","type":"form","summary":"s","form":{"fields":[
		{"id":"a","type":"select","label":"A","options":[{"label":"X","value":"x"}]},
		{"id":"b","type":"text","label":"B"},
		{"id":"c","type":"textarea","label":"C","options":[{"label":"Y","value":"y"}]}]}}`))
}

// Rule 7: a payload of another type is ignored and not stored.
func TestOtherTypesPayloadIsIgnored(t *testing.T) {
	withActions := strings.Replace(agentComponent, `"agent":{`, `"actions":[{"id":"go","label":"Go","kind":"respond"}],"agent":{`, 1)
	stored := acceptVisual(t, visualJSON(withActions))
	if stored.Components[0].Actions != nil {
		t.Fatal("the agent kept an actions payload")
	}
	// A binding is still checked against the host, so one on a type that
	// takes none is refused.
	if _, err := DecodeVisual([]byte(visualJSON(`{"id":"t","type":"tasks","summary":"s","binding":{"kind":"run","id":"r"},"tasks":{"rows":[{"binding":{"kind":"conversation_task","id":"t1"},"label":"L"}]}}`))); err == nil ||
		!strings.Contains(err.Error(), "tasks takes no component binding") {
		t.Fatalf("binding on tasks: %v", err)
	}
}

// Rule 8: keys the format does not have are ignored everywhere.
func TestUnknownKeysAreIgnored(t *testing.T) {
	raw := `{"title":"Answer","state":"streaming","section":"x","components":[
		{"id":"a","type":"text","text":"Hi","section":"intro","children":[{"id":"b","type":"text","text":"never read"}]},
		{"id":"s","type":"section","children":[{"id":"c","type":"stat_row","summary":"s","stat_row":{"stats":[{"label":"L","value":1,"section":"x"}]}}]}]}`
	stored := acceptVisual(t, raw)
	encoded, _ := json.Marshal(stored)
	if strings.Contains(string(encoded), "section\":\"") || strings.Contains(string(encoded), "never read") {
		t.Fatalf("ignored keys were stored: %s", encoded)
	}
	if stored.State != StateReady {
		t.Fatalf("the visual set its own state: %s", stored.State)
	}
	// The host fills coordinates in; the model may not type them.
	if _, err := DecodeVisual([]byte(visualJSON(`{"id":"p","type":"place","summary":"s","place":{"name":"Cafe","lat":1,"lon":2}}`))); err == nil ||
		!strings.Contains(err.Error(), "place.lat and .lon are filled in from find_place") {
		t.Fatalf("typed coordinates: %v", err)
	}
}

// Rule 9: a component whose type is missing or unknown is left out; the
// visual fails only when nothing is left to draw.
func TestComponentsOfNoKnownTypeAreLeftOut(t *testing.T) {
	stored := acceptVisual(t, visualJSON(
		`{"id":"cmp","summary":"s","callout":{"body":"b"}}`,
		`{"id":"x","type":"hologram"}`,
		`{"id":"t","type":"text","text":"Kept"}`,
		`{"id":"s","type":"section","children":[{"id":"cmp2","type":""},{"id":"u","type":"text","text":"Nested"}]}`,
	))
	if len(stored.Components) != 2 || stored.Components[0].ID != "t" || len(stored.Components[1].Children) != 1 {
		t.Fatalf("stored components = %+v", stored.Components)
	}
	for raw, want := range map[string]string{
		visualJSON(`{"id":"cmp","summary":"s"}`):                                      "at least one component with a type from the list is required",
		visualJSON(`{"id":"s","type":"section","children":[{"id":"cmp","type":""}]}`): "section needs 1-12 children with a type from the list (got 0)",
		`{"title":"Answer","components":[]}`:                                          "at least one component",
	} {
		if _, err := DecodeVisual([]byte(raw)); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: error = %v, want %q", raw, err, want)
		}
	}
}
