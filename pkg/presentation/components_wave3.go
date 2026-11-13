package presentation

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // agenda time zones validate the same on every host

	"github.com/axiom-studio/openseal/pkg/presentation/expr"
)

const clockTimePattern = `^([01][0-9]|2[0-3]):[0-5][0-9]$`

// ruleCalendarDates: the date pattern admits 2026-02-30; Go does not.
const ruleCalendarDates = "dates are real calendar days"

// calendarDate checks that a YYYY-MM-DD the schema admitted is a real
// day.
func calendarDate(field, value string) error {
	if value != "" && !validDate(value) {
		return fmt.Errorf("%s %q is not a calendar date", field, value)
	}
	return nil
}

// Agenda shows fixed plans on 1-7 days with optional free slots. Read-only.
type Agenda struct {
	// Timezone is an IANA name shown as small print; times are not converted.
	Timezone  string           `json:"timezone,omitempty"`
	Days      []AgendaDay      `json:"days"`
	FreeSlots []AgendaFreeSlot `json:"freeSlots,omitempty"`
}

type AgendaDay struct {
	Date  string       `json:"date"`
	Label string       `json:"label,omitempty"`
	Items []AgendaItem `json:"items"`
}

type AgendaItem struct {
	ID    string `json:"id"`
	Start string `json:"start"`
	End   string `json:"end,omitempty"`
	Title string `json:"title"`
	Place string `json:"place,omitempty"`
	Tone  Tone   `json:"tone,omitempty"`
}

type AgendaFreeSlot struct {
	Date  string `json:"date"`
	Start string `json:"start"`
	End   string `json:"end"`
}

// Agenda limits.
const (
	maxTimezoneRunes  = 64
	maxAgendaDays     = 7
	maxDayLabelRunes  = 40 // agenda and itinerary days
	maxDayItems       = 12 // agenda and itinerary days
	maxPlanTitleRunes = 80 // agenda and itinerary items
	maxPlanPlaceRunes = 80 // agenda and itinerary items
	maxFreeSlots      = 12
)

const (
	ruleAgendaOrder     = "days in date order, each date once; items in start order, each ending after it starts"
	ruleAgendaFreeSlots = "a free slot is on the date of one of days and ends after it starts"
	ruleTimezone        = "an IANA time zone name, shown only"
)

func validateAgenda(_ *validator, c Component, _ string) error {
	agenda := c.Agenda
	if agenda.Timezone != "" {
		if _, err := time.LoadLocation(agenda.Timezone); err != nil || agenda.Timezone == "Local" {
			return fmt.Errorf("agenda.timezone %q must be an IANA time zone such as Europe/London", agenda.Timezone)
		}
	}
	ids := map[string]bool{}
	dates := map[string]bool{}
	for index, day := range agenda.Days {
		at := fmt.Sprintf("agenda.days[%d]", index)
		if err := calendarDate(at+".date", day.Date); err != nil {
			return err
		}
		if index > 0 && day.Date <= agenda.Days[index-1].Date {
			return fmt.Errorf("%s.date %s must come after %s: list days in date order, each once", at, day.Date, agenda.Days[index-1].Date)
		}
		dates[day.Date] = true
		for item, entry := range day.Items {
			itemAt := fmt.Sprintf("%s.items[%d]", at, item)
			if err := itemIDs(ids, itemAt, entry.ID); err != nil {
				return err
			}
			if entry.End != "" && entry.End <= entry.Start {
				return fmt.Errorf("%s.end %s must be after start %s", itemAt, entry.End, entry.Start)
			}
			if item > 0 && entry.Start < day.Items[item-1].Start {
				return fmt.Errorf("%s.start %s is before the item above it (%s); list items in start order", itemAt, entry.Start, day.Items[item-1].Start)
			}
		}
	}
	for index, slot := range agenda.FreeSlots {
		at := fmt.Sprintf("agenda.freeSlots[%d]", index)
		if !dates[slot.Date] {
			return fmt.Errorf("%s.date %s is not the date of one of agenda.days", at, slot.Date)
		}
		if slot.End <= slot.Start {
			return fmt.Errorf("%s.end %s must be after start %s", at, slot.End, slot.Start)
		}
	}
	return nil
}

