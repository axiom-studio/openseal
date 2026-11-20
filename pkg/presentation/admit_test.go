package presentation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"
)

// The tests in this file hold the host's lenient reading of the model's
// visual (decision 2026-11-20) for every component type.

// The visual of prod run d3e058b9: a converter and a sources table whose
// text column held numbers. Both are shown, the rates as their text.
func TestAdmitReadsNumbersInTextColumnsAsText(t *testing.T) {
	raw := `{"title":"USD → EUR","components":[{"id":"fx","type":"converter","summary":"One US dollar is about 0.911 euro.","converter":{"from":{"code":"USD","label":"US dollar","symbol":"$"},"to":{"code":"EUR","label":"Euro","symbol":"€"},"rate":0.91096,"amount":1,"asOf":"2026-11-19","note":"FX Rates API"}},{"id":"srcs","type":"table","summary":"Three sources agree.","table":{"columns":[{"key":"rate","label":"USD → EUR"},{"key":"source","label":"Source"},{"key":"updated","label":"Updated"}],"rows":[{"rate":0.91096,"source":"FX Rates API","updated":"15:21 UTC"},{"rate":0.90975,"source":"Frankfurter (ECB)","updated":"19 Nov"}]}}]}`
	stored := acceptVisual(t, raw)
	if len(stored.Components) != 2 || stored.Components[0].Converter == nil {
		t.Fatalf("components = %+v", stored.Components)
	}
	rows := stored.Components[1].Table.Rows
	if rate := rows[0]["rate"]; rate.Text == nil || *rate.Text != "0.91096" || rows[1]["rate"].Text == nil || *rows[1]["rate"].Text != "0.90975" {
		t.Fatalf("rate cells = %+v", rows)
	}
	encoded, _ := json.Marshal(stored)
	if !strings.Contains(string(encoded), `"rate":"0.91096"`) {
		t.Fatalf("stored table = %s", encoded)
	}
	// A numeric string in a number column is its number.
	stored = acceptVisual(t, visualJSON(`{"id":"t","type":"table","summary":"s","table":{"columns":[{"key":"n","label":"N","kind":"number"}],"rows":[{"n":"1,250"},{"n":" 3.5 "}]}}`))
	if cell := stored.Components[0].Table.Rows[0]["n"]; cell.Number == nil || *cell.Number != 1250 {
		t.Fatalf("number cell = %+v", cell)
	}
}

func TestCoerceScalar(t *testing.T) {
	for _, test := range []struct {
		value interface{}
		want  string
		got   interface{}
		ok    bool
	}{
		{json.Number("0.91096"), "string", "0.91096", true},
		{true, "string", "true", true},
		{"x", "string", nil, false},
		{"1,250", "number", json.Number("1250"), true},
		{" -2.5e3 ", "number", json.Number("-2500"), true},
		{"1,5", "number", nil, false},
		{"12%", "number", nil, false},
		{"NaN", "number", nil, false},
		{"7", "integer", json.Number("7"), true},
		{"7.5", "integer", nil, false},
		{json.Number("7"), "number", nil, false},
		{"TRUE", "boolean", true, true},
		{"yes", "boolean", nil, false},
		{"one", "array", []interface{}{"one"}, true},
		{[]interface{}{"one"}, "array", nil, false},
		{map[string]interface{}{}, "string", nil, false},
	} {
		got, ok := coerceScalar(test.value, test.want)
		if ok != test.ok || fmt.Sprint(got) != fmt.Sprint(test.got) {
			t.Errorf("coerceScalar(%#v, %s) = %#v, %v; want %#v, %v", test.value, test.want, got, ok, test.got, test.ok)
		}
	}
}

func TestCutText(t *testing.T) {
	for _, test := range []struct {
		text    string
		maximum int
		want    string
	}{
		{"short", 10, "short"},
		{"one two three four", 12, "one two…"},
		{"abcdefghijklmnop", 8, "abcdefg…"},
		{"one, two, three", 10, "one, two…"},
	} {
		if got := cutText(test.text, test.maximum); got != test.want || len([]rune(got)) > test.maximum {
			t.Errorf("cutText(%q, %d) = %q, want %q", test.text, test.maximum, got, test.want)
		}
	}
}

