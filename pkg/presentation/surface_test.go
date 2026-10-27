package presentation

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/skill"
)

var updateGolden = flag.Bool("update", false, "rewrite golden files")

func one(components ...Component) Surface {
	return Surface{Schema: Schema, Title: "Card", State: StateReady, Components: components}
}

func chart(id string) Component {
	return Component{ID: id, Type: "chart", Summary: "Spend rose.", Chart: &Chart{Kind: "bar", XLabel: "Month", YLabel: "£", Series: []ChartSeries{{Name: "Spend", Points: []ChartPoint{{Label: "Jan", Value: 1}, {Label: "Feb", Value: 2}}}}}}
}

func note(id string) Component { return Component{ID: id, Type: "text", Text: "Hello"} }

func float(value float64) *float64 { return &value }

func nested(depth int) Component {
	if depth == 1 {
		return note("leaf")
	}
	return Component{ID: "level" + string(rune('0'+depth)), Type: "section", Children: []Component{nested(depth - 1)}}
}

func many(count int) []Component {
	list := make([]Component, count)
	for index := range list {
		list[index] = note("n" + string(rune('A'+index/26)) + string(rune('a'+index%26)))
	}
	return list
}

func allow(context.Context, Binding) error { return nil }

func TestValidate(t *testing.T) {
	approvalBinding := &Binding{Kind: BindingApproval, ID: "ap-1"}
	cases := []struct {
		name    string
		surface Surface
		opts    ValidateOptions
		err     string // substring; empty means valid
	}{
		// envelope
		{"valid chart", one(chart("c")), ValidateOptions{}, ""},
		{"wrong schema", Surface{Schema: "openseal.dev/presentation/v1", Title: "x", State: StateReady, Components: []Component{note("a")}}, ValidateOptions{}, "schema"},
		{"no components", one(), ValidateOptions{}, "at least one component"},
		{"bad state", Surface{Schema: Schema, Title: "x", State: "done", Components: []Component{note("a")}}, ValidateOptions{}, "streaming or ready"},
		{"unknown type", one(Component{ID: "x", Type: "html", Text: "<b>"}), ValidateOptions{}, `unsupported type "html"`},
		{"canvas is gone", one(Component{ID: "x", Type: "canvas"}), ValidateOptions{}, `unsupported type "canvas"`},
		{"metric is gone", one(Component{ID: "x", Type: "metric"}), ValidateOptions{}, `unsupported type "metric"`},
		{"duplicate id", one(note("a"), note("a")), ValidateOptions{}, "already used"},
		{"duplicate nested id", one(Component{ID: "s", Type: "section", Children: []Component{note("s")}}), ValidateOptions{}, "already used"},
		{"bad id", one(note("-a")), ValidateOptions{}, "id must match"},
		{"wrong payload", one(Component{ID: "x", Type: "text", Text: "hi", Chart: chart("c").Chart}), ValidateOptions{}, `text cannot carry a "chart" payload`},
		{"children on a leaf", one(Component{ID: "x", Type: "text", Text: "hi", Children: []Component{note("y")}}), ValidateOptions{}, "cannot have children"},
		// summary, title, tone
		{"summary required", one(Component{ID: "c", Type: "chart", Chart: chart("c").Chart}), ValidateOptions{}, "summary is required for chart"},
		{"summary too long", one(Component{ID: "c", Type: "chart", Summary: strings.Repeat("a", 281), Chart: chart("c").Chart}), ValidateOptions{}, "summary must be at most 280"},
		{"summary optional on text", one(note("a")), ValidateOptions{}, ""},
		{"tone ok", one(Component{ID: "a", Type: "text", Text: "x", Tone: ToneWarning}), ValidateOptions{}, ""},
		{"tone hex rejected", one(Component{ID: "a", Type: "text", Text: "x", Tone: "#ff0000"}), ValidateOptions{}, "tone"},
		{"title too long", one(Component{ID: "a", Type: "text", Text: "x", Title: strings.Repeat("t", 301)}), ValidateOptions{}, "title"},
		// limits and nesting
		{"depth 3 ok", one(nested(3)), ValidateOptions{}, ""},
		{"depth 4 rejected", one(nested(4)), ValidateOptions{}, "at most 3 levels"},
		{"64 components ok", one(many(64)...), ValidateOptions{}, ""},
		{"65 components rejected", one(many(65)...), ValidateOptions{}, "at most 64 components"},
		{"nested count", one(append(many(60), Component{ID: "s", Type: "section", Children: many(64)[60:]})...), ValidateOptions{}, "at most 64 components"},
		{"64KB", one(func() []Component {
			list := many(60)
			for index := range list {
				list[index].Text = strings.Repeat("x", 1150)
			}
			return list
		}()...), ValidateOptions{}, "bytes of JSON"},
		// text-like
		{"text too long", one(Component{ID: "a", Type: "text", Text: strings.Repeat("a", 1201)}), ValidateOptions{}, "text must contain between 1 and 1200"},
		{"caption", one(Component{ID: "a", Type: "caption", Text: "Source: ONS"}), ValidateOptions{}, ""},
		{"empty caption", one(Component{ID: "a", Type: "caption"}), ValidateOptions{}, "text must contain"},
		{"bare divider", one(Component{ID: "a", Type: "divider"}), ValidateOptions{}, ""},
		// layout
		{"section", one(Component{ID: "s", Type: "section", Title: "Options", Children: []Component{note("a"), note("b")}}), ValidateOptions{}, ""},
		{"empty section", one(Component{ID: "s", Type: "section"}), ValidateOptions{}, "section needs 1-12 children"},
		{"grid", one(Component{ID: "g", Type: "grid", Grid: &Grid{Columns: 2}, Children: []Component{note("a"), note("b")}}), ValidateOptions{}, ""},
		{"grid columns", one(Component{ID: "g", Type: "grid", Grid: &Grid{Columns: 5}, Children: []Component{note("a")}}), ValidateOptions{}, "grid.columns"},
		{"grid without payload", one(Component{ID: "g", Type: "grid", Children: []Component{note("a")}}), ValidateOptions{}, "grid.columns"},
		{"carousel of one", one(Component{ID: "c", Type: "carousel", Children: []Component{note("a")}}), ValidateOptions{}, "carousel needs 2-10"},
		{"carousel", one(Component{ID: "c", Type: "carousel", Children: []Component{note("a"), note("b")}}), ValidateOptions{}, ""},
		{"tabs", one(Component{ID: "t", Type: "tabs", Tabs: []Tab{{Label: "Simple", Children: []Component{note("a")}}, {Label: "Detailed", Children: []Component{chart("c")}}}}), ValidateOptions{}, ""},
		{"one tab", one(Component{ID: "t", Type: "tabs", Tabs: []Tab{{Label: "Simple", Children: []Component{note("a")}}}}), ValidateOptions{}, "tabs needs 2-5"},
		{"duplicate tab label", one(Component{ID: "t", Type: "tabs", Tabs: []Tab{{Label: "A", Children: []Component{note("a")}}, {Label: "A", Children: []Component{note("b")}}}}), ValidateOptions{}, "used twice"},
		{"tab child validated", one(Component{ID: "t", Type: "tabs", Tabs: []Tab{{Label: "A", Children: []Component{note("a")}}, {Label: "B", Children: []Component{{ID: "c", Type: "chart"}}}}}), ValidateOptions{}, `components[0].tabs[1].children[0] (component "c")`},
		{"tabs use tabs not children", one(Component{ID: "t", Type: "tabs", Children: []Component{note("a")}}), ValidateOptions{}, "cannot have children"},
		// chart
		{"chart kind", one(Component{ID: "c", Type: "chart", Summary: "x", Chart: &Chart{Kind: "radar", Series: chart("c").Chart.Series}}), ValidateOptions{}, `chart.kind "radar"`},
		{"chart no series", one(Component{ID: "c", Type: "chart", Summary: "x", Chart: &Chart{Kind: "bar"}}), ValidateOptions{}, "chart.series needs"},
		{"chart point label", one(Component{ID: "c", Type: "chart", Summary: "x", Chart: &Chart{Kind: "bar", Series: []ChartSeries{{Name: "s", Points: []ChartPoint{{Value: 1}}}}}}), ValidateOptions{}, "chart.series[0].points[0].label"},
		// diagram
		{"diagram", one(Component{ID: "d", Type: "diagram", Summary: "x", Diagram: &Diagram{Nodes: []DiagramNode{{ID: "a", Label: "A"}, {ID: "b", Label: "B"}}, Edges: []DiagramEdge{{From: "a", To: "b"}}}}), ValidateOptions{}, ""},
		{"diagram bad edge", one(Component{ID: "d", Type: "diagram", Summary: "x", Diagram: &Diagram{Nodes: []DiagramNode{{ID: "a", Label: "A"}}, Edges: []DiagramEdge{{From: "a", To: "missing"}}}}), ValidateOptions{}, "diagram.edges[0] references a missing node"},
		// form
		{"form", one(Component{ID: "f", Type: "form", Summary: "x", Form: &Form{Fields: []FormField{{ID: "n", Type: "number", Label: "N", Min: float(0), Max: float(5)}}}}), ValidateOptions{}, ""},
		{"form select without options", one(Component{ID: "f", Type: "form", Summary: "x", Form: &Form{Fields: []FormField{{ID: "s", Type: "select", Label: "S"}}}}), ValidateOptions{}, "needs 1-100 options"},
		{"form min on text", one(Component{ID: "f", Type: "form", Summary: "x", Form: &Form{Fields: []FormField{{ID: "s", Type: "text", Label: "S", Min: float(1)}}}}), ValidateOptions{}, "only allowed on number"},
		{"form field type", one(Component{ID: "f", Type: "form", Summary: "x", Form: &Form{Fields: []FormField{{ID: "s", Type: "file", Label: "S"}}}}), ValidateOptions{}, `form.fields[0].type "file"`},
		// stat_row
		{"stat_row", one(Component{ID: "s", Type: "stat_row", Summary: "x", StatRow: &StatRow{Stats: []Stat{{Label: "Saving", Value: 96, Format: StatCurrency, Currency: "GBP", Unit: "/yr", Delta: "+12%", Trend: "up", GoodDirection: "up", Sparkline: []float64{1, 2, 3}}}}}), ValidateOptions{}, ""},
		{"stat_row five stats", one(Component{ID: "s", Type: "stat_row", Summary: "x", StatRow: &StatRow{Stats: make([]Stat, 5)}}), ValidateOptions{}, "stat_row.stats needs 1-4"},
		{"stat_row trend", one(Component{ID: "s", Type: "stat_row", Summary: "x", StatRow: &StatRow{Stats: []Stat{{Label: "a", Value: 1, Trend: "sideways"}}}}), ValidateOptions{}, "trend"},
		{"stat_row percent", one(Component{ID: "s", Type: "stat_row", Summary: "x", StatRow: &StatRow{Stats: []Stat{{Label: "a", Value: 12.5, Format: StatPercent}, {Label: "b", Value: 60, Format: StatDuration}, {Label: "c", Value: -3, Format: StatNumber}}}}), ValidateOptions{}, ""},
		{"stat_row currency missing", one(Component{ID: "s", Type: "stat_row", Summary: "x", StatRow: &StatRow{Stats: []Stat{{Label: "a", Value: 1, Format: StatCurrency}}}}), ValidateOptions{}, "currency is required"},
		{"stat_row currency unknown", one(Component{ID: "s", Type: "stat_row", Summary: "x", StatRow: &StatRow{Stats: []Stat{{Label: "a", Value: 1, Format: StatCurrency, Currency: "ABC"}}}}), ValidateOptions{}, "not an ISO 4217 code"},
		{"stat_row currency lowercase", one(Component{ID: "s", Type: "stat_row", Summary: "x", StatRow: &StatRow{Stats: []Stat{{Label: "a", Value: 1, Format: StatCurrency, Currency: "gbp"}}}}), ValidateOptions{}, "not an ISO 4217 code"},
		{"stat_row currency without format", one(Component{ID: "s", Type: "stat_row", Summary: "x", StatRow: &StatRow{Stats: []Stat{{Label: "a", Value: 1, Currency: "GBP"}}}}), ValidateOptions{}, "only used with format currency"},
		{"stat_row format", one(Component{ID: "s", Type: "stat_row", Summary: "x", StatRow: &StatRow{Stats: []Stat{{Label: "a", Value: 1, Format: "money"}}}}), ValidateOptions{}, `format "money"`},
		{"stat_row sparkline", one(Component{ID: "s", Type: "stat_row", Summary: "x", StatRow: &StatRow{Stats: []Stat{{Label: "a", Value: 1, Sparkline: make([]float64, 31)}}}}), ValidateOptions{}, "sparkline"},
		// actions
		{"respond", one(Component{ID: "a", Type: "actions", Actions: []Action{{ID: "y", Label: "Yes", Kind: ActionRespond, Icon: "check"}}}), ValidateOptions{}, ""},
		{"five actions", one(Component{ID: "a", Type: "actions", Actions: make([]Action, 5)}), ValidateOptions{}, "actions needs 1-4"},
		{"duplicate action id", one(Component{ID: "a", Type: "actions", Actions: []Action{{ID: "y", Label: "Yes", Kind: ActionRespond}, {ID: "y", Label: "No", Kind: ActionRespond}}}), ValidateOptions{}, "actions[1].id"},
		{"action kind", one(Component{ID: "a", Type: "actions", Actions: []Action{{ID: "y", Label: "Go", Kind: "navigate"}}}), ValidateOptions{}, `actions[0].kind "navigate"`},
		{"respond with command", one(Component{ID: "a", Type: "actions", Actions: []Action{{ID: "y", Label: "Go", Kind: ActionRespond, Command: "stop"}}}), ValidateOptions{}, "command is not used by respond"},
		{"approval needs binding", one(Component{ID: "a", Type: "actions", Actions: []Action{{ID: "y", Label: "Send", Kind: ActionApproval, Decision: "approve"}}}), ValidateOptions{SkipHostChecks: true}, "needs an approval binding"},
		{"approval binding on component", one(Component{ID: "a", Type: "actions", Binding: approvalBinding, Actions: []Action{{ID: "y", Label: "Send", Kind: ActionApproval, Decision: "approve"}, {ID: "n", Label: "Don't send", Kind: ActionApproval, Decision: "reject"}}}), ValidateOptions{Bindings: BindingResolverFunc(allow)}, ""},
		{"approval decision", one(Component{ID: "a", Type: "actions", Binding: approvalBinding, Actions: []Action{{ID: "y", Label: "Send", Kind: ActionApproval, Decision: "maybe"}}}), ValidateOptions{SkipHostChecks: true}, "decision must be approve or reject"},
		{"approval wrong binding kind", one(Component{ID: "a", Type: "actions", Actions: []Action{{ID: "y", Label: "Send", Kind: ActionApproval, Decision: "approve", Binding: &Binding{Kind: BindingRun, ID: "r1"}}}}), ValidateOptions{SkipHostChecks: true}, "needs an approval binding"},
		{"entity command", one(Component{ID: "a", Type: "actions", Actions: []Action{{ID: "p", Label: "Pause", Kind: ActionEntity, Command: "pause", Binding: &Binding{Kind: BindingRunbook, ID: "rb1"}}}}), ValidateOptions{Bindings: BindingResolverFunc(allow)}, ""},
		{"entity needs binding", one(Component{ID: "a", Type: "actions", Actions: []Action{{ID: "p", Label: "Pause", Kind: ActionEntity, Command: "pause"}}}), ValidateOptions{}, "needs a binding"},
		{"entity command for kind", one(Component{ID: "a", Type: "actions", Actions: []Action{{ID: "p", Label: "Cancel", Kind: ActionEntity, Command: "cancel", Binding: &Binding{Kind: BindingRunbook, ID: "rb1"}}}}), ValidateOptions{SkipHostChecks: true}, `command "cancel" is not one of pause, resume, run_now, stop`},
		{"entity on run", one(Component{ID: "a", Type: "actions", Actions: []Action{{ID: "p", Label: "Stop", Kind: ActionEntity, Command: "stop", Binding: &Binding{Kind: BindingRun, ID: "r1"}}}}), ValidateOptions{SkipHostChecks: true}, "has no entity commands"},
		{"edit needs a draft", one(note("t"), Component{ID: "a", Type: "actions", Actions: []Action{{ID: "e", Label: "Edit", Kind: ActionEdit, Target: "t"}}}), ValidateOptions{}, "target must be the id of a draft"},
		// icons
		{"icon shape", one(Component{ID: "a", Type: "actions", Actions: []Action{{ID: "y", Label: "Yes", Kind: ActionRespond, Icon: "<svg>"}}}), ValidateOptions{}, "Lucide icon name"},
		// bindings
		{"binding kind", one(Component{ID: "a", Type: "text", Text: "x", Binding: &Binding{Kind: "user", ID: "u1"}}), ValidateOptions{SkipHostChecks: true}, `kind "user"`},
		{"binding without resolver fails closed", one(Component{ID: "a", Type: "text", Text: "x", Binding: &Binding{Kind: BindingRun, ID: "r1"}}), ValidateOptions{}, "remove the binding"},
		{"binding skipped for stored content", one(Component{ID: "a", Type: "text", Text: "x", Binding: &Binding{Kind: BindingRun, ID: "r1"}}), ValidateOptions{SkipHostChecks: true}, ""},
		{"binding rejected by host", one(Component{ID: "a", Type: "text", Text: "x", Binding: &Binding{Kind: BindingConversationTask, ID: "X"}}), ValidateOptions{Bindings: BindingResolverFunc(func(context.Context, Binding) error {
			return errors.New("is not part of this conversation")
		})}, "binding: conversation_task/X: is not part of this conversation"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Validate(context.Background(), tc.surface, tc.opts)
			if tc.err == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.err)
			}
		})
	}
}

