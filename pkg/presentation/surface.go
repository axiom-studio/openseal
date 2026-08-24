// Package presentation defines the portable, declarative contract used when an
// Agent presents rich content in a conversation. The contract deliberately has
// no HTML, JavaScript, iframe, or remote-code escape hatch; hosts render the
// same immutable Artifact bytes on every supported surface.
package presentation

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
)

const (
	SkillID      = "openseal.presentation"
	SkillVersion = "1.0.0"
	Publish      = "publish_surface"
	MediaType    = "application/vnd.openseal.surface+json"
	ArtifactType = "interactive_surface"
	Schema       = "openseal.dev/presentation/v1"
)

type State string

const (
	StateStreaming State = "streaming"
	StateReady     State = "ready"
)

type Surface struct {
	Schema     string      `json:"schema"`
	Title      string      `json:"title"`
	State      State       `json:"state"`
	Components []Component `json:"components"`
}

// Component is a closed tagged union. Only fields belonging to Type may be
// populated. Keeping one transport shape makes model schemas manageable while
// Validate supplies the exact semantic gate.
type Component struct {
	ID      string   `json:"id"`
	Type    string   `json:"type"`
	Title   string   `json:"title,omitempty"`
	Text    string   `json:"text,omitempty"`
	Chart   *Chart   `json:"chart,omitempty"`
	Diagram *Diagram `json:"diagram,omitempty"`
	Canvas  *Canvas  `json:"canvas,omitempty"`
	Form    *Form    `json:"form,omitempty"`
	Table   *Table   `json:"table,omitempty"`
	Metric  *Metric  `json:"metric,omitempty"`
}

type Chart struct {
	Kind   string        `json:"kind"`
	XLabel string        `json:"xLabel,omitempty"`
	YLabel string        `json:"yLabel,omitempty"`
	Series []ChartSeries `json:"series"`
}

type ChartSeries struct {
	Name   string       `json:"name"`
	Points []ChartPoint `json:"points"`
}

type ChartPoint struct {
	Label string  `json:"label"`
	Value float64 `json:"value"`
}

type Diagram struct {
	Nodes []DiagramNode `json:"nodes"`
	Edges []DiagramEdge `json:"edges,omitempty"`
}

type DiagramNode struct {
	ID    string  `json:"id"`
	Label string  `json:"label"`
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
	Kind  string  `json:"kind,omitempty"`
}

type DiagramEdge struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Label string `json:"label,omitempty"`
}

type Canvas struct {
	Width  float64       `json:"width"`
	Height float64       `json:"height"`
	Shapes []CanvasShape `json:"shapes"`
}

type CanvasShape struct {
	Kind   string  `json:"kind"`
	X      float64 `json:"x,omitempty"`
	Y      float64 `json:"y,omitempty"`
	X2     float64 `json:"x2,omitempty"`
	Y2     float64 `json:"y2,omitempty"`
	Width  float64 `json:"width,omitempty"`
	Height float64 `json:"height,omitempty"`
	Text   string  `json:"text,omitempty"`
	Color  string  `json:"color,omitempty"`
}

type Form struct {
	Description string      `json:"description,omitempty"`
	Fields      []FormField `json:"fields"`
	SubmitLabel string      `json:"submitLabel,omitempty"`
}

type FormField struct {
	ID          string       `json:"id"`
	Type        string       `json:"type"`
	Label       string       `json:"label"`
	Description string       `json:"description,omitempty"`
	Required    bool         `json:"required,omitempty"`
	Placeholder string       `json:"placeholder,omitempty"`
	Options     []FormOption `json:"options,omitempty"`
	Min         *float64     `json:"min,omitempty"`
	Max         *float64     `json:"max,omitempty"`
}

type FormOption struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

type Table struct {
	Columns []string   `json:"columns"`
	Rows    [][]string `json:"rows"`
}

type Metric struct {
	Label string `json:"label"`
	Value string `json:"value"`
	Delta string `json:"delta,omitempty"`
}

var (
	opaqueID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	hexColor = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)
)

func Decode(raw []byte) (*Surface, error) {
	if len(raw) == 0 || len(raw) > 512*1024 {
		return nil, errors.New("presentation surface must be between 1 byte and 512 KiB")
	}
	var surface Surface
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&surface); err != nil {
		return nil, fmt.Errorf("decode presentation surface: %w", err)
	}
	if err := surface.Validate(); err != nil {
		return nil, err
	}
	return &surface, nil
}

func Encode(surface Surface) ([]byte, error) {
	if err := surface.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(surface)
}