// Itinerary is a trip plan: places per day with the travel between them.
type Itinerary struct {
	Days []ItineraryDay `json:"days"`
}

type ItineraryDay struct {
	ID    string          `json:"id"`
	Label string          `json:"label"`
	Date  string          `json:"date,omitempty"`
	Items []ItineraryItem `json:"items"`
}

type ItineraryItem struct {
	ID     string    `json:"id"`
	Time   string    `json:"time,omitempty"`
	Title  string    `json:"title"`
	Place  string    `json:"place,omitempty"`
	Detail string    `json:"detail,omitempty"`
	Image  *ImageRef `json:"image,omitempty"`
	// Travel is the leg arriving here from the previous item of the day.
	Travel *Travel `json:"travel,omitempty"`
	// Location is a place id from find_place; the card pins the located
	// items on a map when there are at least two.
	Location string `json:"location,omitempty"`
	// Lat and Lon are output-only: StampPlaces fills them in from Location.
	Lat *float64 `json:"lat,omitempty"`
	Lon *float64 `json:"lon,omitempty"`
}

type Travel struct {
	Mode    string `json:"mode"`
	Minutes int    `json:"minutes"`
}

// TravelModes lists the values of Travel.Mode.
var TravelModes = []string{"walk", "drive", "transit", "bike", "flight", "boat"}

// MaxTravelMinutes bounds one travel leg (a day).
// Itinerary limits.
const (
	MaxTravelMinutes        = 1440
	maxItineraryDays        = 10
	maxItineraryDetailRunes = 200
)

const ruleItineraryTravel = "travel is the leg into an item from the one before, never on a day's first item"

func validateItinerary(v *validator, c Component, _ string) error {
	days := map[string]bool{}
	items := map[string]bool{}
	for index, day := range c.Itinerary.Days {
		at := fmt.Sprintf("itinerary.days[%d]", index)
		if err := firstError(itemIDs(days, at, day.ID), calendarDate(at+".date", day.Date)); err != nil {
			return err
		}
		for item, entry := range day.Items {
			itemAt := fmt.Sprintf("%s.items[%d]", at, item)
			if err := firstError(itemIDs(items, itemAt, entry.ID), v.image(itemAt+".image", entry.Image),
				v.location(itemAt, entry.Location, entry.Lat, entry.Lon)); err != nil {
				return err
			}
			if entry.Travel != nil && item == 0 {
				return fmt.Errorf("%s.travel: the first item of a day has no travel leg; travel is how people get here from the item before", itemAt)
			}
		}
	}
	return nil
}

// Place is one venue: a restaurant, a museum, a hotel. With a Location the
// card shows it on a small map with a Directions button, and Name may be
// left for StampPlaces to fill in.
type Place struct {
	Name       string      `json:"name,omitempty"`
	Category   string      `json:"category,omitempty"`
	Image      *ImageRef   `json:"image,omitempty"`
	Address    string      `json:"address,omitempty"`
	Rating     *ItemRating `json:"rating,omitempty"`
	PriceLevel int         `json:"priceLevel,omitempty"`
	Hours      []string    `json:"hours,omitempty"`
	Facts      []string    `json:"facts,omitempty"`
	// Location is a place id from find_place.
	Location string `json:"location,omitempty"`
	// Lat and Lon are output-only: StampPlaces fills them in from Location.
	Lat *float64 `json:"lat,omitempty"`
	Lon *float64 `json:"lon,omitempty"`
}

// Place limits; the name, category and address limits sit with the fields
// the host stamps in (components_wave5.go).
const (
	maxPriceLevel = 4
	maxHours      = 7
	maxHoursRunes = 40
)

const rulePlaceName = "name is required unless location names a place"