func TestValidateErrorsNameThePath(t *testing.T) {
	surface := one(note("lead"), Component{ID: "grid", Type: "grid", Grid: &Grid{Columns: 2}, Children: []Component{note("a"), {ID: "bad", Type: "stat_row"}}})
	_, err := Validate(context.Background(), surface, ValidateOptions{})
	var failure *ValidationError
	if !errors.As(err, &failure) || failure.Path != "components[1].children[1]" || failure.ComponentID != "bad" {
		t.Fatalf("error = %#v", err)
	}
}

func TestUnknownIconWarns(t *testing.T) {
	surface := one(Component{ID: "a", Type: "actions", Actions: []Action{{ID: "y", Label: "Yes", Kind: ActionRespond, Icon: "unicorn-rainbow"}, {ID: "n", Label: "No", Kind: ActionRespond, Icon: "x"}}})
	warnings, err := Validate(context.Background(), surface, ValidateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 || warnings[0].Code != "unknown_icon" || warnings[0].Path != "actions[0].icon" || warnings[0].ComponentID != "a" {
		t.Fatalf("warnings = %#v", warnings)
	}
}

func TestIconsAreCuratedAndSorted(t *testing.T) {
	if len(Icons) < 140 || len(Icons) > 160 {
		t.Fatalf("icon set has %d names; keep it near 150", len(Icons))
	}
	for index, name := range Icons {
		if !iconPattern.MatchString(name) || index > 0 && Icons[index-1] >= name {
			t.Fatalf("icon %q is malformed, duplicated or out of order", name)
		}
	}
}

func TestLinks(t *testing.T) {
	seen := ValidateOptions{LinkProvenance: func(_ context.Context, rawURL string) (bool, error) {
		return rawURL == "https://shop.example/item/1", nil
	}}
	cases := []struct {
		url  string
		opts ValidateOptions
		err  string
	}{
		{"https://shop.example/item/1", seen, ""},
		{"https://shop.example/item/2", seen, "did not appear"},
		{"http://shop.example/item/1", seen, "https"},
		{"javascript:alert(1)", seen, "https"},
		{"https://user:pw@shop.example/", seen, "credentials"},
		{"/relative", seen, "https"},
		{"https://shop.example/item/1", ValidateOptions{}, "cannot be verified"},
		{"https://anything.example/", ValidateOptions{SkipHostChecks: true}, ""},
		{"https://shop.example/" + strings.Repeat("a", MaxLinkBytes), seen, "URL of 1-2048 bytes"},
	}
	for _, tc := range cases {
		err := ValidateLink(context.Background(), tc.url, tc.opts)
		if tc.err == "" && err != nil || tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)) {
			t.Fatalf("ValidateLink(%q) = %v, want %q", tc.url, err, tc.err)
		}
	}
}

