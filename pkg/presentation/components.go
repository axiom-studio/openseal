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

// typeSpec describes one member of the closed component union.
type typeSpec struct {
	// payload is the JSON key of the type's payload; empty when it has none.
	payload string
	// payloadOptional types may leave their payload out.
	payloadOptional bool
	// children bounds Component.Children; a zero max allows none.
	children bounds
	// bindings lists the binding kinds the type accepts; nil accepts any
	// and an empty list none.
	bindings []BindingKind
	// bindingRequired types need a component binding.
	bindingRequired bool
	// rules are the type's own rules that only Go checks; the visual format
	// lists them on the type's line.
	rules []string
	// validate checks the payload; nil when the spec's bounds say it all.
	validate func(v *validator, c Component, path string) error
	// coerce reads the values of the payload's JSON whose type only Go
	// knows as that type, through coerceScalar, before the payload is
	// checked (Admit); nil when the schema states every type.
	coerce func(payload map[string]interface{})
	// sentence is the type's one-line purpose in the visual format.
	sentence string
}

// bounds is an inclusive range of a length or count.
type bounds struct{ min, max int }

// sharedRules are the rules only Go checks that hold for every type; the
// visual format lists them once.
var sharedRules = []string{
	ruleUniqueComponentIDs, ruleUniqueItemIDs, ruleMarkdown, ruleCurrency, ruleCalendarDates, ruleBindings, rulePlaces, ruleSources,
}

// Layout and text limits. The type specs below carry them so the schema
// and Validate read the same numbers.
const (
	// maxChildren bounds the children of a section or grid and of a tab.
	maxChildren      = 12
	maxCarouselCards = 10
	minGridColumns   = 2
	maxGridColumns   = 4
	minTabs          = 2
	maxTabs          = 5
)

