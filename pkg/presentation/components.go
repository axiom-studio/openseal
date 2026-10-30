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
	// bindings lists the binding kinds the type accepts; nil accepts any
	// and an empty list none.
	bindings []BindingKind
	// bindingRequired types need a component binding.
	bindingRequired bool
	validate        func(v *validator, c Component, path string) error
	// sentence is the type's one-line purpose in the visual format.
	sentence string
}

var catalogue = map[string]typeSpec{
	"text":            {payload: "text", validate: validateText, sentence: "Short markdown block (GFM, no HTML)."},
	"caption":         {payload: "text", validate: validateCaption, sentence: "Small print under a visual (sources, assumptions)."},
	"divider":         {payload: "text", validate: validateDivider, sentence: "Visual break, optionally labelled."},
	"section":         {children: true, validate: validateSection, sentence: "Group of children under the component title."},
	"grid":            {payload: "grid", children: true, validate: validateGrid, sentence: "Children side by side in 2-4 columns."},
	"tabs":            {payload: "tabs", validate: validateTabs, sentence: "2-5 alternative views, e.g. Simple / Detailed."},
	"carousel":        {children: true, validate: validateCarousel, sentence: "2-10 swipeable cards, e.g. recipes or places."},
	"chart":           {payload: "chart", summary: summaryRequired, validate: validateChart, sentence: "Trend or distribution of numbers; pie and donut take one series of at most 7 slices; stacked_bar and grouped_bar need 2+ series with the same labels; scatter labels are x numbers."},
	"diagram":         {payload: "diagram", summary: summaryRequired, validate: validateDiagram, sentence: "How 2-30 ideas connect; the app places the nodes by layout; tap a node for its detail."},
	"form":            {payload: "form", summary: summaryRequired, validate: validateForm, sentence: "Structured input the user submits once."},
	"table":           {payload: "table", summary: summaryRequired, validate: validateTable, sentence: "Exact values in rows and columns."},
	"stat_row":        {payload: "stat_row", summary: summaryRequired, validate: validateStatRow, sentence: "1-4 headline numbers."},
	"callout":         {payload: "callout", summary: summaryRequired, validate: validateCallout, sentence: "Key takeaway, tip or warning (markdown body); tone sets its colour, info by default."},
	"steps":           {payload: "steps", summary: summaryRequired, validate: validateSteps, sentence: "2-10 steps in order; checkable lets people tick them off."},
	"comparison":      {payload: "comparison", summary: summaryRequired, validate: validateComparison, sentence: "2-4 options against the same attributes (every option gives every attribute a value); pickable lets the user pick one, which answers you."},
	"timeline":        {payload: "timeline", summary: summaryRequired, validate: validateTimeline, sentence: "2-20 events in time order; when is the label shown, date (YYYY-MM-DD) only orders."},
	"image":           {payload: "image", summary: summaryRequired, validate: validateImage, sentence: "One image already in this chat (by the id and version shown with it, never a URL) with a caption."},
	"gallery":         {payload: "gallery", summary: summaryRequired, validate: validateGallery, sentence: "2-12 images already in this chat."},
	"choices":         {payload: "choices", summary: summaryRequired, validate: validateChoices, sentence: "2-5 quick-reply chips under your answer; a tap answers you."},
	"checklist":       {payload: "checklist", summary: summaryRequired, validate: validateChecklist, sentence: "1-40 items people tick off together (packing, shopping, to-dos); you see progress next turn."},
	"quiz":            {payload: "quiz", summary: summaryRequired, validate: validateQuiz, sentence: "One question with 2-6 options; answerKey stays hidden until they answer, then you hear whether they were right."},
	"labeled_image":   {payload: "labeled_image", summary: summaryRequired, validate: validateLabeledImage, sentence: "Numbered markers on an image already in this chat; x and y are 0-1 from the top-left; tap a marker for its detail."},
	"quantity_visual": {payload: "quantity_visual", summary: summaryRequired, validate: validateQuantityVisual, sentence: "Numbers made tangible for learners: fraction_bars (fractions), number_line (min, max, ticks, points, ranges), unit_blocks (groups), percent_waffle (parts totalling at most 100) or scale_compare (items); steps (at most 4) let people step through a change, each with the same fields."},
	"math":            {payload: "math", summary: summaryRequired, validate: validateMath, sentence: "A worked solution, one LaTeX line (no $ delimiters) per step with an optional note; reveal stepwise to show one step at a time."},
	"definition":      {payload: "definition", summary: summaryRequired, validate: validateDefinition, sentence: "One term made simple, with an optional example and image."},
	"flashcards":      {payload: "flashcards", summary: summaryRequired, validate: validateFlashcards, sentence: "A practice deck of 2-30 cards; people flip each, mark it Got it or Again, and you hear their tally when they finish."},
	"before_after":    {payload: "before_after", summary: summaryRequired, validate: validateBeforeAfter, sentence: "A change shown side by side: two texts (diff highlights changed words, or split) or two images already in this chat (slider or split)."},
	"draft":           {payload: "draft", summary: summaryRequired, bindings: []BindingKind{BindingApproval}, validate: validateDraft, sentence: "A message you wrote for them (email, message, post or note); they can say it looks good, ask for it shorter, or edit it; bind it to the approval of a send you proposed so its buttons send or cancel."},
	"recipe":          {payload: "recipe", summary: summaryRequired, validate: validateRecipe, sentence: "A dish with ingredients (qty for the servings given; people rescale it) and steps; checkable lets people tick ingredients off together."},
	"item":            {payload: "item", summary: summaryRequired, validate: validateItem, sentence: "One product, option or listing with price, rating and up to 4 facts."},
	"analogy":         {payload: "analogy", summary: summaryRequired, validate: validateAnalogy, sentence: "Something new explained as something familiar, with 1-5 matching pairs."},
	"agenda":          {payload: "agenda", summary: summaryRequired, validate: validateAgenda, sentence: "Fixed plans on 1-7 days in date order, items in start order, with free slots."},
	"itinerary":       {payload: "itinerary", summary: summaryRequired, validate: validateItinerary, sentence: "A trip day by day; travel is the leg into an item from the one before (never on a day's first)."},
	"place":           {payload: "place", summary: summaryRequired, validate: validatePlace, sentence: "One venue; several go in a carousel; location adds a map."},
	"breakdown":       {payload: "breakdown", summary: summaryRequired, validate: validateBreakdown, sentence: "Parts of a whole; tap a part for its share."},
	"progress":        {payload: "progress", summary: summaryRequired, validate: validateProgress, sentence: "Values against targets."},
	"poll":            {payload: "poll", summary: summaryRequired, validate: validatePoll, sentence: "A group vote; people can change their vote and you hear the tally once everyone has voted."},
	"explorer":        {payload: "explorer", summary: summaryRequired, validate: validateExplorer, sentence: "A what-if: people move inputs and outputs recompute. Expressions use input ids, + - * / ^, comparisons, && || !, min max round floor ceil abs sqrt log exp, if(c,a,b), sum(i,from,to,body); a toggle is true/false."},
	"routine":         {payload: "routine", summary: summaryRequired, bindings: []BindingKind{BindingRunbook}, bindingRequired: true, validate: validateRoutine, sentence: "A routine with live schedule, status and buttons; items are its latest highlights."},
	"tasks":           {payload: "tasks", summary: summaryRequired, bindings: []BindingKind{}, validate: validateTasks, sentence: "Work handed to helpers in this chat, each row bound to its task; status is live."},
	"agent":           {payload: "agent", summary: summaryRequired, bindings: []BindingKind{BindingAgent}, bindingRequired: true, validate: validateAgentCard, sentence: "Introduces an agent people can start chatting with."},
	"map":             {payload: "map", summary: summaryRequired, validate: validateMap, sentence: "Pins for 1-20 real places from find_place; tap a pin for its detail."},
	"actions":         {payload: "actions", validate: validateActions, sentence: "Up to 4 buttons: respond sends a reply to you; approval and entity act on the bound item."},
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
