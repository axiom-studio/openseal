package presentation

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

type summaryRule int

const (
	summaryOptional summaryRule = iota
	summaryRequired
)

// typeSpec describes one member of the closed component union.
type typeSpec struct {
	// payload is the JSON key of the type's payload; empty when it has none.
	payload string
	// children reports whether Component.Children is allowed.
	children bool
	summary  summaryRule
	validate func(v *validator, c Component, path string) error
	// sentence and example feed the model schema.
	sentence string
	example  string
}

var catalogue = map[string]typeSpec{
	"text": {payload: "text", validate: validateText, sentence: "Short markdown block (GFM, no HTML).",
		example: `{"id":"lead","type":"text","text":"You use about **half** the data you pay for."}`},
	"caption": {payload: "text", validate: validateCaption, sentence: "Small print under a visual (sources, assumptions).",
		example: `{"id":"src","type":"caption","text":"Prices checked today."}`},
	"divider": {payload: "text", validate: validateDivider, sentence: "Visual break, optionally labelled.",
		example: `{"id":"d1","type":"divider"}`},
	"section": {children: true, validate: validateSection, sentence: "Group of children under the component title.",
		example: `{"id":"s1","type":"section","title":"Your options","children":[…]}`},
	"grid": {payload: "grid", children: true, validate: validateGrid, sentence: "Children side by side in 2-4 columns.",
		example: `{"id":"g1","type":"grid","grid":{"columns":2},"children":[…]}`},
	"tabs": {payload: "tabs", validate: validateTabs, sentence: "2-5 alternative views, e.g. Simple / Detailed.",
		example: `{"id":"t1","type":"tabs","tabs":[{"label":"Simple","children":[…]},{"label":"Detailed","children":[…]}]}`},
	"carousel": {children: true, validate: validateCarousel, sentence: "2-10 swipeable cards, e.g. recipes or places.",
		example: `{"id":"c1","type":"carousel","children":[…]}`},
	"chart": {payload: "chart", summary: summaryRequired, validate: validateChart, sentence: "Trend or distribution of numbers.",
		example: `{"id":"spend","type":"chart","summary":"Spending rose each month.","chart":{"kind":"bar","xLabel":"Month","yLabel":"£","series":[{"name":"Spend","points":[{"label":"Jan","value":120},{"label":"Feb","value":140}]}]}}`},
	"diagram": {payload: "diagram", summary: summaryRequired, validate: validateDiagram, sentence: "Boxes and arrows positioned with x/y.",
		example: `{"id":"flow","type":"diagram","summary":"Water evaporates, condenses and falls as rain.","diagram":{"nodes":[{"id":"a","label":"Evaporation","x":0,"y":0},{"id":"b","label":"Rain","x":200,"y":0}],"edges":[{"from":"a","to":"b"}]}}`},
	"form": {payload: "form", summary: summaryRequired, validate: validateForm, sentence: "Structured input the user submits once.",
		example: `{"id":"prefs","type":"form","summary":"Asks for budget and dates.","form":{"fields":[{"id":"budget","type":"number","label":"Budget","min":0}],"submitLabel":"Send"}}`},
	"table": {payload: "table", summary: summaryRequired, validate: validateTable, sentence: "Exact values in rows and columns.",
		example: `{"id":"plans","type":"table","summary":"Three plans by price and data.","table":{"columns":["Plan","Price"],"rows":[["Basic","£10"],["Plus","£15"]]}}`},
	"stat_row": {payload: "stat_row", summary: summaryRequired, validate: validateStatRow, sentence: "1-4 headline numbers.",
		example: `{"id":"saving","type":"stat_row","summary":"Switching saves £96 a year.","stat_row":{"stats":[{"label":"Yearly saving","value":"£96","trend":"down","goodDirection":"down"}]}}`},
	"actions": {payload: "actions", validate: validateActions, sentence: "Up to 4 buttons: respond sends a reply to you; approval and entity act on the bound item.",
		example: `{"id":"next","type":"actions","actions":[{"id":"yes","label":"Switch plan","kind":"respond","value":"Switch me to Plus"}]}`},
}

