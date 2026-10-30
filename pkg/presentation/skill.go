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
	{"Progress toward a target → %s.", []string{"progress"}},
	{"A trend or distribution → %s.", []string{"chart"}},
	{"Exact values → %s.", []string{"table"}},
	{"Parts of a whole → %s.", []string{"breakdown"}},
	{"Maths for learners → %s.", []string{"quantity_visual", "math"}},
	{"Learning → finish with %s.", []string{"quiz", "flashcards"}},
	{"Lists the user will act on → %s.", []string{"checklist"}},
	{"Cooking → %s.", []string{"recipe"}},
	{"One product, listing or place → %s.", []string{"item", "place"}},
	{"Where things are → find_place, then a %s (several places, a day out, a trip) or a place card with its location (one venue). Never guess coordinates.", []string{"map"}},
	{"Improving something they wrote → %s (the changes, then the new version).", []string{"before_after", "draft"}},
	{"\"What if\" questions → %s.", []string{"explorer"}},
	{"A routine you run, work you handed to helpers, another agent to chat with → %s.", []string{"routine", "tasks", "agent"}},
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
	b.WriteString("- Practice or progress they'll come back to (quizzing the same topic again, \"track my scores\", a leaderboard, \"how did I do last week\") → " +
		"answer as usual, then add `choices` \"Make the app\" / \"Not now\"; don't fake tracking with cards.\n")
	b.WriteString("- An interactive app only when the user explicitly wants to play or experiment and no card fits. Apps can report results back to you.\n")
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
				"Give every component except text, caption, divider, layouts and actions a one-sentence summary. Never write HTML, scripts, colours, URLs or coordinates; images must already be in this chat and places come from find_place.",
			UserInvocable: true, AllowedTools: []string{UpdateVisual},
		},
		Requirements: capability.Requirements{AlwaysAvailable: true},
	}
}

// fieldExamples names the worked examples the visual field shows: one visual
// answer and the replies that stay text.
var fieldExamples = []string{"phone-plan", "text-feelings", "text-fact"}

// VisualFieldDescription is the description of an answer's visual field: the
// rubric, the exact format in the compact notation, and a few worked
// examples, one compact JSON object per line.
func VisualFieldDescription() string {
	var b strings.Builder
	b.WriteString("Optional visual shown with your answer, directly under its text. ")
	b.WriteString(VisualRubric())
	b.WriteString("\n")
	b.WriteString(VisualFormat())
	b.WriteString("\nExamples (userAsks, your answer text, and the visual if any):")
	for _, example := range VisualExamples() {
		if !contains(fieldExamples, example.Name) {
			continue
		}
		encoded, err := json.Marshal(example)
		if err != nil {
			continue
		}
		b.WriteString("\n")
		b.Write(encoded)
	}
	return b.String()
}

