package presentation

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
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
	"chart": {payload: "chart", summary: summaryRequired, validate: validateChart, sentence: "Trend or distribution of numbers; pie and donut take one series of at most 7 slices; stacked_bar and grouped_bar need 2+ series with the same labels; scatter labels are x numbers.",
		example: `{"id":"spend","type":"chart","summary":"Spending rose each month.","chart":{"kind":"bar","xLabel":"Month","yLabel":"Spend","format":"currency","currency":"GBP","series":[{"name":"Spend","points":[{"label":"Jan","value":120},{"label":"Feb","value":140}]}],"highlight":{"x":"Feb"}}}`},
	"diagram": {payload: "diagram", summary: summaryRequired, validate: validateDiagram, sentence: "Boxes and arrows positioned with x/y.",
		example: `{"id":"flow","type":"diagram","summary":"Water evaporates, condenses and falls as rain.","diagram":{"nodes":[{"id":"a","label":"Evaporation","x":0,"y":0},{"id":"b","label":"Rain","x":200,"y":0}],"edges":[{"from":"a","to":"b"}]}}`},
	"form": {payload: "form", summary: summaryRequired, validate: validateForm, sentence: "Structured input the user submits once.",
		example: `{"id":"prefs","type":"form","summary":"Asks for budget and dates.","form":{"fields":[{"id":"budget","type":"number","label":"Budget","min":0}],"submitLabel":"Send"}}`},
	"table": {payload: "table", summary: summaryRequired, validate: validateTable, sentence: "Exact values in rows and columns; each row maps column keys to cells typed by the column kind.",
		example: `{"id":"plans","type":"table","summary":"Two plans by price and data.","table":{"columns":[{"key":"plan","label":"Plan","kind":"text"},{"key":"price","label":"Price","kind":"currency","currency":"GBP"}],"rows":[{"plan":"Basic","price":10},{"plan":"Plus","price":15}]}}`},
	"stat_row": {payload: "stat_row", summary: summaryRequired, validate: validateStatRow, sentence: "1-4 headline numbers.",
		example: `{"id":"saving","type":"stat_row","summary":"Switching saves £96 a year.","stat_row":{"stats":[{"label":"Yearly saving","value":96,"format":"currency","currency":"GBP","trend":"down","goodDirection":"down"}]}}`},
	"callout": {payload: "callout", summary: summaryRequired, validate: validateCallout, sentence: "Key takeaway, tip or warning (markdown body); tone sets its colour, info by default.",
		example: `{"id":"tip","type":"callout","summary":"Two streaming services go unused.","tone":"warning","callout":{"icon":"lightbulb","body":"You pay for **three** streaming services and watch one."}}`},
	"steps": {payload: "steps", summary: summaryRequired, validate: validateSteps, sentence: "2-10 steps in order; checkable lets people tick them off.",
		example: `{"id":"how","type":"steps","summary":"Three steps to reset the router.","steps":{"items":[{"id":"unplug","title":"Unplug the router"},{"id":"wait","title":"Wait 30 seconds"},{"id":"plug","title":"Plug it back in","detail":"Lights turn green within a minute."}]}}`},
	"comparison": {payload: "comparison", summary: summaryRequired, validate: validateComparison, sentence: "2-4 options against the same attributes (every option gives every attribute a value); pickable lets the user pick one, which answers you.",
		example: `{"id":"plans","type":"comparison","summary":"Sim Plus is cheapest with enough data.","comparison":{"attributes":[{"key":"data","label":"Data"}],"options":[{"id":"now","name":"Current","price":{"value":25,"format":"currency","currency":"GBP","per":"month"},"values":{"data":"60 GB"}},{"id":"plus","name":"Sim Plus","badge":"Best value","values":{"data":"30 GB"}}],"recommended":"plus","pickable":true}}`},
	"timeline": {payload: "timeline", summary: summaryRequired, validate: validateTimeline, sentence: "2-20 events in time order; when is the label shown, date (YYYY-MM-DD) only orders.",
		example: `{"id":"history","type":"timeline","summary":"From the Bastille in 1789 to Napoleon in 1799.","timeline":{"events":[{"id":"bastille","when":"1789","title":"Storming of the Bastille"},{"id":"napoleon","when":"1799","title":"Napoleon seizes power"}]}}`},
	"image": {payload: "image", summary: summaryRequired, validate: validateImage, sentence: "One image already in this chat (by the id and version shown with it, never a URL) with a caption.",
		example: `{"id":"cell","type":"image","summary":"A labelled animal cell.","image":{"image":{"artifactId":"img-1","version":1},"caption":"An animal cell"}}`},
	"gallery": {payload: "gallery", summary: summaryRequired, validate: validateGallery, sentence: "2-12 images already in this chat.",
		example: `{"id":"rooms","type":"gallery","summary":"Three photos of the flat.","gallery":{"images":[{"image":{"artifactId":"img-1","version":1}},{"image":{"artifactId":"img-2","version":1}}],"layout":"strip"}}`},
	"choices": {payload: "choices", summary: summaryRequired, validate: validateChoices, sentence: "2-5 quick-reply chips under your answer; a tap answers you.",
		example: `{"id":"next","type":"choices","summary":"Offers a shorter or a vegetarian version.","choices":{"chips":[{"id":"short","label":"Shorter"},{"id":"veg","label":"Vegetarian"}]}}`},
	"checklist": {payload: "checklist", summary: summaryRequired, validate: validateChecklist, sentence: "1-40 items people tick off together (packing, shopping, to-dos); you see progress next turn.",
		example: `{"id":"pack","type":"checklist","title":"Packing list","summary":"Eight things to pack.","checklist":{"items":[{"id":"passport","label":"Passport","group":"Documents"},{"id":"charger","label":"Phone charger"}]}}`},
	"quiz": {payload: "quiz", summary: summaryRequired, validate: validateQuiz, sentence: "One question with 2-6 options; answerKey stays hidden until they answer, then you hear whether they were right.",
		example: `{"id":"check","type":"quiz","summary":"Where most ATP is made.","quiz":{"question":"Where is most ATP made?","options":[{"id":"nucleus","label":"Nucleus"},{"id":"mito","label":"Mitochondria"}],"answerKey":{"correct":["mito"],"explanation":"The electron chain in the **mitochondria** makes about 34 ATP."}}}`},
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
	Kind   string `json:"kind"`
	XLabel string `json:"xLabel,omitempty"`
	YLabel string `json:"yLabel,omitempty"`
	// Format, Currency and Unit say how values read, as for a stat.
	Format        StatFormat        `json:"format,omitempty"`
	Currency      string            `json:"currency,omitempty"`
	Unit          string            `json:"unit,omitempty"`
	Series        []ChartSeries     `json:"series"`
	Annotations   []ChartAnnotation `json:"annotations,omitempty"`
	ReferenceLine *ReferenceLine    `json:"referenceLine,omitempty"`
	Highlight     *ChartHighlight   `json:"highlight,omitempty"`
}

