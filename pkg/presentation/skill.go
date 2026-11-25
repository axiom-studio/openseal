package presentation

import (
	"fmt"
	"sort"
	"strings"
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
	{"A few things, each with one headline number → %s.", []string{"cards"}},
	{"Headline numbers → %s.", []string{"stat_row"}},
	{"Progress toward a target → %s.", []string{"progress"}},
	{"A trend or distribution → %s.", []string{"chart"}},
	{"Exact values → %s.", []string{"table"}},
	{"Unit or currency conversion → %s.", []string{"converter"}},
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
	b.WriteString("Cite pages you fetched in the component's sources instead of a caption line; only real URLs from this chat.\n")
	b.WriteString("Never restate in prose what the visual already shows.")
	return b.String()
}

// fieldExamples names the worked examples the answer's description shows:
// one visual answer and the replies that stay text.
var fieldExamples = []string{"phone-plan", "text-feelings", "text-fact"}

// BlocksDescription is what the answer's description adds when the answer
// may carry visuals: how to write a block, the rubric, the exact format of
// every type and a few worked answers.
func BlocksDescription() string {
	var b strings.Builder
	b.WriteString("Visuals go inside the answer, where they belong in the text, as blocks:\n" +
		":::chart{#spend kind=bar title=\"Spend by month\"}\n| Month | Spend |\n|---|---|\n| Jan | 320 |\n:::\n" +
		"A block opens with ::: and a type on its own line and closes with a ::: line. In {…}: #id (unique, short), title, summary, tone, " +
		"and any field of the type's payload whose value is one word, number or quoted text (kind=bar, label=\"Rent\"). " +
		"The body: for a layout (section, grid, carousel) its child blocks, with markdown between them read as text children, " +
		"using more colons on the layout than on its children (::::section … :::text … ::: … ::::); " +
		"for tabs one :::tab{label=\"…\"} block per tab; for text, caption and divider the text itself; " +
		"for chart a markdown table whose first column is the point labels and each other column one series; " +
		"for table a markdown table (add columns: in YAML above it for column kinds); " +
		"for every other type the rest of its payload as YAML (lists and nested objects), " +
		"where a markdown table can fill the payload's one list of flat items. binding and sources go in the YAML too.\n")
	b.WriteString(VisualRubric())
	b.WriteString("\n")
	b.WriteString(VisualFormat())
	b.WriteString("\nExamples:")
	for _, example := range VisualExamples() {
		if !contains(fieldExamples, example.Name) {
			continue
		}
		b.WriteString("\nUser: " + example.Prompt + "\nAnswer:\n" + example.Answer() + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
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
	envelope := envelopeProperties(componentRef())
	delete(envelope, "children")
	envelope["type"] = describe(map[string]interface{}{"type": "string"}, "the block's type, one of the types below")
	b.WriteString("Each block is one Component = " + notation.render(objectOf([]interface{}{"id", "type"}, envelope)) +
		" plus the payload of its type, written as above. A block of any other type is left out.\n")
	var layouts []string
	for _, name := range TypeNames() {
		if IsLayout(name) {
			layouts = append(layouts, name)
		}
	}
	fmt.Fprintf(&b, "Blocks nest at most %d levels deep (top level is 1, so no %s at level %d); at most %d components in all including nested ones; at most %d KB in all.\n",
		MaxDepth, strings.Join(layouts, ", "), MaxDepth, MaxComponents, MaxSurfaceBytes/1024)
	b.WriteString("Also checked: " + strings.Join(sharedRules, "; ") + ".\n")
	b.WriteString("Types:")
	for _, name := range TypeNames() {
		spec := catalogue[name]
		properties := componentTypeSchema(name, componentRef())["properties"].(map[string]interface{})
		line := "\n" + name + ": " + spec.sentence
		if spec.payload != "" {
			line += " " + spec.payload + ":" + notation.render(properties[spec.payload].(map[string]interface{}))
		}
		if spec.children.max > 0 {
			line += " children:" + notation.render(properties["children"].(map[string]interface{}))
		}
		if len(spec.rules) > 0 {
			line += " Checks: " + strings.Join(spec.rules, "; ") + "."
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

// componentRef stands for a nested component in the strict schemas.
func componentRef() map[string]interface{} {
	return map[string]interface{}{"$ref": "#/$defs/Component"}
}

// summaryNote says what a component's summary is and what stands in for it.
const summaryNote = "one plain sentence for screen readers and voice; when left out, the title stands in, or else the first sentence of the component's main text"

// envelopeProperties are the fields every component shares; component is
// the schema of a nested child. Children take the widest bounds of any
// type; componentTypeSchema narrows them per type.
func envelopeProperties(component map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"id": idSchema(), "type": enum(TypeNames()), "summary": describe(freeText(), summaryNote), "title": freeText(),
		"tone": enum(toneNames()), "binding": bindingSchema(BindingKinds), "children": array(component, 1, maxChildren),
		"sources": array(sourceSchema(), 1, maxSources),
	}
}

// componentTypeSchema is the schema of one component type: the envelope
// narrowed to the type's binding and children rules, plus its own payload.
// component is the schema of a nested child. Validate reads the same type
// spec, so the two agree by construction.
func componentTypeSchema(name string, component map[string]interface{}) map[string]interface{} {
	spec := catalogue[name]
	properties := envelopeProperties(component)
	properties["type"] = enum([]string{name})
	required := []interface{}{"id", "type"}
	// Children of a type that holds none are ignored like any other key.
	delete(properties, "children")
	if spec.children.max > 0 {
		properties["children"] = childrenSchema(component, spec.children.min, spec.children.max)
		required = append(required, "children")
	}
	if spec.bindings != nil {
		// A binding is checked against the host, so one on a type that
		// takes none is refused rather than ignored.
		properties["binding"] = false
		if len(spec.bindings) > 0 {
			properties["binding"] = bindingSchema(spec.bindings)
		}
	}
	if spec.bindingRequired {
		required = append(required, "binding")
	}
	if spec.payload != "" {
		properties[spec.payload] = payloadSchema(name)
		if !spec.payloadOptional {
			required = append(required, spec.payload)
		}
	}
	return objectOf(required, properties)
}

// childrenSchema is a list of nested components. Its bounds count only the
// components that are drawn: one whose type is missing or unknown is left
// out and counts toward nothing.
func childrenSchema(component map[string]interface{}, minimum, maximum int) map[string]interface{} {
	schema := map[string]interface{}{"type": "array", "items": component, "contains": knownComponent(), "maxContains": maximum}
	if minimum > 0 {
		schema["minContains"] = minimum
	}
	return schema
}

// knownComponent matches a component whose type is in the catalogue.
func knownComponent() map[string]interface{} {
	return map[string]interface{}{"required": []interface{}{"type"}, "properties": map[string]interface{}{"type": enum(TypeNames())}}
}

// payloadSchema is the payload schema of one component type. text, caption
// and divider share the text key; only divider may leave it out.
func payloadSchema(name string) map[string]interface{} {
	spec := catalogue[name]
	if spec.payload == "text" {
		if spec.payloadOptional {
			return freeText()
		}
		return filledText()
	}
	return payloadSchemas()[spec.payload]
}

// payloadSchemas maps every payload key except text, whose limits depend
// on the type, to its strict schema.
func payloadSchemas() map[string]map[string]interface{} {
	return map[string]map[string]interface{}{
		"chart": chartSchema(), "diagram": diagramSchema(), "form": formSchema(),
		"table": tableSchema(), "stat_row": statRowSchema(), "actions": actionsSchema(),
		"callout": calloutSchema(), "steps": stepsSchema(), "comparison": comparisonSchema(), "timeline": timelineSchema(),
		"image": imageBlockSchema(), "gallery": gallerySchema(), "choices": choicesSchema(), "checklist": checklistSchema(), "quiz": quizSchema(),
		"labeled_image": labeledImageSchema(), "quantity_visual": quantityVisualSchema(), "math": mathSchema(), "definition": definitionSchema(),
		"flashcards": flashcardsSchema(), "before_after": beforeAfterSchema(), "draft": draftSchema(), "recipe": recipeSchema(),
		"item": itemSchema(), "analogy": analogySchema(),
		"agenda": agendaSchema(), "itinerary": itinerarySchema(), "place": placeSchema(), "breakdown": breakdownSchema(),
		"progress": progressSchema(), "poll": pollSchema(), "explorer": explorerSchema(), "routine": routineSchema(),
		"tasks": tasksSchema(), "agent": agentCardSchema(), "map": mapSchema(), "cards": cardsSchema(), "converter": converterSchema(),
		"grid": objectOf([]interface{}{"columns"}, map[string]interface{}{"columns": integer(minGridColumns, maxGridColumns)}),
		"tabs": array(objectOf([]interface{}{"label", "children"}, map[string]interface{}{
			"label": filledText(), "children": childrenSchema(componentRef(), 1, maxChildren),
		}), minTabs, maxTabs),
	}
}

func bindingSchema(allowed []BindingKind) map[string]interface{} {
	kinds := make([]string, len(allowed))
	for index, kind := range allowed {
		kinds[index] = string(kind)
	}
	return objectOf([]interface{}{"kind", "id"}, map[string]interface{}{"kind": enum(kinds), "id": idSchema()})
}

func chartSchema() map[string]interface{} {
	point := objectOf([]interface{}{"label", "value"}, map[string]interface{}{"label": filledText(), "value": number()})
	series := objectOf([]interface{}{"name", "points"}, map[string]interface{}{
		"name": filledText(), "points": array(point, 1, MaxChartPoints),
	})
	return objectOf([]interface{}{"kind", "series"}, map[string]interface{}{
		"kind": enum(ChartKinds), "xLabel": freeText(), "yLabel": freeText(),
		"format": enum(formatNames()), "currency": currencySchema(), "unit": freeText(),
		"series": array(series, 1, MaxChartSeries),
		"annotations": shownList(objectOf([]interface{}{"x", "label"}, map[string]interface{}{
			"x": filledText(), "label": filledText(),
		}), 0, MaxChartAnnotations),
		"referenceLine": objectOf([]interface{}{"value"}, map[string]interface{}{"value": number(), "label": freeText()}),
		"highlight":     objectOf([]interface{}{}, map[string]interface{}{"series": freeText(), "x": freeText()}),
	})
}

func diagramSchema() map[string]interface{} {
	node := objectOf([]interface{}{"id", "label"}, map[string]interface{}{
		"id": itemIDSchema(), "label": filledText(), "icon": iconSchema(),
		"group": describe(itemIDSchema(), ruleDiagramGroup), "detail": describe(freeText(), "Markdown shown when the node is tapped."),
	})
	edge := objectOf([]interface{}{"from", "to"}, map[string]interface{}{"from": itemIDSchema(), "to": itemIDSchema(), "label": freeText()})
	group := objectOf([]interface{}{"id", "label"}, map[string]interface{}{"id": itemIDSchema(), "label": filledText()})
	return objectOf([]interface{}{"layout", "nodes"}, map[string]interface{}{
		"layout":    describe(enum(DiagramLayouts), ruleDiagramLayouts),
		"direction": describe(enum(DiagramDirections), ruleDiagramDirection),
		"nodes":     array(node, 2, MaxDiagramNodes), "edges": array(edge, 0, MaxDiagramEdges), "groups": array(group, 0, MaxDiagramGroups),
	})
}

func labeledImageSchema() map[string]interface{} {
	fraction := map[string]interface{}{"type": "number", "minimum": 0, "maximum": 1}
	marker := objectOf([]interface{}{"id", "x", "y", "label"}, map[string]interface{}{
		"id": itemIDSchema(), "x": fraction, "y": fraction, "label": filledText(), "detail": freeText(),
	})
	return objectOf([]interface{}{"image", "markers"}, map[string]interface{}{"image": imageRefSchema(), "markers": array(marker, 1, maxMarkers)})
}

func integer(minimum, maximum int) map[string]interface{} {
	return map[string]interface{}{"type": "integer", "minimum": minimum, "maximum": maximum}
}

// quantityValueProperties are the per-kind fields of a quantity visual and
// of each of its steps.
func quantityValueProperties() map[string]interface{} {
	label := freeText()
	return map[string]interface{}{
		"fractions": describe(array(objectOf([]interface{}{"numerator", "denominator"}, map[string]interface{}{
			"numerator": describe(integer(0, MaxDenominator), ruleQuantityNumerator), "denominator": integer(1, MaxDenominator), "label": label,
		}), 1, maxQuantityGroupings), "fraction_bars: one equal-width bar per fraction."),
		"min": describe(number(), "number_line: required with max."), "max": number(),
		"ticks":  describe(map[string]interface{}{"type": "number", "exclusiveMinimum": 0}, "number_line: gap between ticks."),
		"points": array(objectOf([]interface{}{"value"}, map[string]interface{}{"value": number(), "label": label}), 0, maxNumberLinePoints),
		"ranges": array(objectOf([]interface{}{"from", "to"}, map[string]interface{}{"from": number(), "to": number(), "label": label}), 0, maxNumberLineRanges),
		"groups": describe(array(objectOf([]interface{}{"count"}, map[string]interface{}{"count": integer(0, MaxUnitBlocks), "label": label}), 1, maxQuantityGroupings),
			"unit_blocks: one row of blocks per group."),
		"parts": describe(array(objectOf([]interface{}{"value", "label"}, map[string]interface{}{
			"value": map[string]interface{}{"type": "number", "minimum": 0, "maximum": maxPercent}, "label": filledText(),
		}), 1, maxQuantityGroupings), "percent_waffle: percentages totalling at most 100 on a 10x10 grid."),
		"items": describe(array(objectOf([]interface{}{"label", "value"}, map[string]interface{}{"label": filledText(), "value": amountSchema()}), 2, maxQuantityGroupings),
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
	step["label"] = filledText()
	properties["kind"] = enum(QuantityKinds)
	properties["steps"] = describe(array(objectOf([]interface{}{"label"}, step), 0, MaxQuantitySteps),
		"Stages people step through, each with the same fields as the kind, e.g. both fractions re-cut into twelfths.")
	return objectOf([]interface{}{"kind"}, properties)
}

func amountSchema() map[string]interface{} {
	return objectOf([]interface{}{"value"}, map[string]interface{}{"value": number(), "format": enum(formatNames()), "currency": currencySchema(), "unit": freeText()})
}

func priceSchema() map[string]interface{} {
	price := amountSchema()
	price["properties"].(map[string]interface{})["per"] = enum(PricePeriods)
	return price
}

func mathSchema() map[string]interface{} {
	step := objectOf([]interface{}{"latex"}, map[string]interface{}{
		"latex": describe(strRange(1, MaxLaTeXRunes), ruleLaTeX), "note": freeText(),
	})
	return objectOf([]interface{}{"steps"}, map[string]interface{}{
		"steps": array(step, 1, maxMathSteps), "reveal": enum(MathReveals),
	})
}

func definitionSchema() map[string]interface{} {
	return objectOf([]interface{}{"term", "meaning"}, map[string]interface{}{
		"term": filledText(), "phonetic": freeText(), "partOfSpeech": freeText(),
		"meaning": filledText(), "example": freeText(), "image": imageRefSchema(),
	})
}

func flashcardsSchema() map[string]interface{} {
	card := objectOf([]interface{}{"id", "front", "back"}, map[string]interface{}{
		"id": itemIDSchema(), "front": filledText(), "back": filledText(), "image": imageRefSchema(),
	})
	return objectOf([]interface{}{"cards"}, map[string]interface{}{"cards": array(card, minFlashcards, maxFlashcards), "shuffle": boolean()})
}

func beforeAfterSchema() map[string]interface{} {
	side := beforeAfterSideSchema()
	return objectOf([]interface{}{"mode", "before", "after"}, map[string]interface{}{
		"mode":   describe(enum(BeforeAfterModes), ruleBeforeAfterModes),
		"before": side, "after": side,
		"labels": objectOf([]interface{}{}, map[string]interface{}{"before": freeText(), "after": freeText()}),
	})
}

func beforeAfterSideSchema() map[string]interface{} {
	return describe(objectOf([]interface{}{}, map[string]interface{}{"text": freeText(), "image": imageRefSchema()}), ruleBeforeAfterSides)
}

func draftSchema() map[string]interface{} {
	return objectOf([]interface{}{"channel", "body"}, map[string]interface{}{
		"channel": enum(DraftChannels), "to": describe(freeText(), "Shown only; it sends nothing."), "subject": freeText(),
		"body": describe(filledText(), "Markdown."),
	})
}

func recipeSchema() map[string]interface{} {
	minutes := integer(0, MaxRecipeMinutes)
	ingredient := objectOf([]interface{}{"id", "name"}, map[string]interface{}{
		"id": itemIDSchema(), "qty": describe(map[string]interface{}{"type": "number", "exclusiveMinimum": 0}, "Leave out for to taste."),
		"unit": freeText(), "name": filledText(), "group": freeText(),
	})
	step := objectOf([]interface{}{"id", "text"}, map[string]interface{}{"id": itemIDSchema(), "text": filledText()})
	return objectOf([]interface{}{"servings", "ingredients", "steps"}, map[string]interface{}{
		"servings": integer(1, maxServings), "time": describe(objectOf([]interface{}{}, map[string]interface{}{"prep": minutes, "cook": minutes}), "Minutes."),
		"ingredients": array(ingredient, 1, maxIngredients), "steps": array(step, 1, maxRecipeSteps), "image": imageRefSchema(),
		"checkable": boolean(),
	})
}

func itemSchema() map[string]interface{} {
	return objectOf([]interface{}{"name"}, map[string]interface{}{
		"name": filledText(), "image": imageRefSchema(), "price": priceSchema(), "rating": ratingSchema(),
		"facts": shownList(filledText(), 0, maxFacts), "badge": freeText(),
	})
}

func analogySchema() map[string]interface{} {
	side := objectOf([]interface{}{"label"}, map[string]interface{}{"label": filledText(), "icon": iconSchema()})
	pair := objectOf([]interface{}{"a", "b"}, map[string]interface{}{
		"a": describe(filledText(), "A part of from."), "b": describe(filledText(), "What it is like in to."),
	})
	return objectOf([]interface{}{"from", "to", "mapping"}, map[string]interface{}{"from": side, "to": side, "mapping": shownList(pair, 1, maxAnalogyPairs)})
}

func formSchema() map[string]interface{} {
	option := objectOf([]interface{}{"label", "value"}, map[string]interface{}{"label": filledText(), "value": filledText()})
	field := objectOf([]interface{}{"id", "type", "label"}, map[string]interface{}{
		"id": idSchema(), "type": enum(FormFieldTypes),
		"label": filledText(), "description": freeText(), "required": boolean(), "placeholder": freeText(),
		"options": array(option, 0, maxFormOptions), "min": number(), "max": number(),
	})
	return objectOf([]interface{}{"fields"}, map[string]interface{}{
		"description": freeText(), "submitLabel": freeText(),
		"fields": array(field, 1, maxFormFields),
	})
}

func tableSchema() map[string]interface{} {
	column := objectOf([]interface{}{"key", "label"}, map[string]interface{}{
		"key": itemIDSchema(), "label": filledText(),
		"kind": describe(enum(append([]string{""}, TableColumnKinds...)), "Empty or left out means text."), "currency": currencySchema(),
	})
	cell := map[string]interface{}{"anyOf": []interface{}{freeText(), number(), imageRefSchema()}}
	return objectOf([]interface{}{"columns"}, map[string]interface{}{
		"columns": array(column, 1, MaxTableColumns),
		"rows": describe(shownList(map[string]interface{}{"type": "object", "additionalProperties": cell}, 0, MaxTableRows),
			ruleTableCells),
	})
}

func calloutSchema() map[string]interface{} {
	return objectOf([]interface{}{"body"}, map[string]interface{}{"icon": iconSchema(), "body": filledText()})
}

func stepsSchema() map[string]interface{} {
	step := objectOf([]interface{}{"id", "title"}, map[string]interface{}{
		"id": itemIDSchema(), "title": filledText(), "detail": freeText(), "icon": iconSchema(), "image": imageRefSchema(),
	})
	return objectOf([]interface{}{"items"}, map[string]interface{}{
		"items": shownList(step, minSteps, maxSteps), "checkable": boolean(),
	})
}

func comparisonSchema() map[string]interface{} {
	attribute := objectOf([]interface{}{"key", "label"}, map[string]interface{}{"key": itemIDSchema(), "label": filledText()})
	price := priceSchema()
	option := objectOf([]interface{}{"id", "name", "values"}, map[string]interface{}{
		"id": itemIDSchema(), "name": filledText(), "image": imageRefSchema(), "badge": freeText(), "price": price,
		"values": describe(map[string]interface{}{"type": "object", "additionalProperties": freeText()}, ruleComparisonValues),
		"pros":   shownList(filledText(), 0, maxProsCons), "cons": shownList(filledText(), 0, maxProsCons),
	})
	return objectOf([]interface{}{"attributes", "options"}, map[string]interface{}{
		"attributes": array(attribute, 1, maxComparisonAttributes), "options": array(option, minComparisonOptions, maxComparisonOptions),
		"recommended": describe(itemIDSchema(), ruleComparisonRecommended),
		"pickable":    boolean(),
	})
}

func timelineSchema() map[string]interface{} {
	event := objectOf([]interface{}{"id", "when", "title"}, map[string]interface{}{
		"id": itemIDSchema(), "when": describe(filledText(), "Label shown, e.g. 1789, Mon 3 Nov or Week 2."),
		"date":  describe(map[string]interface{}{"type": "string", "pattern": isoDate.String()}, "YYYY-MM-DD, used only to order events."),
		"title": filledText(), "detail": freeText(), "icon": iconSchema(), "image": imageRefSchema(),
	})
	return objectOf([]interface{}{"events"}, map[string]interface{}{"events": shownList(event, minTimelineEvents, maxTimelineEvents)})
}

func imageBlockSchema() map[string]interface{} {
	return objectOf([]interface{}{"image"}, map[string]interface{}{"image": imageRefSchema(), "caption": freeText(), "aspect": enum(ImageAspects)})
}

func gallerySchema() map[string]interface{} {
	image := objectOf([]interface{}{"image"}, map[string]interface{}{"image": imageRefSchema(), "caption": freeText()})
	return objectOf([]interface{}{"images"}, map[string]interface{}{"images": shownList(image, minGalleryImages, maxGalleryImages), "layout": enum(GalleryLayouts)})
}

func choicesSchema() map[string]interface{} {
	chip := objectOf([]interface{}{"id", "label"}, map[string]interface{}{"id": itemIDSchema(), "label": filledText()})
	return objectOf([]interface{}{"chips"}, map[string]interface{}{"chips": array(chip, minChips, maxChips)})
}

func checklistSchema() map[string]interface{} {
	item := objectOf([]interface{}{"id", "label"}, map[string]interface{}{
		"id": itemIDSchema(), "label": filledText(), "group": freeText(),
	})
	return objectOf([]interface{}{"items"}, map[string]interface{}{"items": array(item, 1, maxChecklistItems)})
}

func quizSchema() map[string]interface{} {
	option := objectOf([]interface{}{"id", "label"}, map[string]interface{}{"id": itemIDSchema(), "label": filledText(), "image": imageRefSchema()})
	key := objectOf([]interface{}{"correct"}, map[string]interface{}{
		"correct":     describe(array(itemIDSchema(), 1, maxQuizOptions), ruleQuizCorrect),
		"explanation": freeText(),
	})
	return objectOf([]interface{}{"question", "options", "answerKey"}, map[string]interface{}{
		"question": filledText(), "options": array(option, minQuizOptions, maxQuizOptions), "allowMultiple": boolean(), "answerKey": key,
	})
}

func imageRefSchema() map[string]interface{} {
	return describe(objectOf([]interface{}{"artifactId", "version"}, map[string]interface{}{
		"artifactId": idSchema(), "version": map[string]interface{}{"type": "integer", "minimum": 1},
	}), ruleImagesInChat)
}

func idSchema() map[string]interface{} {
	return map[string]interface{}{"type": "string", "pattern": opaqueIDPattern}
}

func itemIDSchema() map[string]interface{} {
	return map[string]interface{}{"type": "string", "pattern": itemIDPattern}
}

func iconSchema() map[string]interface{} {
	return map[string]interface{}{"type": "string", "maxLength": maxIconRunes, "pattern": iconPattern.String()}
}

func currencySchema() map[string]interface{} {
	return describe(map[string]interface{}{"type": "string", "pattern": currencyPattern}, "ISO 4217 code such as GBP, USD or EUR; required with format currency and ignored otherwise.")
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
	stat := objectOf([]interface{}{"label", "value"}, map[string]interface{}{
		"label":    filledText(),
		"value":    describe(number(), "The raw number; the app formats it for the reader's language and region."),
		"format":   describe(enum(formatNames()), "number (default); currency (set currency); percent (12 means 12%); duration (value in seconds)."),
		"currency": currencySchema(),
		"unit":     describe(freeText(), "Shown after the value, e.g. GB/mo or per month."), "delta": freeText(),
		"trend": enum(Trends), "goodDirection": enum(GoodDirections),
		"sparkline": shownList(number(), 0, maxSparkline),
	})
	return objectOf([]interface{}{"stats"}, map[string]interface{}{
		"stats": shownList(stat, 1, maxStats),
	})
}

func cardsSchema() map[string]interface{} {
	chip := objectOf([]interface{}{"text"}, map[string]interface{}{
		"text": strRange(1, maxCardChipRunes), "tone": enum(toneNames()),
	})
	card := objectOf([]interface{}{"title"}, map[string]interface{}{
		"title": strRange(1, maxCardTitleRunes), "image": imageRefSchema(),
		"value": describe(str(maxCardValueRunes), "The one headline value, as shown, e.g. 14°C."),
		"line":  str(maxCardLineRunes), "detail": str(maxCardLineRunes), "chip": chip,
	})
	return objectOf([]interface{}{"items"}, map[string]interface{}{"items": array(card, 1, maxCards)})
}

func converterSchema() map[string]interface{} {
	positive := map[string]interface{}{"type": "number", "exclusiveMinimum": 0}
	unit := objectOf([]interface{}{"code", "label"}, map[string]interface{}{
		"code":  describe(strRange(1, maxConverterCode), "ISO 4217 for a currency, else a unit such as km."),
		"label": strRange(1, maxConverterLabel), "symbol": str(maxConverterSymbol),
	})
	return objectOf([]interface{}{"from", "to", "rate"}, map[string]interface{}{
		"from": unit, "to": unit,
		"rate":   describe(positive, "1 from is rate to."),
		"amount": describe(map[string]interface{}{"type": "number", "exclusiveMinimum": 0}, "Default 1."),
		"asOf":   map[string]interface{}{"type": "string", "pattern": isoDate.String()},
		"note":   str(maxConverterNote),
	})
}

func sourceSchema() map[string]interface{} {
	return objectOf([]interface{}{"url", "title"}, map[string]interface{}{
		"url":   map[string]interface{}{"type": "string", "maxLength": MaxLinkBytes, "pattern": sourceURLPattern},
		"title": strRange(1, maxSourceTitleRunes),
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
	kind := describe(enum(kinds), ruleActionKinds)
	action := objectOf([]interface{}{"id", "label", "kind"}, map[string]interface{}{
		"id": idSchema(), "label": strRange(1, maxActionLabelRunes), "kind": kind, "icon": iconSchema(),
		"value": str(maxActionValueRunes), "target": idSchema(),
		"decision": enum([]string{"approve", "reject"}), "command": enum(commands), "binding": bindingSchema(BindingKinds),
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

// objectOf is an object of a visual. Keys it does not list are ignored, so
// a stray or misplaced key never costs the visual.
func objectOf(required []interface{}, properties map[string]interface{}) map[string]interface{} {
	schema := map[string]interface{}{"type": "object", "properties": properties}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

// closedObject is an object of a tool's own input or output, which admits
// only the keys it lists.
func closedObject(required []interface{}, properties map[string]interface{}) map[string]interface{} {
	schema := objectOf(required, properties)
	schema["additionalProperties"] = false
	return schema
}

// freeText is display text of any length; the app clamps long text when it
// draws it, and the visual's byte limit bounds the whole.
func freeText() map[string]interface{} {
	return map[string]interface{}{"type": "string"}
}

// filledText is display text of any length that is not blank: empty would
// leave nothing to show.
func filledText() map[string]interface{} {
	return strRange(1, 0)
}

func str(maximum int) map[string]interface{} {
	if maximum == 0 {
		return freeText()
	}
	return map[string]interface{}{"type": "string", "maxLength": maximum}
}

// strRange is text of minimum to maximum characters; with a minimum, it
// must not be blank either.
func strRange(minimum, maximum int) map[string]interface{} {
	schema := str(maximum)
	if minimum > 0 {
		schema["minLength"] = minimum
		schema["pattern"] = notBlankPattern
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

// describe adds description to schema's own.
func describe(schema map[string]interface{}, description string) map[string]interface{} {
	if own, ok := schema["description"].(string); ok && own != "" {
		description = own + " " + description
	}
	schema["description"] = description
	return schema
}

// shownList is a list of which the app shows only the first shown items.
// More is not an error: the rest are kept but not drawn.
func shownList(items map[string]interface{}, minimum, shown int) map[string]interface{} {
	schema := map[string]interface{}{"type": "array", "items": items}
	if minimum > 0 {
		schema["minItems"] = minimum
	}
	return describe(schema, fmt.Sprintf("Only the first %d are shown.", shown))
}

// shownItems is the part of a list the app shows: its first shown items.
func shownItems[T any](items []T, shown int) []T {
	return items[:min(len(items), shown)]
}