func TestLint(t *testing.T) {
	cases := []struct {
		name      string
		component Component
		codes     []string
	}{
		{"clean chart", chart("c"), nil},
		{"no axis labels", Component{ID: "c", Type: "chart", Summary: "x", Chart: &Chart{Kind: "bar", Series: chart("c").Chart.Series}}, []string{LintChartAxisLabels}},
		{"pie needs no axes", Component{ID: "pie", Type: "chart", Summary: "Shares.", Chart: &Chart{Kind: "pie", Series: chart("c").Chart.Series}}, nil},
		{"single point", Component{ID: "c", Type: "chart", Summary: "x", Chart: &Chart{Kind: "bar", XLabel: "a", YLabel: "b", Series: []ChartSeries{{Name: "s", Points: []ChartPoint{{Label: "a", Value: 1}}}}}}, []string{LintChartSinglePoint}},
		{"summary repeats title", Component{ID: "s", Type: "stat_row", Title: "Yearly saving", Summary: "Yearly saving.", StatRow: &StatRow{Stats: []Stat{{Label: "a", Value: 1}}}}, []string{LintSummaryRepeatsTitle}},
		{"single child section", Component{ID: "s", Type: "section", Children: []Component{note("a")}}, []string{LintSingleChild}},
		{"nested lint", Component{ID: "s", Type: "carousel", Children: []Component{note("a"), {ID: "c", Type: "chart", Summary: "x", Chart: &Chart{Kind: "bar", Series: chart("c").Chart.Series}}}}, []string{LintChartAxisLabels}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var codes []string
			for _, warning := range Lint(one(tc.component)) {
				codes = append(codes, warning.Code)
			}
			if strings.Join(codes, ",") != strings.Join(tc.codes, ",") {
				t.Fatalf("lint codes = %v, want %v", codes, tc.codes)
			}
		})
	}
}

