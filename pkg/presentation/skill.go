package presentation

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// rubric is the §6.1 "when to use what" guidance. A line names only the
// types that are in the catalogue and is left out when none is, so the
// rubric grows as types land and never points the model at a type Validate
// rejects.
var rubric = []struct {
	line  string
	types []string
}{
	{"Choosing between things → %s.", []string{"comparison"}},
	{"A key takeaway, tip or warning → %s.", []string{"callout"}},
	{"A group decision in a shared chat → %s.", []string{"poll"}},
	{"How to do something → %s.", []string{"steps"}},
	{"Parts of something → %s.", []string{"labeled_image"}},
	{"A process, cycle or hierarchy → %s.", []string{"diagram"}},
	{"A new term or idea → %s.", []string{"definition", "analogy"}},
	{"Things over time → %s.", []string{"timeline", "agenda", "itinerary"}},
	{"Headline numbers → %s.", []string{"stat_row"}},
	{"A trend or distribution → %s.", []string{"chart"}},
	{"Exact values → %s.", []string{"table"}},
	{"Parts of a whole → %s.", []string{"breakdown"}},
	{"Maths for learners → %s.", []string{"quantity_visual", "math"}},
	{"Learning → finish with %s.", []string{"quiz", "flashcards"}},
	{"Lists the user will act on → %s.", []string{"checklist"}},
	{"Cooking → %s.", []string{"recipe"}},
	{"One product or listing → %s.", []string{"item"}},
	{"Improving something they wrote → %s (the changes, then the new version).", []string{"before_after", "draft"}},
	{"\"What if\" questions → %s.", []string{"explorer"}},
	{"Information you need from the user → %s.", []string{"form"}},
	{"Likely next requests → %s chips.", []string{"choices"}},
	{"Images already in this chat → %s.", []string{"image", "gallery"}},
}

// VisualRubric returns the "when to show what" guidance for the current
// catalogue. Hosts put it on the description of the answer's visual field.
func VisualRubric() string {
	var b strings.Builder
	b.WriteString("Most replies are just text. Add a visual only when it makes the answer clearer or easier to act on than words would. " +
		"Then lead with one or two sentences, show the visual, and add at most one short line.\n")
	b.WriteString("When the content has shape:\n")
	for _, entry := range rubric {
		var names []string
		for _, name := range entry.types {
			if _, available := catalogue[name]; available {
				names = append(names, "`"+name+"`")
			}
		}
		if len(names) > 0 {
			fmt.Fprintf(&b, "- "+entry.line+"\n", strings.Join(names, ", "))
		}
	}
	b.WriteString("- Ongoing tracking (scores over time, a streak, a plan they'll come back to) → offer to make them an app; don't fake it with cards.\n")
	b.WriteString("Stay with text for: conversation and small talk; feelings, worries and sensitive topics; nuanced advice, opinions and reasoning; " +
		"writing where the words are the deliverable (emails, stories, essays); short facts and one-line answers; " +
		"and whenever the user asked for text or you are in a quick back-and-forth.\n")
	b.WriteString("Never restate in prose what the visual already shows.")
	return b.String()
}

