package presentation

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

var noHost = ValidateOptions{SkipHostChecks: true}

func readOne(t *testing.T, answer string) Component {
	t.Helper()
	visual, drops := ReadBlocks(context.Background(), answer, noHost)
	if visual == nil || len(drops) > 0 {
		t.Fatalf("ReadBlocks(%q): visual %v, drops %+v", answer, visual, drops)
	}
	if len(visual.Components) != 1 {
		t.Fatalf("got %d components, want 1", len(visual.Components))
	}
	return visual.Components[0]
}

func TestParseAnswerFindsTopLevelBlocks(t *testing.T) {
	answer := "Lead.\n\n:::callout{#tip tone=warning}\nBody\n:::\n\nMiddle.\n\n::::section{title=\"S\"}\n:::text\nx\n:::\n::::\n\n```md\n:::chart\n```\n\n:::chart{kind=bar}\n| A | B |\n"
	blocks := ParseAnswer(answer)
	want := []Block{{ID: "tip", Type: "callout", Closed: true}, {ID: "section-1", Type: "section", Closed: true}, {ID: "chart-1", Type: "chart"}}
	if len(blocks) != len(want) {
		t.Fatalf("blocks %+v", blocks)
	}
	for index, block := range blocks {
		if block.ID != want[index].ID || block.Type != want[index].Type || block.Closed != want[index].Closed {
			t.Fatalf("block %d = %+v, want %+v", index, block, want[index])
		}
	}
	if got := answer[blocks[0].Start:blocks[0].End]; got != ":::callout{#tip tone=warning}\nBody\n:::\n" {
		t.Fatalf("block 0 text %q", got)
	}
	if blocks[2].End != len(answer) {
		t.Fatalf("an open block runs to the end")
	}
}

func TestSameFenceNestingCloses(t *testing.T) {
	// Models often write the same colons for a layout and its children.
	answer := ":::section{#s title=Plan}\n:::text{#a}\none\n:::\n:::text{#b}\ntwo\n:::\n:::\n"
	component := readOne(t, answer)
	if component.Type != "section" || len(component.Children) != 2 || component.Children[1].Text != "two" {
		t.Fatalf("component %+v", component)
	}
}

func TestBlockIDs(t *testing.T) {
	blocks := ParseAnswer(":::text\na\n:::\n:::text{#x}\nb\n:::\n:::text{#x}\nc\n:::\n:::text\nd\n:::\n")
	var ids []string
	for _, block := range blocks {
		ids = append(ids, block.ID)
	}
	if want := []string{"text-1", "x", "x-2", "text-4"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("ids %v, want %v", ids, want)
	}
}

func TestAttributes(t *testing.T) {
	attrs := parseAttributes(`{#id .class key=value quoted="a \"b\" c" single='x y' flag binding.kind=routine}`)
	want := map[string]interface{}{"id": "id", "key": "value", "quoted": `a "b" c`, "single": "x y", "flag": true,
		"binding": map[string]interface{}{"kind": "routine"}}
	if !reflect.DeepEqual(attrs, want) {
		t.Fatalf("attrs %#v", attrs)
	}
}

func TestChartFromTable(t *testing.T) {
	component := readOne(t, ":::chart{#spend kind=bar title=\"Spend\" format=currency currency=EUR}\n| Month | Groceries | Travel |\n|---|---:|---|\n| Jan | €320 | 1,200 |\n| Feb | 290 | 410 |\n:::")
	chart := component.Chart
	if chart == nil || chart.Kind != "bar" || chart.Currency != "EUR" || len(chart.Series) != 2 {
		t.Fatalf("chart %+v", chart)
	}
	if got := chart.Series[1]; got.Name != "Travel" || got.Points[0] != (ChartPoint{Label: "Jan", Value: 1200}) {
		t.Fatalf("series %+v", got)
	}
	if component.Title != "Spend" || component.ID != "spend" {
		t.Fatalf("envelope %+v", component)
	}
}

func TestTableFromTable(t *testing.T) {
	component := readOne(t, ":::table{#rates}\n| Currency | Rate |\n|---|---|\n| EUR | 0.91 |\n| GBP | 0.79 |\n:::")
	table := component.Table
	if table == nil || len(table.Columns) != 2 || table.Columns[1].Key != "rate" || len(table.Rows) != 2 {
		t.Fatalf("table %+v", table)
	}
	if cell := table.Rows[0]["rate"]; cell.Text == nil || *cell.Text != "0.91" {
		t.Fatalf("a number in a text column is read as text: %+v", cell)
	}
}

