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
				"Give each visual component a one-sentence summary for screen readers and voice. Never write HTML, scripts, colours, URLs or coordinates; images must already be in this chat and places come from find_place.",
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
	delete(envelope, "children")
	envelope["type"] = describe(map[string]interface{}{"type": "string"}, "one of the types below")
	b.WriteString("Component = " + notation.render(objectOf([]interface{}{"id", "type"}, envelope)) +
		" plus only the payload whose key equals type (text, caption and divider use text); payloads of other types are ignored. " +
		"A component whose type is missing or not one of the types below is left out.\n")
	var layouts []string
	for _, name := range TypeNames() {
		if IsLayout(name) {
			layouts = append(layouts, name)
		}
	}
	fmt.Fprintf(&b, "Components nest at most %d levels deep (top level is 1, so no %s at level %d); at most %d components in all including nested ones; at most %d KB of JSON.\n",
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
		"title":                 filledText(),
		"state":                 enum([]string{string(StateStreaming), string(StateReady)}),
		"components": describe(array(looseComponentSchema(), 1, MaxComponents),
			"Components in exactly the format described on your answer's visual field."),
		"requirementName": strRange(1, 128),
	})
}

func visualSchema(component map[string]interface{}, description string) map[string]interface{} {
	components := array(component, 1, MaxComponents)
	if description != "" {
		components = describe(components, description)
	}
	return objectOf([]interface{}{"title", "components"}, map[string]interface{}{"title": filledText(), "components": components})
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
		"tasks": tasksSchema(), "agent": agentCardSchema(), "map": mapSchema(),
		"grid": objectOf([]interface{}{"columns"}, map[string]interface{}{"columns": integer(minGridColumns, maxGridColumns)}),
		"tabs": array(objectOf([]interface{}{"label", "children"}, map[string]interface{}{
			"label": filledText(), "children": childrenSchema(componentRef(), 1, maxChildren),
		}), minTabs, maxTabs),
	}
}

// looseComponentSchema lists every envelope and payload key with payloads
// and nested components left open. Where one key means different things
// per type (summary, text), its description gives each type's rule.
func looseComponentSchema() map[string]interface{} {
	open := map[string]interface{}{"type": "object"}
	properties := envelopeProperties(open)
	properties["type"] = describe(properties["type"].(map[string]interface{}), "Set only the payload whose key equals type (text, caption and divider use text).")
	properties["text"] = textPayloadSchema()
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
	return objectOf([]interface{}{"id", "type"}, properties)
}

// textPayloadSchema is the loose schema of the shared text key, with the
// types that may leave it out in the description.
func textPayloadSchema() map[string]interface{} {
	var optional []string
	for _, name := range TypeNames() {
		spec := catalogue[name]
		if spec.payload == "text" && spec.payloadOptional {
			optional = append(optional, name)
		}
	}
	return describe(freeText(), "Not blank, except that "+strings.Join(optional, ", ")+" may leave it out.")
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