func SkillDefinition() *skill.Definition {
	artifactRefs := map[string]interface{}{"type": "array", "minItems": 1, "maxItems": 1, "items": closedObject([]interface{}{"id", "version", "requirementName"}, map[string]interface{}{
		"id": str(0), "version": map[string]interface{}{"type": "integer", "minimum": 1}, "requirementName": str(0),
	})}
	return &skill.Definition{
		ID: SkillID, Version: SkillVersion, Name: "Visuals",
		Description: "Replace a visual you already showed in this chat with a newer version. New visuals belong on your answer.",
		Icon:        "layout-dashboard", Category: "documents", Tags: []string{"interactive", "chart", "diagram", "form", "visual"},
		Transport: skill.TransportReference{Kind: "tool", Endpoint: SkillID},
		Actions: map[string]skill.Action{UpdateVisual: {
			Name: UpdateVisual, Description: "Replace a visual you already showed in this chat with a newer version (e.g. as long work progresses).",
			InputSchema: UpdateVisualInputSchema(),
			OutputSchema: closedObject([]interface{}{"artifactRefs", "state"}, map[string]interface{}{
				"artifactRefs": artifactRefs, "state": enum([]string{string(StateStreaming), string(StateReady)}),
			}),
			SideEffect: skill.SideEffectWrite, Risk: skill.RiskLevelWrite, Idempotency: skill.IdempotencyRequired,
			Retry: skill.ActionRetryPolicy{MaxAttempts: 1}, EmittedArtifactTypes: []string{ArtifactType},
		}},
		Prompt: &capability.PromptModule{
			Instructions: "Visuals go on your answer. Use update_visual only to replace a visual you already showed with a newer version, e.g. as long work progresses. " +
				"Give every component except text, caption, divider, layouts and actions a one-sentence summary. Never write HTML, scripts, colours or URLs; images must already be in this chat.",
			UserInvocable: true, AllowedTools: []string{UpdateVisual},
		},
		Requirements: capability.Requirements{AlwaysAvailable: true},
	}
}

// VisualFieldDescription is the description of an answer's visual field: the
// rubric followed by the worked examples, one compact JSON object per line.
func VisualFieldDescription() string {
	var b strings.Builder
	b.WriteString("Optional visual shown with your answer, directly under its text. ")
	b.WriteString(VisualRubric())
	b.WriteString("\nWorked examples (userAsks, your answer text, and the visual if any):")
	for _, example := range VisualExamples() {
		encoded, err := json.Marshal(example)
		if err != nil {
			continue
		}
		b.WriteString("\n")
		b.Write(encoded)
	}
	return b.String()
}

// Visual is the model-facing shape of one visual: what an answer's visual
// field carries. The host completes it into a Surface.
type Visual struct {
	Title      string      `json:"title"`
	Components []Component `json:"components"`
}

// Surface completes a visual into a surface in the given state.
func (v Visual) Surface(state State) Surface {
	return Surface{Schema: Schema, Title: v.Title, State: state, Components: v.Components}
}

// VisualJSONSchema is the JSON Schema of a Visual, for an answer's visual
// field. The component tree is unrolled to MaxDepth levels instead of using
// $ref so the schema can be embedded anywhere.
func VisualJSONSchema() map[string]interface{} {
	return closedObject([]interface{}{"title", "components"}, map[string]interface{}{
		"title": strRange(1, MaxTitleRunes), "components": componentsSchema(),
	})
}

// ComponentJSONSchema is the JSON Schema of one top-level component.
func ComponentJSONSchema() map[string]interface{} {
	return componentSchema(1)
}

func componentsSchema() map[string]interface{} {
	return describe(map[string]interface{}{"type": "array", "minItems": 1, "maxItems": MaxComponents, "items": componentSchema(1)},
		fmt.Sprintf("At most %d components in total including nested ones, nested at most %d deep, %d KB of JSON.", MaxComponents, MaxDepth, MaxSurfaceBytes/1024))
}

// UpdateVisualInputSchema is the update_visual input schema.
func UpdateVisualInputSchema() map[string]interface{} {
	return closedObject([]interface{}{"visualId", "expectedLatestVersion", "title", "state", "components", "requirementName"}, map[string]interface{}{
		"visualId":              describe(map[string]interface{}{"type": "string", "pattern": opaqueIDPattern}, "Id of the visual to replace."),
		"expectedLatestVersion": map[string]interface{}{"type": "integer", "minimum": 0, "description": "The visual's current version; 0 only when it does not exist yet."},
		"title":                 strRange(1, MaxTitleRunes),
		"state":                 enum([]string{string(StateStreaming), string(StateReady)}),
		"components":            componentsSchema(),
		"requirementName":       strRange(1, 128),
	})
}

