package presentation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func nodes(ids ...string) []DiagramNode {
	list := make([]DiagramNode, len(ids))
	for index, id := range ids {
		list[index] = DiagramNode{ID: id, Label: strings.ToUpper(id)}
	}
	return list
}

func diagramOf(layout string, list []DiagramNode, edges ...DiagramEdge) Component {
	return Component{ID: "d", Type: "diagram", Summary: "x", Diagram: &Diagram{Layout: layout, Nodes: list, Edges: edges}}
}

func edge(from, to string) DiagramEdge { return DiagramEdge{From: from, To: to} }

func manyNodes(count int) []DiagramNode {
	ids := make([]string, count)
	for index := range ids {
		ids[index] = fmt.Sprintf("n%d", index)
	}
	return nodes(ids...)
}

func fractions(values ...int) []Fraction {
	list := make([]Fraction, 0, len(values)/2)
	for index := 0; index+1 < len(values); index += 2 {
		list = append(list, Fraction{Numerator: values[index], Denominator: values[index+1]})
	}
	return list
}

func quantity(kind string, values QuantityValues, steps ...QuantityStep) Component {
	return Component{ID: "q", Type: "quantity_visual", Summary: "x", QuantityVisual: &QuantityVisual{Kind: kind, QuantityValues: values, Steps: steps}}
}

func num(value float64) *float64 { return &value }

func mathOf(latex ...string) Component {
	steps := make([]MathStep, len(latex))
	for index, line := range latex {
		steps[index] = MathStep{LaTeX: line, Note: "n"}
	}
	return Component{ID: "m", Type: "math", Summary: "x", Math: &Math{Steps: steps}}
}

func deck(count int) Component {
	cards := make([]Flashcard, count)
	for index := range cards {
		cards[index] = Flashcard{ID: fmt.Sprintf("c%d", index), Front: fmt.Sprintf("front %d", index), Back: fmt.Sprintf("back %d", index)}
	}
	return Component{ID: "deck", Type: "flashcards", Title: "French food words", Summary: "x", Flashcards: &Flashcards{Cards: cards}}
}

func draftOf(binding *Binding) Component {
	return Component{ID: "reply", Type: "draft", Title: "Reply to Sam", Summary: "x", Binding: binding, Draft: &Draft{Channel: "email", To: "Sam", Body: "Hi Sam"}}
}

func recipeOf(checkable bool) Component {
	return Component{ID: "r", Type: "recipe", Title: "Pancakes", Summary: "x", Recipe: &Recipe{
		Servings:    4,
		Ingredients: []RecipeIngredient{{ID: "flour", Qty: num(100), Unit: "g", Name: "plain flour"}, {ID: "eggs", Qty: num(2), Name: "eggs"}, {ID: "salt", Name: "salt"}},
		Steps:       []RecipeStep{{ID: "mix", Text: "Whisk everything into a batter."}},
		Checkable:   checkable,
	}}
}

func minutes(value int) *int { return &value }

