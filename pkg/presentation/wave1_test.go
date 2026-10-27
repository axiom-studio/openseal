package presentation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func images(ids ...string) ValidateOptions {
	known := map[string]bool{}
	for _, id := range ids {
		known[id] = true
	}
	return ValidateOptions{Images: ImageResolverFunc(func(_ context.Context, ref ImageRef) error {
		if !known[ref.ArtifactID] {
			return errors.New("is not an image in this chat")
		}
		return nil
	})}
}

func img(id string) *ImageRef { return &ImageRef{ArtifactID: id, Version: 1} }

func stepsOf(count int) []Step {
	list := make([]Step, count)
	for index := range list {
		list[index] = Step{ID: fmt.Sprintf("s%d", index), Title: "Step"}
	}
	return list
}

func compare(options ...ComparisonOption) Component {
	return Component{ID: "cmp", Type: "comparison", Summary: "x", Comparison: &Comparison{
		Attributes: []ComparisonAttribute{{Key: "data", Label: "Data"}, {Key: "term", Label: "Term"}}, Options: options,
	}}
}

func option(id string) ComparisonOption {
	return ComparisonOption{ID: id, Name: strings.ToUpper(id), Values: map[string]string{"data": "30 GB", "term": ""}}
}

func points(count int) []ChartPoint {
	list := make([]ChartPoint, count)
	for index := range list {
		list[index] = ChartPoint{Label: fmt.Sprintf("p%d", index), Value: float64(index)}
	}
	return list
}

func chartOf(kind string, list ...ChartSeries) Component {
	return Component{ID: "c", Type: "chart", Summary: "x", Chart: &Chart{Kind: kind, XLabel: "x", YLabel: "y", Series: list}}
}

func quizOf(multiple bool, correct ...string) Component {
	return Component{ID: "q", Type: "quiz", Title: "Cells", Summary: "x", Quiz: &Quiz{
		Question: "Where is most ATP made?", AllowMultiple: multiple,
		Options:   []QuizOption{{ID: "nucleus", Label: "Nucleus"}, {ID: "mito", Label: "Mitochondria"}, {ID: "wall", Label: "Cell wall"}},
		AnswerKey: &QuizAnswerKey{Correct: correct, Explanation: "The **mitochondria**."},
	}}
}

func checklistOf(count int) Component {
	items := make([]ChecklistItem, count)
	for index := range items {
		items[index] = ChecklistItem{ID: fmt.Sprintf("i%d", index), Label: fmt.Sprintf("Item %d", index)}
	}
	return Component{ID: "pack", Type: "checklist", Title: "Packing list", Summary: "x", Checklist: &Checklist{Items: items}}
}

func tableOf(columns []TableColumn, rows ...map[string]TableCell) Component {
	return Component{ID: "t", Type: "table", Summary: "x", Table: &Table{Columns: columns, Rows: rows}}
}