func componentSchema(depth int) map[string]interface{} {
	top := depth == 1
	types := TypeNames()
	if depth == MaxDepth {
		leaves := types[:0:0]
		for _, name := range types {
			if !IsLayout(name) {
				leaves = append(leaves, name)
			}
		}
		types = leaves
	}
	typeSchema := enum(types)
	summary := strRange(0, MaxSummaryRunes)
	if top {
		lines := make([]string, 0, len(catalogue))
		for _, name := range TypeNames() {
			spec := catalogue[name]
			lines = append(lines, fmt.Sprintf("%s: %s Example: %s", name, spec.sentence, spec.example))
		}
		typeSchema = describe(typeSchema, "Set only the payload whose key equals type (text, caption and divider use text). Types:\n"+strings.Join(lines, "\n"))
		var needs []string
		for _, name := range TypeNames() {
			if RequiresSummary(name) {
				needs = append(needs, name)
			}
		}
		summary = describe(summary, "One plain sentence for screen readers and voice. Required for "+strings.Join(needs, ", ")+".")
	}
	properties := map[string]interface{}{
		"id": map[string]interface{}{"type": "string", "pattern": opaqueIDPattern}, "type": typeSchema,
		"summary": summary, "title": str(MaxTitleRunes), "tone": enum(toneNames()), "binding": bindingSchema(),
		"text": str(1200), "chart": chartSchema(), "diagram": diagramSchema(), "form": formSchema(),
		"table": tableSchema(), "stat_row": statRowSchema(), "actions": actionsSchema(top),
		"callout": calloutSchema(), "steps": stepsSchema(), "comparison": comparisonSchema(), "timeline": timelineSchema(),
		"image": imageBlockSchema(), "gallery": gallerySchema(), "choices": choicesSchema(), "checklist": checklistSchema(), "quiz": quizSchema(),
		"labeled_image": labeledImageSchema(), "quantity_visual": quantityVisualSchema(), "math": mathSchema(), "definition": definitionSchema(),
		"flashcards": flashcardsSchema(), "before_after": beforeAfterSchema(), "draft": draftSchema(), "recipe": recipeSchema(),
		"item": itemSchema(), "analogy": analogySchema(),
	}
	if depth < MaxDepth {
		child := componentSchema(depth + 1)
		properties["children"] = map[string]interface{}{"type": "array", "minItems": 1, "maxItems": 12, "items": child}
		properties["grid"] = closedObject([]interface{}{"columns"}, map[string]interface{}{"columns": map[string]interface{}{"type": "integer", "minimum": 2, "maximum": 4}})
		properties["tabs"] = map[string]interface{}{"type": "array", "minItems": 2, "maxItems": 5, "items": closedObject([]interface{}{"label", "children"}, map[string]interface{}{
			"label": strRange(1, 40), "children": map[string]interface{}{"type": "array", "minItems": 1, "maxItems": 12, "items": child},
		})}
	}
	return closedObject([]interface{}{"id", "type"}, properties)
}

func bindingSchema() map[string]interface{} {
	kinds := make([]string, len(BindingKinds))
	for index, kind := range BindingKinds {
		kinds[index] = string(kind)
	}
	return closedObject([]interface{}{"kind", "id"}, map[string]interface{}{"kind": enum(kinds), "id": map[string]interface{}{"type": "string", "pattern": opaqueIDPattern}})
}

func chartSchema() map[string]interface{} {
	point := closedObject([]interface{}{"label", "value"}, map[string]interface{}{"label": strRange(1, 160), "value": number()})
	series := closedObject([]interface{}{"name", "points"}, map[string]interface{}{
		"name": strRange(1, 120), "points": array(point, 1, MaxChartPoints),
	})
	return closedObject([]interface{}{"kind", "series"}, map[string]interface{}{
		"kind": enum(ChartKinds), "xLabel": str(120), "yLabel": str(120),
		"format": enum(formatNames()), "currency": currencySchema(), "unit": str(24),
		"series":        array(series, 1, MaxChartSeries),
		"annotations":   array(closedObject([]interface{}{"x", "label"}, map[string]interface{}{"x": strRange(1, 160), "label": strRange(1, 40)}), 0, MaxChartAnnotations),
		"referenceLine": closedObject([]interface{}{"value"}, map[string]interface{}{"value": number(), "label": str(40)}),
		"highlight":     closedObject([]interface{}{}, map[string]interface{}{"series": str(120), "x": str(160)}),
	})
}