func TestValidateWave2(t *testing.T) {
	text := func(value string) BeforeAfterSide { return BeforeAfterSide{Text: value} }
	picture := func(id string) BeforeAfterSide { return BeforeAfterSide{Image: img(id)} }
	approval := &Binding{Kind: BindingApproval, ID: "ap-1"}
	cases := []struct {
		name      string
		component Component
		opts      ValidateOptions
		err       string
	}{
		// every new type needs a summary and its payload
		{"diagram summary", Component{ID: "a", Type: "diagram", Diagram: diagramOf("flow", nodes("a", "b")).Diagram}, ValidateOptions{}, "summary is required for diagram"},
		{"math payload", Component{ID: "a", Type: "math", Summary: "x"}, ValidateOptions{}, "math payload is required"},
		{"foreign payload", Component{ID: "a", Type: "definition", Summary: "x", Definition: &Definition{Term: "a", Meaning: "b"}, Math: mathOf("x").Math}, ValidateOptions{}, `definition cannot carry a "math" payload`},
		// diagram
		{"flow", diagramOf("flow", nodes("a", "b", "c"), edge("a", "b"), edge("b", "c")), ValidateOptions{}, ""},
		{"layout required", diagramOf("", nodes("a", "b")), ValidateOptions{}, `diagram.layout "" is not one of flow, cycle, hierarchy, mindmap, network`},
		{"v1 coordinates are gone", Component{ID: "d", Type: "diagram", Summary: "x"}, ValidateOptions{}, "diagram payload is required"},
		{"direction", Component{ID: "d", Type: "diagram", Summary: "x", Diagram: &Diagram{Layout: "hierarchy", Direction: "right", Nodes: nodes("a", "b"), Edges: []DiagramEdge{edge("a", "b")}}}, ValidateOptions{}, ""},
		{"direction on network", Component{ID: "d", Type: "diagram", Summary: "x", Diagram: &Diagram{Layout: "network", Direction: "right", Nodes: nodes("a", "b")}}, ValidateOptions{}, "diagram.direction is only used by flow and hierarchy"},
		{"direction value", Component{ID: "d", Type: "diagram", Summary: "x", Diagram: &Diagram{Layout: "flow", Direction: "up", Nodes: nodes("a", "b")}}, ValidateOptions{}, `diagram.direction "up"`},
		{"one node", diagramOf("flow", nodes("a")), ValidateOptions{}, "diagram.nodes needs 2-30 (got 1)"},
		{"31 nodes", diagramOf("network", manyNodes(31)), ValidateOptions{}, "diagram.nodes needs 2-30 (got 31)"},
		{"41 edges", diagramOf("network", nodes("a", "b"), func() []DiagramEdge {
			list := make([]DiagramEdge, 41)
			for index := range list {
				list[index] = edge("a", "b")
			}
			return list
		}()...), ValidateOptions{}, "diagram.edges allows at most 40 (got 41)"},
		{"node id", diagramOf("flow", []DiagramNode{{ID: "A", Label: "a"}, {ID: "b", Label: "b"}}), ValidateOptions{}, `diagram.nodes[0].id "A" must match`},
		{"node label", diagramOf("flow", []DiagramNode{{ID: "a", Label: strings.Repeat("l", 41)}, {ID: "b", Label: "b"}}), ValidateOptions{}, "diagram.nodes[0].label must contain between 1 and 40"},
		{"node detail", diagramOf("flow", []DiagramNode{{ID: "a", Label: "a", Detail: "<i>x</i>"}, {ID: "b", Label: "b"}}), ValidateOptions{}, "diagram.nodes[0].detail must not contain HTML"},
		{"node group", diagramOf("flow", []DiagramNode{{ID: "a", Label: "a", Group: "g"}, {ID: "b", Label: "b"}}), ValidateOptions{}, `diagram.nodes[0].group "g" is not the id of a group`},
		{"groups", Component{ID: "d", Type: "diagram", Summary: "x", Diagram: &Diagram{Layout: "network", Groups: []DiagramGroup{{ID: "g", Label: "Plants"}},
			Nodes: []DiagramNode{{ID: "a", Label: "a", Group: "g", Icon: "leaf"}, {ID: "b", Label: "b"}}, Edges: []DiagramEdge{{From: "a", To: "b", Label: "eats"}}}}, ValidateOptions{}, ""},
		{"seven groups", Component{ID: "d", Type: "diagram", Summary: "x", Diagram: &Diagram{Layout: "network", Nodes: nodes("a", "b"), Groups: make([]DiagramGroup, 7)}}, ValidateOptions{}, "diagram.groups allows at most 6 (got 7)"},
		{"edge from", diagramOf("flow", nodes("a", "b"), edge("x", "b")), ValidateOptions{}, `diagram.edges[0].from "x" is not the id of a node`},
		{"edge label", diagramOf("flow", nodes("a", "b"), DiagramEdge{From: "a", To: "b", Label: strings.Repeat("l", 31)}), ValidateOptions{}, "diagram.edges[0].label must be at most 30"},
		{"cycle of two", diagramOf("cycle", nodes("a", "b")), ValidateOptions{}, "a cycle diagram needs at least 3 nodes (got 2)"},
		{"cycle without edges", diagramOf("cycle", nodes("a", "b", "c")), ValidateOptions{}, ""},
		{"hierarchy forest", diagramOf("hierarchy", nodes("a", "b", "c", "d"), edge("a", "b"), edge("a", "c")), ValidateOptions{}, ""},
		{"hierarchy two parents", diagramOf("hierarchy", nodes("a", "b", "c"), edge("a", "c"), edge("b", "c")), ValidateOptions{}, `diagram.edges[1]: "c" already has the parent "a"`},
		{"hierarchy loop", diagramOf("hierarchy", nodes("a", "b", "c"), edge("a", "b"), edge("b", "c"), edge("c", "a")), ValidateOptions{}, "a hierarchy has no loops"},
		{"hierarchy self", diagramOf("hierarchy", nodes("a", "b"), edge("a", "a")), ValidateOptions{}, `diagram.edges[0] joins "a" to itself`},
		{"mindmap", diagramOf("mindmap", nodes("a", "b", "c"), edge("a", "b"), edge("c", "a")), ValidateOptions{}, ""},
		{"mindmap without edges", diagramOf("mindmap", nodes("a", "b")), ValidateOptions{}, "a mindmap diagram needs edges"},
		{"mindmap unreachable", diagramOf("mindmap", nodes("a", "b", "c", "d"), edge("a", "b"), edge("c", "d")), ValidateOptions{}, `diagram.nodes[2] "c" is not connected to the centre "a"`},
		// labeled_image
		{"labeled_image", Component{ID: "l", Type: "labeled_image", Summary: "x", LabeledImage: &LabeledImage{Image: *img("img-1"), Markers: []ImageMarker{{ID: "n", X: 0, Y: 1, Label: "Nucleus", Detail: "Holds **DNA**."}}}}, images("img-1"), ""},
		{"labeled_image needs a resolver", Component{ID: "l", Type: "labeled_image", Summary: "x", LabeledImage: &LabeledImage{Image: *img("img-1"), Markers: []ImageMarker{{ID: "n", Label: "N"}}}}, ValidateOptions{}, "labeled_image.image: this host cannot show images yet"},
		{"labeled_image no markers", Component{ID: "l", Type: "labeled_image", Summary: "x", LabeledImage: &LabeledImage{Image: *img("img-1")}}, images("img-1"), "labeled_image.markers needs 1-12 (got 0)"},
		{"labeled_image 13 markers", Component{ID: "l", Type: "labeled_image", Summary: "x", LabeledImage: &LabeledImage{Image: *img("img-1"), Markers: make([]ImageMarker, 13)}}, images("img-1"), "labeled_image.markers needs 1-12 (got 13)"},
		{"marker x", Component{ID: "l", Type: "labeled_image", Summary: "x", LabeledImage: &LabeledImage{Image: *img("img-1"), Markers: []ImageMarker{{ID: "n", X: 1.2, Label: "N"}}}}, images("img-1"), "labeled_image.markers[0].x must be between 0 and 1"},
		{"marker label", Component{ID: "l", Type: "labeled_image", Summary: "x", LabeledImage: &LabeledImage{Image: *img("img-1"), Markers: []ImageMarker{{ID: "n", Label: strings.Repeat("l", 41)}}}}, images("img-1"), "labeled_image.markers[0].label"},
		// quantity_visual
		{"fraction bars with steps", quantity("fraction_bars", QuantityValues{Fractions: fractions(3, 4, 2, 3)}, QuantityStep{Label: "Twelfths", QuantityValues: QuantityValues{Fractions: fractions(9, 12, 8, 12)}}), ValidateOptions{}, ""},
		{"quantity kind", quantity("pie", QuantityValues{}), ValidateOptions{}, `quantity_visual.kind "pie" is not one of fraction_bars`},
		{"denominator", quantity("fraction_bars", QuantityValues{Fractions: fractions(1, 25)}), ValidateOptions{}, "quantity_visual.fractions[0].denominator must be 1-24 (got 25)"},
		{"numerator", quantity("fraction_bars", QuantityValues{Fractions: fractions(5, 4)}), ValidateOptions{}, "quantity_visual.fractions[0].numerator must be 0-4"},
		{"five fractions", quantity("fraction_bars", QuantityValues{Fractions: fractions(1, 2, 1, 2, 1, 2, 1, 2, 1, 2)}), ValidateOptions{}, "quantity_visual.fractions needs 1-4 (got 5)"},
		{"fields of another kind", quantity("fraction_bars", QuantityValues{Fractions: fractions(1, 2), Groups: []UnitGroup{{Count: 1}}}), ValidateOptions{}, "quantity_visual.groups is not used by fraction_bars (it takes fractions)"},
		{"step fields of another kind", quantity("fraction_bars", QuantityValues{Fractions: fractions(1, 2)}, QuantityStep{Label: "s", QuantityValues: QuantityValues{Parts: []WafflePart{{Value: 1, Label: "a"}}}}), ValidateOptions{}, "quantity_visual.steps[0].parts is not used by fraction_bars"},
		{"step needs values", quantity("fraction_bars", QuantityValues{Fractions: fractions(1, 2)}, QuantityStep{Label: "s"}), ValidateOptions{}, "quantity_visual.steps[0].fractions needs 1-4 (got 0)"},
		{"step label", quantity("fraction_bars", QuantityValues{Fractions: fractions(1, 2)}, QuantityStep{QuantityValues: QuantityValues{Fractions: fractions(1, 2)}}), ValidateOptions{}, "quantity_visual.steps[0].label"},
		{"five steps", quantity("unit_blocks", QuantityValues{Groups: []UnitGroup{{Count: 1}}}, make([]QuantityStep, 5)...), ValidateOptions{}, "quantity_visual.steps allows at most 4 (got 5)"},
		{"number line", quantity("number_line", QuantityValues{Min: num(-5), Max: num(5), Ticks: num(1), Points: []NumberPoint{{Value: -3, Label: "-3"}}, Ranges: []NumberRange{{From: 0, To: 2}}}), ValidateOptions{}, ""},
		{"number line bounds", quantity("number_line", QuantityValues{Min: num(0)}), ValidateOptions{}, "quantity_visual needs min and max for a number_line"},
		{"number line min above max", quantity("number_line", QuantityValues{Min: num(3), Max: num(1)}), ValidateOptions{}, "quantity_visual.min must be a number below max"},
		{"number line ticks", quantity("number_line", QuantityValues{Min: num(0), Max: num(1000), Ticks: num(1)}), ValidateOptions{}, "at most 100 ticks"},
		{"number line point outside", quantity("number_line", QuantityValues{Min: num(0), Max: num(10), Points: []NumberPoint{{Value: 11}}}), ValidateOptions{}, "quantity_visual.points[0].value must be between min 0 and max 10"},
		{"number line seven points", quantity("number_line", QuantityValues{Min: num(0), Max: num(10), Points: make([]NumberPoint, 7)}), ValidateOptions{}, "quantity_visual.points allows at most 6 (got 7)"},
		{"number line range order", quantity("number_line", QuantityValues{Min: num(0), Max: num(10), Ranges: []NumberRange{{From: 5, To: 2}}}), ValidateOptions{}, "quantity_visual.ranges[0].from must be below to"},
		{"unit blocks", quantity("unit_blocks", QuantityValues{Groups: []UnitGroup{{Count: 100, Label: "hundreds"}, {Count: 0}}}), ValidateOptions{}, ""},
		{"unit blocks count", quantity("unit_blocks", QuantityValues{Groups: []UnitGroup{{Count: 101}}}), ValidateOptions{}, "quantity_visual.groups[0].count must be 0-100 (got 101)"},
		{"waffle", quantity("percent_waffle", QuantityValues{Parts: []WafflePart{{Value: 60, Label: "Water"}, {Value: 40, Label: "Rest"}}}), ValidateOptions{}, ""},
		{"waffle over 100", quantity("percent_waffle", QuantityValues{Parts: []WafflePart{{Value: 60, Label: "a"}, {Value: 50, Label: "b"}}}), ValidateOptions{}, "quantity_visual.parts add up to 110%"},
		{"waffle label", quantity("percent_waffle", QuantityValues{Parts: []WafflePart{{Value: 60}}}), ValidateOptions{}, "quantity_visual.parts[0].label"},
		{"scale compare", quantity("scale_compare", QuantityValues{Items: []ScaleItem{{Label: "Earth", Value: Amount{Value: 12742, Unit: "km"}}, {Label: "Moon", Value: Amount{Value: 3474, Unit: "km"}}}}), ValidateOptions{}, ""},
		{"scale compare of one", quantity("scale_compare", QuantityValues{Items: []ScaleItem{{Label: "Earth", Value: Amount{Value: 1}}}}), ValidateOptions{}, "quantity_visual.items needs 2-4 (got 1)"},
		{"scale compare negative", quantity("scale_compare", QuantityValues{Items: []ScaleItem{{Label: "a", Value: Amount{Value: -1}}, {Label: "b", Value: Amount{Value: 1}}}}), ValidateOptions{}, "quantity_visual.items[0].value.value must be 0 or more"},
		{"scale compare currency", quantity("scale_compare", QuantityValues{Items: []ScaleItem{{Label: "a", Value: Amount{Value: 1, Format: StatCurrency}}, {Label: "b", Value: Amount{Value: 1}}}}), ValidateOptions{}, "quantity_visual.items[0].value.currency is required"},
		// math
		{"math", Component{ID: "m", Type: "math", Summary: "x", Math: &Math{Steps: []MathStep{{LaTeX: `\frac{3}{4} = \frac{9}{12}`, Note: "Times 3"}}, Reveal: "stepwise"}}, ValidateOptions{}, ""},
		{"math no steps", Component{ID: "m", Type: "math", Summary: "x", Math: &Math{}}, ValidateOptions{}, "math.steps needs 1-12 (got 0)"},
		{"math 13 steps", mathOf(strings.Split(strings.Repeat("x,", 13), ",")[:13]...), ValidateOptions{}, "math.steps needs 1-12 (got 13)"},
		{"math reveal", Component{ID: "m", Type: "math", Summary: "x", Math: &Math{Steps: []MathStep{{LaTeX: "x"}}, Reveal: "slow"}}, ValidateOptions{}, `math.reveal "slow"`},
		{"latex length", mathOf(strings.Repeat("x", 301)), ValidateOptions{}, "math.steps[0].latex must contain between 1 and 300"},
		{"latex href", mathOf("x", `\href{https://x.example}{y}`), ValidateOptions{}, `math.steps[1].latex must not use \href`},
		{"math note", Component{ID: "m", Type: "math", Summary: "x", Math: &Math{Steps: []MathStep{{LaTeX: "x", Note: strings.Repeat("n", 201)}}}}, ValidateOptions{}, "math.steps[0].note"},
		// definition
		{"definition", Component{ID: "d", Type: "definition", Summary: "x", Definition: &Definition{Term: "Osmosis", Phonetic: "/ɒzˈməʊsɪs/", PartOfSpeech: "noun", Meaning: "Water moving through a membrane.", Example: "Roots take up water by osmosis.", Image: img("img-1")}}, images("img-1"), ""},
		{"definition meaning", Component{ID: "d", Type: "definition", Summary: "x", Definition: &Definition{Term: "Osmosis"}}, ValidateOptions{}, "definition.meaning must contain between 1 and 300"},
		{"definition term", Component{ID: "d", Type: "definition", Summary: "x", Definition: &Definition{Term: strings.Repeat("t", 61), Meaning: "m"}}, ValidateOptions{}, "definition.term"},
		{"definition part of speech", Component{ID: "d", Type: "definition", Summary: "x", Definition: &Definition{Term: "t", Meaning: "m", PartOfSpeech: strings.Repeat("p", 21)}}, ValidateOptions{}, "definition.partOfSpeech"},
		// flashcards
		{"flashcards", deck(30), ValidateOptions{}, ""},
		{"flashcards of one", deck(1), ValidateOptions{}, "flashcards.cards needs 2-30 (got 1)"},
		{"flashcards 31", deck(31), ValidateOptions{}, "flashcards.cards needs 2-30 (got 31)"},
		{"flashcard back", Component{ID: "f", Type: "flashcards", Summary: "x", Flashcards: &Flashcards{Cards: []Flashcard{{ID: "a", Front: "a", Back: strings.Repeat("b", 401)}, {ID: "b", Front: "b", Back: "b"}}}}, ValidateOptions{}, "flashcards.cards[0].back must contain between 1 and 400"},
		{"flashcard id twice", Component{ID: "f", Type: "flashcards", Summary: "x", Flashcards: &Flashcards{Cards: []Flashcard{{ID: "a", Front: "a", Back: "b"}, {ID: "a", Front: "c", Back: "d"}}}}, ValidateOptions{}, `flashcards.cards[1].id "a" is used twice`},
		// before_after
		{"text diff", Component{ID: "b", Type: "before_after", Summary: "x", BeforeAfter: &BeforeAfter{Mode: "diff", Before: text("a"), After: text("b"), Labels: &BeforeAfterLabels{Before: "Yours", After: "Mine"}}}, ValidateOptions{}, ""},
		{"image slider", Component{ID: "b", Type: "before_after", Summary: "x", BeforeAfter: &BeforeAfter{Mode: "slider", Before: picture("a"), After: picture("b")}}, images("a", "b"), ""},
		{"mixed sides", Component{ID: "b", Type: "before_after", Summary: "x", BeforeAfter: &BeforeAfter{Mode: "split", Before: text("a"), After: picture("b")}}, images("b"), "before_after.before is text but after is image"},
		{"diff of images", Component{ID: "b", Type: "before_after", Summary: "x", BeforeAfter: &BeforeAfter{Mode: "diff", Before: picture("a"), After: picture("b")}}, images("a", "b"), "before_after.mode diff compares texts"},
		{"slider of texts", Component{ID: "b", Type: "before_after", Summary: "x", BeforeAfter: &BeforeAfter{Mode: "slider", Before: text("a"), After: text("b")}}, ValidateOptions{}, "before_after.mode slider compares images"},
		{"both on one side", Component{ID: "b", Type: "before_after", Summary: "x", BeforeAfter: &BeforeAfter{Mode: "split", Before: BeforeAfterSide{Text: "a", Image: img("a")}, After: text("b")}}, images("a"), "before_after.before takes text or image, not both"},
		{"empty side", Component{ID: "b", Type: "before_after", Summary: "x", BeforeAfter: &BeforeAfter{Mode: "split", Before: text("a")}}, ValidateOptions{}, "before_after.after.text must contain between 1 and 4000"},
		{"before_after mode", Component{ID: "b", Type: "before_after", Summary: "x", BeforeAfter: &BeforeAfter{Mode: "wipe", Before: text("a"), After: text("b")}}, ValidateOptions{}, `before_after.mode "wipe"`},
		{"before_after label", Component{ID: "b", Type: "before_after", Summary: "x", BeforeAfter: &BeforeAfter{Mode: "split", Before: text("a"), After: text("b"), Labels: &BeforeAfterLabels{After: strings.Repeat("l", 21)}}}, ValidateOptions{}, "before_after.labels.after"},
		// draft
		{"draft", draftOf(nil), ValidateOptions{}, ""},
		{"draft channel", Component{ID: "d", Type: "draft", Summary: "x", Draft: &Draft{Channel: "fax", Body: "x"}}, ValidateOptions{}, `draft.channel "fax" is not one of email, message, post, note`},
		{"draft body", Component{ID: "d", Type: "draft", Summary: "x", Draft: &Draft{Channel: "note", Body: strings.Repeat("b", 4001)}}, ValidateOptions{}, "draft.body must contain between 1 and 4000"},
		{"draft html", Component{ID: "d", Type: "draft", Summary: "x", Draft: &Draft{Channel: "note", Body: "<script>x</script>"}}, ValidateOptions{}, "draft.body must not contain HTML"},
		{"draft bound", draftOf(approval), ValidateOptions{Bindings: BindingResolverFunc(allow)}, ""},
		{"draft bound to a run", draftOf(&Binding{Kind: BindingRun, ID: "r1"}), ValidateOptions{Bindings: BindingResolverFunc(allow)}, `binding.kind "run" is not allowed on draft; use approval`},
		{"draft binding checked by host", draftOf(approval), ValidateOptions{Bindings: BindingResolverFunc(func(context.Context, Binding) error {
			return errors.New("was not proposed by you in this chat")
		})}, "binding: approval/ap-1: was not proposed by you in this chat"},
		// recipe
		{"recipe", recipeOf(true), ValidateOptions{}, ""},
		{"recipe servings", Component{ID: "r", Type: "recipe", Summary: "x", Recipe: &Recipe{Servings: 25}}, ValidateOptions{}, "recipe.servings must be 1-24 (got 25)"},
		{"recipe time", func() Component {
			c := recipeOf(false)
			c.Recipe.Time = &RecipeTime{Prep: minutes(10), Cook: minutes(-1)}
			return c
		}(), ValidateOptions{}, "recipe.time.cook is in minutes"},
		{"recipe no steps", Component{ID: "r", Type: "recipe", Summary: "x", Recipe: &Recipe{Servings: 2, Ingredients: recipeOf(false).Recipe.Ingredients}}, ValidateOptions{}, "recipe.steps needs 1-15 (got 0)"},
		{"recipe 31 ingredients", Component{ID: "r", Type: "recipe", Summary: "x", Recipe: &Recipe{Servings: 2, Ingredients: make([]RecipeIngredient, 31)}}, ValidateOptions{}, "recipe.ingredients needs 1-30 (got 31)"},
		{"recipe qty", func() Component {
			c := recipeOf(false)
			c.Recipe.Ingredients[0].Qty = num(0)
			return c
		}(), ValidateOptions{}, "recipe.ingredients[0].qty must be a number above 0"},
		{"recipe unit", func() Component {
			c := recipeOf(false)
			c.Recipe.Ingredients[0].Unit = strings.Repeat("u", 17)
			return c
		}(), ValidateOptions{}, "recipe.ingredients[0].unit"},
		// item
		{"item", Component{ID: "i", Type: "item", Summary: "x", Item: &Item{Name: "Kettle", Image: img("a"), Price: &Price{Amount: Amount{Value: 35, Format: StatCurrency, Currency: "GBP"}, Per: "once"}, Rating: &ItemRating{Value: 4.5, Count: minutes(10)}, Facts: []string{"1.7 l"}, Badge: "Quiet"}}, images("a"), ""},
		{"item rating", Component{ID: "i", Type: "item", Summary: "x", Item: &Item{Name: "Kettle", Rating: &ItemRating{Value: 6}}}, ValidateOptions{}, "item.rating.value must be 0-5"},
		{"item facts", Component{ID: "i", Type: "item", Summary: "x", Item: &Item{Name: "Kettle", Facts: []string{"a", "b", "c", "d", "e"}}}, ValidateOptions{}, "item.facts allows at most 4 (got 5)"},
		{"item fact length", Component{ID: "i", Type: "item", Summary: "x", Item: &Item{Name: "Kettle", Facts: []string{strings.Repeat("f", 61)}}}, ValidateOptions{}, "item.facts[0]"},
		{"item price per", Component{ID: "i", Type: "item", Summary: "x", Item: &Item{Name: "Kettle", Price: &Price{Amount: Amount{Value: 1}, Per: "week"}}}, ValidateOptions{}, `item.price.per "week"`},
		// analogy
		{"analogy", Component{ID: "a", Type: "analogy", Summary: "x", Analogy: &Analogy{From: AnalogySide{Label: "Cell", Icon: "dna"}, To: AnalogySide{Label: "Factory"}, Mapping: []AnalogyMapping{{A: "Nucleus", B: "Office"}}}}, ValidateOptions{}, ""},
		{"analogy mapping", Component{ID: "a", Type: "analogy", Summary: "x", Analogy: &Analogy{From: AnalogySide{Label: "Cell"}, To: AnalogySide{Label: "Factory"}, Mapping: make([]AnalogyMapping, 6)}}, ValidateOptions{}, "analogy.mapping needs 1-5 (got 6)"},
		{"analogy to", Component{ID: "a", Type: "analogy", Summary: "x", Analogy: &Analogy{From: AnalogySide{Label: "Cell"}, Mapping: []AnalogyMapping{{A: "a", B: "b"}}}}, ValidateOptions{}, "analogy.to.label"},
		{"analogy pair", Component{ID: "a", Type: "analogy", Summary: "x", Analogy: &Analogy{From: AnalogySide{Label: "Cell"}, To: AnalogySide{Label: "Factory"}, Mapping: []AnalogyMapping{{A: "a"}}}}, ValidateOptions{}, "analogy.mapping[0].b"},
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
			if !strings.Contains(err.Error(), `components[0] (component "`) {
				t.Fatalf("error %q does not name the component path", err)
			}
		})
	}
}