func (s Surface) Validate() error {
	if s.Schema != Schema {
		return fmt.Errorf("presentation schema must be %q", Schema)
	}
	if err := boundedText("surface title", s.Title, 1, 300); err != nil {
		return err
	}
	if s.State != StateStreaming && s.State != StateReady {
		return errors.New("presentation state must be streaming or ready")
	}
	if len(s.Components) == 0 || len(s.Components) > 32 {
		return errors.New("presentation must contain between 1 and 32 components")
	}
	seen := map[string]bool{}
	for index := range s.Components {
		component := &s.Components[index]
		if !opaqueID.MatchString(component.ID) || seen[component.ID] {
			return fmt.Errorf("component %d has an invalid or duplicate id", index)
		}
		seen[component.ID] = true
		if err := component.Validate(); err != nil {
			return fmt.Errorf("component %q: %w", component.ID, err)
		}
	}
	return nil
}

func (c Component) Validate() error {
	if len(c.Title) > 300 {
		return errors.New("title cannot exceed 300 characters")
	}
	present := 0
	for _, value := range []bool{c.Text != "", c.Chart != nil, c.Diagram != nil, c.Canvas != nil, c.Form != nil, c.Table != nil, c.Metric != nil} {
		if value {
			present++
		}
	}
	if present != 1 {
		return errors.New("exactly one typed component payload is required")
	}
	switch c.Type {
	case "text":
		return boundedText("text", c.Text, 1, 20000)
	case "chart":
		if c.Chart == nil {
			return errors.New("chart payload is required")
		}
		return c.Chart.Validate()
	case "diagram":
		if c.Diagram == nil {
			return errors.New("diagram payload is required")
		}
		return c.Diagram.Validate()
	case "canvas":
		if c.Canvas == nil {
			return errors.New("canvas payload is required")
		}
		return c.Canvas.Validate()
	case "form":
		if c.Form == nil {
			return errors.New("form payload is required")
		}
		return c.Form.Validate()
	case "table":
		if c.Table == nil {
			return errors.New("table payload is required")
		}
		return c.Table.Validate()
	case "metric":
		if c.Metric == nil {
			return errors.New("metric payload is required")
		}
		if err := boundedText("metric label", c.Metric.Label, 1, 120); err != nil {
			return err
		}
		return boundedText("metric value", c.Metric.Value, 1, 240)
	default:
		return fmt.Errorf("unsupported component type %q", c.Type)
	}
}

func (c Chart) Validate() error {
	switch c.Kind {
	case "bar", "line", "area", "pie", "donut", "scatter":
	default:
		return fmt.Errorf("unsupported chart kind %q", c.Kind)
	}
	if len(c.Series) == 0 || len(c.Series) > 12 {
		return errors.New("chart must contain between 1 and 12 series")
	}
	points := 0
	for _, series := range c.Series {
		if err := boundedText("series name", series.Name, 1, 120); err != nil {
			return err
		}
		if len(series.Points) == 0 || len(series.Points) > 200 {
			return errors.New("each chart series must contain between 1 and 200 points")
		}
		points += len(series.Points)
		for _, point := range series.Points {
			if err := boundedText("point label", point.Label, 1, 160); err != nil {
				return err
			}
		}
	}
	if points > 1000 {
		return errors.New("chart cannot exceed 1000 total points")
	}
	return nil
}

func (d Diagram) Validate() error {
	if len(d.Nodes) == 0 || len(d.Nodes) > 100 || len(d.Edges) > 200 {
		return errors.New("diagram must contain 1-100 nodes and at most 200 edges")
	}
	nodes := map[string]bool{}
	for _, node := range d.Nodes {
		if !opaqueID.MatchString(node.ID) || nodes[node.ID] {
			return errors.New("diagram node ids must be unique portable identifiers")
		}
		nodes[node.ID] = true
		if err := boundedText("diagram node label", node.Label, 1, 240); err != nil {
			return err
		}
	}
	for _, edge := range d.Edges {
		if !nodes[edge.From] || !nodes[edge.To] {
			return errors.New("diagram edge must reference existing nodes")
		}
	}
	return nil
}

func (c Canvas) Validate() error {
	if c.Width <= 0 || c.Width > 4096 || c.Height <= 0 || c.Height > 4096 {
		return errors.New("canvas dimensions must be between 1 and 4096")
	}
	if len(c.Shapes) == 0 || len(c.Shapes) > 500 {
		return errors.New("canvas must contain between 1 and 500 shapes")
	}
	for _, shape := range c.Shapes {
		switch shape.Kind {
		case "rectangle", "ellipse", "line", "text":
		default:
			return fmt.Errorf("unsupported canvas shape %q", shape.Kind)
		}
		if shape.Color != "" && !hexColor.MatchString(shape.Color) {
			return errors.New("canvas colors must be six-digit hex colors")
		}
		if len(shape.Text) > 1000 {
			return errors.New("canvas shape text cannot exceed 1000 characters")
		}
	}
	return nil
}