func diagramSchema() map[string]interface{} {
	node := closedObject([]interface{}{"id", "label"}, map[string]interface{}{
		"id": itemIDSchema(), "label": strRange(1, 40), "icon": iconSchema(),
		"group": describe(itemIDSchema(), "Id of one of groups."), "detail": describe(str(300), "Markdown shown when the node is tapped."),
	})
	edge := closedObject([]interface{}{"from", "to"}, map[string]interface{}{"from": itemIDSchema(), "to": itemIDSchema(), "label": str(30)})
	group := closedObject([]interface{}{"id", "label"}, map[string]interface{}{"id": itemIDSchema(), "label": strRange(1, 30)})
	return closedObject([]interface{}{"layout", "nodes"}, map[string]interface{}{
		"layout":    describe(enum(DiagramLayouts), "flow: steps with arrows; cycle: nodes in order around a ring (at least 3; edges optional); hierarchy: parent -> child edges, one parent each; mindmap: the first node is the centre and every node connects to it; network: any links."),
		"direction": describe(enum(DiagramDirections), "flow and hierarchy only; down by default."),
		"nodes":     array(node, 2, MaxDiagramNodes), "edges": array(edge, 0, MaxDiagramEdges), "groups": array(group, 0, MaxDiagramGroups),
	})
}

func labeledImageSchema() map[string]interface{} {
	fraction := describe(map[string]interface{}{"type": "number", "minimum": 0, "maximum": 1}, "0-1 from the top-left of the image.")
	marker := closedObject([]interface{}{"id", "x", "y", "label"}, map[string]interface{}{
		"id": itemIDSchema(), "x": fraction, "y": fraction, "label": strRange(1, 40), "detail": str(300),
	})
	return closedObject([]interface{}{"image", "markers"}, map[string]interface{}{"image": imageRefSchema(), "markers": array(marker, 1, 12)})
}

func integer(minimum, maximum int) map[string]interface{} {
	return map[string]interface{}{"type": "integer", "minimum": minimum, "maximum": maximum}
}

// quantityValueProperties are the per-kind fields of a quantity visual and
// of each of its steps.
func quantityValueProperties() map[string]interface{} {
	label := str(quantityLabelRunes)
	return map[string]interface{}{
		"fractions": describe(array(closedObject([]interface{}{"numerator", "denominator"}, map[string]interface{}{
			"numerator": describe(integer(0, MaxDenominator), "At most the denominator."), "denominator": integer(1, MaxDenominator), "label": label,
		}), 1, maxQuantityGroupings), "fraction_bars: one equal-width bar per fraction."),
		"min": describe(number(), "number_line: required with max."), "max": number(),
		"ticks":  describe(map[string]interface{}{"type": "number", "exclusiveMinimum": 0}, "number_line: gap between ticks."),
		"points": array(closedObject([]interface{}{"value"}, map[string]interface{}{"value": number(), "label": label}), 0, maxNumberLinePoints),
		"ranges": array(closedObject([]interface{}{"from", "to"}, map[string]interface{}{"from": number(), "to": number(), "label": label}), 0, maxNumberLineRanges),
		"groups": describe(array(closedObject([]interface{}{"count"}, map[string]interface{}{"count": integer(0, MaxUnitBlocks), "label": label}), 1, maxQuantityGroupings),
			"unit_blocks: one row of blocks per group."),
		"parts": describe(array(closedObject([]interface{}{"value", "label"}, map[string]interface{}{
			"value": map[string]interface{}{"type": "number", "minimum": 0, "maximum": 100}, "label": strRange(1, quantityLabelRunes),
		}), 1, maxQuantityGroupings), "percent_waffle: percentages totalling at most 100 on a 10x10 grid."),
		"items": describe(array(closedObject([]interface{}{"label", "value"}, map[string]interface{}{"label": strRange(1, quantityLabelRunes), "value": amountSchema()}), 2, maxQuantityGroupings),
			"scale_compare: sizes drawn in proportion."),
	}
}