type ChartSeries struct {
	Name   string       `json:"name"`
	Points []ChartPoint `json:"points"`
}

type ChartPoint struct {
	Label string  `json:"label"`
	Value float64 `json:"value"`
}

// ChartAnnotation labels the point at X (a point label).
type ChartAnnotation struct {
	X     string `json:"x"`
	Label string `json:"label"`
}

// ReferenceLine draws a horizontal line at Value, e.g. a target or average.
type ReferenceLine struct {
	Value float64 `json:"value"`
	Label string  `json:"label,omitempty"`
}

// ChartHighlight marks one series, one x position, or one point.
type ChartHighlight struct {
	Series string `json:"series,omitempty"`
	X      string `json:"x,omitempty"`
}

// ChartKinds lists the supported chart kinds.
var ChartKinds = []string{"bar", "stacked_bar", "grouped_bar", "line", "area", "pie", "donut", "scatter"}

// Chart limits.
const (
	MaxChartSeries      = 6
	MaxChartPoints      = 60
	MaxChartAnnotations = 4
	// MaxPieSlices is the most slices a pie or donut may have.
	MaxPieSlices = 7
)

func validateChart(_ *validator, c Component, _ string) error {
	chart := c.Chart
	if chart == nil {
		return payloadRequired("chart")
	}
	if !contains(ChartKinds, chart.Kind) {
		return fmt.Errorf("chart.kind %q is not one of %s", chart.Kind, strings.Join(ChartKinds, ", "))
	}
	for _, check := range []error{
		optional("chart.xLabel", chart.XLabel, 120), optional("chart.yLabel", chart.YLabel, 120), optional("chart.unit", chart.Unit, 24),
		validateFormat("chart", chart.Format, chart.Currency), bounded("chart.series", len(chart.Series), 1, MaxChartSeries),
	} {
		if check != nil {
			return check
		}
	}
	labels := map[string]bool{}
	names := map[string]bool{}
	for index, series := range chart.Series {
		at := fmt.Sprintf("chart.series[%d]", index)
		if err := required(at+".name", series.Name, 120); err != nil {
			return err
		}
		if names[series.Name] {
			return fmt.Errorf("%s.name %q is used twice", at, series.Name)
		}
		names[series.Name] = true
		if err := bounded(at+".points", len(series.Points), 1, MaxChartPoints); err != nil {
			return err
		}
		for point, value := range series.Points {
			pointAt := fmt.Sprintf("%s.points[%d]", at, point)
			if err := required(pointAt+".label", value.Label, 160); err != nil {
				return err
			}
			if math.IsNaN(value.Value) || math.IsInf(value.Value, 0) {
				return fmt.Errorf("%s.value must be a finite number", pointAt)
			}
			if chart.Kind == "scatter" {
				if x, err := strconv.ParseFloat(strings.TrimSpace(value.Label), 64); err != nil || math.IsNaN(x) || math.IsInf(x, 0) {
					return fmt.Errorf("%s.label %q must be a number: in a scatter chart the label is the x value", pointAt, value.Label)
				}
			}
			labels[value.Label] = true
		}
	}
	switch chart.Kind {
	case "pie", "donut":
		if len(chart.Series) != 1 {
			return fmt.Errorf("a %s chart takes exactly 1 series (got %d)", chart.Kind, len(chart.Series))
		}
		if slices := len(chart.Series[0].Points); slices > MaxPieSlices {
			return fmt.Errorf("a %s chart allows at most %d slices (got %d); group the smallest as \"Other\" or use a bar chart", chart.Kind, MaxPieSlices, slices)
		}
	case "stacked_bar", "grouped_bar":
		if len(chart.Series) < 2 {
			return fmt.Errorf("a %s chart needs at least 2 series (got %d); use bar for one series", chart.Kind, len(chart.Series))
		}
		first := chart.Series[0].Points
		for index, series := range chart.Series[1:] {
			same := len(series.Points) == len(first)
			for point := 0; same && point < len(first); point++ {
				same = series.Points[point].Label == first[point].Label
			}
			if !same {
				return fmt.Errorf("chart.series[%d] must use the same point labels in the same order as chart.series[0]", index+1)
			}
		}
	}
	if err := bounded("chart.annotations", len(chart.Annotations), 0, MaxChartAnnotations); err != nil {
		return err
	}
	for index, annotation := range chart.Annotations {
		at := fmt.Sprintf("chart.annotations[%d]", index)
		if !labels[annotation.X] {
			return fmt.Errorf("%s.x %q must be the label of a point in the chart", at, annotation.X)
		}
		if err := required(at+".label", annotation.Label, 40); err != nil {
			return err
		}
	}
	if line := chart.ReferenceLine; line != nil {
		if math.IsNaN(line.Value) || math.IsInf(line.Value, 0) {
			return errors.New("chart.referenceLine.value must be a finite number")
		}
		if err := optional("chart.referenceLine.label", line.Label, 40); err != nil {
			return err
		}
	}
	if highlight := chart.Highlight; highlight != nil {
		if highlight.Series == "" && highlight.X == "" {
			return errors.New("chart.highlight needs a series name, an x label, or both")
		}
		if highlight.Series != "" && !names[highlight.Series] {
			return fmt.Errorf("chart.highlight.series %q is not the name of a series", highlight.Series)
		}
		if highlight.X != "" && !labels[highlight.X] {
			return fmt.Errorf("chart.highlight.x %q is not the label of a point", highlight.X)
		}
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
	Columns []TableColumn `json:"columns"`
	// Rows map column keys to cells. A missing cell renders empty.
	Rows []map[string]TableCell `json:"rows"`
}

type TableColumn struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Kind  string `json:"kind"`
	// Currency is the ISO 4217 code of a currency column.
	Currency string `json:"currency,omitempty"`
}