// TypeNames lists every component type in the catalogue, sorted.
func TypeNames() []string {
	names := make([]string, 0, len(catalogue))
	for name := range catalogue {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// RequiresSummary reports whether a component type must carry a summary.
// Visual types need one; prose, layout and button rows speak for themselves.
func RequiresSummary(componentType string) bool {
	return catalogue[componentType].summary == summaryRequired
}

// IsLayout reports whether a component type holds other components.
func IsLayout(componentType string) bool {
	return catalogue[componentType].children || componentType == "tabs"
}

// Layout payloads.

type Grid struct {
	Columns int `json:"columns"`
}

type Tab struct {
	Label    string      `json:"label"`
	Children []Component `json:"children"`
}

func validateText(_ *validator, c Component, _ string) error {
	return required("text", c.Text, 1200)
}

func validateCaption(_ *validator, c Component, _ string) error {
	return required("text", c.Text, 300)
}

func validateDivider(_ *validator, c Component, _ string) error {
	return optional("text", c.Text, 80)
}

func childCount(c Component, minimum, maximum int) error {
	if len(c.Children) < minimum || len(c.Children) > maximum {
		return fmt.Errorf("%s needs %d-%d children (got %d)", c.Type, minimum, maximum, len(c.Children))
	}
	return nil
}

func validateSection(_ *validator, c Component, _ string) error {
	return childCount(c, 1, 12)
}

func validateGrid(_ *validator, c Component, _ string) error {
	if c.Grid == nil || c.Grid.Columns < 2 || c.Grid.Columns > 4 {
		return errors.New("grid.columns must be 2, 3 or 4")
	}
	return childCount(c, 1, 12)
}

func validateCarousel(_ *validator, c Component, _ string) error {
	return childCount(c, 2, 10)
}

func validateTabs(_ *validator, c Component, _ string) error {
	if len(c.Tabs) < 2 || len(c.Tabs) > 5 {
		return fmt.Errorf("tabs needs 2-5 tabs (got %d)", len(c.Tabs))
	}
	labels := map[string]bool{}
	for index, tab := range c.Tabs {
		if err := required(fmt.Sprintf("tabs[%d].label", index), tab.Label, 40); err != nil {
			return err
		}
		if labels[strings.TrimSpace(tab.Label)] {
			return fmt.Errorf("tabs[%d].label %q is used twice", index, tab.Label)
		}
		labels[strings.TrimSpace(tab.Label)] = true
		if len(tab.Children) < 1 || len(tab.Children) > 12 {
			return fmt.Errorf("tabs[%d] needs 1-12 children (got %d)", index, len(tab.Children))
		}
	}
	return nil
}

// Chart

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

// ChartKinds lists the supported chart kinds.
var ChartKinds = []string{"bar", "line", "area", "pie", "donut", "scatter"}

func validateChart(_ *validator, c Component, _ string) error {
	if c.Chart == nil {
		return errors.New("chart payload is required")
	}
	if !contains(ChartKinds, c.Chart.Kind) {
		return fmt.Errorf("chart.kind %q is not one of %s", c.Chart.Kind, strings.Join(ChartKinds, ", "))
	}
	if err := optional("chart.xLabel", c.Chart.XLabel, 120); err != nil {
		return err
	}
	if err := optional("chart.yLabel", c.Chart.YLabel, 120); err != nil {
		return err
	}
	if len(c.Chart.Series) == 0 || len(c.Chart.Series) > 12 {
		return fmt.Errorf("chart.series needs 1-12 series (got %d)", len(c.Chart.Series))
	}
	points := 0
	for index, series := range c.Chart.Series {
		if err := required(fmt.Sprintf("chart.series[%d].name", index), series.Name, 120); err != nil {
			return err
		}
		if len(series.Points) == 0 || len(series.Points) > 200 {
			return fmt.Errorf("chart.series[%d].points needs 1-200 points (got %d)", index, len(series.Points))
		}
		points += len(series.Points)
		for point, value := range series.Points {
			if err := required(fmt.Sprintf("chart.series[%d].points[%d].label", index, point), value.Label, 160); err != nil {
				return err
			}
		}
	}
	if points > 1000 {
		return fmt.Errorf("chart has %d points; the limit is 1000", points)
	}
	return nil
}

// Diagram

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

func validateDiagram(_ *validator, c Component, _ string) error {
	d := c.Diagram
	if d == nil {
		return errors.New("diagram payload is required")
	}
	if len(d.Nodes) == 0 || len(d.Nodes) > 100 {
		return fmt.Errorf("diagram.nodes needs 1-100 nodes (got %d)", len(d.Nodes))
	}
	if len(d.Edges) > 200 {
		return fmt.Errorf("diagram.edges allows at most 200 edges (got %d)", len(d.Edges))
	}
	nodes := map[string]bool{}
	for index, node := range d.Nodes {
		if !opaqueID.MatchString(node.ID) || nodes[node.ID] {
			return fmt.Errorf("diagram.nodes[%d].id %q must be a unique id matching %s", index, node.ID, opaqueIDPattern)
		}
		nodes[node.ID] = true
		if err := required(fmt.Sprintf("diagram.nodes[%d].label", index), node.Label, 240); err != nil {
			return err
		}
		if err := optional(fmt.Sprintf("diagram.nodes[%d].kind", index), node.Kind, 80); err != nil {
			return err
		}
	}
	for index, edge := range d.Edges {
		if !nodes[edge.From] || !nodes[edge.To] {
			return fmt.Errorf("diagram.edges[%d] references a missing node (%q -> %q)", index, edge.From, edge.To)
		}
		if err := optional(fmt.Sprintf("diagram.edges[%d].label", index), edge.Label, 240); err != nil {
			return err
		}
	}
	return nil
}

// Form

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

// FormFieldTypes lists the supported form field types.
var FormFieldTypes = []string{"text", "textarea", "number", "select", "radio", "checkbox", "date"}

func validateForm(_ *validator, c Component, _ string) error {
	f := c.Form
	if f == nil {
		return errors.New("form payload is required")
	}
	if err := optional("form.description", f.Description, 2000); err != nil {
		return err
	}
	if err := optional("form.submitLabel", f.SubmitLabel, 120); err != nil {
		return err
	}
	if len(f.Fields) == 0 || len(f.Fields) > 40 {
		return fmt.Errorf("form.fields needs 1-40 fields (got %d)", len(f.Fields))
	}
	seen := map[string]bool{}
	for index, field := range f.Fields {
		at := fmt.Sprintf("form.fields[%d]", index)
		if !opaqueID.MatchString(field.ID) || seen[field.ID] {
			return fmt.Errorf("%s.id %q must be a unique id matching %s", at, field.ID, opaqueIDPattern)
		}
		seen[field.ID] = true
		if !contains(FormFieldTypes, field.Type) {
			return fmt.Errorf("%s.type %q is not one of %s", at, field.Type, strings.Join(FormFieldTypes, ", "))
		}
		if err := required(at+".label", field.Label, 240); err != nil {
			return err
		}
		if err := optional(at+".description", field.Description, 1000); err != nil {
			return err
		}
		if err := optional(at+".placeholder", field.Placeholder, 500); err != nil {
			return err
		}
		choice := field.Type == "select" || field.Type == "radio"
		if choice && (len(field.Options) == 0 || len(field.Options) > 100) {
			return fmt.Errorf("%s is a %s field and needs 1-100 options", at, field.Type)
		}
		if !choice && len(field.Options) > 0 {
			return fmt.Errorf("%s.options are only allowed on select and radio fields", at)
		}
		for option, value := range field.Options {
			if err := required(fmt.Sprintf("%s.options[%d].label", at, option), value.Label, 240); err != nil {
				return err
			}
			if err := required(fmt.Sprintf("%s.options[%d].value", at, option), value.Value, 240); err != nil {
				return err
			}
		}
		if (field.Min != nil || field.Max != nil) && field.Type != "number" {
			return fmt.Errorf("%s.min/max are only allowed on number fields", at)
		}
		if field.Min != nil && field.Max != nil && *field.Min > *field.Max {
			return fmt.Errorf("%s.min cannot exceed max", at)
		}
	}
	return nil
}

// Table

type Table struct {
	Columns []string   `json:"columns"`
	Rows    [][]string `json:"rows"`
}

func validateTable(_ *validator, c Component, _ string) error {
	t := c.Table
	if t == nil {
		return errors.New("table payload is required")
	}
	if len(t.Columns) == 0 || len(t.Columns) > 30 {
		return fmt.Errorf("table.columns needs 1-30 columns (got %d)", len(t.Columns))
	}
	if len(t.Rows) > 1000 {
		return fmt.Errorf("table.rows allows at most 1000 rows (got %d)", len(t.Rows))
	}
	for index, column := range t.Columns {
		if err := required(fmt.Sprintf("table.columns[%d]", index), column, 240); err != nil {
			return err
		}
	}
	for index, row := range t.Rows {
		if len(row) != len(t.Columns) {
			return fmt.Errorf("table.rows[%d] has %d cells; every row needs %d (one per column)", index, len(row), len(t.Columns))
		}
		for cell, value := range row {
			if err := optional(fmt.Sprintf("table.rows[%d][%d]", index, cell), value, 2000); err != nil {
				return err
			}
		}
	}
	return nil
}

// Stat row

type StatRow struct {
	Stats []Stat `json:"stats"`
}

type Stat struct {
	Label         string    `json:"label"`
	Value         string    `json:"value"`
	Unit          string    `json:"unit,omitempty"`
	Delta         string    `json:"delta,omitempty"`
	Trend         string    `json:"trend,omitempty"`
	GoodDirection string    `json:"goodDirection,omitempty"`
	Sparkline     []float64 `json:"sparkline,omitempty"`
}

var (
	Trends         = []string{"up", "down", "flat"}
	GoodDirections = []string{"up", "down"}
)

func validateStatRow(_ *validator, c Component, _ string) error {
	if c.StatRow == nil {
		return errors.New("stat_row payload is required")
	}
	stats := c.StatRow.Stats
	if len(stats) == 0 || len(stats) > 4 {
		return fmt.Errorf("stat_row.stats needs 1-4 stats (got %d)", len(stats))
	}
	for index, stat := range stats {
		at := fmt.Sprintf("stat_row.stats[%d]", index)
		for _, check := range []error{
			required(at+".label", stat.Label, 60), required(at+".value", stat.Value, 40),
			optional(at+".unit", stat.Unit, 24), optional(at+".delta", stat.Delta, 40),
		} {
			if check != nil {
				return check
			}
		}
		if stat.Trend != "" && !contains(Trends, stat.Trend) {
			return fmt.Errorf("%s.trend %q is not one of up, down, flat", at, stat.Trend)
		}
		if stat.GoodDirection != "" && !contains(GoodDirections, stat.GoodDirection) {
			return fmt.Errorf("%s.goodDirection %q is not one of up, down", at, stat.GoodDirection)
		}
		if len(stat.Sparkline) > 30 {
			return fmt.Errorf("%s.sparkline allows at most 30 values (got %d)", at, len(stat.Sparkline))
		}
	}
	return nil
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