func TestItemHasNoLink(t *testing.T) {
	raw := `{"schema":"` + Schema + `","title":"x","state":"ready","components":[{"id":"i","type":"item","summary":"x","item":{"name":"Kettle","link":"https://shop.example/k"}}]}`
	if _, err := Decode([]byte(raw)); err == nil || !strings.Contains(err.Error(), `unknown field "link"`) {
		t.Fatalf("item link decoded: %v", err)
	}
	raw = `{"schema":"` + Schema + `","title":"x","state":"ready","components":[{"id":"d","type":"diagram","summary":"x","diagram":{"layout":"flow","nodes":[{"id":"a","label":"A","x":0,"y":0},{"id":"b","label":"B"}]}}]}`
	if _, err := Decode([]byte(raw)); err == nil || !strings.Contains(err.Error(), `unknown field "x"`) {
		t.Fatalf("diagram coordinates decoded: %v", err)
	}
}

func TestValidateLaTeX(t *testing.T) {
	for _, latex := range []string{`\frac{3}{4}`, `x^2 + 2x + 1 = (x+1)^2`, `a \\ b`, `\{1, 2\}`, `\text{url}`, `\sqrt{\hrefx}`, `\\href`} {
		if err := ValidateLaTeX("latex", latex); err != nil {
			t.Fatalf("%s: %v", latex, err)
		}
	}
	for latex, want := range map[string]string{
		`\href{https://x.example}{a}`:  `\href`,
		`\url{https://x.example}`:      `\url`,
		`\includegraphics{a.png}`:      `\includegraphics`,
		`\htmlClass{x}{y}`:             `\htmlClass`,
		`\htmlStyle{color:red}{y}`:     `\htmlStyle`,
		`\def\x{1} \x`:                 `\def`,
		`\newcommand{\x}{1}`:           `\newcommand`,
		`\renewcommand{\frac}{1}`:      `\renewcommand`,
		`a \\ \gdef\y{2}`:              `\gdef`,
		`$x$`:                          "without $ delimiters",
		strings.Repeat("x", 301):       "between 1 and 300",
		"   ":                          "between 1 and 300",
		`x + \let\a\b`:                 `\let`,
		`\frac{1}{2}\providecommand\z`: `\providecommand`,
	} {
		if err := ValidateLaTeX("latex", latex); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%q: error = %v, want %q", latex, err, want)
		}
	}
}