func quantityVisualSchema() map[string]interface{} {
	properties := quantityValueProperties()
	step := quantityValueProperties()
	step["label"] = strRange(1, quantityStepRunes)
	properties["kind"] = enum(QuantityKinds)
	properties["steps"] = describe(array(closedObject([]interface{}{"label"}, step), 0, MaxQuantitySteps),
		"Stages people step through, each with the same fields as the kind, e.g. both fractions re-cut into twelfths.")
	return closedObject([]interface{}{"kind"}, properties)
}

func amountSchema() map[string]interface{} {
	return closedObject([]interface{}{"value"}, map[string]interface{}{"value": number(), "format": enum(formatNames()), "currency": currencySchema(), "unit": str(24)})
}

func priceSchema() map[string]interface{} {
	price := amountSchema()
	price["properties"].(map[string]interface{})["per"] = enum(PricePeriods)
	return price
}

func mathSchema() map[string]interface{} {
	step := closedObject([]interface{}{"latex"}, map[string]interface{}{
		"latex": describe(strRange(1, MaxLaTeXRunes), "LaTeX without $ delimiters; no \\href, \\url, \\includegraphics, \\html or new macros."), "note": str(200),
	})
	return closedObject([]interface{}{"steps"}, map[string]interface{}{
		"steps": array(step, 1, 12), "reveal": describe(enum(MathReveals), "all (default) or stepwise: one step at a time."),
	})
}

func definitionSchema() map[string]interface{} {
	return closedObject([]interface{}{"term", "meaning"}, map[string]interface{}{
		"term": strRange(1, 60), "phonetic": str(60), "partOfSpeech": str(20), "meaning": strRange(1, 300), "example": str(200), "image": imageRefSchema(),
	})
}

func flashcardsSchema() map[string]interface{} {
	card := closedObject([]interface{}{"id", "front", "back"}, map[string]interface{}{
		"id": itemIDSchema(), "front": strRange(1, 200), "back": strRange(1, 400), "image": imageRefSchema(),
	})
	return closedObject([]interface{}{"cards"}, map[string]interface{}{"cards": array(card, 2, 30), "shuffle": boolean()})
}

func beforeAfterSchema() map[string]interface{} {
	side := describe(closedObject([]interface{}{}, map[string]interface{}{"text": str(4000), "image": imageRefSchema()}), "Text or an image, not both; both sides the same kind.")
	return closedObject([]interface{}{"mode", "before", "after"}, map[string]interface{}{
		"mode":   describe(enum(BeforeAfterModes), "diff highlights changed words (texts); split shows both side by side; slider wipes between images."),
		"before": side, "after": side,
		"labels": closedObject([]interface{}{}, map[string]interface{}{"before": str(20), "after": str(20)}),
	})
}

func draftSchema() map[string]interface{} {
	return closedObject([]interface{}{"channel", "body"}, map[string]interface{}{
		"channel": enum(DraftChannels), "to": describe(str(200), "Shown only; it sends nothing."), "subject": str(200), "body": describe(strRange(1, 4000), "Markdown."),
	})
}

func recipeSchema() map[string]interface{} {
	minutes := integer(0, MaxRecipeMinutes)
	ingredient := closedObject([]interface{}{"id", "name"}, map[string]interface{}{
		"id": itemIDSchema(), "qty": describe(map[string]interface{}{"type": "number", "exclusiveMinimum": 0}, "For servings; people rescale it. Leave out for to taste."),
		"unit": str(16), "name": strRange(1, 60), "group": str(30),
	})
	step := closedObject([]interface{}{"id", "text"}, map[string]interface{}{"id": itemIDSchema(), "text": strRange(1, 300)})
	return closedObject([]interface{}{"servings", "ingredients", "steps"}, map[string]interface{}{
		"servings": integer(1, 24), "time": describe(closedObject([]interface{}{}, map[string]interface{}{"prep": minutes, "cook": minutes}), "Minutes."),
		"ingredients": array(ingredient, 1, 30), "steps": array(step, 1, 15), "image": imageRefSchema(),
		"checkable": describe(boolean(), "Let people tick ingredients off together."),
	})
}