func TestValidateWave1(t *testing.T) {
	textColumn := TableColumn{Key: "name", Label: "Name", Kind: "text"}
	manyRows := make([]map[string]TableCell, 51)
	for index := range manyRows {
		manyRows[index] = map[string]TableCell{"name": TextCell("x")}
	}
	cases := []struct {
		name      string
		component Component
		opts      ValidateOptions
		err       string
	}{
		// every new type needs a summary
		{"callout summary", Component{ID: "a", Type: "callout", Callout: &Callout{Body: "x"}}, ValidateOptions{}, "summary is required for callout"},
		{"choices summary", Component{ID: "a", Type: "choices", Choices: &Choices{Chips: []Chip{{ID: "a", Label: "A"}, {ID: "b", Label: "B"}}}}, ValidateOptions{}, "summary is required for choices"},
		{"payload missing", Component{ID: "a", Type: "steps", Summary: "x"}, ValidateOptions{}, "steps payload is required"},
		{"foreign payload", Component{ID: "a", Type: "callout", Summary: "x", Callout: &Callout{Body: "x"}, Checklist: checklistOf(1).Checklist}, ValidateOptions{}, `callout cannot carry a "checklist" payload`},
		// callout
		{"callout", Component{ID: "a", Type: "callout", Summary: "x", Tone: ToneWarning, Callout: &Callout{Icon: "lightbulb", Body: "Use **less** $x^2$ < 3"}}, ValidateOptions{}, ""},
		{"callout empty", Component{ID: "a", Type: "callout", Summary: "x", Callout: &Callout{}}, ValidateOptions{}, "callout.body must contain between 1 and 300"},
		{"callout long", Component{ID: "a", Type: "callout", Summary: "x", Callout: &Callout{Body: strings.Repeat("a", 301)}}, ValidateOptions{}, "callout.body must contain between 1 and 300"},
		{"callout html", Component{ID: "a", Type: "callout", Summary: "x", Callout: &Callout{Body: "<b>hi</b>"}}, ValidateOptions{}, "callout.body must not contain HTML"},
		{"callout md image", Component{ID: "a", Type: "callout", Summary: "x", Callout: &Callout{Body: "![x](https://x.example/a.png)"}}, ValidateOptions{}, "must not contain markdown images"},
		{"callout icon shape", Component{ID: "a", Type: "callout", Summary: "x", Callout: &Callout{Icon: "Bad Icon", Body: "x"}}, ValidateOptions{}, "callout.icon"},
		// steps
		{"steps", Component{ID: "a", Type: "steps", Summary: "x", Steps: &Steps{Items: stepsOf(10), Checkable: true}}, ValidateOptions{}, ""},
		{"one step", Component{ID: "a", Type: "steps", Summary: "x", Steps: &Steps{Items: stepsOf(1)}}, ValidateOptions{}, "steps.items needs 2-10 (got 1)"},
		{"eleven steps", Component{ID: "a", Type: "steps", Summary: "x", Steps: &Steps{Items: stepsOf(11)}}, ValidateOptions{}, "steps.items needs 2-10 (got 11)"},
		{"step id", Component{ID: "a", Type: "steps", Summary: "x", Steps: &Steps{Items: []Step{{ID: "A", Title: "a"}, {ID: "b", Title: "b"}}}}, ValidateOptions{}, `steps.items[0].id "A" must match`},
		{"step id twice", Component{ID: "a", Type: "steps", Summary: "x", Steps: &Steps{Items: []Step{{ID: "a", Title: "a"}, {ID: "a", Title: "b"}}}}, ValidateOptions{}, `steps.items[1].id "a" is used twice`},
		{"step title", Component{ID: "a", Type: "steps", Summary: "x", Steps: &Steps{Items: []Step{{ID: "a", Title: strings.Repeat("t", 81)}, {ID: "b", Title: "b"}}}}, ValidateOptions{}, "steps.items[0].title"},
		{"step detail", Component{ID: "a", Type: "steps", Summary: "x", Steps: &Steps{Items: []Step{{ID: "a", Title: "a", Detail: strings.Repeat("d", 401)}, {ID: "b", Title: "b"}}}}, ValidateOptions{}, "steps.items[0].detail"},
		{"step image", Component{ID: "a", Type: "steps", Summary: "x", Steps: &Steps{Items: []Step{{ID: "a", Title: "a", Image: img("img-1")}, {ID: "b", Title: "b"}}}}, images("img-1"), ""},
		// images
		{"image without resolver", Component{ID: "a", Type: "image", Summary: "x", Image: &ImageBlock{Image: *img("img-1")}}, ValidateOptions{}, "image.image: this host cannot show images yet; leave it out"},
		{"image resolved", Component{ID: "a", Type: "image", Summary: "x", Image: &ImageBlock{Image: *img("img-1"), Caption: "A cell", Aspect: "square"}}, images("img-1"), ""},
		{"image unknown", Component{ID: "a", Type: "image", Summary: "x", Image: &ImageBlock{Image: *img("img-2")}}, images("img-1"), "image.image (image img-2 version 1): is not an image in this chat"},
		{"image skipped for stored content", Component{ID: "a", Type: "image", Summary: "x", Image: &ImageBlock{Image: *img("img-2")}}, ValidateOptions{SkipHostChecks: true}, ""},
		{"image version", Component{ID: "a", Type: "image", Summary: "x", Image: &ImageBlock{Image: ImageRef{ArtifactID: "img-1"}}}, images("img-1"), "image.image.version must be 1 or more"},
		{"image url", Component{ID: "a", Type: "image", Summary: "x", Image: &ImageBlock{Image: ImageRef{ArtifactID: "https://x.example/a.png", Version: 1}}}, images(), "must be the id of an image in this chat"},
		{"image aspect", Component{ID: "a", Type: "image", Summary: "x", Image: &ImageBlock{Image: *img("img-1"), Aspect: "wide"}}, images("img-1"), `image.aspect "wide"`},
		{"image caption", Component{ID: "a", Type: "image", Summary: "x", Image: &ImageBlock{Image: *img("img-1"), Caption: strings.Repeat("c", 201)}}, images("img-1"), "image.caption"},
		{"gallery", Component{ID: "a", Type: "gallery", Summary: "x", Gallery: &Gallery{Layout: "strip", Images: []GalleryImage{{Image: *img("a")}, {Image: *img("b"), Caption: "B"}}}}, images("a", "b"), ""},
		{"gallery of one", Component{ID: "a", Type: "gallery", Summary: "x", Gallery: &Gallery{Images: []GalleryImage{{Image: *img("a")}}}}, images("a"), "gallery.images needs 2-12 (got 1)"},
		{"gallery second unknown", Component{ID: "a", Type: "gallery", Summary: "x", Gallery: &Gallery{Images: []GalleryImage{{Image: *img("a")}, {Image: *img("b")}}}}, images("a"), "gallery.images[1].image"},
		{"gallery layout", Component{ID: "a", Type: "gallery", Summary: "x", Gallery: &Gallery{Layout: "masonry", Images: []GalleryImage{{Image: *img("a")}, {Image: *img("b")}}}}, images("a", "b"), `gallery.layout "masonry"`},
		// comparison
		{"comparison", func() Component {
			c := compare(option("a"), option("b"))
			c.Comparison.Recommended, c.Comparison.Pickable = "b", true
			c.Comparison.Options[0].Price = &Price{Amount: Amount{Value: 17, Format: StatCurrency, Currency: "GBP"}, Per: "month"}
			c.Comparison.Options[0].Pros = []string{"Cheap"}
			return c
		}(), ValidateOptions{}, ""},
		{"comparison one option", compare(option("a")), ValidateOptions{}, "comparison.options needs 2-4 (got 1)"},
		{"comparison five options", compare(option("a"), option("b"), option("c"), option("d"), option("e")), ValidateOptions{}, "comparison.options needs 2-4 (got 5)"},
		{"comparison missing value", func() Component {
			c := compare(option("a"), option("b"))
			c.Comparison.Options[1].Values = map[string]string{"data": "1 GB"}
			return c
		}(), ValidateOptions{}, `comparison.options[1].values needs a value for "term"`},
		{"comparison unknown value", func() Component {
			c := compare(option("a"), option("b"))
			c.Comparison.Options[0].Values["speed"] = "fast"
			return c
		}(), ValidateOptions{}, `comparison.options[0].values.speed: no attribute has key "speed"`},
		{"comparison recommended", func() Component {
			c := compare(option("a"), option("b"))
			c.Comparison.Recommended = "z"
			return c
		}(), ValidateOptions{}, `comparison.recommended "z"`},
		{"comparison price currency", func() Component {
			c := compare(option("a"), option("b"))
			c.Comparison.Options[0].Price = &Price{Amount: Amount{Value: 17, Format: StatCurrency}}
			return c
		}(), ValidateOptions{}, "comparison.options[0].price.currency is required"},
		{"comparison price per", func() Component {
			c := compare(option("a"), option("b"))
			c.Comparison.Options[0].Price = &Price{Amount: Amount{Value: 17}, Per: "week"}
			return c
		}(), ValidateOptions{}, `comparison.options[0].price.per "week"`},
		{"comparison five pros", func() Component {
			c := compare(option("a"), option("b"))
			c.Comparison.Options[0].Pros = []string{"a", "b", "c", "d", "e"}
			return c
		}(), ValidateOptions{}, "comparison.options[0].pros allows at most 4"},
		{"comparison eleven attributes", func() Component {
			c := compare(option("a"), option("b"))
			for index := 0; index < 9; index++ {
				c.Comparison.Attributes = append(c.Comparison.Attributes, ComparisonAttribute{Key: fmt.Sprintf("k%d", index), Label: "K"})
			}
			return c
		}(), ValidateOptions{}, "comparison.attributes needs 1-10 (got 11)"},
		{"comparison option image", func() Component {
			c := compare(option("a"), option("b"))
			c.Comparison.Options[0].Image = img("x")
			return c
		}(), ValidateOptions{}, "comparison.options[0].image: this host cannot show images yet"},
		// chart
		{"chart six series", chartOf("line", series("a", []string{"1", "2", "3"}, 1, 2, 3), series("b", []string{"1", "2", "3"}, 1, 2, 3)), ValidateOptions{}, ""},
		{"chart seven series", chartOf("line", func() []ChartSeries {
			list := make([]ChartSeries, 7)
			for index := range list {
				list[index] = ChartSeries{Name: fmt.Sprintf("s%d", index), Points: points(3)}
			}
			return list
		}()...), ValidateOptions{}, "chart.series needs 1-6 (got 7)"},
		{"chart 61 points", chartOf("bar", ChartSeries{Name: "s", Points: points(61)}), ValidateOptions{}, "chart.series[0].points needs 1-60 (got 61)"},
		{"pie seven", chartOf("pie", ChartSeries{Name: "s", Points: points(7)}), ValidateOptions{}, ""},
		{"pie eight", chartOf("donut", ChartSeries{Name: "s", Points: points(8)}), ValidateOptions{}, `a donut chart allows at most 7 slices (got 8); group the smallest as "Other"`},
		{"pie two series", chartOf("pie", ChartSeries{Name: "a", Points: points(2)}, ChartSeries{Name: "b", Points: points(2)}), ValidateOptions{}, "a pie chart takes exactly 1 series"},
		{"stacked one series", chartOf("stacked_bar", ChartSeries{Name: "a", Points: points(2)}), ValidateOptions{}, "a stacked_bar chart needs at least 2 series"},
		{"grouped labels differ", chartOf("grouped_bar", ChartSeries{Name: "a", Points: points(2)}, series("b", []string{"p0", "x"}, 1, 2)), ValidateOptions{}, "chart.series[1] must use the same point labels"},
		{"grouped", chartOf("grouped_bar", ChartSeries{Name: "a", Points: points(3)}, ChartSeries{Name: "b", Points: points(3)}), ValidateOptions{}, ""},
		{"scatter numeric", chartOf("scatter", series("a", []string{"1.5", "-2", "3e2"}, 1, 2, 3)), ValidateOptions{}, ""},
		{"scatter label", chartOf("scatter", series("a", []string{"1", "two"}, 1, 2)), ValidateOptions{}, `chart.series[0].points[1].label "two" must be a number`},
		{"series name twice", chartOf("line", ChartSeries{Name: "a", Points: points(3)}, ChartSeries{Name: "a", Points: points(3)}), ValidateOptions{}, `chart.series[1].name "a" is used twice`},
		{"chart format", func() Component {
			c := chartOf("bar", ChartSeries{Name: "a", Points: points(3)})
			c.Chart.Format, c.Chart.Currency, c.Chart.Unit = StatCurrency, "EUR", "/mo"
			c.Chart.Annotations = []ChartAnnotation{{X: "p1", Label: "Price rise"}}
			c.Chart.ReferenceLine = &ReferenceLine{Value: 1.5, Label: "Average"}
			c.Chart.Highlight = &ChartHighlight{Series: "a", X: "p2"}
			return c
		}(), ValidateOptions{}, ""},
		{"chart currency without format", func() Component {
			c := chartOf("bar", ChartSeries{Name: "a", Points: points(3)})
			c.Chart.Currency = "EUR"
			return c
		}(), ValidateOptions{}, "chart.currency is only used with format currency"},
		{"annotation x", func() Component {
			c := chartOf("bar", ChartSeries{Name: "a", Points: points(3)})
			c.Chart.Annotations = []ChartAnnotation{{X: "nope", Label: "x"}}
			return c
		}(), ValidateOptions{}, `chart.annotations[0].x "nope" must be the label of a point`},
		{"five annotations", func() Component {
			c := chartOf("bar", ChartSeries{Name: "a", Points: points(3)})
			c.Chart.Annotations = make([]ChartAnnotation, 5)
			return c
		}(), ValidateOptions{}, "chart.annotations allows at most 4"},
		{"highlight series", func() Component {
			c := chartOf("bar", ChartSeries{Name: "a", Points: points(3)})
			c.Chart.Highlight = &ChartHighlight{Series: "b"}
			return c
		}(), ValidateOptions{}, `chart.highlight.series "b"`},
		{"empty highlight", func() Component {
			c := chartOf("bar", ChartSeries{Name: "a", Points: points(3)})
			c.Chart.Highlight = &ChartHighlight{}
			return c
		}(), ValidateOptions{}, "chart.highlight needs"},
		// table
		{"table", tableOf([]TableColumn{textColumn, {Key: "price", Label: "Price", Kind: "currency", Currency: "GBP"}, {Key: "share", Label: "Share", Kind: "percent"},
			{Key: "when", Label: "When", Kind: "date"}, {Key: "pic", Label: "Photo", Kind: "image"}, {Key: "tag", Label: "Tag", Kind: "badge"}},
			map[string]TableCell{"name": TextCell("Basic"), "price": NumberCell(10), "share": NumberCell(12.5), "when": TextCell("2026-10-27"), "pic": ImageCell(*img("a")), "tag": TextCell("New")},
			map[string]TableCell{"name": TextCell("Plus")}), images("a"), ""},
		{"table nine columns", tableOf(func() []TableColumn {
			list := make([]TableColumn, 9)
			for index := range list {
				list[index] = TableColumn{Key: fmt.Sprintf("c%d", index), Label: "C", Kind: "text"}
			}
			return list
		}()), ValidateOptions{}, "table.columns needs 1-8 (got 9)"},
		{"table 51 rows", tableOf([]TableColumn{textColumn}, manyRows...), ValidateOptions{}, "table.rows allows at most 50 (got 51)"},
		{"table column kind", tableOf([]TableColumn{{Key: "a", Label: "A", Kind: "link"}}), ValidateOptions{}, `table.columns[0].kind "link"`},
		{"table currency column", tableOf([]TableColumn{{Key: "a", Label: "A", Kind: "currency"}}), ValidateOptions{}, "table.columns[0].currency is required"},
		{"table currency on text", tableOf([]TableColumn{{Key: "a", Label: "A", Kind: "text", Currency: "GBP"}}), ValidateOptions{}, "only used by currency columns"},
		{"table key twice", tableOf([]TableColumn{textColumn, textColumn}), ValidateOptions{}, `table.columns[1].key "name" is used twice`},
		{"table unknown cell", tableOf([]TableColumn{textColumn}, map[string]TableCell{"price": NumberCell(1)}), ValidateOptions{}, `table.rows[0].price: no column has key "price"`},
		{"table number as text", tableOf([]TableColumn{{Key: "n", Label: "N", Kind: "number"}}, map[string]TableCell{"n": TextCell("10")}), ValidateOptions{}, `table.rows[0].n must be a number because column "n" is number`},
		{"table text as number", tableOf([]TableColumn{textColumn}, map[string]TableCell{"name": NumberCell(1)}), ValidateOptions{}, `table.rows[0].name must be text`},
		{"table date", tableOf([]TableColumn{{Key: "d", Label: "D", Kind: "date"}}, map[string]TableCell{"d": TextCell("2026-02-30")}), ValidateOptions{}, "must be a date as YYYY-MM-DD"},
		{"table long text", tableOf([]TableColumn{textColumn}, map[string]TableCell{"name": TextCell(strings.Repeat("x", 121))}), ValidateOptions{}, "table.rows[0].name must be at most 120"},
		{"table image needs resolver", tableOf([]TableColumn{{Key: "p", Label: "P", Kind: "image"}}, map[string]TableCell{"p": ImageCell(*img("a"))}), ValidateOptions{}, "table.rows[0].p: this host cannot show images yet"},
		// timeline
		{"timeline", Component{ID: "a", Type: "timeline", Summary: "x", Timeline: &Timeline{Events: []TimelineEvent{{ID: "a", When: "Mon 3 Nov", Date: "2026-11-03", Title: "A", Detail: "x", Icon: "flag"}, {ID: "b", When: "Week 2", Title: "B"}}}}, ValidateOptions{}, ""},
		{"timeline one", Component{ID: "a", Type: "timeline", Summary: "x", Timeline: &Timeline{Events: []TimelineEvent{{ID: "a", When: "1", Title: "A"}}}}, ValidateOptions{}, "timeline.events needs 2-20 (got 1)"},
		{"timeline date", Component{ID: "a", Type: "timeline", Summary: "x", Timeline: &Timeline{Events: []TimelineEvent{{ID: "a", When: "1", Title: "A", Date: "3 Nov"}, {ID: "b", When: "2", Title: "B"}}}}, ValidateOptions{}, `timeline.events[0].date "3 Nov"`},
		{"timeline when", Component{ID: "a", Type: "timeline", Summary: "x", Timeline: &Timeline{Events: []TimelineEvent{{ID: "a", Title: "A"}, {ID: "b", When: "2", Title: "B"}}}}, ValidateOptions{}, "timeline.events[0].when"},
		// choices
		{"choices six", Component{ID: "a", Type: "choices", Summary: "x", Choices: &Choices{Chips: func() []Chip {
			list := make([]Chip, 6)
			for index := range list {
				list[index] = Chip{ID: fmt.Sprintf("c%d", index), Label: "C"}
			}
			return list
		}()}}, ValidateOptions{}, "choices.chips needs 2-5 (got 6)"},
		{"chip label", Component{ID: "a", Type: "choices", Summary: "x", Choices: &Choices{Chips: []Chip{{ID: "a", Label: strings.Repeat("l", 41)}, {ID: "b", Label: "B"}}}}, ValidateOptions{}, "choices.chips[0].label"},
		// checklist
		{"checklist 40", checklistOf(40), ValidateOptions{}, ""},
		{"checklist 41", checklistOf(41), ValidateOptions{}, "checklist.items needs 1-40 (got 41)"},
		{"checklist empty", checklistOf(0), ValidateOptions{}, "checklist.items needs 1-40 (got 0)"},
		{"checklist group", func() Component {
			c := checklistOf(1)
			c.Checklist.Items[0].Group = strings.Repeat("g", 41)
			return c
		}(), ValidateOptions{}, "checklist.items[0].group"},
		// quiz
		{"quiz", quizOf(false, "mito"), ValidateOptions{}, ""},
		{"quiz multiple", quizOf(true, "mito", "wall"), ValidateOptions{}, ""},
		{"quiz two correct", quizOf(false, "mito", "wall"), ValidateOptions{}, "needs exactly 1 option id unless allowMultiple"},
		{"quiz none correct", quizOf(true), ValidateOptions{}, "quiz.answerKey.correct needs at least 1"},
		{"quiz unknown correct", quizOf(false, "dna"), ValidateOptions{}, `quiz.answerKey.correct[0] "dna" is not the id of an option`},
		{"quiz duplicate correct", quizOf(true, "mito", "mito"), ValidateOptions{}, "listed twice"},
		{"quiz without key", func() Component {
			c := quizOf(false, "mito")
			c.Quiz.AnswerKey = nil
			return c
		}(), ValidateOptions{}, "quiz.answerKey is required"},
		{"quiz seven options", func() Component {
			c := quizOf(false, "mito")
			for index := 0; index < 4; index++ {
				c.Quiz.Options = append(c.Quiz.Options, QuizOption{ID: fmt.Sprintf("o%d", index), Label: "O"})
			}
			return c
		}(), ValidateOptions{}, "quiz.options needs 2-6 (got 7)"},
		{"quiz explanation", func() Component {
			c := quizOf(false, "mito")
			c.Quiz.AnswerKey.Explanation = strings.Repeat("e", 401)
			return c
		}(), ValidateOptions{}, "quiz.answerKey.explanation"},
		{"quiz question", func() Component {
			c := quizOf(false, "mito")
			c.Quiz.Question = ""
			return c
		}(), ValidateOptions{}, "quiz.question"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Validate(context.Background(), one(tc.component), tc.opts)
			if tc.err == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.err)
			}
			var failure *ValidationError
			if !errors.As(err, &failure) || failure.ComponentID != tc.component.ID || failure.Path != "components[0]" {
				t.Fatalf("error does not name the component: %#v", err)
			}
		})
	}
}