var catalogue = map[string]typeSpec{
	"text":            {payload: "text", sentence: "Short markdown block (GFM, no HTML)."},
	"caption":         {payload: "text", sentence: "Small print under a visual (assumptions, caveats)."},
	"divider":         {payload: "text", payloadOptional: true, sentence: "Visual break, optionally labelled."},
	"section":         {children: bounds{1, maxChildren}, sentence: "Group of children under the component title."},
	"grid":            {payload: "grid", children: bounds{1, maxChildren}, sentence: "Children side by side in 2-4 columns."},
	"tabs":            {payload: "tabs", validate: validateTabs, rules: []string{ruleTabLabels}, sentence: "2-5 alternative views, e.g. Simple / Detailed."},
	"carousel":        {children: bounds{2, maxCarouselCards}, sentence: "2-10 swipeable cards, e.g. recipes or places."},
	"chart":           {payload: "chart", validate: validateChart, rules: []string{ruleChartSeriesNames, ruleChartScatter, ruleChartPie, ruleChartBars, ruleChartReferences}, sentence: "Trend or distribution of numbers."},
	"diagram":         {payload: "diagram", validate: validateDiagram, rules: []string{ruleDiagramEdges}, sentence: "How 2-30 ideas connect; the app places the nodes by layout; tap a node for its detail."},
	"form":            {payload: "form", validate: validateForm, rules: []string{ruleFormOptions, ruleFormRange}, sentence: "Structured input the user submits once."},
	"table":           {payload: "table", validate: validateTable, coerce: coerceTableCells, rules: []string{ruleTableColumns}, sentence: "Exact values in rows and columns."},
	"stat_row":        {payload: "stat_row", validate: validateStatRow, sentence: "1-4 headline numbers."},
	"callout":         {payload: "callout", validate: validateCallout, sentence: "Key takeaway, tip or warning (markdown body); tone sets its colour, info by default."},
	"steps":           {payload: "steps", validate: validateSteps, sentence: "2-10 steps in order; checkable lets people tick them off."},
	"comparison":      {payload: "comparison", validate: validateComparison, sentence: "2-4 options against the same attributes; pickable lets the user pick one, which answers you."},
	"timeline":        {payload: "timeline", validate: validateTimeline, sentence: "2-20 events in time order; when is the label shown, date (YYYY-MM-DD) only orders."},
	"image":           {payload: "image", validate: validateImage, sentence: "One image already in this chat (by the id and version shown with it, never a URL) with a caption."},
	"gallery":         {payload: "gallery", validate: validateGallery, sentence: "2-12 images already in this chat."},
	"choices":         {payload: "choices", validate: validateChoices, sentence: "2-5 quick-reply chips under your answer; a tap answers you."},
	"checklist":       {payload: "checklist", validate: validateChecklist, sentence: "1-40 items people tick off together (packing, shopping, to-dos); you see progress next turn."},
	"quiz":            {payload: "quiz", validate: validateQuiz, sentence: "One question with 2-6 options; answerKey stays hidden until they answer, then you hear whether they were right."},
	"labeled_image":   {payload: "labeled_image", validate: validateLabeledImage, sentence: "Numbered markers on an image already in this chat; x and y are 0-1 from the top-left; tap a marker for its detail."},
	"quantity_visual": {payload: "quantity_visual", validate: validateQuantityVisual, rules: []string{ruleQuantityFields, ruleQuantityNumberLine, ruleQuantityPercent, ruleQuantityScale}, sentence: "Numbers made tangible for learners; steps (at most 4) let people step through a change, each with the same fields."},
	"math":            {payload: "math", validate: validateMath, sentence: "A worked solution, one LaTeX line (no $ delimiters) per step with an optional note; reveal stepwise to show one step at a time."},
	"definition":      {payload: "definition", validate: validateDefinition, sentence: "One term made simple, with an optional example and image."},
	"flashcards":      {payload: "flashcards", validate: validateFlashcards, sentence: "A practice deck of 2-30 cards; people flip each, mark it Got it or Again, and you hear their tally when they finish."},
	"before_after":    {payload: "before_after", validate: validateBeforeAfter, coerce: coerceBeforeAfterLabels, sentence: "A change shown side by side: two texts (diff highlights changed words, or split) or two images already in this chat (slider or split)."},
	"draft":           {payload: "draft", bindings: []BindingKind{BindingApproval}, validate: validateDraft, sentence: "A message you wrote for them (email, message, post or note); they can say it looks good, ask for it shorter, or edit it; bind it to the approval of a send you proposed so its buttons send or cancel."},
	"recipe":          {payload: "recipe", validate: validateRecipe, sentence: "A dish with ingredients (qty for the servings given; people rescale it) and steps; checkable lets people tick ingredients off together."},
	"item":            {payload: "item", validate: validateItem, sentence: "One product, option or listing with price, rating and up to 4 facts."},
	"analogy":         {payload: "analogy", validate: validateAnalogy, sentence: "Something new explained as something familiar, with 1-5 matching pairs."},
	"agenda":          {payload: "agenda", validate: validateAgenda, rules: []string{ruleAgendaOrder, ruleAgendaFreeSlots}, sentence: "Fixed plans on 1-7 days, with free slots."},
	"itinerary":       {payload: "itinerary", validate: validateItinerary, rules: []string{ruleItineraryTravel}, sentence: "A trip day by day."},
	"place":           {payload: "place", validate: validatePlace, rules: []string{rulePlaceName}, sentence: "One venue; several go in a carousel; location adds a map."},
	"breakdown":       {payload: "breakdown", validate: validateBreakdown, rules: []string{ruleBreakdownPositive}, sentence: "Parts of a whole; tap a part for its share."},
	"progress":        {payload: "progress", validate: validateProgress, sentence: "Values against targets."},
	"poll":            {payload: "poll", validate: validatePoll, sentence: "A group vote; people can change their vote and you hear the tally once everyone has voted."},
	"explorer":        {payload: "explorer", validate: validateExplorer, rules: []string{ruleExplorerReserved, ruleExplorerExpr}, sentence: "A what-if: people move inputs and outputs recompute. Expressions use input ids, + - * / ^, comparisons, && || !, min max round floor ceil abs sqrt log exp, if(c,a,b), sum(i,from,to,body); a toggle is true/false."},
	"routine":         {payload: "routine", payloadOptional: true, bindings: []BindingKind{BindingRunbook}, bindingRequired: true, sentence: "A routine with live schedule, status and buttons; items are its latest highlights."},
	"tasks":           {payload: "tasks", bindings: []BindingKind{}, validate: validateTasks, rules: []string{ruleTaskRows}, sentence: "Work handed to helpers in this chat, each row bound to its task; status is live."},
	"agent":           {payload: "agent", bindings: []BindingKind{BindingAgent}, bindingRequired: true, sentence: "Introduces an agent people can start chatting with."},
	"map":             {payload: "map", validate: validateMap, rules: []string{ruleMapPins}, sentence: "Pins for 1-20 real places from find_place; tap a pin for its detail."},
	"cards":           {payload: "cards", validate: validateCards, sentence: "1-8 things, each with an optional photo, one headline value, a line, a detail and a status chip (weather by city, stocks, people, plans)."},
	"converter":       {payload: "converter", validate: validateConverter, sentence: "Two units and a rate; people type in either box and it converts both ways (currencies, km/mi, kg/lb)."},
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

// AccessibleSummary is the plain sentence screen readers and voice read for
// c. A summary is optional: without one, the title stands in, or else the
// first sentence of the component's main text.
func (c Component) AccessibleSummary() string {
	if summary := strings.TrimSpace(c.Summary); summary != "" {
		return summary
	}
	if title := strings.TrimSpace(c.Title); title != "" {
		return title
	}
	return firstSentence(mainText(c))
}

// mainText is the prose a component of each type leads with, if it has one.
func mainText(c Component) string {
	switch {
	case c.Type == "text" || c.Type == "caption" || c.Type == "divider":
		return c.Text
	case c.Type == "callout" && c.Callout != nil:
		return c.Callout.Body
	case c.Type == "definition" && c.Definition != nil:
		return c.Definition.Term + ": " + c.Definition.Meaning
	case c.Type == "quiz" && c.Quiz != nil:
		return c.Quiz.Question
	case c.Type == "poll" && c.Poll != nil:
		return c.Poll.Question
	case c.Type == "draft" && c.Draft != nil:
		return c.Draft.Body
	case c.Type == "item" && c.Item != nil:
		return c.Item.Name
	case c.Type == "place" && c.Place != nil:
		return c.Place.Name
	case c.Type == "agent" && c.Agent != nil:
		return c.Agent.Headline
	case c.Type == "image" && c.Image != nil:
		return c.Image.Caption
	}
	return ""
}

// firstSentence is text up to and including its first full stop, question
// or exclamation mark that ends a sentence, or all of it on one line.
func firstSentence(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	for index, r := range text {
		if (r == '.' || r == '?' || r == '!') && (index+1 == len(text) || text[index+1] == ' ') {
			return text[:index+1]
		}
	}
	return text
}

// IsLayout reports whether a component type holds other components.
func IsLayout(componentType string) bool {
	return catalogue[componentType].children.max > 0 || componentType == "tabs"
}

// Layout payloads.

type Grid struct {
	Columns int `json:"columns"`
}

type Tab struct {
	Label    string      `json:"label"`
	Children []Component `json:"children"`
}

const ruleTabLabels = "tab labels are unique"

func validateTabs(_ *validator, c Component, _ string) error {
	labels := map[string]bool{}
	for index, tab := range c.Tabs {
		label := strings.TrimSpace(tab.Label)
		if labels[label] {
			return fmt.Errorf("tabs[%d].label %q is used twice", index, tab.Label)
		}
		labels[label] = true
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

// Chart rules the schema cannot state. The pie limit is MaxPieSlices.
const (
	ruleChartSeriesNames = "series names are unique"
	ruleChartScatter     = "in a scatter chart each point label is its x number"
	ruleChartPie         = "pie and donut take exactly 1 series of at most 7 slices"
	ruleChartBars        = "stacked_bar and grouped_bar take 2 or more series with the same point labels in the same order"
	ruleChartReferences  = "annotations[].x and highlight.x name a point label, highlight.series names a series, and highlight sets at least one"
)

func validateChart(_ *validator, c Component, _ string) error {
	chart := c.Chart
	if err := validateFormat("chart", chart.Format, chart.Currency); err != nil {
		return err
	}
	labels := map[string]bool{}
	names := map[string]bool{}
	for index, series := range chart.Series {
		at := fmt.Sprintf("chart.series[%d]", index)
		if names[series.Name] {
			return fmt.Errorf("%s.name %q is used twice", at, series.Name)
		}
		names[series.Name] = true
		for point, value := range series.Points {
			if chart.Kind == "scatter" {
				if x, err := strconv.ParseFloat(strings.TrimSpace(value.Label), 64); err != nil || math.IsNaN(x) || math.IsInf(x, 0) {
					return fmt.Errorf("%s.points[%d].label %q must be a number: in a scatter chart the label is the x value", at, point, value.Label)
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
	for index, annotation := range chart.Annotations {
		if !labels[annotation.X] {
			return fmt.Errorf("chart.annotations[%d].x %q must be the label of a point in the chart", index, annotation.X)
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

// Form limits.
const (
	maxFormFields  = 40
	maxFormOptions = 100
)

const (
	ruleFormOptions = "select and radio fields need options; other fields ignore them"
	ruleFormRange   = "min and max apply only to number fields, where min is at most max"
)

func validateForm(_ *validator, c Component, _ string) error {
	seen := map[string]bool{}
	for index, field := range c.Form.Fields {
		at := fmt.Sprintf("form.fields[%d]", index)
		if seen[field.ID] {
			return fmt.Errorf("%s.id %q is used twice", at, field.ID)
		}
		seen[field.ID] = true
		choice := field.Type == "select" || field.Type == "radio"
		if choice && len(field.Options) == 0 {
			return fmt.Errorf("%s is a %s field and needs 1-%d options (got 0)", at, field.Type, maxFormOptions)
		}
		if field.Type == "number" && field.Min != nil && field.Max != nil && *field.Min > *field.Max {
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
	// Kind is one of TableColumnKinds; empty means text.
	Kind string `json:"kind,omitempty"`
	// Currency is the ISO 4217 code of a currency column; other columns
	// ignore it.
	Currency string `json:"currency,omitempty"`
}

// TableColumnKinds lists the column kinds. number, currency and percent
// cells are numbers; date cells are YYYY-MM-DD strings; image cells are
// image references; text and badge cells are strings.
var TableColumnKinds = []string{"text", "number", "currency", "percent", "date", "badge", "image"}

// ColumnKind is the kind a column is read as: its kind, or text when it
// has none.
func (c TableColumn) ColumnKind() string {
	if c.Kind == "" {
		return "text"
	}
	return c.Kind
}

// Table limits.
const (
	MaxTableColumns = 8
	MaxTableRows    = 50
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
		if err := json.Unmarshal(trimmed, &ref); err != nil {
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

const (
	ruleTableColumns = "column keys are unique; a currency column has a currency, which other columns ignore"
	ruleTableCells   = "each row maps column keys to cells: numbers for number, currency and percent; YYYY-MM-DD for date; an image for image; text otherwise"
)

func validateTable(v *validator, c Component, _ string) error {
	t := c.Table
	columns := map[string]TableColumn{}
	for index, column := range t.Columns {
		at := fmt.Sprintf("table.columns[%d]", index)
		if _, used := columns[column.Key]; used {
			return fmt.Errorf("%s.key %q is used twice", at, column.Key)
		}
		columns[column.Key] = column
		if column.Kind == "currency" {
			if err := validateCurrency(at, column.Currency); err != nil {
				return err
			}
		}
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
	kind := column.ColumnKind()
	switch {
	case cellType(kind) == "number":
		if cell.Number == nil {
			return fmt.Errorf("%s must be a number because column %q is %s", at, column.Key, kind)
		}
	case kind == "date":
		if cell.Text == nil || !validDate(*cell.Text) {
			return fmt.Errorf("%s must be a date as YYYY-MM-DD because column %q is date", at, column.Key)
		}
	case kind == "image":
		if cell.Image == nil {
			return fmt.Errorf("%s must be an image reference {artifactId, version} because column %q is image", at, column.Key)
		}
		return v.image(at, cell.Image)
	default:
		if cell.Text == nil {
			return fmt.Errorf("%s must be text because column %q is %s", at, column.Key, kind)
		}
	}
	return nil
}

// cellType is the JSON type of the cells of a column kind: number for
// number, currency and percent, object (an image reference) for image, and
// string otherwise.
func cellType(kind string) string {
	switch kind {
	case "number", "currency", "percent":
		return "number"
	case "image":
		return "object"
	}
	return "string"
}

// coerceTableCells reads each cell of a table as its column's kind, so a
// number in a text column is its text and a numeric string in a number
// column its number.
func coerceTableCells(payload map[string]interface{}) {
	kinds := map[string]string{}
	for _, column := range objectsAt(payload, "columns") {
		key, _ := column["key"].(string)
		kind, _ := column["kind"].(string)
		kinds[key] = TableColumn{Kind: kind}.ColumnKind()
	}
	for _, row := range objectsAt(payload, "rows") {
		for key, cell := range row {
			kind, known := kinds[key]
			if !known {
				continue
			}
			if value, ok := coerceScalar(cell, cellType(kind)); ok {
				row[key] = value
			}
		}
	}
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

// Stat row limits.
const (
	maxStats     = 4
	maxSparkline = 30
)

var (
	StatFormats    = []StatFormat{StatNumber, StatCurrency, StatPercent, StatDuration}
	Trends         = []string{"up", "down", "flat"}
	GoodDirections = []string{"up", "down"}
)

func validateStatRow(_ *validator, c Component, _ string) error {
	for index, stat := range c.StatRow.Stats {
		if err := validateFormat(fmt.Sprintf("stat_row.stats[%d]", index), stat.Format, stat.Currency); err != nil {
			return err
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