func itemSchema() map[string]interface{} {
	rating := closedObject([]interface{}{"value"}, map[string]interface{}{
		"value": map[string]interface{}{"type": "number", "minimum": 0, "maximum": 5}, "count": map[string]interface{}{"type": "integer", "minimum": 0},
	})
	return closedObject([]interface{}{"name"}, map[string]interface{}{
		"name": strRange(1, 80), "image": imageRefSchema(), "price": priceSchema(), "rating": rating,
		"facts": array(strRange(1, 60), 0, 4), "badge": str(24),
	})
}

func analogySchema() map[string]interface{} {
	side := closedObject([]interface{}{"label"}, map[string]interface{}{"label": strRange(1, 40), "icon": iconSchema()})
	pair := closedObject([]interface{}{"a", "b"}, map[string]interface{}{"a": describe(strRange(1, 60), "A part of from."), "b": describe(strRange(1, 60), "What it is like in to.")})
	return closedObject([]interface{}{"from", "to", "mapping"}, map[string]interface{}{"from": side, "to": side, "mapping": array(pair, 1, 5)})
}

func formSchema() map[string]interface{} {
	option := closedObject([]interface{}{"label", "value"}, map[string]interface{}{"label": strRange(1, 240), "value": strRange(1, 240)})
	field := closedObject([]interface{}{"id", "type", "label"}, map[string]interface{}{
		"id": map[string]interface{}{"type": "string", "pattern": opaqueIDPattern}, "type": enum(FormFieldTypes),
		"label": strRange(1, 240), "description": str(1000), "required": map[string]interface{}{"type": "boolean"}, "placeholder": str(500),
		"options": map[string]interface{}{"type": "array", "maxItems": 100, "items": option}, "min": number(), "max": number(),
	})
	return closedObject([]interface{}{"fields"}, map[string]interface{}{
		"description": str(2000), "submitLabel": str(120),
		"fields": map[string]interface{}{"type": "array", "minItems": 1, "maxItems": 40, "items": field},
	})
}

func tableSchema() map[string]interface{} {
	column := closedObject([]interface{}{"key", "label", "kind"}, map[string]interface{}{
		"key": itemIDSchema(), "label": strRange(1, 40), "kind": enum(TableColumnKinds), "currency": currencySchema(),
	})
	cell := map[string]interface{}{"anyOf": []interface{}{str(MaxTableCellRunes), number(), imageRefSchema()}}
	return closedObject([]interface{}{"columns", "rows"}, map[string]interface{}{
		"columns": array(column, 1, MaxTableColumns),
		"rows": describe(array(map[string]interface{}{"type": "object", "additionalProperties": cell}, 0, MaxTableRows),
			"Each row maps column keys to cells: numbers for number, currency and percent; YYYY-MM-DD for date; an image for image; text otherwise."),
	})
}

func calloutSchema() map[string]interface{} {
	return closedObject([]interface{}{"body"}, map[string]interface{}{"icon": iconSchema(), "body": strRange(1, 300)})
}

func stepsSchema() map[string]interface{} {
	step := closedObject([]interface{}{"id", "title"}, map[string]interface{}{
		"id": itemIDSchema(), "title": strRange(1, 80), "detail": str(400), "icon": iconSchema(), "image": imageRefSchema(),
	})
	return closedObject([]interface{}{"items"}, map[string]interface{}{
		"items": array(step, 2, 10), "checkable": describe(boolean(), "Let people tick steps off as they go."),
	})
}