func TestNestedImageNeedsResolver(t *testing.T) {
	surface := one(Component{ID: "g", Type: "grid", Grid: &Grid{Columns: 2}, Children: []Component{note("a"),
		{ID: "pic", Type: "image", Summary: "x", Image: &ImageBlock{Image: *img("a")}}}})
	_, err := Validate(context.Background(), surface, ValidateOptions{})
	if err == nil || err.Error() != `components[0].children[1] (component "pic"): image.image: this host cannot show images yet; leave it out` {
		t.Fatalf("error = %v", err)
	}
}

func TestTableCellJSON(t *testing.T) {
	raw := `{"columns":[{"key":"a","label":"A","kind":"text"},{"key":"b","label":"B","kind":"number"},{"key":"c","label":"C","kind":"image"}],` +
		`"rows":[{"a":"x","b":1.5,"c":{"artifactId":"img-1","version":2}}]}`
	var table Table
	if err := json.Unmarshal([]byte(raw), &table); err != nil {
		t.Fatal(err)
	}
	row := table.Rows[0]
	if *row["a"].Text != "x" || *row["b"].Number != 1.5 || *row["c"].Image != (ImageRef{ArtifactID: "img-1", Version: 2}) {
		t.Fatalf("row = %#v", row)
	}
	encoded, err := json.Marshal(table)
	if err != nil || string(encoded) != raw {
		t.Fatalf("encoded = %s, %v", encoded, err)
	}
	for _, bad := range []string{`{"a":null}`, `{"a":true}`, `{"a":{"url":"https://x.example"}}`} {
		var row map[string]TableCell
		if err := json.Unmarshal([]byte(bad), &row); err == nil {
			t.Fatalf("%s decoded", bad)
		}
	}
}