func TestLintWave2(t *testing.T) {
	deep := diagramOf("mindmap", nodes("a", "b", "c", "d", "e"), edge("a", "b"), edge("b", "c"), edge("c", "d"), edge("d", "e"))
	long := draftOf(nil)
	long.Draft.Channel = "message"
	long.Draft.Body = strings.Repeat("word ", 301)
	email := draftOf(nil)
	email.Draft.Body = long.Draft.Body
	thin := recipeOf(false)
	thin.Recipe.Steps = []RecipeStep{{ID: "a", Text: "Mix."}, {ID: "b", Text: "Bake."}}
	repeated := deck(2)
	repeated.Flashcards.Cards[1].Back = "Front 1!"
	marker := Component{ID: "l", Type: "labeled_image", Summary: "x", LabeledImage: &LabeledImage{Image: *img("a"), Markers: []ImageMarker{{ID: "a", Label: "A"}}}}
	cases := []struct {
		name      string
		component Component
		codes     []string
	}{
		{"flow without edges", diagramOf("flow", nodes("a", "b")), []string{LintDiagramWithoutEdges}},
		{"cycle without edges", diagramOf("cycle", nodes("a", "b", "c")), nil},
		{"shallow mindmap", diagramOf("mindmap", nodes("a", "b", "c", "d"), edge("a", "b"), edge("b", "c"), edge("c", "d")), nil},
		{"deep mindmap", deep, []string{LintMindmapTooDeep}},
		{"one marker", marker, []string{LintLabeledImageOneMarker}},
		{"front is back", repeated, []string{LintFlashcardFrontIsBack}},
		{"distinct cards", deck(3), nil},
		{"single bare step", Component{ID: "m", Type: "math", Summary: "x", Math: &Math{Steps: []MathStep{{LaTeX: "x = 4"}}}}, []string{LintMathSingleStep}},
		{"single step with note", mathOf("x = 4"), nil},
		{"long message", long, []string{LintDraftLongMessage}},
		{"long email", email, nil},
		{"thin recipe", thin, []string{LintRecipeThinSteps}},
		{"recipe", recipeOf(true), nil},
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

func TestWave2SchemaEntries(t *testing.T) {
	properties := componentJSONSchema()["properties"].(map[string]interface{})
	for _, name := range []string{"diagram", "labeled_image", "quantity_visual", "math", "definition", "flashcards", "before_after", "draft", "recipe", "item", "analogy"} {
		if properties[name] == nil {
			t.Fatalf("schema has no %s payload", name)
		}
		spec := catalogue[name]
		if spec.payload != name || !RequiresSummary(name) || spec.sentence == "" {
			t.Fatalf("%s catalogue entry = %#v", name, spec)
		}
	}
	diagram := properties["diagram"].(map[string]interface{})["properties"].(map[string]interface{})
	node := diagram["nodes"].(map[string]interface{})["items"].(map[string]interface{})["properties"].(map[string]interface{})
	if node["x"] != nil || node["y"] != nil || diagram["layout"] == nil {
		t.Fatalf("diagram schema = %v", diagram)
	}
	if item := properties["item"].(map[string]interface{})["properties"].(map[string]interface{}); item["link"] != nil {
		t.Fatal("item schema has a link")
	}
}