func TestInteraction(t *testing.T) {
	form := Component{ID: "f", Type: "form", Summary: "x", Form: &Form{Fields: []FormField{
		{ID: "name", Type: "text", Label: "Name", Required: true},
		{ID: "size", Type: "number", Label: "Size", Min: float(1), Max: float(10)},
		{ID: "kind", Type: "select", Label: "Kind", Options: []FormOption{{Label: "A", Value: "a"}}},
		{ID: "ok", Type: "checkbox", Label: "OK"},
		{ID: "when", Type: "date", Label: "When"},
	}}}
	actions := Component{ID: "a", Type: "actions", Binding: &Binding{Kind: BindingApproval, ID: "ap"}, Actions: []Action{
		{ID: "yes", Label: "Yes", Kind: ActionRespond}, {ID: "send", Label: "Send", Kind: ActionApproval, Decision: "approve"},
	}}
	if rule, err := Interaction(form, ""); err != nil || rule != (InteractionRule{Kind: InteractionSubmit, SingleUse: true, NotifyAgent: true}) {
		t.Fatalf("form rule = %#v, %v", rule, err)
	}
	if rule, err := Interaction(actions, "yes"); err != nil || rule != (InteractionRule{Kind: InteractionRespond, SingleUse: true, NotifyAgent: true}) {
		t.Fatalf("respond rule = %#v, %v", rule, err)
	}
	for name, call := range map[string]func() error{
		"approval is not an interaction": func() error { _, err := Interaction(actions, "send"); return err },
		"missing action":                 func() error { _, err := Interaction(actions, "nope"); return err },
		"form with action id":            func() error { _, err := Interaction(form, "x"); return err },
		"chart":                          func() error { _, err := Interaction(chart("c"), ""); return err },
	} {
		if call() == nil {
			t.Fatalf("%s: expected an error", name)
		}
	}
	values := []struct {
		values map[string]interface{}
		err    string
	}{
		{map[string]interface{}{"name": "Kev", "size": 3.0, "kind": "a", "ok": true, "when": "2026-10-27"}, ""},
		{map[string]interface{}{"size": 3.0}, `field "name" is required`},
		{map[string]interface{}{"name": "Kev", "size": 11.0}, "out of range"},
		{map[string]interface{}{"name": "Kev", "kind": "b"}, "not one of the field's options"},
		{map[string]interface{}{"name": "Kev", "ok": "yes"}, "true or false"},
		{map[string]interface{}{"name": "Kev", "when": "27/10/2026"}, "YYYY-MM-DD"},
		{map[string]interface{}{"name": "Kev", "extra": 1.0}, `no field "extra"`},
	}
	for _, tc := range values {
		err := ValidateInteractionValues(form, "", tc.values)
		if tc.err == "" && err != nil || tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)) {
			t.Fatalf("values %v: error = %v, want %q", tc.values, err, tc.err)
		}
	}
	if err := ValidateInteractionValues(actions, "yes", map[string]interface{}{"x": 1.0}); err == nil {
		t.Fatal("respond accepted values")
	}
}