func TestLintWave1(t *testing.T) {
	long := strings.Repeat("words ", 10)
	columns := []TableColumn{{Key: "a", Label: "A", Kind: "text"}, {Key: "b", Label: "B", Kind: "text"}, {Key: "c", Label: "C", Kind: "text"}}
	cases := []struct {
		name      string
		component Component
		codes     []string
	}{
		{"comparison one attribute", Component{ID: "c", Type: "comparison", Summary: "x", Comparison: &Comparison{Attributes: []ComparisonAttribute{{Key: "a", Label: "A"}}}}, []string{LintComparisonOneAttribute}},
		{"comparison two attributes", compare(option("a"), option("b")), nil},
		{"two bare steps", Component{ID: "s", Type: "steps", Summary: "x", Steps: &Steps{Items: stepsOf(2)}}, []string{LintStepsThin}},
		{"two detailed steps", Component{ID: "s", Type: "steps", Summary: "x", Steps: &Steps{Items: []Step{{ID: "a", Title: "A", Detail: "d"}, {ID: "b", Title: "B"}}}}, nil},
		{"three bare steps", Component{ID: "s", Type: "steps", Summary: "x", Steps: &Steps{Items: stepsOf(3)}}, nil},
		{"wordy short table", tableOf(columns, map[string]TableCell{"a": TextCell(long), "b": TextCell(long), "c": TextCell(long)}), []string{LintTableReadsAsComparison}},
		{"terse short table", tableOf(columns, map[string]TableCell{"a": TextCell("x"), "b": TextCell(long), "c": TextCell(long)}), nil},
		{"line of two points", chartOf("line", ChartSeries{Name: "a", Points: points(2)}), []string{LintChartFewPoints}},
		{"bar of two points", chartOf("bar", ChartSeries{Name: "a", Points: points(2)}), nil},
		{"line of one point", chartOf("line", ChartSeries{Name: "a", Points: points(1)}), []string{LintChartSinglePoint}},
		{"quiz without explanation", func() Component {
			c := quizOf(false, "mito")
			c.Quiz.AnswerKey.Explanation = ""
			return c
		}(), []string{LintQuizWithoutExplanation}},
		{"timeline out of order", Component{ID: "t", Type: "timeline", Summary: "x", Timeline: &Timeline{Events: []TimelineEvent{{ID: "a", When: "1799", Title: "A"}, {ID: "b", When: "1789", Title: "B"}}}}, []string{LintTimelineLooksUnordered}},
		{"timeline with dates", Component{ID: "t", Type: "timeline", Summary: "x", Timeline: &Timeline{Events: []TimelineEvent{{ID: "a", When: "1799", Title: "A"}, {ID: "b", When: "1789", Title: "B", Date: "1789-07-14"}}}}, nil},
		{"timeline of labels", Component{ID: "t", Type: "timeline", Summary: "x", Timeline: &Timeline{Events: []TimelineEvent{{ID: "a", When: "Week 2", Title: "A"}, {ID: "b", When: "Week 1", Title: "B"}}}}, nil},
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

func TestInteractionWave1(t *testing.T) {
	pickable := compare(option("a"), option("b"))
	pickable.Title = "Plans"
	pickable.Comparison.Pickable = true
	checkable := Component{ID: "how", Type: "steps", Title: "Set up the router", Summary: "x", Steps: &Steps{Items: stepsOf(3), Checkable: true}}
	choices := Component{ID: "next", Type: "choices", Summary: "x", Choices: &Choices{Chips: []Chip{{ID: "short", Label: "Shorter"}, {ID: "veg", Label: "Vegetarian"}}}}
	check := InteractionRule{Kind: InteractionCheck, ItemKeyed: true, Shared: true}
	rules := []struct {
		component Component
		want      InteractionRule
	}{
		{pickable, InteractionRule{Kind: InteractionPick, SingleUse: true, NotifyAgent: true}},
		{choices, InteractionRule{Kind: InteractionRespond, NotifyAgent: true}},
		{checkable, check},
		{checklistOf(3), check},
		{quizOf(false, "mito"), InteractionRule{Kind: InteractionAnswer, SingleUse: true, NotifyAgent: true}},
	}
	for _, tc := range rules {
		if rule, err := Interaction(tc.component, ""); err != nil || rule != tc.want {
			t.Fatalf("%s rule = %#v, %v; want %#v", tc.component.Type, rule, err, tc.want)
		}
		if _, err := Interaction(tc.component, "x"); err == nil {
			t.Fatalf("%s accepted an action id", tc.component.Type)
		}
	}
	for name, component := range map[string]Component{
		"comparison not pickable": compare(option("a"), option("b")),
		"steps not checkable":     {ID: "s", Type: "steps", Summary: "x", Steps: &Steps{Items: stepsOf(2)}},
		"callout":                 {ID: "c", Type: "callout", Summary: "x", Callout: &Callout{Body: "x"}},
		"timeline":                {ID: "t", Type: "timeline", Summary: "x", Timeline: &Timeline{}},
	} {
		if _, err := Interaction(component, ""); err == nil {
			t.Fatalf("%s accepts interactions", name)
		}
	}
	items := []struct {
		component Component
		itemKey   string
		err       string
	}{
		{checklistOf(3), "i1", ""},
		{checklistOf(3), "", "need the item id"},
		{checklistOf(3), "i9", `has no item "i9"`},
		{checkable, "s2", ""},
		{quizOf(false, "mito"), "mito", "do not take an item key"},
		{choices, "", ""},
	}
	for _, tc := range items {
		err := ValidateInteractionItem(tc.component, "", tc.itemKey)
		if tc.err == "" && err != nil || tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)) {
			t.Fatalf("%s item %q: error = %v, want %q", tc.component.Type, tc.itemKey, err, tc.err)
		}
	}
	values := []struct {
		component Component
		values    map[string]interface{}
		err       string
	}{
		{pickable, map[string]interface{}{"optionId": "b"}, ""},
		{pickable, map[string]interface{}{"optionId": "z"}, "optionId must be one of a, b"},
		{pickable, map[string]interface{}{}, "optionId is required"},
		{pickable, map[string]interface{}{"optionId": "a", "note": "x"}, `unexpected value "note"`},
		{choices, map[string]interface{}{"chipId": "veg"}, ""},
		{choices, map[string]interface{}{"chipId": 1.0}, "chipId must be one of short, veg"},
		{checklistOf(2), map[string]interface{}{"checked": true}, ""},
		{checkable, map[string]interface{}{"checked": false}, ""},
		{checklistOf(2), map[string]interface{}{"checked": "yes"}, "checked must be true or false"},
		{checklistOf(2), nil, "checked is required"},
		{quizOf(false, "mito"), map[string]interface{}{"selected": []interface{}{"mito"}}, ""},
		{quizOf(false, "mito"), map[string]interface{}{"selected": []interface{}{"mito", "wall"}}, "exactly one option id"},
		{quizOf(true, "mito"), map[string]interface{}{"selected": []interface{}{"mito", "wall"}}, ""},
		{quizOf(true, "mito"), map[string]interface{}{"selected": []interface{}{"mito", "mito"}}, "selected twice"},
		{quizOf(true, "mito"), map[string]interface{}{"selected": []interface{}{}}, "at least one option id"},
		{quizOf(false, "mito"), map[string]interface{}{"selected": []interface{}{"dna"}}, `quiz has no option "dna"`},
		{quizOf(false, "mito"), map[string]interface{}{"selected": "mito"}, "list of option ids"},
	}
	for _, tc := range values {
		err := ValidateInteractionValues(tc.component, "", tc.values)
		if tc.err == "" && err != nil || tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)) {
			t.Fatalf("%s values %v: error = %v, want %q", tc.component.Type, tc.values, err, tc.err)
		}
	}
}

