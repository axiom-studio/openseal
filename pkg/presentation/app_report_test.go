package presentation

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAppInteractionAcceptsOnlyReports(t *testing.T) {
	rule, err := AppInteraction(AppReportComponentID, AppReportAction, "")
	if err != nil || rule != (InteractionRule{Kind: InteractionReport, NotifyAgent: true}) {
		t.Fatalf("rule = %#v, %v", rule, err)
	}
	for _, tc := range []struct{ component, action, item string }{
		{"quiz", AppReportAction, ""}, {AppReportComponentID, "share", ""}, {AppReportComponentID, "", ""}, {AppReportComponentID, AppReportAction, "score"},
	} {
		if _, err := AppInteraction(tc.component, tc.action, tc.item); err == nil {
			t.Fatalf("%+v accepted", tc)
		}
	}
	// Visual components never take the report action.
	if _, err := Interaction(Component{ID: "app", Type: "choices", Choices: &Choices{Chips: []Chip{{ID: "a", Label: "A"}}}}, AppReportAction); err == nil {
		t.Fatal("a visual component accepted the report action")
	}
}

func TestValidateAppReport(t *testing.T) {
	report, err := ValidateAppReport(json.RawMessage(`{"values": {"score": 8, "of": 10, "topic": "times tables"}, "summary": "  Scored 8 out of 10 "}`))
	if err != nil || string(report.Values) != `{"score":8,"of":10,"topic":"times tables"}` || report.Summary != "Scored 8 out of 10" {
		t.Fatalf("report = %s %q, %v", report.Values, report.Summary, err)
	}
	if report, err := ValidateAppReport(json.RawMessage(`{"values":{}}`)); err != nil || string(report.Values) != "{}" || report.Summary != "" {
		t.Fatalf("empty values: %#v, %v", report, err)
	}
	big := `{"values":{"text":"` + strings.Repeat("a", MaxAppReportValuesBytes) + `"}}`
	long := `{"values":{},"summary":"` + strings.Repeat("é", MaxAppReportSummaryRunes+1) + `"}`
	if _, err := ValidateAppReport(json.RawMessage(`{"values":{},"summary":"` + strings.Repeat("é", MaxAppReportSummaryRunes) + `"}`)); err != nil {
		t.Fatalf("200-character summary rejected: %v", err)
	}
	for name, raw := range map[string]string{
		"not an object": `[1]`, "null": `null`, "no values": `{"summary":"hi"}`, "array values": `{"values":[1]}`,
		"string values": `{"values":"8/10"}`, "null values": `{"values":null}`, "unknown key": `{"values":{},"score":8}`,
		"number summary": `{"values":{},"summary":8}`, "multiline summary": `{"values":{},"summary":"a\nb"}`,
		"too big": big, "too long": long, "trailing": `{"values":{}} {}`,
	} {
		if _, err := ValidateAppReport(json.RawMessage(raw)); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

func TestAppReportText(t *testing.T) {
	report := AppReport{Values: json.RawMessage(`{"score":8,"of":10,"topic":"times tables"}`), Summary: "Scored 8 out of 10"}
	if got := AppReportText("Times tables", report); got != `App result ("Times tables"): Scored 8 out of 10 {"score":8,"of":10,"topic":"times tables"}` {
		t.Fatalf("text = %s", got)
	}
	if got := AppReportText("", AppReport{Values: json.RawMessage(`{"done":true}`)}); got != `App result: {"done":true}` {
		t.Fatalf("text = %s", got)
	}
	// The largest report still fits, with its values whole.
	values := `{"t":"` + strings.Repeat("😀", (MaxAppReportValuesBytes-8)/4) + `"}`
	raw, _ := json.Marshal(map[string]interface{}{"values": json.RawMessage(values), "summary": strings.Repeat("😀", MaxAppReportSummaryRunes)})
	full, err := ValidateAppReport(raw)
	if err != nil {
		t.Fatal(err)
	}
	text := AppReportText(strings.Repeat("“Quiz” ", 60), full)
	if len(text) > MaxAppReportTextBytes || !strings.HasSuffix(text, " "+values) || !strings.Contains(text, "…") {
		t.Fatalf("text is %d bytes: %.120s…", len(text), text)
	}
}
