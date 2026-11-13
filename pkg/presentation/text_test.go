package presentation

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestFallbackTextReadsEachComponent(t *testing.T) {
	east, west := "East", "West"
	revenue, growth := 1200.5, 12.0
	surface := Surface{Schema: Schema, Title: "Quarter review", State: StateReady, Components: []Component{
		{ID: "intro", Type: "text", Text: "Revenue **grew**."},
		{ID: "stats", Type: "stat_row", Summary: "Two headline numbers", StatRow: &StatRow{Stats: []Stat{
			{Label: "Revenue", Value: 96000, Format: StatCurrency, Currency: "GBP", Delta: "+12%"},
			{Label: "Call time", Value: 3900, Format: StatDuration},
		}}},
		{ID: "regions", Type: "table", Summary: "Revenue by region", Table: &Table{
			Columns: []TableColumn{{Key: "region", Label: "Region", Kind: "text"}, {Key: "revenue", Label: "Revenue", Kind: "currency", Currency: "USD"}, {Key: "growth", Label: "Growth", Kind: "percent"}},
			Rows: []map[string]TableCell{
				{"region": TextCell(east), "revenue": NumberCell(revenue), "growth": NumberCell(growth)},
				{"region": TextCell(west), "revenue": NumberCell(800), "growth": NumberCell(-3)},
			},
		}},
		{ID: "pick", Type: "choices", Summary: "Next step", Choices: &Choices{Chips: []Chip{{ID: "a", Label: "Dig into East"}, {ID: "b", Label: "Plan Q3"}}}},
		{ID: "views", Type: "tabs", Tabs: []Tab{
			{Label: "Quiz", Children: []Component{{ID: "q", Type: "quiz", Summary: "Check", Quiz: &Quiz{
				Question: "Which region grew?", Options: []QuizOption{{ID: "e", Label: "East"}, {ID: "w", Label: "West"}},
				AnswerKey: &QuizAnswerKey{Correct: []string{"e"}, Explanation: "SECRET-EXPLANATION"},
			}}}},
			{Label: "Notes", Children: []Component{{ID: "n", Type: "caption", Text: "Unaudited."}}},
		}},
	}}
	want := strings.Join([]string{
		"**Quarter review**",
		"Revenue **grew**.",
		"Two headline numbers",
		"- Revenue: GBP 96,000 (+12%)",
		"- Call time: 1h 5m",
		"Revenue by region",
		"- Region | Revenue | Growth",
		"- East | USD 1,200.5 | 12%",
		"- West | USD 800 | -3%",
		"Next step",
		"- Dig into East",
		"- Plan Q3",
		"**Quiz**",
		"Check",
		"- Which region grew?",
		"- East",
		"- West",
		"**Notes**",
		"Unaudited.",
	}, "\n")
	if got := FallbackText(surface); got != want {
		t.Fatalf("fallback =\n%s\nwant\n%s", got, want)
	}
}

func TestFallbackTextIsBounded(t *testing.T) {
	items := make([]ChecklistItem, 40)
	for index := range items {
		items[index] = ChecklistItem{ID: string(rune('a' + index%26)), Label: strings.Repeat("x", 200)}
	}
	list := Component{ID: "pack", Type: "checklist", Summary: "Packing", Checklist: &Checklist{Items: items}}
	lines := componentFallbackLines(list)
	if len(lines) != 1+maxFallbackFacts+1 || lines[len(lines)-1] != "- …and 34 more" {
		t.Fatalf("lines = %q", lines)
	}
	if utf8.RuneCountInString(lines[1]) != 2+maxFallbackFactRunes {
		t.Fatalf("fact runes = %d", utf8.RuneCountInString(lines[1]))
	}
	components := make([]Component, 30)
	for index := range components {
		components[index] = list
	}
	text := FallbackText(Surface{Title: "Trip", Components: components})
	if utf8.RuneCountInString(text) > MaxFallbackRunes || !strings.HasSuffix(text, "…") {
		t.Fatalf("fallback runes = %d", utf8.RuneCountInString(text))
	}
}