// VisualFormat spells out the exact shape of a visual in the compact
// notation, generated from the strict schemas Validate mirrors: the named
// patterns and shapes, the visual and component envelope, nesting limits and
// one line per type.
func VisualFormat() string {
	notation := visualNotation()
	var b strings.Builder
	b.WriteString("Format. " + compactLegend + "\n")
	for _, pattern := range visualPatterns() {
		line := pattern.name + " = " + compactRenderer{}.render(map[string]interface{}{"type": "string", "pattern": pattern.pattern})
		if pattern.note != "" {
			line += " (" + pattern.note + ")"
		}
		b.WriteString(line + "\n")
	}
	for _, shape := range visualShapes() {
		line := shape.name + " = " + notation.define(shape.schema)
		if shape.name == "Icon" {
			line += " (a Lucide name from: " + strings.Join(Icons, ", ") + "; any other renders as a dot)"
		}
		b.WriteString(line + "\n")
	}
	b.WriteString("Visual = " + notation.render(visualSchema(componentRef(), "")) + "\n")
	envelope := envelopeProperties(componentRef())
	children := envelope["children"]
	delete(envelope, "children")
	envelope["type"] = describe(map[string]interface{}{"type": "string"}, "one of the types below")
	envelope["summary"] = describe(envelope["summary"].(map[string]interface{}), "one plain sentence for screen readers and voice; required for types marked *")
	b.WriteString("Component = " + notation.render(closedObject([]interface{}{"id", "type"}, envelope)) +
		" plus only the payload whose key equals type (text, caption and divider use text).\n")
	var layouts []string
	for _, name := range TypeNames() {
		if IsLayout(name) {
			layouts = append(layouts, name)
		}
	}
	fmt.Fprintf(&b, "Components nest at most %d levels deep (top level is 1, so no %s at level %d); at most %d components in all including nested ones; at most %d KB of JSON; ids are unique across the visual.\n",
		MaxDepth, strings.Join(layouts, ", "), MaxDepth, MaxComponents, MaxSurfaceBytes/1024)
	b.WriteString("Markdown fields are GFM without HTML or images; $…$ maths is allowed.\n")
	b.WriteString("Types (* = summary required):")
	payloads := payloadSchemas()
	for _, name := range TypeNames() {
		spec := catalogue[name]
		line := "\n" + name
		if spec.summary == summaryRequired {
			line += "*"
		}
		line += ": " + spec.sentence
		if spec.payload != "" {
			line += " " + spec.payload + ":" + notation.render(payloads[spec.payload])
		}
		if spec.children {
			line += " children:" + notation.render(children)
		}
		if len(spec.bindings) > 0 {
			kinds := make([]string, len(spec.bindings))
			for index, kind := range spec.bindings {
				kinds[index] = string(kind)
			}
			line += " binding.kind:" + strings.Join(kinds, "|")
		}
		b.WriteString(line)
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

// VisualJSONSchema is the JSON Schema of a Visual for an answer's visual
// field. It is deliberately loose: components list every envelope and
// payload key but leave payloads open, because VisualFieldDescription
// spells out every exact shape in a fraction of the tokens and Validate
// checks each visual strictly.
func VisualJSONSchema() map[string]interface{} {
	return visualSchema(looseComponentSchema(), "Each component, its payload and any nested components follow the format in this visual's description exactly.")
}

// UpdateVisualInputSchema is the update_visual input schema. Its components
// are as loose as an answer's visual field, which always travels with it.
func UpdateVisualInputSchema() map[string]interface{} {
	return closedObject([]interface{}{"visualId", "expectedLatestVersion", "title", "state", "components", "requirementName"}, map[string]interface{}{
		"visualId":              describe(idSchema(), "Id of the visual to replace."),
		"expectedLatestVersion": map[string]interface{}{"type": "integer", "minimum": 0, "description": "The visual's current version; 0 only when it does not exist yet."},
		"title":                 strRange(1, MaxTitleRunes),
		"state":                 enum([]string{string(StateStreaming), string(StateReady)}),
		"components": describe(array(looseComponentSchema(), 1, MaxComponents),
			"Components in exactly the format described on your answer's visual field."),
		"requirementName": strRange(1, 128),
	})
}

// maxChildren bounds children and tab children.
const maxChildren = 12

func visualSchema(component map[string]interface{}, description string) map[string]interface{} {
	components := array(component, 1, MaxComponents)
	if description != "" {
		components = describe(components, description)
	}
	return closedObject([]interface{}{"title", "components"}, map[string]interface{}{"title": strRange(1, MaxTitleRunes), "components": components})
}

// componentRef stands for a nested component in the strict schemas.
func componentRef() map[string]interface{} {
	return map[string]interface{}{"$ref": "#/$defs/Component"}
}

// envelopeProperties are the fields every component shares; component is
// the schema of a nested child.
func envelopeProperties(component map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"id": idSchema(), "type": enum(TypeNames()), "summary": str(MaxSummaryRunes), "title": str(MaxTitleRunes),
		"tone": enum(toneNames()), "binding": bindingSchema(), "children": array(component, 1, maxChildren),
	}
}

// payloadSchemas maps every payload key to its strict schema.
func payloadSchemas() map[string]map[string]interface{} {
	return map[string]map[string]interface{}{
		"text": str(1200), "chart": chartSchema(), "diagram": diagramSchema(), "form": formSchema(),
		"table": tableSchema(), "stat_row": statRowSchema(), "actions": actionsSchema(),
		"callout": calloutSchema(), "steps": stepsSchema(), "comparison": comparisonSchema(), "timeline": timelineSchema(),
		"image": imageBlockSchema(), "gallery": gallerySchema(), "choices": choicesSchema(), "checklist": checklistSchema(), "quiz": quizSchema(),
		"labeled_image": labeledImageSchema(), "quantity_visual": quantityVisualSchema(), "math": mathSchema(), "definition": definitionSchema(),
		"flashcards": flashcardsSchema(), "before_after": beforeAfterSchema(), "draft": draftSchema(), "recipe": recipeSchema(),
		"item": itemSchema(), "analogy": analogySchema(),
		"agenda": agendaSchema(), "itinerary": itinerarySchema(), "place": placeSchema(), "breakdown": breakdownSchema(),
		"progress": progressSchema(), "poll": pollSchema(), "explorer": explorerSchema(), "routine": routineSchema(),
		"tasks": tasksSchema(), "agent": agentCardSchema(), "map": mapSchema(),
		"grid": closedObject([]interface{}{"columns"}, map[string]interface{}{"columns": integer(2, 4)}),
		"tabs": array(closedObject([]interface{}{"label", "children"}, map[string]interface{}{
			"label": strRange(1, 40), "children": array(componentRef(), 1, maxChildren),
		}), 2, 5),
	}
}

// componentJSONSchema is the strict schema of one component, with nested
// components as references to it. It feeds VisualFormat and tests; models
// get the loose schema plus the compact format instead.
func componentJSONSchema() map[string]interface{} {
	properties := envelopeProperties(componentRef())
	for key, payload := range payloadSchemas() {
		properties[key] = payload
	}
	return closedObject([]interface{}{"id", "type"}, properties)
}

// looseComponentSchema lists every envelope and payload key with payloads
// and nested components left open.
func looseComponentSchema() map[string]interface{} {
	open := map[string]interface{}{"type": "object"}
	properties := envelopeProperties(open)
	properties["type"] = describe(properties["type"].(map[string]interface{}), "Set only the payload whose key equals type (text, caption and divider use text).")
	for key, payload := range payloadSchemas() {
		switch payload["type"] {
		case "string":
			properties[key] = payload
		case "array":
			properties[key] = map[string]interface{}{"type": "array", "items": open}
		default:
			properties[key] = open
		}
	}
	return closedObject([]interface{}{"id", "type"}, properties)
}

func bindingSchema() map[string]interface{} {
	kinds := make([]string, len(BindingKinds))
	for index, kind := range BindingKinds {
		kinds[index] = string(kind)
	}
	return closedObject([]interface{}{"kind", "id"}, map[string]interface{}{"kind": enum(kinds), "id": idSchema()})
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
		"layout":    describe(enum(DiagramLayouts), "flow: steps with arrows; cycle: nodes in order around a ring (at least 3; edges optional); hierarchy: parent to child edges, one parent each; mindmap: the first node is the centre and every node connects to it; network: any links."),
		"direction": describe(enum(DiagramDirections), "flow and hierarchy only; down by default."),
		"nodes":     array(node, 2, MaxDiagramNodes), "edges": array(edge, 0, MaxDiagramEdges), "groups": array(group, 0, MaxDiagramGroups),
	})
}