func validatePlace(v *validator, c Component, _ string) error {
	place := c.Place
	if place.Location == "" && strings.TrimSpace(place.Name) == "" {
		return errors.New("place.name is required unless location names a place from find_place")
	}
	return firstError(v.location("place", place.Location, place.Lat, place.Lon), v.image("place.image", place.Image))
}

// Breakdown shows the parts of a whole.
type Breakdown struct {
	Kind     string          `json:"kind"`
	Format   StatFormat      `json:"format,omitempty"`
	Currency string          `json:"currency,omitempty"`
	Unit     string          `json:"unit,omitempty"`
	Items    []BreakdownItem `json:"items"`
}

type BreakdownItem struct {
	ID    string  `json:"id"`
	Label string  `json:"label"`
	Value float64 `json:"value"`
	Icon  string  `json:"icon,omitempty"`
}

// BreakdownKinds lists the values of Breakdown.Kind.
var BreakdownKinds = []string{"treemap", "waffle", "stacked_bar"}

// Breakdown and progress limits.
const (
	minBreakdownItems = 2
	maxBreakdownItems = 12
	maxPartLabelRunes = 40 // breakdown and progress items
	maxProgressItems  = 6
)

const ruleBreakdownPositive = "at least one value is above 0"

func validateBreakdown(v *validator, c Component, _ string) error {
	breakdown := c.Breakdown
	if err := validateFormat("breakdown", breakdown.Format, breakdown.Currency); err != nil {
		return err
	}
	seen := map[string]bool{}
	positive := false
	for index, item := range breakdown.Items {
		at := fmt.Sprintf("breakdown.items[%d]", index)
		v.icon(at+".icon", c.ID, item.Icon)
		if err := itemIDs(seen, at, item.ID); err != nil {
			return err
		}
		positive = positive || item.Value > 0
	}
	if !positive {
		return errors.New("breakdown.items: at least one value must be above 0")
	}
	return nil
}

// Progress shows values against targets.
type Progress struct {
	Kind  string         `json:"kind"`
	Items []ProgressItem `json:"items"`
}

type ProgressItem struct {
	ID       string     `json:"id"`
	Label    string     `json:"label"`
	Value    float64    `json:"value"`
	Target   float64    `json:"target"`
	Format   StatFormat `json:"format,omitempty"`
	Currency string     `json:"currency,omitempty"`
	Unit     string     `json:"unit,omitempty"`
}

// ProgressKinds lists the values of Progress.Kind.
var ProgressKinds = []string{"ring", "bar"}

func validateProgress(_ *validator, c Component, _ string) error {
	seen := map[string]bool{}
	for index, item := range c.Progress.Items {
		at := fmt.Sprintf("progress.items[%d]", index)
		if err := firstError(itemIDs(seen, at, item.ID), validateFormat(at, item.Format, item.Currency)); err != nil {
			return err
		}
	}
	return nil
}

// Poll asks the people in the chat to vote.
type Poll struct {
	Question string       `json:"question"`
	Options  []PollOption `json:"options"`
	Multiple bool         `json:"multiple,omitempty"`
	// ClosesAt (RFC 3339) ends voting.
	ClosesAt string `json:"closesAt,omitempty"`
}

type PollOption struct {
	ID    string    `json:"id"`
	Label string    `json:"label"`
	Image *ImageRef `json:"image,omitempty"`
}

// Poll limits.
const (
	maxPollQuestionRunes = 200
	minPollOptions       = 2
	maxPollOptions       = 8
	maxPollLabelRunes    = 60
)

const rulePollClosesAt = "RFC 3339 time voting ends"

func validatePoll(v *validator, c Component, _ string) error {
	poll := c.Poll
	seen := map[string]bool{}
	for index, option := range poll.Options {
		at := fmt.Sprintf("poll.options[%d]", index)
		if err := firstError(itemIDs(seen, at, option.ID), v.image(at+".image", option.Image)); err != nil {
			return err
		}
	}
	if poll.ClosesAt != "" {
		if _, err := time.Parse(time.RFC3339, poll.ClosesAt); err != nil {
			return fmt.Errorf("poll.closesAt %q must be an RFC 3339 time such as 2026-11-01T18:00:00Z", poll.ClosesAt)
		}
	}
	return nil
}