func TestGradeQuiz(t *testing.T) {
	cases := []struct {
		quiz     Component
		selected []string
		correct  bool
		err      string
	}{
		{quizOf(false, "mito"), []string{"mito"}, true, ""},
		{quizOf(false, "mito"), []string{"wall"}, false, ""},
		{quizOf(true, "mito", "wall"), []string{"wall", "mito"}, true, ""},
		{quizOf(true, "mito", "wall"), []string{"mito"}, false, ""},
		{quizOf(true, "mito", "wall"), []string{"mito", "wall", "nucleus"}, false, ""},
		{quizOf(false, "mito"), []string{"mito", "wall"}, false, "exactly one"},
		{quizOf(false, "mito"), nil, false, "at least one"},
		{func() Component { c := quizOf(false, "mito"); c.Quiz.AnswerKey = nil; return c }(), []string{"mito"}, false, "no answer key"},
		{checklistOf(1), []string{"mito"}, false, "is not a quiz"},
	}
	for index, tc := range cases {
		outcome, err := GradeQuiz(tc.quiz, tc.selected)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("case %d: error = %v, want %q", index, err, tc.err)
			}
			continue
		}
		if err != nil || outcome.Correct != tc.correct || strings.Join(outcome.CorrectIDs, ",") != strings.Join(tc.quiz.Quiz.AnswerKey.Correct, ",") || outcome.Explanation != "The **mitochondria**." {
			t.Fatalf("case %d: outcome = %#v, %v", index, outcome, err)
		}
	}
	encoded, _ := json.Marshal(QuizOutcome{Correct: true, CorrectIDs: []string{"mito"}, Explanation: "x"})
	if string(encoded) != `{"correct":true,"correctIds":["mito"],"explanation":"x"}` {
		t.Fatalf("outcome JSON = %s", encoded)
	}
}

