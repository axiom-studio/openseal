package presentation

import (
	"fmt"
	"sort"
	"strings"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// rubric is the §6.1 "when to use what" guidance. A line is included only
// when every type it names is in the catalogue, so the rubric grows as types
// land and never points the model at a type Validate rejects.
var rubric = []struct {
	line  string
	types []string
}{
	{"Choosing between things → %s.", []string{"comparison"}},
	{"A group decision in a shared chat → %s.", []string{"poll"}},
	{"How to do something → %s.", []string{"steps"}},
	{"Parts of something → %s.", []string{"labeled_image"}},
	{"A process, cycle or hierarchy → %s.", []string{"diagram"}},
	{"Things over time → %s.", []string{"timeline", "agenda", "itinerary"}},
	{"Headline numbers → %s.", []string{"stat_row"}},
	{"A trend or distribution → %s.", []string{"chart"}},
	{"Exact values → %s.", []string{"table"}},
	{"Parts of a whole → %s.", []string{"breakdown"}},
	{"Maths for learners → %s.", []string{"quantity_visual", "math"}},
	{"Learning → finish with %s.", []string{"quiz", "flashcards"}},
	{"Lists the user will act on → %s.", []string{"checklist"}},
	{"\"What if\" questions → %s.", []string{"explorer"}},
	{"Information you need from the user → %s.", []string{"form"}},
}

// VisualRubric returns the "when to show what" guidance for the current
// catalogue. Hosts put it on the description of the answer's visual field.
func VisualRubric() string {
	var b strings.Builder
	b.WriteString("Most replies are just text. Add a visual only when it makes the answer clearer or easier to act on than words would. " +
		"Then lead with one or two sentences, show the visual, and add at most one short line.\n")
	b.WriteString("When the content has shape:\n")
	for _, entry := range rubric {
		available := true
		names := make([]string, len(entry.types))
		for index, name := range entry.types {
			_, available = catalogue[name]
			if !available {
				break
			}
			names[index] = "`" + name + "`"
		}
		if available {
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

// PlotChartInput is the plot_chart action input.
type PlotChartInput struct {
	Title   string       `json:"title"`
	Summary string       `json:"summary"`
	Kind    string       `json:"kind"`
	Points  []ChartPoint `json:"points"`
	XLabel  string       `json:"xLabel,omitempty"`
	YLabel  string       `json:"yLabel,omitempty"`
}

// Surface lowers a plot_chart input into a one-chart surface.
func (p PlotChartInput) Surface() Surface {
	title := strings.TrimSpace(p.Title)
	return Surface{Schema: Schema, Title: title, State: StateReady, Components: []Component{{
		ID: "chart", Type: "chart", Summary: strings.TrimSpace(p.Summary),
		Chart: &Chart{Kind: p.Kind, XLabel: p.XLabel, YLabel: p.YLabel, Series: []ChartSeries{{Name: title, Points: p.Points}}},
	}}}
}

func SkillDefinition() *skill.Definition {
	artifactRefs := map[string]interface{}{"type": "array", "minItems": 1, "maxItems": 1, "items": closedObject([]interface{}{"id", "version", "requirementName"}, map[string]interface{}{
		"id": str(0), "version": map[string]interface{}{"type": "integer", "minimum": 1}, "requirementName": str(0),
	})}
	return &skill.Definition{
		ID: SkillID, Version: SkillVersion, Name: "Visuals",
		Description: "Replace a visual you already showed with a new version, e.g. while work is running, or plot one quick chart. New visuals belong on your answer.",
		Icon:        "layout-dashboard", Category: "documents", Tags: []string{"interactive", "chart", "diagram", "form", "artifact"},
		Transport: skill.TransportReference{Kind: "tool", Endpoint: SkillID},
		Actions: map[string]skill.Action{PlotChart: {
			Name: PlotChart, Description: "Plot one chart from labeled numeric values in one call. Use this for an ordinary graph.",
			InputSchema: closedObject([]interface{}{"title", "summary", "kind", "points"}, map[string]interface{}{
				"title":   strRange(1, MaxTitleRunes),
				"summary": describe(strRange(1, MaxSummaryRunes), "One plain sentence saying what the chart shows."),
				"kind":    enum(ChartKinds),
				"points": map[string]interface{}{"type": "array", "minItems": 1, "maxItems": 200, "items": closedObject([]interface{}{"label", "value"}, map[string]interface{}{
					"label": strRange(1, 160), "value": number(),
				})},
				"xLabel": str(120), "yLabel": str(120),
			}),
			OutputSchema: closedObject([]interface{}{"artifactRefs", "state"}, map[string]interface{}{
				"artifactRefs": artifactRefs, "state": enum([]string{string(StateReady)}),
			}),
			SideEffect: skill.SideEffectWrite, Risk: skill.RiskLevelWrite, Idempotency: skill.IdempotencyRequired,
			Retry: skill.ActionRetryPolicy{MaxAttempts: 1}, EmittedArtifactTypes: []string{ArtifactType},
		}, Publish: {
			Name: Publish, Description: "Replace a visual you already showed with a new version, e.g. to show progress while work is running.",
			InputSchema: PublishInputSchema(),
			OutputSchema: closedObject([]interface{}{"artifactRefs", "state"}, map[string]interface{}{
				"artifactRefs": artifactRefs, "state": enum([]string{string(StateStreaming), string(StateReady)}),
			}),
			SideEffect: skill.SideEffectWrite, Risk: skill.RiskLevelWrite, Idempotency: skill.IdempotencyRequired,
			Retry: skill.ActionRetryPolicy{MaxAttempts: 1}, EmittedArtifactTypes: []string{ArtifactType},
		}},
		Prompt: &capability.PromptModule{
			Instructions: "Visuals normally go on your answer. Use publish_surface only to replace a visual you already showed with a new version while work is running, and plot_chart for one quick chart. " +
				"Give every chart, diagram, form, table and stat_row a one-sentence summary. Never write HTML, scripts, colours or URLs.",
			UserInvocable: true, AllowedTools: []string{PlotChart, Publish},
		},
		Requirements: capability.Requirements{AlwaysAvailable: true},
	}
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

// PublishInputSchema is the publish_surface input schema.
func PublishInputSchema() map[string]interface{} {
	return closedObject([]interface{}{"surfaceId", "expectedLatestVersion", "title", "state", "components", "requirementName"}, map[string]interface{}{
		"surfaceId":             describe(map[string]interface{}{"type": "string", "pattern": opaqueIDPattern}, "Id of the visual to replace."),
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
		"name": strRange(1, 120), "points": map[string]interface{}{"type": "array", "minItems": 1, "maxItems": 200, "items": point},
	})
	return closedObject([]interface{}{"kind", "series"}, map[string]interface{}{
		"kind": enum(ChartKinds), "xLabel": str(120), "yLabel": str(120),
		"series": map[string]interface{}{"type": "array", "minItems": 1, "maxItems": 12, "items": series},
	})
}

func diagramSchema() map[string]interface{} {
	id := map[string]interface{}{"type": "string", "pattern": opaqueIDPattern}
	node := closedObject([]interface{}{"id", "label", "x", "y"}, map[string]interface{}{"id": id, "label": strRange(1, 240), "x": number(), "y": number(), "kind": str(80)})
	edge := closedObject([]interface{}{"from", "to"}, map[string]interface{}{"from": id, "to": id, "label": str(240)})
	return closedObject([]interface{}{"nodes"}, map[string]interface{}{
		"nodes": map[string]interface{}{"type": "array", "minItems": 1, "maxItems": 100, "items": node},
		"edges": map[string]interface{}{"type": "array", "maxItems": 200, "items": edge},
	})
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
	return closedObject([]interface{}{"columns", "rows"}, map[string]interface{}{
		"columns": map[string]interface{}{"type": "array", "minItems": 1, "maxItems": 30, "items": strRange(1, 240)},
		"rows":    map[string]interface{}{"type": "array", "maxItems": 1000, "items": map[string]interface{}{"type": "array", "maxItems": 30, "items": str(2000)}},
	})
}

func statRowSchema() map[string]interface{} {
	stat := closedObject([]interface{}{"label", "value"}, map[string]interface{}{
		"label": strRange(1, 60), "value": strRange(1, 40), "unit": str(24), "delta": str(40),
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
	icon := map[string]interface{}{"type": "string", "maxLength": 40, "pattern": iconPattern.String()}
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
	return map[string]interface{}{"type": "object", "additionalProperties": false, "required": required, "properties": properties}
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