// Explorer lets people change inputs and see outputs recomputed from
// expressions (pkg/presentation/expr).
type Explorer struct {
	Inputs  []ExplorerInput  `json:"inputs"`
	Outputs []ExplorerOutput `json:"outputs"`
	Chart   *ExplorerChart   `json:"chart,omitempty"`
}

// ExplorerInput is one control. Its id is the name expressions use.
type ExplorerInput struct {
	ID       string     `json:"id"`
	Label    string     `json:"label"`
	Kind     string     `json:"kind"`
	Min      *float64   `json:"min,omitempty"`
	Max      *float64   `json:"max,omitempty"`
	Step     *float64   `json:"step,omitempty"`
	Default  *float64   `json:"default"`
	Unit     string     `json:"unit,omitempty"`
	Format   StatFormat `json:"format,omitempty"`
	Currency string     `json:"currency,omitempty"`
}

type ExplorerOutput struct {
	ID       string     `json:"id"`
	Label    string     `json:"label"`
	Expr     string     `json:"expr"`
	Format   StatFormat `json:"format,omitempty"`
	Currency string     `json:"currency,omitempty"`
	Unit     string     `json:"unit,omitempty"`
	Digits   *int       `json:"digits,omitempty"`
}

// ExplorerChart draws series over X: an input id (swept from its min to max)
// or "t" (from T.From to T.To).
type ExplorerChart struct {
	X      string           `json:"x"`
	T      *ExplorerRange   `json:"t,omitempty"`
	Points *int             `json:"points,omitempty"`
	Series []ExplorerSeries `json:"series"`
}

type ExplorerRange struct {
	From  float64 `json:"from"`
	To    float64 `json:"to"`
	Label string  `json:"label,omitempty"`
}

type ExplorerSeries struct {
	Label string `json:"label"`
	Expr  string `json:"expr"`
}

// ExplorerInputKinds lists the values of ExplorerInput.Kind.
var ExplorerInputKinds = []string{"slider", "stepper", "toggle"}

// Explorer limits.
const (
	ExplorerTime          = "t"
	DefaultExplorerPoints = 41
	MaxExplorerPoints     = 61
	MaxExplorerExprRunes  = 300
	MaxExplorerDigits     = 6
	// explorerStepTolerance absorbs binary rounding when checking that a
	// value sits on a step.
	explorerStepTolerance = 1e-9

	maxExplorerInputs      = 4
	maxExplorerOutputs     = 4
	maxExplorerLabelRunes  = 40
	maxExplorerUnitRunes   = 16
	maxExplorerSeries      = 3
	maxExplorerSeriesRunes = 30 // series and t labels
)

const explorerNamePattern = `^[a-z][a-z0-9_]{0,39}$`

const (
	ruleExplorerReserved = "input ids are not t or a function name"
	ruleExplorerInputs   = "slider and stepper: min below max, default on a step; toggle: default 0 or 1, no min, max or step"
	ruleExplorerExpr     = "every expr gives a number and can be worked out at the inputs' defaults"
	ruleExplorerX        = "the id of a slider or stepper input (swept min to max), or t"
	ruleExplorerT        = "required when x is t, with from below to, and only then; series may use t"
)