func TestDecodeRoundTripAndFindNested(t *testing.T) {
	surface := VisualExamples()[6].Visual.Surface(StateReady)
	raw, err := Encode(surface)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if component, ok := decoded.Find("dates"); !ok || component.Type != "timeline" {
		t.Fatalf("Find(dates) = %#v", component)
	}
	if _, err := Decode([]byte(`{"schema":"` + Schema + `","title":"x","state":"ready","components":[{"id":"a","type":"text","text":"x","color":"#f00"}]}`)); err == nil {
		t.Fatal("unknown colour field decoded")
	}
}

func TestVisualExamplesAreValidAndClean(t *testing.T) {
	visuals, texts := 0, 0
	for _, example := range VisualExamples() {
		if example.Visual == nil {
			texts++
			continue
		}
		visuals++
		surface := example.Visual.Surface(StateReady)
		warnings, err := Validate(context.Background(), surface, ValidateOptions{})
		if err != nil {
			t.Fatalf("%s: %v", example.Name, err)
		}
		if lint := Lint(surface); len(warnings)+len(lint) > 0 {
			t.Fatalf("%s: warnings %v lint %v", example.Name, warnings, lint)
		}
	}
	if visuals < 5 || visuals > 8 || texts < 2 || texts > 3 {
		t.Fatalf("want 5-8 visual and 2-3 text-only examples, got %d and %d", visuals, texts)
	}
}