func labeledImageSchema() map[string]interface{} {
	fraction := map[string]interface{}{"type": "number", "minimum": 0, "maximum": 1}
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
	// Steps repeat the fields without their notes, which the top level
	// already gives.
	step := quantityValueProperties()
	for _, field := range step {
		delete(field.(map[string]interface{}), "description")
	}
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
		"latex": describe(strRange(1, MaxLaTeXRunes), "No \\href, \\url, \\includegraphics, \\html or new macros."), "note": str(200),
	})
	return closedObject([]interface{}{"steps"}, map[string]interface{}{
		"steps": array(step, 1, 12), "reveal": enum(MathReveals),
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
	side := beforeAfterSideSchema()
	return closedObject([]interface{}{"mode", "before", "after"}, map[string]interface{}{
		"mode":   describe(enum(BeforeAfterModes), "diff highlights changed words (texts); split shows both side by side; slider wipes between images."),
		"before": side, "after": side,
		"labels": closedObject([]interface{}{}, map[string]interface{}{"before": str(20), "after": str(20)}),
	})
}

func beforeAfterSideSchema() map[string]interface{} {
	return describe(closedObject([]interface{}{}, map[string]interface{}{"text": str(4000), "image": imageRefSchema()}), "Text or an image, not both; both sides the same kind.")
}

func draftSchema() map[string]interface{} {
	return closedObject([]interface{}{"channel", "body"}, map[string]interface{}{
		"channel": enum(DraftChannels), "to": describe(str(200), "Shown only; it sends nothing."), "subject": str(200), "body": describe(strRange(1, 4000), "Markdown."),
	})
}

func recipeSchema() map[string]interface{} {
	minutes := integer(0, MaxRecipeMinutes)
	ingredient := closedObject([]interface{}{"id", "name"}, map[string]interface{}{
		"id": itemIDSchema(), "qty": describe(map[string]interface{}{"type": "number", "exclusiveMinimum": 0}, "Leave out for to taste."),
		"unit": str(16), "name": strRange(1, 60), "group": str(30),
	})
	step := closedObject([]interface{}{"id", "text"}, map[string]interface{}{"id": itemIDSchema(), "text": strRange(1, 300)})
	return closedObject([]interface{}{"servings", "ingredients", "steps"}, map[string]interface{}{
		"servings": integer(1, 24), "time": describe(closedObject([]interface{}{}, map[string]interface{}{"prep": minutes, "cook": minutes}), "Minutes."),
		"ingredients": array(ingredient, 1, 30), "steps": array(step, 1, 15), "image": imageRefSchema(),
		"checkable": boolean(),
	})
}

func itemSchema() map[string]interface{} {
	return closedObject([]interface{}{"name"}, map[string]interface{}{
		"name": strRange(1, 80), "image": imageRefSchema(), "price": priceSchema(), "rating": ratingSchema(),
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
		"id": idSchema(), "type": enum(FormFieldTypes),
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
		"items": array(step, 2, 10), "checkable": boolean(),
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
		"pickable":    boolean(),
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
	key := closedObject([]interface{}{"correct", "explanation"}, map[string]interface{}{
		"correct": describe(array(itemIDSchema(), 1, 6), "Ids of the correct options: exactly one unless allowMultiple."), "explanation": str(400),
	})
	return closedObject([]interface{}{"question", "options", "answerKey"}, map[string]interface{}{
		"question": strRange(1, 300), "options": array(option, 2, 6), "allowMultiple": boolean(), "answerKey": key,
	})
}

func imageRefSchema() map[string]interface{} {
	return describe(closedObject([]interface{}{"artifactId", "version"}, map[string]interface{}{
		"artifactId": idSchema(), "version": map[string]interface{}{"type": "integer", "minimum": 1},
	}), "An image already in this chat (generated, uploaded or a screenshot), by the id and version it was shown with. Never a URL.")
}

func idSchema() map[string]interface{} {
	return map[string]interface{}{"type": "string", "pattern": opaqueIDPattern}
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

func actionsSchema() map[string]interface{} {
	kinds := make([]string, len(ActionKinds))
	for index, kind := range ActionKinds {
		kinds[index] = string(kind)
	}
	var commands []string
	for _, list := range EntityCommands {
		commands = append(commands, list...)
	}
	sort.Strings(commands)
	kind := describe(enum(kinds), "respond sends value (default label) as the user's reply; edit opens the draft named by target; approval sends decision on the approval binding; entity sends command to the bound routine (pause, resume, run_now, stop), task (cancel) or agent (open_chat).")
	action := closedObject([]interface{}{"id", "label", "kind"}, map[string]interface{}{
		"id": idSchema(), "label": strRange(1, 40), "kind": kind, "icon": iconSchema(),
		"value": str(500), "target": idSchema(),
		"decision": enum([]string{"approve", "reject"}), "command": enum(commands), "binding": bindingSchema(),
	})
	return array(action, 1, MaxActions)
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