func validateExplorer(_ *validator, c Component, _ string) error {
	explorer := c.Explorer
	inputs := map[string]bool{}
	reserved := map[string]bool{ExplorerTime: true}
	for _, name := range expr.FunctionNames() {
		reserved[name] = true
	}
	for index, input := range explorer.Inputs {
		at := fmt.Sprintf("explorer.inputs[%d]", index)
		if reserved[input.ID] {
			return fmt.Errorf("%s.id %q is reserved (t and the function names); pick another", at, input.ID)
		}
		if err := firstError(itemIDs(inputs, at, input.ID), validateFormat(at, input.Format, input.Currency), validateExplorerInput(at, input)); err != nil {
			return err
		}
	}
	types := explorerTypes(explorer.Inputs)
	env := explorerDefaults(explorer.Inputs)
	outputs := map[string]bool{}
	for index, output := range explorer.Outputs {
		at := fmt.Sprintf("explorer.outputs[%d]", index)
		if err := firstError(itemIDs(outputs, at, output.ID), validateFormat(at, output.Format, output.Currency)); err != nil {
			return err
		}
		program, err := compileExplorerExpr(at+".expr", output.Expr, types)
		if err != nil {
			return err
		}
		if _, err := program.Eval(env); err != nil {
			return fmt.Errorf("%s.expr cannot be worked out at the inputs' defaults: %v", at, err)
		}
	}
	if explorer.Chart != nil {
		return validateExplorerChart(explorer, types, env)
	}
	return nil
}

func validateExplorerInput(at string, input ExplorerInput) error {
	if input.Kind == "toggle" {
		if input.Min != nil || input.Max != nil || input.Step != nil {
			return fmt.Errorf("%s is a toggle and takes no min, max or step", at)
		}
		if *input.Default != 0 && *input.Default != 1 {
			return fmt.Errorf("%s.default must be 0 (off) or 1 (on) for a toggle", at)
		}
		return nil
	}
	if input.Min == nil || input.Max == nil || input.Step == nil {
		return fmt.Errorf("%s is a %s and needs min, max and step", at, input.Kind)
	}
	if *input.Min >= *input.Max {
		return fmt.Errorf("%s.min must be below max", at)
	}
	if err := explorerValue(input, *input.Default); err != nil {
		return fmt.Errorf("%s.default %v", at, err)
	}
	return nil
}

// explorerValue checks a value for an input: 0 or 1 for a toggle; within
// [min, max] and on a step from min otherwise.
func explorerValue(input ExplorerInput, value float64) error {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return errors.New("must be a finite number")
	}
	if input.Kind == "toggle" {
		if value != 0 && value != 1 {
			return errors.New("must be 0 or 1")
		}
		return nil
	}
	if input.Min == nil || input.Max == nil || input.Step == nil {
		return errors.New("has no range")
	}
	low, high, step := *input.Min, *input.Max, *input.Step
	if value < low || value > high {
		return fmt.Errorf("%s is outside %s-%s", formatNumber(value), formatNumber(low), formatNumber(high))
	}
	steps := (value - low) / step
	if math.Abs(steps-math.Round(steps)) > explorerStepTolerance*math.Max(1, math.Abs(steps)) {
		return fmt.Errorf("%s is not on a step of %s from %s", formatNumber(value), formatNumber(step), formatNumber(low))
	}
	return nil
}

func compileExplorerExpr(field, source string, types map[string]expr.Type) (*expr.Program, error) {
	program, err := expr.Compile(source, types)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", field, err)
	}
	if program.Type() != expr.Number {
		return nil, fmt.Errorf("%s must give a number, not true/false; wrap a condition in if(cond, a, b)", field)
	}
	return program, nil
}

func validateExplorerChart(explorer *Explorer, types map[string]expr.Type, env map[string]float64) error {
	chart := explorer.Chart
	seriesTypes := types
	seriesEnv := env
	if chart.X == ExplorerTime {
		t := chart.T
		if t == nil {
			return errors.New(`explorer.chart.t {from, to} is required when x is "t"`)
		}
		if t.From >= t.To {
			return errors.New("explorer.chart.t.from must be below to")
		}
		seriesTypes = map[string]expr.Type{ExplorerTime: expr.Number}
		seriesEnv = map[string]float64{ExplorerTime: t.From}
		for name, typ := range types {
			seriesTypes[name] = typ
			seriesEnv[name] = env[name]
		}
	} else {
		if chart.T != nil {
			return errors.New(`explorer.chart.t is only used when x is "t"`)
		}
		var swept *ExplorerInput
		for index := range explorer.Inputs {
			if explorer.Inputs[index].ID == chart.X {
				swept = &explorer.Inputs[index]
			}
		}
		if swept == nil {
			return fmt.Errorf(`explorer.chart.x %q must be the id of an input or "t"`, chart.X)
		}
		if swept.Kind == "toggle" {
			return fmt.Errorf("explorer.chart.x %q is a toggle; chart a slider or stepper", chart.X)
		}
	}
	for index, series := range chart.Series {
		at := fmt.Sprintf("explorer.chart.series[%d]", index)
		program, err := compileExplorerExpr(at+".expr", series.Expr, seriesTypes)
		if err != nil {
			return err
		}
		if _, err := program.Eval(seriesEnv); err != nil {
			return fmt.Errorf("%s.expr cannot be worked out at the inputs' defaults: %v", at, err)
		}
	}
	return nil
}