func TestVisualRubricOnlyNamesCatalogueTypes(t *testing.T) {
	rubric := VisualRubric()
	for _, name := range []string{"Most replies are just text", "Stay with text for", "`stat_row`", "`chart`", "`diagram`", "`table`", "Never restate",
		"`comparison`", "`steps`", "`timeline`", "`quiz`", "`checklist`", "`callout`", "`choices`", "`image`, `gallery`"} {
		if !strings.Contains(rubric, name) {
			t.Fatalf("rubric lacks %s:\n%s", name, rubric)
		}
	}
	for _, missing := range []string{"explorer", "poll", "agenda", "flashcards", "labeled_image", "breakdown"} {
		if strings.Contains(rubric, missing) {
			t.Fatalf("rubric names %s, which is not in the catalogue yet", missing)
		}
	}
}

func TestModelFacingTextAvoidsInternalWords(t *testing.T) {
	definition := SkillDefinition()
	examples, _ := json.Marshal(VisualExamples())
	schema, _ := json.Marshal(VisualJSONSchema())
	texts := map[string]string{
		"rubric": VisualRubric(), "examples": string(examples), "schema": string(schema),
		"description": definition.Description, "instructions": definition.Prompt.Instructions, "field": VisualFieldDescription(),
		"update": definition.Actions[UpdateVisual].Description, "updateSchema": mustJSON(t, UpdateVisualInputSchema()), "name": definition.Name,
		"tags": strings.Join(definition.Tags, " "),
	}
	for name, text := range texts {
		// artifactId is the shared JSON key of an image reference; its
		// description says "image already in this chat".
		lower := strings.ToLower(strings.ReplaceAll(text, "artifactId", ""))
		for _, word := range []string{"presentation", "surface", "artifact", "publish"} {
			if strings.Contains(lower, word) {
				t.Fatalf("%s mentions %q", name, word)
			}
		}
	}
}