func TestTableColumnsFromYAML(t *testing.T) {
	component := readOne(t, ":::table\ncolumns:\n  - {key: item, label: Item}\n  - {key: price, label: Price, kind: currency, currency: GBP}\n| Item | Price |\n|---|---|\n| Tea | 2.50 |\n:::")
	if cell := component.Table.Rows[0]["price"]; cell.Number == nil || *cell.Number != 2.5 {
		t.Fatalf("price cell %+v", cell)
	}
}

func TestYAMLAndJSONBodies(t *testing.T) {
	yamlBlock := ":::callout{tone=info}\ntitle: Note\nbody: Bring a coat.\n:::"
	jsonBlock := `:::callout{tone=info}
{"title": "Note", "body": "Bring a coat."}
:::`
	for _, answer := range []string{yamlBlock, jsonBlock} {
		component := readOne(t, answer)
		if component.Callout == nil || component.Callout.Body != "Bring a coat." {
			t.Fatalf("%q: %+v", answer, component)
		}
	}
}

func TestPayloadNestedUnderItsKey(t *testing.T) {
	component := readOne(t, ":::callout\ncallout:\n  body: Nested.\n:::")
	if component.Callout == nil || component.Callout.Body != "Nested." {
		t.Fatalf("%+v", component)
	}
}

func TestLayoutProseBecomesText(t *testing.T) {
	component := readOne(t, "::::section{#s title=Trip}\nIntro **bold**.\n:::callout\nbody: Pack light.\n:::\nOutro.\n::::")
	if len(component.Children) != 3 || component.Children[0].Text != "Intro **bold**." || component.Children[1].Type != "callout" || component.Children[2].Text != "Outro." {
		t.Fatalf("children %+v", component.Children)
	}
}

func TestTabs(t *testing.T) {
	component := readOne(t, "::::tabs{#views}\n:::tab{label=Simple}\nShort.\n:::\n:::tab{label=Detailed}\nLong.\n:::\n::::")
	if len(component.Tabs) != 2 || component.Tabs[1].Label != "Detailed" || component.Tabs[1].Children[0].Text != "Long." {
		t.Fatalf("tabs %+v", component.Tabs)
	}
}

func TestFailingBlockCostsOnlyItself(t *testing.T) {
	answer := ":::callout{#good}\nbody: Fine.\n:::\n\n:::chart{#bad kind=nope}\n| A | B |\n|---|---|\n| x | 1 |\n:::\n\n:::sparkle{#odd}\n:::\n\n::visual{#old}\n"
	visual, drops := ReadBlocks(context.Background(), answer, noHost)
	if visual == nil || len(visual.Components) != 1 || visual.Components[0].ID != "good" {
		t.Fatalf("visual %+v", visual)
	}
	whole := map[string]string{}
	for _, drop := range drops {
		if drop.Whole {
			whole[drop.BlockID] = drop.Reason
		}
	}
	for _, id := range []string{"bad", "odd", "old"} {
		if whole[id] == "" {
			t.Fatalf("block %s not dropped whole: %+v", id, drops)
		}
	}
	if !strings.Contains(whole["odd"], "not a visual type") || !strings.Contains(whole["old"], "earlier answer") {
		t.Fatalf("reasons %+v", whole)
	}
}

func TestOpenBlocksAreNotRead(t *testing.T) {
	visual, drops := ReadBlocks(context.Background(), "Lead\n:::callout\nbody: half", noHost)
	if visual != nil || len(drops) > 0 {
		t.Fatalf("visual %+v drops %+v", visual, drops)
	}
}