func explorerTypes(inputs []ExplorerInput) map[string]expr.Type {
	types := make(map[string]expr.Type, len(inputs))
	for _, input := range inputs {
		types[input.ID] = expr.Number
		if input.Kind == "toggle" {
			types[input.ID] = expr.Bool
		}
	}
	return types
}

func explorerDefaults(inputs []ExplorerInput) map[string]float64 {
	env := make(map[string]float64, len(inputs))
	for _, input := range inputs {
		if input.Default != nil {
			env[input.ID] = *input.Default
		}
	}
	return env
}

// Routine shows a routine (a bound runbook activation). Schedule, status and
// next run come from the routine itself; Items are the latest output
// highlights the agent chose.
type Routine struct {
	Items []RoutineItem `json:"items,omitempty"`
}

type RoutineItem struct {
	Label string `json:"label"`
	Text  string `json:"text"`
}

// Routine, tasks and agent card limits.
const (
	maxRoutineItems      = 14
	maxRoutineLabelRunes = 40
	maxRoutineTextRunes  = 300
	maxTaskRows          = 12
	maxTaskLabelRunes    = 80
	maxAgentPoints       = 4
	maxAgentLineRunes    = 80 // headline and points
)

// Tasks points at work handed to helpers in this chat. Each row is bound to
// a conversation task; assignee, status and result come from the task.
type Tasks struct {
	Rows []TaskRow `json:"rows"`
}

type TaskRow struct {
	Binding Binding `json:"binding"`
	Label   string  `json:"label"`
}

const ruleTaskRows = "each task is listed once"

func validateTasks(v *validator, c Component, _ string) error {
	seen := map[string]bool{}
	for index, row := range c.Tasks.Rows {
		at := fmt.Sprintf("tasks.rows[%d]", index)
		if seen[row.Binding.ID] {
			return fmt.Errorf("%s.binding.id %q is listed twice", at, row.Binding.ID)
		}
		seen[row.Binding.ID] = true
		if err := v.binding(row.Binding); err != nil {
			return fmt.Errorf("%s.binding: %w", at, err)
		}
	}
	return nil
}

// AgentCard introduces another agent people can chat with.
type AgentCard struct {
	Headline string   `json:"headline"`
	Points   []string `json:"points,omitempty"`
}

// formatNumber writes a number in its shortest form without an exponent for
// everyday sizes.
func formatNumber(value float64) string {
	if math.Abs(value) >= 1e15 || value != 0 && math.Abs(value) < 1e-6 {
		return strconv.FormatFloat(value, 'g', -1, 64)
	}
	return strconv.FormatFloat(value, 'f', -1, 64)
}

// RoutineBindings lists the ids of the routines (runbook activations) that
// routine components of surface are bound to, in order and without repeats.
// Hosts link them to the conversation when the visual is stored, so the
// conversation's change stream carries their live state.
func RoutineBindings(surface Surface) []string {
	var ids []string
	seen := map[string]bool{}
	walk(surface.Components, func(component *Component, _ string, _ int) bool {
		if component.Type == "routine" && component.Binding != nil && component.Binding.Kind == BindingRunbook && !seen[component.Binding.ID] {
			seen[component.Binding.ID] = true
			ids = append(ids, component.Binding.ID)
		}
		return true
	})
	return ids
}