func TestStripAnswerKeys(t *testing.T) {
	surface := one(quizOf(false, "mito"),
		Component{ID: "g", Type: "grid", Grid: &Grid{Columns: 2}, Children: []Component{note("a"), {ID: "q2", Type: "quiz", Summary: "x", Quiz: quizOf(false, "wall").Quiz}}},
		Component{ID: "t", Type: "tabs", Tabs: []Tab{{Label: "A", Children: []Component{note("b")}}, {Label: "B", Children: []Component{
			{ID: "s", Type: "section", Children: []Component{{ID: "q3", Type: "quiz", Summary: "x", Quiz: quizOf(true, "mito").Quiz}}},
		}}}})
	if _, err := Validate(context.Background(), surface, ValidateOptions{}); err != nil {
		t.Fatal(err)
	}
	original, _ := json.Marshal(surface)
	stripped := StripAnswerKeys(surface)
	if HasAnswerKeys(stripped) || !HasAnswerKeys(surface) {
		t.Fatal("answer keys not stripped, or the input was modified")
	}
	after, _ := json.Marshal(surface)
	if string(after) != string(original) {
		t.Fatal("StripAnswerKeys modified its input")
	}
	encoded, _ := json.Marshal(stripped)
	if strings.Contains(string(encoded), "answerKey") || strings.Contains(string(encoded), "mitochondria**") || !strings.Contains(string(encoded), `"question":"Where is most ATP made?"`) {
		t.Fatalf("stripped = %s", encoded)
	}
	if q3, ok := stripped.Find("q3"); !ok || q3.Quiz.AnswerKey != nil || len(q3.Quiz.Options) != 3 || !q3.Quiz.AllowMultiple {
		t.Fatalf("q3 = %#v", q3)
	}
	raw, err := Encode(surface)
	if err != nil {
		t.Fatal(err)
	}
	strippedRaw, err := StripAnswerKeysJSON(raw)
	if err != nil || string(strippedRaw) != string(encoded) {
		t.Fatalf("StripAnswerKeysJSON = %s, %v", strippedRaw, err)
	}
	plain, _ := Encode(one(note("a")))
	if same, err := StripAnswerKeysJSON(plain); err != nil || string(same) != string(plain) {
		t.Fatalf("plain surface changed: %s, %v", same, err)
	}
	if _, err := StripAnswerKeysJSON([]byte(`{"answerKey":`)); err == nil {
		t.Fatal("broken JSON accepted")
	}
}