func comparisonSchema() map[string]interface{} {
	attribute := closedObject([]interface{}{"key", "label"}, map[string]interface{}{"key": itemIDSchema(), "label": strRange(1, 40)})
	price := priceSchema()
	option := closedObject([]interface{}{"id", "name", "values"}, map[string]interface{}{
		"id": itemIDSchema(), "name": strRange(1, 60), "image": imageRefSchema(), "badge": str(24), "price": price,
		"values": describe(map[string]interface{}{"type": "object", "additionalProperties": str(80)}, "A value for every attribute key; an empty string is allowed."),
		"pros":   array(strRange(1, 80), 0, 4), "cons": array(strRange(1, 80), 0, 4),
	})
	return closedObject([]interface{}{"attributes", "options"}, map[string]interface{}{
		"attributes": array(attribute, 1, 10), "options": array(option, 2, 4),
		"recommended": describe(itemIDSchema(), "Id of the option you recommend."),
		"pickable":    describe(boolean(), "Let the user pick one option; their pick answers you."),
	})
}

func timelineSchema() map[string]interface{} {
	event := closedObject([]interface{}{"id", "when", "title"}, map[string]interface{}{
		"id": itemIDSchema(), "when": describe(strRange(1, 40), "Label shown, e.g. 1789, Mon 3 Nov or Week 2."),
		"date":  describe(map[string]interface{}{"type": "string", "pattern": isoDate.String()}, "YYYY-MM-DD, used only to order events."),
		"title": strRange(1, 80), "detail": str(300), "icon": iconSchema(), "image": imageRefSchema(),
	})
	return closedObject([]interface{}{"events"}, map[string]interface{}{"events": array(event, 2, 20)})
}

func imageBlockSchema() map[string]interface{} {
	return closedObject([]interface{}{"image"}, map[string]interface{}{"image": imageRefSchema(), "caption": str(200), "aspect": enum(ImageAspects)})
}

func gallerySchema() map[string]interface{} {
	image := closedObject([]interface{}{"image"}, map[string]interface{}{"image": imageRefSchema(), "caption": str(120)})
	return closedObject([]interface{}{"images"}, map[string]interface{}{"images": array(image, 2, 12), "layout": enum(GalleryLayouts)})
}

func choicesSchema() map[string]interface{} {
	chip := closedObject([]interface{}{"id", "label"}, map[string]interface{}{"id": itemIDSchema(), "label": strRange(1, 40)})
	return closedObject([]interface{}{"chips"}, map[string]interface{}{"chips": array(chip, 2, 5)})
}

func checklistSchema() map[string]interface{} {
	item := closedObject([]interface{}{"id", "label"}, map[string]interface{}{"id": itemIDSchema(), "label": strRange(1, 100), "group": str(40)})
	return closedObject([]interface{}{"items"}, map[string]interface{}{"items": array(item, 1, 40)})
}

func quizSchema() map[string]interface{} {
	option := closedObject([]interface{}{"id", "label"}, map[string]interface{}{"id": itemIDSchema(), "label": strRange(1, 120), "image": imageRefSchema()})
	key := describe(closedObject([]interface{}{"correct", "explanation"}, map[string]interface{}{
		"correct": describe(array(itemIDSchema(), 1, 6), "Ids of the correct options: exactly one unless allowMultiple."), "explanation": str(400),
	}), "Hidden from people until they answer.")
	return closedObject([]interface{}{"question", "options", "answerKey"}, map[string]interface{}{
		"question": strRange(1, 300), "options": array(option, 2, 6), "allowMultiple": boolean(), "answerKey": key,
	})
}

func imageRefSchema() map[string]interface{} {
	return describe(closedObject([]interface{}{"artifactId", "version"}, map[string]interface{}{
		"artifactId": map[string]interface{}{"type": "string", "pattern": opaqueIDPattern}, "version": map[string]interface{}{"type": "integer", "minimum": 1},
	}), "An image already in this chat (generated, uploaded or a screenshot), by the id and version it was shown with. Never a URL.")
}

func itemIDSchema() map[string]interface{} {
	return map[string]interface{}{"type": "string", "pattern": itemIDPattern}
}

func iconSchema() map[string]interface{} {
	return map[string]interface{}{"type": "string", "maxLength": 40, "pattern": iconPattern.String()}
}