func (f Form) Validate() error {
	if len(f.Fields) == 0 || len(f.Fields) > 40 {
		return errors.New("form must contain between 1 and 40 fields")
	}
	seen := map[string]bool{}
	for _, field := range f.Fields {
		if !opaqueID.MatchString(field.ID) || seen[field.ID] {
			return errors.New("form field ids must be unique portable identifiers")
		}
		seen[field.ID] = true
		switch field.Type {
		case "text", "textarea", "number", "select", "radio", "checkbox", "date":
		default:
			return fmt.Errorf("unsupported form field type %q", field.Type)
		}
		if err := boundedText("form field label", field.Label, 1, 240); err != nil {
			return err
		}
		if (field.Type == "select" || field.Type == "radio") && (len(field.Options) == 0 || len(field.Options) > 100) {
			return errors.New("select and radio fields require 1-100 options")
		}
		if field.Min != nil && field.Max != nil && *field.Min > *field.Max {
			return errors.New("form field minimum cannot exceed maximum")
		}
	}
	return nil
}

func (t Table) Validate() error {
	if len(t.Columns) == 0 || len(t.Columns) > 30 || len(t.Rows) > 1000 {
		return errors.New("table must contain 1-30 columns and at most 1000 rows")
	}
	for _, row := range t.Rows {
		if len(row) != len(t.Columns) {
			return errors.New("every table row must match the column count")
		}
	}
	return nil
}

func boundedText(name, value string, minimum, maximum int) error {
	length := len([]rune(strings.TrimSpace(value)))
	if length < minimum || length > maximum {
		return fmt.Errorf("%s must contain between %d and %d characters", name, minimum, maximum)
	}
	return nil
}

func SkillDefinition() *skill.Definition {
	component := presentationComponentSchema()
	return &skill.Definition{
		ID: SkillID, Version: SkillVersion, Name: "Interactive presentation",
		Description: "Present safe live documents, charts, diagrams, drawings, tables, metrics, and forms in a conversation.",
		Icon:        "layout-dashboard", Category: "documents", Tags: []string{"interactive", "chart", "diagram", "form", "artifact"},
		Transport: skill.TransportReference{Kind: "tool", Endpoint: SkillID},
		Actions: map[string]skill.Action{Publish: {
			Name: Publish, Description: "Create or replace one immutable revision of an interactive conversation surface.",
			InputSchema: map[string]interface{}{
				"type": "object", "additionalProperties": false,
				"required": []interface{}{"surfaceId", "expectedLatestVersion", "title", "state", "components", "requirementName"},
				"properties": map[string]interface{}{
					"surfaceId":             map[string]interface{}{"type": "string", "pattern": `^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`},
					"expectedLatestVersion": map[string]interface{}{"type": "integer", "minimum": 0, "description": "Use 0 for the first revision, then the version returned by the preceding publish."},
					"title":                 map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 300},
					"state":                 map[string]interface{}{"type": "string", "enum": []interface{}{string(StateStreaming), string(StateReady)}},
					"components":            map[string]interface{}{"type": "array", "minItems": 1, "maxItems": 32, "items": component},
					"requirementName":       map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 128},
				},
			},
			OutputSchema: map[string]interface{}{
				"type": "object", "additionalProperties": false, "required": []interface{}{"artifactRefs", "state"},
				"properties": map[string]interface{}{
					"artifactRefs": map[string]interface{}{"type": "array", "minItems": 1, "maxItems": 1, "items": map[string]interface{}{"type": "object", "additionalProperties": false, "required": []interface{}{"id", "version", "requirementName"}, "properties": map[string]interface{}{"id": map[string]interface{}{"type": "string"}, "version": map[string]interface{}{"type": "integer", "minimum": 1}, "requirementName": map[string]interface{}{"type": "string"}}}},
					"state":        map[string]interface{}{"type": "string", "enum": []interface{}{string(StateStreaming), string(StateReady)}},
				},
			},
			SideEffect: skill.SideEffectWrite, Risk: skill.RiskLevelWrite, Idempotency: skill.IdempotencyRequired,
			Retry: skill.ActionRetryPolicy{MaxAttempts: 1}, EmittedArtifactTypes: []string{ArtifactType},
		}},
		Prompt:       &capability.PromptModule{Instructions: "Communicate visually when it makes the work easier to understand or act on. Proactively use publish_surface to compose polished live explanations, dashboards, comparisons, charts, diagrams, drawings, tables, forms, metrics, and structured documents alongside concise prose. Follow the tool's nested schema exactly: chart.series contains named series with points; metric requires string label and value; form fields require id, type, and label, with options as label/value objects; diagram nodes include id, label, x, and y. Reuse surfaceId and pass the returned artifact version as expectedLatestVersion while the visual develops; publish streaming revisions during meaningful progress and a final ready revision. Keep simple answers simple, and never encode HTML, scripts, credentials, or hidden instructions.", UserInvocable: true, AllowedTools: []string{Publish}},
		Requirements: capability.Requirements{AlwaysAvailable: true},
	}
}