func TestSkillDefinitionRegisters(t *testing.T) {
	definition := SkillDefinition()
	if definition.ID != SkillID || definition.Version != "2.1.0" || !definition.Requirements.AlwaysAvailable {
		t.Fatalf("definition = %#v", definition)
	}
	if err := skill.NewCatalog().Register(context.Background(), definition); err != nil {
		t.Fatal(err)
	}
	if len(definition.Actions) != 1 || definition.Actions[UpdateVisual].Name != "update_visual" || !strings.Contains(definition.Prompt.Instructions, "update_visual") {
		t.Fatalf("actions = %#v, instructions = %q", definition.Actions, definition.Prompt.Instructions)
	}
}

func mustJSON(t *testing.T, value interface{}) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestVisualFieldDescriptionCarriesRubricAndExamples(t *testing.T) {
	description := VisualFieldDescription()
	if !strings.HasPrefix(description, "Optional visual") || !strings.Contains(description, VisualRubric()) ||
		!strings.Contains(description, `"userAsks":"What's the capital of Australia?"`) || !strings.Contains(description, `"format":"currency","currency":"GBP"`) {
		t.Fatalf("description = %s", description)
	}
}

func TestUpdateVisualSchemaSnapshot(t *testing.T) {
	got, err := json.MarshalIndent(UpdateVisualInputSchema(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	const golden = "testdata/update_visual.schema.json"
	if *updateGolden {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (run go test ./pkg/presentation -update)", err)
	}
	if string(got) != string(want) {
		t.Fatal("update_visual schema changed; review and run go test ./pkg/presentation -update")
	}
}

func TestSchemaUnrollsToMaxDepth(t *testing.T) {
	level := ComponentJSONSchema()
	for depth := 1; depth <= MaxDepth; depth++ {
		properties := level["properties"].(map[string]interface{})
		types := properties["type"].(map[string]interface{})["enum"].([]interface{})
		if depth == MaxDepth {
			if properties["children"] != nil || properties["tabs"] != nil {
				t.Fatalf("depth %d still allows children", depth)
			}
			for _, name := range types {
				if IsLayout(name.(string)) {
					t.Fatalf("depth %d allows layout type %s", depth, name)
				}
			}
			return
		}
		if level["additionalProperties"] != false {
			t.Fatalf("depth %d component schema is open", depth)
		}
		level = properties["children"].(map[string]interface{})["items"].(map[string]interface{})
	}
}