func currencySchema() map[string]interface{} {
	return describe(map[string]interface{}{"type": "string", "pattern": "^[A-Z]{3}$"}, "ISO 4217 code such as GBP, USD or EUR; required with format currency.")
}

func formatNames() []string {
	formats := make([]string, len(StatFormats))
	for index, format := range StatFormats {
		formats[index] = string(format)
	}
	return formats
}

func array(items map[string]interface{}, minimum, maximum int) map[string]interface{} {
	schema := map[string]interface{}{"type": "array", "maxItems": maximum, "items": items}
	if minimum > 0 {
		schema["minItems"] = minimum
	}
	return schema
}

func boolean() map[string]interface{} { return map[string]interface{}{"type": "boolean"} }

func statRowSchema() map[string]interface{} {
	stat := closedObject([]interface{}{"label", "value"}, map[string]interface{}{
		"label":    strRange(1, 60),
		"value":    describe(number(), "The raw number; the app formats it for the reader's language and region."),
		"format":   describe(enum(formatNames()), "number (default); currency (set currency); percent (12 means 12%); duration (value in seconds)."),
		"currency": currencySchema(),
		"unit":     describe(str(24), "Shown after the value, e.g. GB/mo or per month."), "delta": str(40),
		"trend": enum(Trends), "goodDirection": enum(GoodDirections),
		"sparkline": map[string]interface{}{"type": "array", "maxItems": 30, "items": number()},
	})
	return closedObject([]interface{}{"stats"}, map[string]interface{}{
		"stats": map[string]interface{}{"type": "array", "minItems": 1, "maxItems": 4, "items": stat},
	})
}

func actionsSchema(top bool) map[string]interface{} {
	kinds := make([]string, len(ActionKinds))
	for index, kind := range ActionKinds {
		kinds[index] = string(kind)
	}
	var commands []string
	for _, list := range EntityCommands {
		commands = append(commands, list...)
	}
	sort.Strings(commands)
	icon := iconSchema()
	kind := enum(kinds)
	if top {
		icon = describe(icon, "Lucide icon name from: "+strings.Join(Icons, ", ")+".")
		kind = describe(kind, "respond sends value (default label) as the user's reply; edit opens the draft named by target; approval sends decision on the approval binding; entity sends command to the bound routine (pause, resume, run_now, stop), task (cancel) or agent (open_chat).")
	}
	action := closedObject([]interface{}{"id", "label", "kind"}, map[string]interface{}{
		"id": map[string]interface{}{"type": "string", "pattern": opaqueIDPattern}, "label": strRange(1, 40), "kind": kind, "icon": icon,
		"value": str(500), "target": map[string]interface{}{"type": "string", "pattern": opaqueIDPattern},
		"decision": enum([]string{"approve", "reject"}), "command": enum(commands), "binding": bindingSchema(),
	})
	return map[string]interface{}{"type": "array", "minItems": 1, "maxItems": MaxActions, "items": action}
}

func toneNames() []string {
	names := make([]string, len(Tones))
	for index, tone := range Tones {
		names[index] = string(tone)
	}
	return names
}

func closedObject(required []interface{}, properties map[string]interface{}) map[string]interface{} {
	schema := map[string]interface{}{"type": "object", "additionalProperties": false, "properties": properties}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func str(maximum int) map[string]interface{} {
	if maximum == 0 {
		return map[string]interface{}{"type": "string"}
	}
	return map[string]interface{}{"type": "string", "maxLength": maximum}
}

func strRange(minimum, maximum int) map[string]interface{} {
	schema := str(maximum)
	if minimum > 0 {
		schema["minLength"] = minimum
	}
	return schema
}

func number() map[string]interface{} { return map[string]interface{}{"type": "number"} }

func enum(values []string) map[string]interface{} {
	items := make([]interface{}, len(values))
	for index, value := range values {
		items[index] = value
	}
	return map[string]interface{}{"type": "string", "enum": items}
}

func describe(schema map[string]interface{}, description string) map[string]interface{} {
	schema["description"] = description
	return schema
}