// TableColumnKinds lists the column kinds. number, currency and percent
// cells are numbers; date cells are YYYY-MM-DD strings; image cells are
// image references; text and badge cells are strings.
var TableColumnKinds = []string{"text", "number", "currency", "percent", "date", "badge", "image"}

// Table limits.
const (
	MaxTableColumns   = 8
	MaxTableRows      = 50
	MaxTableCellRunes = 120
)

// TableCell is one table value: exactly one of Text, Number or Image. In
// JSON it is a string, a number, or an image reference object.
type TableCell struct {
	Text   *string
	Number *float64
	Image  *ImageRef
}

// TextCell, NumberCell and ImageCell build table cells.
func TextCell(value string) TableCell    { return TableCell{Text: &value} }
func NumberCell(value float64) TableCell { return TableCell{Number: &value} }
func ImageCell(ref ImageRef) TableCell   { return TableCell{Image: &ref} }

func (c TableCell) MarshalJSON() ([]byte, error) {
	switch {
	case c.Text != nil:
		return json.Marshal(*c.Text)
	case c.Number != nil:
		return json.Marshal(*c.Number)
	case c.Image != nil:
		return json.Marshal(*c.Image)
	default:
		return []byte("null"), nil
	}
}

func (c *TableCell) UnmarshalJSON(raw []byte) error {
	*c = TableCell{}
	trimmed := bytes.TrimSpace(raw)
	switch {
	case len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")):
		return errors.New("a table cell must be a string, a number or an image reference")
	case trimmed[0] == '"':
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return err
		}
		c.Text = &text
	case trimmed[0] == '{':
		var ref ImageRef
		decoder := json.NewDecoder(bytes.NewReader(trimmed))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&ref); err != nil {
			return fmt.Errorf("a table cell object must be an image reference {artifactId, version}: %w", err)
		}
		c.Image = &ref
	default:
		var number float64
		if err := json.Unmarshal(trimmed, &number); err != nil {
			return errors.New("a table cell must be a string, a number or an image reference")
		}
		c.Number = &number
	}
	return nil
}