func TestCardResponseText(t *testing.T) {
	pickable := compare(option("a"), option("b"))
	pickable.Title = "Plans"
	pickable.Comparison.Pickable = true
	actions := Component{ID: "next", Type: "actions", Actions: []Action{{ID: "book", Label: "Book it", Kind: ActionRespond, Value: "Book the first one"}}}
	form := Component{ID: "trip", Type: "form", Title: "Trip details", Summary: "x", Form: &Form{Fields: []FormField{{ID: "budget", Type: "number", Label: "Budget"}}}}
	choices := Component{ID: "next", Type: "choices", Summary: "x", Choices: &Choices{Chips: []Chip{{ID: "veg", Label: "Vegetarian"}, {ID: "short", Label: "Shorter"}}}}
	checkable := Component{ID: "how", Type: "steps", Title: "Set up the router", Summary: "x", Steps: &Steps{Items: stepsOf(3), Checkable: true}}
	cases := []struct {
		component Component
		actionID  string
		itemKey   string
		values    string
		outcome   string
		want      string
	}{
		{actions, "book", "", `{}`, "", `Card response (button "Book it"): "Book the first one"`},
		{form, "", "", `{"budget":500}`, "", `Card response (form "Trip details"): {"budget":500}`},
		{choices, "", "", `{"chipId":"veg"}`, "", `Card response (choices): "Vegetarian"`},
		{pickable, "", "", `{"optionId":"b"}`, "", `Card response (comparison "Plans"): picked "B"`},
		{quizOf(false, "mito"), "", "", `{"selected":["mito"]}`, `{"correct":true,"correctIds":["mito"],"explanation":"x"}`, `Card response (quiz "Cells"): picked "Mitochondria" (correct)`},
		{quizOf(true, "mito"), "", "", `{"selected":["mito","wall"]}`, `{"correct":false,"correctIds":["mito"],"explanation":"x"}`, `Card response (quiz "Cells"): picked "Mitochondria", "Cell wall" (incorrect)`},
		{checklistOf(2), "", "i1", `{"checked":true}`, "", `Card response (checklist "Packing list"): ticked "Item 1"`},
		{checkable, "", "s0", `{"checked":false}`, "", `Card response (steps "Set up the router"): unticked "Step"`},
	}
	for _, tc := range cases {
		got := CardResponseText(tc.component, tc.actionID, tc.itemKey, json.RawMessage(tc.values), json.RawMessage(tc.outcome))
		if got != tc.want {
			t.Fatalf("got %s, want %s", got, tc.want)
		}
	}
}