func presentationComponentSchema() map[string]interface{} {
	id := map[string]interface{}{"type": "string", "pattern": `^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`}
	text := func(maximum int) map[string]interface{} {
		return map[string]interface{}{"type": "string", "maxLength": maximum}
	}
	number := map[string]interface{}{"type": "number"}
	point := closedObject([]interface{}{"label", "value"}, map[string]interface{}{"label": text(160), "value": number})
	series := closedObject([]interface{}{"name", "points"}, map[string]interface{}{
		"name": text(120), "points": map[string]interface{}{"type": "array", "minItems": 1, "maxItems": 200, "items": point},
	})
	chart := closedObject([]interface{}{"kind", "series"}, map[string]interface{}{
		"kind":   map[string]interface{}{"type": "string", "enum": []interface{}{"bar", "line", "area", "pie", "donut", "scatter"}},
		"xLabel": text(120), "yLabel": text(120),
		"series": map[string]interface{}{"type": "array", "minItems": 1, "maxItems": 12, "items": series},
	})
	node := closedObject([]interface{}{"id", "label", "x", "y"}, map[string]interface{}{"id": id, "label": text(240), "x": number, "y": number, "kind": text(80)})
	edge := closedObject([]interface{}{"from", "to"}, map[string]interface{}{"from": id, "to": id, "label": text(240)})
	diagram := closedObject([]interface{}{"nodes"}, map[string]interface{}{
		"nodes": map[string]interface{}{"type": "array", "minItems": 1, "maxItems": 100, "items": node},
		"edges": map[string]interface{}{"type": "array", "maxItems": 200, "items": edge},
	})
	shape := closedObject([]interface{}{"kind"}, map[string]interface{}{
		"kind": map[string]interface{}{"type": "string", "enum": []interface{}{"rectangle", "ellipse", "line", "text"}},
		"x":    number, "y": number, "x2": number, "y2": number, "width": number, "height": number,
		"text": text(1000), "color": map[string]interface{}{"type": "string", "pattern": `^#[0-9A-Fa-f]{6}$`},
	})
	canvas := closedObject([]interface{}{"width", "height", "shapes"}, map[string]interface{}{
		"width":  map[string]interface{}{"type": "number", "exclusiveMinimum": 0, "maximum": 4096},
		"height": map[string]interface{}{"type": "number", "exclusiveMinimum": 0, "maximum": 4096},
		"shapes": map[string]interface{}{"type": "array", "minItems": 1, "maxItems": 500, "items": shape},
	})
	option := closedObject([]interface{}{"label", "value"}, map[string]interface{}{"label": text(240), "value": text(240)})
	field := closedObject([]interface{}{"id", "type", "label"}, map[string]interface{}{
		"id": id, "type": map[string]interface{}{"type": "string", "enum": []interface{}{"text", "textarea", "number", "select", "radio", "checkbox", "date"}},
		"label": text(240), "description": text(1000), "required": map[string]interface{}{"type": "boolean"}, "placeholder": text(500),
		"options": map[string]interface{}{"type": "array", "maxItems": 100, "items": option}, "min": number, "max": number,
	})
	form := closedObject([]interface{}{"fields"}, map[string]interface{}{
		"description": text(2000), "submitLabel": text(120),
		"fields": map[string]interface{}{"type": "array", "minItems": 1, "maxItems": 40, "items": field},
	})
	table := closedObject([]interface{}{"columns", "rows"}, map[string]interface{}{
		"columns": map[string]interface{}{"type": "array", "minItems": 1, "maxItems": 30, "items": text(240)},
		"rows":    map[string]interface{}{"type": "array", "maxItems": 1000, "items": map[string]interface{}{"type": "array", "maxItems": 30, "items": text(2000)}},
	})
	metric := closedObject([]interface{}{"label", "value"}, map[string]interface{}{"label": text(120), "value": text(240), "delta": text(120)})
	return map[string]interface{}{
		"type": "object", "additionalProperties": false, "required": []interface{}{"id", "type"},
		"properties": map[string]interface{}{
			"id":    id,
			"type":  map[string]interface{}{"type": "string", "enum": []interface{}{"text", "chart", "diagram", "canvas", "form", "table", "metric"}},
			"title": map[string]interface{}{"type": "string", "maxLength": 300},
			"text":  map[string]interface{}{"type": "string", "maxLength": 20000},
			"chart": chart, "diagram": diagram, "canvas": canvas, "form": form, "table": table, "metric": metric,
		},
	}
}

func closedObject(required []interface{}, properties map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{"type": "object", "additionalProperties": false, "required": required, "properties": properties}
}