func TestPlacedAndPlainAnswer(t *testing.T) {
	answer := "Lead.\n\n:::callout{#tip}\nbody: x\n:::\n\nMiddle.\n\n:::chart{#gone}\n:::\n\nEnd."
	placed := PlacedAnswer(answer, map[string]bool{"gone": true})
	if placed != "Lead.\n\n::visual{#tip}\n\nMiddle.\n\nEnd." {
		t.Fatalf("placed %q", placed)
	}
	if plain := PlainAnswer(placed); plain != "Lead.\n\nMiddle.\n\nEnd." {
		t.Fatalf("plain of placed %q", plain)
	}
	if plain := PlainAnswer(answer); plain != "Lead.\n\nMiddle.\n\nEnd." {
		t.Fatalf("plain %q", plain)
	}
}

func TestReplaceBlocks(t *testing.T) {
	answer := "A\n:::chart{#c kind=nope}\n:::\nB\n"
	repaired, missing := ReplaceBlocks(answer, map[string]string{"c": ":::callout{#c}\nbody: ok\n:::", "zz": "x"})
	if repaired != "A\n:::callout{#c}\nbody: ok\n:::\nB\n" || !reflect.DeepEqual(missing, []string{"zz"}) {
		t.Fatalf("repaired %q missing %v", repaired, missing)
	}
}

func TestNeedsHost(t *testing.T) {
	if NeedsHost(Component{ID: "a", Type: "text", Text: "x"}) {
		t.Fatal("text needs no host")
	}
	if !NeedsHost(Component{ID: "a", Type: "text", Text: "x", Sources: []Source{{URL: "https://example.com", Title: "E"}}}) {
		t.Fatal("a link needs the host")
	}
	if !NeedsHost(Component{ID: "a", Type: "routine", Binding: &Binding{Kind: BindingRunbook, ID: "r"}}) {
		t.Fatal("a binding needs the host")
	}
}

// TestEveryExampleRoundTrips writes every worked example as blocks and
// reads it back: the format the model is shown is the format the host
// reads, for every type the examples use.
func TestEveryExampleRoundTrips(t *testing.T) {
	for _, example := range VisualExamples() {
		if example.Visual == nil {
			continue
		}
		t.Run(example.Name, func(t *testing.T) {
			want, drops, err := DecodeVisual([]byte(mustJSON(t, example.Visual)))
			if err != nil || len(drops) > 0 {
				t.Fatalf("example does not decode: %v %+v", err, drops)
			}
			var answer strings.Builder
			answer.WriteString(example.Lead + "\n\n")
			for _, component := range example.Visual.Components {
				answer.WriteString(FormatBlock(component) + "\n\n")
			}
			got, blockDrops := ReadBlocks(context.Background(), answer.String(), noHost)
			if got == nil || len(blockDrops) > 0 {
				t.Fatalf("blocks do not read back: %+v\n%s", blockDrops, answer.String())
			}
			if a, b := mustJSON(t, got.Components), mustJSON(t, want.Components); a != b {
				t.Fatalf("round trip differs\n got %s\nwant %s\nblocks:\n%s", a, b, answer.String())
			}
		})
	}
}

// TestEveryTypeReadsFromABlock writes one sample of every catalogue type as
// a block and reads it back.
func TestEveryTypeReadsFromABlock(t *testing.T) {
	for _, example := range blockSamples(t) {
		answer := FormatBlock(example)
		visual, drops := ReadBlocks(context.Background(), answer, noHost)
		if visual == nil || len(drops) > 0 {
			t.Errorf("%s: drops %+v\n%s", example.Type, drops, answer)
		}
	}
}

// blockSamples is one component of every type the examples use.
func blockSamples(t *testing.T) []Component {
	t.Helper()
	seen := map[string]bool{}
	var samples []Component
	var visit func(components []Component)
	visit = func(components []Component) {
		for _, component := range components {
			if !seen[component.Type] {
				seen[component.Type] = true
				samples = append(samples, component)
			}
			visit(component.Children)
		}
	}
	for _, example := range VisualExamples() {
		if example.Visual != nil {
			visit(example.Visual.Components)
		}
	}
	return samples
}

func TestPreviewBlocksSkipOpenAndHostBlocks(t *testing.T) {
	blocks := PreviewBlocks(context.Background(), ":::text{#a}\nShown.\n:::\n\n:::callout{#b}\nbody: Cited.\nsources:\n  - {url: \"https://example.com\", title: E}\n:::\n\n:::text{#c}\nstill writing")
	if len(blocks) != 1 || blocks[0].ID != "a" {
		t.Fatalf("blocks = %+v", blocks)
	}
}