func validateTable(v *validator, c Component, _ string) error {
	t := c.Table
	if t == nil {
		return payloadRequired("table")
	}
	if err := bounded("table.columns", len(t.Columns), 1, MaxTableColumns); err != nil {
		return err
	}
	columns := map[string]TableColumn{}
	for index, column := range t.Columns {
		at := fmt.Sprintf("table.columns[%d]", index)
		if !itemID.MatchString(column.Key) {
			return fmt.Errorf("%s.key %q must match %s", at, column.Key, itemIDPattern)
		}
		if _, used := columns[column.Key]; used {
			return fmt.Errorf("%s.key %q is used twice", at, column.Key)
		}
		columns[column.Key] = column
		if err := required(at+".label", column.Label, 40); err != nil {
			return err
		}
		if !contains(TableColumnKinds, column.Kind) {
			return fmt.Errorf("%s.kind %q is not one of %s", at, column.Kind, strings.Join(TableColumnKinds, ", "))
		}
		if column.Kind == "currency" {
			if err := validateCurrency(at, column.Currency); err != nil {
				return err
			}
		} else if column.Currency != "" {
			return fmt.Errorf("%s.currency is only used by currency columns", at)
		}
	}
	if err := bounded("table.rows", len(t.Rows), 0, MaxTableRows); err != nil {
		return err
	}
	for index, row := range t.Rows {
		keys := make([]string, 0, len(row))
		for key := range row {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			at := fmt.Sprintf("table.rows[%d].%s", index, key)
			column, ok := columns[key]
			if !ok {
				return fmt.Errorf("%s: no column has key %q", at, key)
			}
			if err := v.tableCell(at, column, row[key]); err != nil {
				return err
			}
		}
	}
	return nil
}