func TestCardStateLine(t *testing.T) {
	list := checklistOf(14)
	checked := map[string]bool{"i7": false, "unknown": true}
	for index := 0; index < 9; index++ {
		checked[fmt.Sprintf("i%d", index)] = index != 7
	}
	checked["i9"] = true
	line, ok := CardStateLine(list, checked)
	if !ok || line != "Packing list: 9/14 ticked (unticked: Item 7, Item 10, Item 11, Item 12, Item 13)" {
		t.Fatalf("line = %q", line)
	}
	if line, _ := CardStateLine(checklistOf(8), map[string]bool{"i0": true}); line != "Packing list: 1/8 ticked (unticked: Item 1, Item 2, Item 3, Item 4, Item 5 and 2 more)" {
		t.Fatalf("line = %q", line)
	}
	if line, _ := CardStateLine(checklistOf(2), map[string]bool{"i0": true, "i1": true}); line != "Packing list: 2/2 ticked" {
		t.Fatalf("line = %q", line)
	}
	steps := Component{ID: "how", Type: "steps", Title: "Set up the router", Summary: "x", Steps: &Steps{Items: stepsOf(5), Checkable: true}}
	if line, _ := CardStateLine(steps, map[string]bool{"s0": true, "s1": true, "s3": true}); line != `Steps "Set up the router": 3/5 done` {
		t.Fatalf("line = %q", line)
	}
	if _, ok := CardStateLine(quizOf(false, "mito"), nil); ok {
		t.Fatal("quiz has card state")
	}
}