// A component that fails is left out on its own, with its reason; the ones
// that depend on it are checked again and left out only if they now fail.
func TestAdmitIsolatesFailures(t *testing.T) {
	raw := visualJSON(
		`{"id":"intro","type":"text","text":"Here is the draft."}`,
		`{"id":"d","type":"draft","summary":"s","draft":{"channel":"carrier-pigeon","body":"Hello"}}`,
		`{"id":"buttons","type":"actions","actions":[{"id":"e","label":"Edit","kind":"edit","target":"d"}]}`,
		`{"id":"box","type":"section","children":[{"id":"bad","type":"callout","callout":{"body":" "}}]}`,
		`{"id":"keep","type":"section","children":[{"id":"ok","type":"text","text":"Kept"},{"id":"gone","type":"callout","callout":{"body":" "}}]}`,
	)
	visual, drops, err := DecodeVisual([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, component := range visual.Components {
		ids = append(ids, component.ID)
	}
	if strings.Join(ids, ",") != "intro,keep" || len(visual.Components[1].Children) != 1 {
		t.Fatalf("kept %v, children %+v", ids, visual.Components[1].Children)
	}
	want := map[string]string{
		"d": `draft.channel "carrier-pigeon" is not one of`, "bad": "callout.body must not be blank", "gone": "callout.body must not be blank",
		"box": "section needs 1-12 children", "buttons": "target must be the id of a draft",
	}
	if len(drops) != len(want) {
		t.Fatalf("drops = %+v", drops)
	}
	for _, drop := range drops {
		if drop.Path == "" || !strings.Contains(drop.Reason, want[drop.ComponentID]) {
			t.Fatalf("drop %+v, want reason %q", drop, want[drop.ComponentID])
		}
	}
	if _, err := Encode(visual.Surface(StateReady)); err != nil {
		t.Fatalf("the admitted visual does not encode: %v", err)
	}
}

// Host checks leave out only the component that fails them.
func TestAdmitIsolatesHostCheckFailures(t *testing.T) {
	visual := Visual{Title: "T", Components: []Component{
		text("intro", "Hi"),
		{ID: "ag", Type: "agent", Summary: "s", Binding: &Binding{Kind: BindingAgent, ID: "missing"}, Agent: &AgentCard{Headline: "H"}},
	}}
	refuse := BindingResolverFunc(func(context.Context, Binding) error { return errors.New("not in this chat") })
	admitted, drops, _, err := Admit(context.Background(), visual, ValidateOptions{Bindings: refuse})
	if err != nil {
		t.Fatal(err)
	}
	if len(admitted.Components) != 1 || len(drops) != 1 || drops[0].ComponentID != "ag" || drops[0].Path != "components[1]" || !strings.Contains(drops[0].Reason, "not in this chat") {
		t.Fatalf("components %+v, drops %+v", admitted.Components, drops)
	}
	// Nothing left: the whole visual is dropped, last.
	_, drops, _, err = Admit(context.Background(), Visual{Title: "T", Components: visual.Components[1:]}, ValidateOptions{Bindings: refuse})
	if err == nil || len(drops) != 2 || drops[1].Path != "" || drops[1].Reason != err.Error() {
		t.Fatalf("err %v, drops %+v", err, drops)
	}
}

// TestEveryTypeReadsSlips is the slip corpus every catalogue type must
// pass (rule V5). For each type it starts from its parity seed and, field
// by field as the type's schema lists them, sends a slip: a number in a
// text slot, a numeric string in a number slot, a string true/false in a
// boolean slot, a single item for a list, and a list or text over its
// maximum. Each slip must read exactly as the value it stands for, and an
// unreadable component next to a good one must leave the good one shown.
func TestEveryTypeReadsSlips(t *testing.T) {
	covered := map[string]int{}
	for _, raw := range paritySeeds {
		var seed map[string]interface{}
		if err := json.Unmarshal([]byte(raw), &seed); err != nil {
			t.Fatal(err)
		}
		name := seed["type"].(string)
		corpus := &slipCorpus{t: t, name: name, seed: seed}
		corpus.visit(componentTypeSchema(name, componentRef()), nil, "")
		covered[name] += corpus.slips
		brokenNextToGood(t, name, seed)
	}
	for _, name := range TypeNames() {
		if covered[name] == 0 {
			t.Errorf("no slip was sent for %s; give it a parity seed", name)
		}
	}
}

// brokenNextToGood checks that an unreadable component costs only itself,
// before and after a component of the type.
func brokenNextToGood(t *testing.T, name string, seed map[string]interface{}) {
	t.Helper()
	broken := func(component interface{}) map[string]interface{} {
		copied := deepCopy(component).(map[string]interface{})
		copied["id"] = "not an id!"
		return copied
	}
	good := map[string]interface{}{"id": "z", "type": "text", "text": "x"}
	for _, pair := range [][2]map[string]interface{}{{seed, broken(good)}, {good, broken(seed)}} {
		visual, drops, err := DecodeVisual(mustJSONBytes(t, map[string]interface{}{"title": "T", "components": []interface{}{pair[0], pair[1]}}))
		if err != nil || len(visual.Components) != 1 || visual.Components[0].ID != pair[0]["id"] || len(drops) != 1 || drops[0].Path != "components[1]" {
			t.Errorf("%s: a good component next to a broken one: %v, %+v", name, err, drops)
		}
	}
}

type slipCorpus struct {
	t     *testing.T
	name  string
	seed  map[string]interface{}
	slips int
}

// visit sends the slips of each field the seed fills in.
func (c *slipCorpus) visit(schema map[string]interface{}, path []step, field string) {
	if _, ref := schema["$ref"]; ref || schema["anyOf"] != nil {
		return
	}
	value := get(c.seed, path)
	switch schema["type"] {
	case "object":
		node, ok := value.(map[string]interface{})
		properties, _ := schema["properties"].(map[string]interface{})
		if !ok {
			return
		}
		for key := range node {
			if property, ok := properties[key].(map[string]interface{}); ok {
				c.visit(property, append(append([]step{}, path...), key), joinField(field, key))
			}
		}
	case "array":
		c.list(schema, path, field)
	case "string":
		c.text(schema, path, field)
	case "number", "integer":
		number, ok := value.(float64)
		if !ok {
			return
		}
		c.same(field+" as a numeric string", path, withThousands(number), number, true)
	case "boolean":
		flag, ok := value.(bool)
		if !ok {
			return
		}
		c.same(field+" as a string", path, strconv.FormatBool(flag), flag, true)
	}
}

func (c *slipCorpus) text(schema map[string]interface{}, path []step, field string) {
	if schema["enum"] != nil || schema["pattern"] != nil && schema["pattern"] != notBlankPattern {
		return
	}
	if _, ok := get(c.seed, path).(string); !ok {
		return
	}
	c.same(field+" as a number", path, 7.25, "7.25", false)
	maximum, bounded := intKeyword(schema, "maxLength")
	if !bounded {
		return
	}
	long := strings.TrimSpace(strings.Repeat("word ", maximum/5+4))
	c.same(field+" over its maximum", path, long, cutText(long, maximum), false)
}

func (c *slipCorpus) list(schema map[string]interface{}, path []step, field string) {
	list, ok := get(c.seed, path).([]interface{})
	if !ok || len(list) == 0 {
		return
	}
	c.same(field+" as a single item", path, deepCopy(list[0]), []interface{}{deepCopy(list[0])}, false)
	maximumKey := "maxItems"
	if _, counted := schema["contains"]; counted {
		maximumKey = "maxContains"
	}
	if maximum, ok := intKeyword(schema, maximumKey); ok {
		items := make([]interface{}, 0, maximum+1)
		for index := 0; index <= maximum; index++ {
			if index < len(list) {
				items = append(items, deepCopy(list[index]))
				continue
			}
			items = append(items, renameIDs(deepCopy(list[0]), index))
		}
		c.same(field+" over its maximum", path, items, items[:maximum], false)
	}
	items := schema["items"].(map[string]interface{})
	for index := range list {
		c.visit(items, append(append([]step{}, path...), index), fmt.Sprintf("%s[%d]", field, index))
	}
}

// same sends the seed with slip at path and with meant there, and requires
// both to read the same. accepted requires the meant value to be shown too:
// it is the seed's own value. Other meant values (a made-up text or list
// item) may fail a rule of another field, and the slip must fail it the
// same way.
func (c *slipCorpus) same(probe string, path []step, slip, meant interface{}, accepted bool) {
	c.t.Helper()
	c.slips++
	read := func(value interface{}) (string, error) {
		component := deepCopy(c.seed)
		set(component, path, value)
		visual, drops, err := DecodeVisual(mustJSONBytes(c.t, map[string]interface{}{"title": "T", "components": []interface{}{component}}))
		if err != nil {
			return "", err
		}
		if len(drops) > 0 {
			return "", fmt.Errorf("drops %+v", drops)
		}
		if _, err := Decode(mustJSONBytes(c.t, visual.Surface(StateReady))); err != nil {
			return "", fmt.Errorf("the admitted visual does not decode: %w", err)
		}
		return string(mustJSONBytes(c.t, visual)), nil
	}
	got, slipErr := read(slip)
	want, meantErr := read(meant)
	switch {
	case accepted && meantErr != nil:
		c.t.Errorf("%s: %s: the value it stands for is refused: %v", c.name, probe, meantErr)
	case (slipErr == nil) != (meantErr == nil):
		c.t.Errorf("%s: %s: slip error %v, meant error %v", c.name, probe, slipErr, meantErr)
	case got != want:
		c.t.Errorf("%s: %s reads as\n%s\nwant\n%s", c.name, probe, got, want)
	}
}

// withThousands writes a number as the model sometimes does: with
// thousands separators, or with spaces around it.
func withThousands(number float64) string {
	if number < 1000 || number != math.Trunc(number) {
		return " " + strconv.FormatFloat(number, 'f', -1, 64) + " "
	}
	digits := strconv.FormatFloat(number, 'f', -1, 64)
	var b strings.Builder
	for index, digit := range digits {
		if index > 0 && (len(digits)-index)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(digit)
	}
	return b.String()
}

func mustJSONBytes(t *testing.T, value interface{}) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