func (v *validator) tableCell(at string, column TableColumn, cell TableCell) error {
	set := 0
	for _, present := range []bool{cell.Text != nil, cell.Number != nil, cell.Image != nil} {
		if present {
			set++
		}
	}
	if set != 1 {
		return fmt.Errorf("%s must be one value", at)
	}
	switch column.Kind {
	case "number", "currency", "percent":
		if cell.Number == nil || math.IsNaN(*cell.Number) || math.IsInf(*cell.Number, 0) {
			return fmt.Errorf("%s must be a number because column %q is %s", at, column.Key, column.Kind)
		}
	case "date":
		if cell.Text == nil || !validDate(*cell.Text) {
			return fmt.Errorf("%s must be a date as YYYY-MM-DD because column %q is date", at, column.Key)
		}
	case "image":
		if cell.Image == nil {
			return fmt.Errorf("%s must be an image reference {artifactId, version} because column %q is image", at, column.Key)
		}
		return v.image(at, cell.Image)
	default:
		if cell.Text == nil {
			return fmt.Errorf("%s must be text because column %q is %s", at, column.Key, column.Kind)
		}
		return optional(at, *cell.Text, MaxTableCellRunes)
	}
	return nil
}

func validDate(value string) bool {
	if !isoDate.MatchString(value) {
		return false
	}
	_, err := time.Parse("2006-01-02", value)
	return err == nil
}

// Stat row

type StatRow struct {
	Stats []Stat `json:"stats"`
}

type Stat struct {
	Label string  `json:"label"`
	Value float64 `json:"value"`
	// Format tells the renderer how to show Value in the viewer's locale.
	// Empty means StatNumber.
	Format StatFormat `json:"format,omitempty"`
	// Currency is the ISO 4217 code; required for StatCurrency only.
	Currency      string    `json:"currency,omitempty"`
	Unit          string    `json:"unit,omitempty"`
	Delta         string    `json:"delta,omitempty"`
	Trend         string    `json:"trend,omitempty"`
	GoodDirection string    `json:"goodDirection,omitempty"`
	Sparkline     []float64 `json:"sparkline,omitempty"`
}

// StatFormat selects how a stat value is formatted. Hosts format with the
// viewer's locale (Intl.NumberFormat on the web).
type StatFormat string

const (
	// StatNumber is a plain number, e.g. 28.
	StatNumber StatFormat = "number"
	// StatCurrency is an amount in Currency, e.g. 96 with GBP.
	StatCurrency StatFormat = "currency"
	// StatPercent is the percentage itself: 12 means 12%.
	StatPercent StatFormat = "percent"
	// StatDuration is a length of time in seconds: 10800 means 3 hours.
	StatDuration StatFormat = "duration"
)

var (
	StatFormats    = []StatFormat{StatNumber, StatCurrency, StatPercent, StatDuration}
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
			required(at+".label", stat.Label, 60), optional(at+".unit", stat.Unit, 24), optional(at+".delta", stat.Delta, 40),
		} {
			if check != nil {
				return check
			}
		}
		if math.IsNaN(stat.Value) || math.IsInf(stat.Value, 0) {
			return fmt.Errorf("%s.value must be a finite number", at)
		}
		if err := validateFormat(at, stat.Format, stat.Currency); err != nil {
			return err
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
