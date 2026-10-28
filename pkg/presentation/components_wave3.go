package presentation

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // agenda time zones validate the same on every host

	"github.com/axiom-studio/openseal/pkg/presentation/expr"
)

// clockTime matches a 24-hour HH:MM time of day.
var clockTime = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)

const clockTimePattern = `^([01][0-9]|2[0-3]):[0-5][0-9]$`

func validClock(field, value string, required bool) error {
	if value == "" && !required {
		return nil
	}
	if !clockTime.MatchString(value) {
		return fmt.Errorf("%s %q must be a 24-hour time as HH:MM", field, value)
	}
	return nil
}

func dateField(field, value string, required bool) error {
	if value == "" && !required {
		return nil
	}
	if !validDate(value) {
		return fmt.Errorf("%s %q must be a date as YYYY-MM-DD", field, value)
	}
	return nil
}

func validateTone(field string, tone Tone) error {
	if tone != "" && !validTone(tone) {
		return fmt.Errorf("%s %q is not one of neutral, accent, positive, negative, warning, info", field, tone)
	}
	return nil
}

func finiteNumber(field string, value float64) error {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return fmt.Errorf("%s must be a finite number", field)
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

func validateAgenda(_ *validator, c Component, _ string) error {
	agenda := c.Agenda
	if agenda == nil {
		return payloadRequired("agenda")
	}
	if agenda.Timezone != "" {
		if len(agenda.Timezone) > 64 || agenda.Timezone == "Local" {
			return fmt.Errorf("agenda.timezone %q must be an IANA time zone such as Europe/London", agenda.Timezone)
		}
		if _, err := time.LoadLocation(agenda.Timezone); err != nil {
			return fmt.Errorf("agenda.timezone %q must be an IANA time zone such as Europe/London", agenda.Timezone)
		}
	}
	if err := firstError(bounded("agenda.days", len(agenda.Days), 1, 7), bounded("agenda.freeSlots", len(agenda.FreeSlots), 0, 12)); err != nil {
		return err
	}
	ids := map[string]bool{}
	dates := map[string]bool{}
	for index, day := range agenda.Days {
		at := fmt.Sprintf("agenda.days[%d]", index)
		if err := firstError(dateField(at+".date", day.Date, true), optional(at+".label", day.Label, 40), bounded(at+".items", len(day.Items), 0, 12)); err != nil {
			return err
		}
		if index > 0 && day.Date <= agenda.Days[index-1].Date {
			return fmt.Errorf("%s.date %s must come after %s: list days in date order, each once", at, day.Date, agenda.Days[index-1].Date)
		}
		dates[day.Date] = true
		for item, entry := range day.Items {
			itemAt := fmt.Sprintf("%s.items[%d]", at, item)
			if err := firstError(itemIDs(ids, itemAt, entry.ID), validClock(itemAt+".start", entry.Start, true), validClock(itemAt+".end", entry.End, false),
				required(itemAt+".title", entry.Title, 80), optional(itemAt+".place", entry.Place, 80), validateTone(itemAt+".tone", entry.Tone)); err != nil {
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
		if err := firstError(dateField(at+".date", slot.Date, true), validClock(at+".start", slot.Start, true), validClock(at+".end", slot.End, true)); err != nil {
			return err
		}
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
}

type Travel struct {
	Mode    string `json:"mode"`
	Minutes int    `json:"minutes"`
}

// TravelModes lists the values of Travel.Mode.
var TravelModes = []string{"walk", "drive", "transit", "bike", "flight", "boat"}

// MaxTravelMinutes bounds one travel leg (a day).
const MaxTravelMinutes = 1440

func validateItinerary(v *validator, c Component, _ string) error {
	itinerary := c.Itinerary
	if itinerary == nil {
		return payloadRequired("itinerary")
	}
	if err := bounded("itinerary.days", len(itinerary.Days), 1, 10); err != nil {
		return err
	}
	days := map[string]bool{}
	items := map[string]bool{}
	for index, day := range itinerary.Days {
		at := fmt.Sprintf("itinerary.days[%d]", index)
		if err := firstError(itemIDs(days, at, day.ID), required(at+".label", day.Label, 40), dateField(at+".date", day.Date, false),
			bounded(at+".items", len(day.Items), 1, 12)); err != nil {
			return err
		}
		for item, entry := range day.Items {
			itemAt := fmt.Sprintf("%s.items[%d]", at, item)
			if err := firstError(itemIDs(items, itemAt, entry.ID), validClock(itemAt+".time", entry.Time, false), required(itemAt+".title", entry.Title, 80),
				optional(itemAt+".place", entry.Place, 80), optional(itemAt+".detail", entry.Detail, 200), v.image(itemAt+".image", entry.Image)); err != nil {
				return err
			}
			if travel := entry.Travel; travel != nil {
				if item == 0 {
					return fmt.Errorf("%s.travel: the first item of a day has no travel leg; travel is how people get here from the item before", itemAt)
				}
				if !contains(TravelModes, travel.Mode) {
					return fmt.Errorf("%s.travel.mode %q is not one of %s", itemAt, travel.Mode, strings.Join(TravelModes, ", "))
				}
				if travel.Minutes < 1 || travel.Minutes > MaxTravelMinutes {
					return fmt.Errorf("%s.travel.minutes must be 1-%d (got %d)", itemAt, MaxTravelMinutes, travel.Minutes)
				}
			}
		}
	}
	return nil
}

// Place is one venue: a restaurant, a museum, a hotel.
type Place struct {
	Name       string      `json:"name"`
	Category   string      `json:"category,omitempty"`
	Image      *ImageRef   `json:"image,omitempty"`
	Address    string      `json:"address,omitempty"`
	Rating     *ItemRating `json:"rating,omitempty"`
	PriceLevel int         `json:"priceLevel,omitempty"`
	Hours      []string    `json:"hours,omitempty"`
	Facts      []string    `json:"facts,omitempty"`
}

func validateRating(field string, rating *ItemRating) error {
	if rating == nil {
		return nil
	}
	if math.IsNaN(rating.Value) || rating.Value < 0 || rating.Value > 5 {
		return fmt.Errorf("%s.value must be 0-5", field)
	}
	if rating.Count != nil && *rating.Count < 0 {
		return fmt.Errorf("%s.count must be 0 or more", field)
	}
	return nil
}

func validatePlace(v *validator, c Component, _ string) error {
	place := c.Place
	if place == nil {
		return payloadRequired("place")
	}
	if err := firstError(required("place.name", place.Name, 80), optional("place.category", place.Category, 30), v.image("place.image", place.Image),
		optional("place.address", place.Address, 160), validateRating("place.rating", place.Rating),
		bounded("place.hours", len(place.Hours), 0, 7), bounded("place.facts", len(place.Facts), 0, 4)); err != nil {
		return err
	}
	if place.PriceLevel != 0 && (place.PriceLevel < 1 || place.PriceLevel > 4) {
		return fmt.Errorf("place.priceLevel must be 1-4 (got %d)", place.PriceLevel)
	}
	for index, hours := range place.Hours {
		if err := required(fmt.Sprintf("place.hours[%d]", index), hours, 40); err != nil {
			return err
		}
	}
	for index, fact := range place.Facts {
		if err := required(fmt.Sprintf("place.facts[%d]", index), fact, 60); err != nil {
			return err
		}
	}
	return nil
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

func validateBreakdown(v *validator, c Component, _ string) error {
	breakdown := c.Breakdown
	if breakdown == nil {
		return payloadRequired("breakdown")
	}
	if !contains(BreakdownKinds, breakdown.Kind) {
		return fmt.Errorf("breakdown.kind %q is not one of %s", breakdown.Kind, strings.Join(BreakdownKinds, ", "))
	}
	if err := firstError(validateFormat("breakdown", breakdown.Format, breakdown.Currency), optional("breakdown.unit", breakdown.Unit, 24),
		bounded("breakdown.items", len(breakdown.Items), 2, 12)); err != nil {
		return err
	}
	seen := map[string]bool{}
	positive := false
	for index, item := range breakdown.Items {
		at := fmt.Sprintf("breakdown.items[%d]", index)
		if err := firstError(itemIDs(seen, at, item.ID), required(at+".label", item.Label, 40), finiteNumber(at+".value", item.Value),
			v.icon(at+".icon", c.ID, item.Icon)); err != nil {
			return err
		}
		if item.Value < 0 {
			return fmt.Errorf("%s.value must be 0 or more", at)
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
	progress := c.Progress
	if progress == nil {
		return payloadRequired("progress")
	}
	if !contains(ProgressKinds, progress.Kind) {
		return fmt.Errorf("progress.kind %q is not one of %s", progress.Kind, strings.Join(ProgressKinds, ", "))
	}
	if err := bounded("progress.items", len(progress.Items), 1, 6); err != nil {
		return err
	}
	seen := map[string]bool{}
	for index, item := range progress.Items {
		at := fmt.Sprintf("progress.items[%d]", index)
		if err := firstError(itemIDs(seen, at, item.ID), required(at+".label", item.Label, 40), finiteNumber(at+".value", item.Value),
			finiteNumber(at+".target", item.Target), validateFormat(at, item.Format, item.Currency), optional(at+".unit", item.Unit, 24)); err != nil {
			return err
		}
		if item.Value < 0 {
			return fmt.Errorf("%s.value must be 0 or more", at)
		}
		if item.Target <= 0 {
			return fmt.Errorf("%s.target must be above 0", at)
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

func validatePoll(v *validator, c Component, _ string) error {
	poll := c.Poll
	if poll == nil {
		return payloadRequired("poll")
	}
	if err := firstError(required("poll.question", poll.Question, 200), bounded("poll.options", len(poll.Options), 2, 8)); err != nil {
		return err
	}
	seen := map[string]bool{}
	for index, option := range poll.Options {
		at := fmt.Sprintf("poll.options[%d]", index)
		if err := firstError(itemIDs(seen, at, option.ID), required(at+".label", option.Label, 60), v.image(at+".image", option.Image)); err != nil {
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
)

// explorerName is an input id that expressions can use as a name.
var explorerName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)

const explorerNamePattern = `^[a-z][a-z0-9_]{0,39}$`

func validateExplorer(_ *validator, c Component, _ string) error {
	explorer := c.Explorer
	if explorer == nil {
		return payloadRequired("explorer")
	}
	if err := firstError(bounded("explorer.inputs", len(explorer.Inputs), 1, 4), bounded("explorer.outputs", len(explorer.Outputs), 1, 4)); err != nil {
		return err
	}
	inputs := map[string]bool{}
	reserved := map[string]bool{ExplorerTime: true}
	for _, name := range expr.FunctionNames() {
		reserved[name] = true
	}
	for index, input := range explorer.Inputs {
		at := fmt.Sprintf("explorer.inputs[%d]", index)
		if !explorerName.MatchString(input.ID) {
			return fmt.Errorf("%s.id %q must match %s: expressions use it as a name", at, input.ID, explorerNamePattern)
		}
		if reserved[input.ID] {
			return fmt.Errorf("%s.id %q is reserved (t and the function names); pick another", at, input.ID)
		}
		if err := itemIDs(inputs, at, input.ID); err != nil {
			return err
		}
		if err := firstError(required(at+".label", input.Label, 40), optional(at+".unit", input.Unit, 16), validateFormat(at, input.Format, input.Currency)); err != nil {
			return err
		}
		if err := validateExplorerInput(at, input); err != nil {
			return err
		}
	}
	types := explorerTypes(explorer.Inputs)
	env := explorerDefaults(explorer.Inputs)
	outputs := map[string]bool{}
	for index, output := range explorer.Outputs {
		at := fmt.Sprintf("explorer.outputs[%d]", index)
		if err := firstError(itemIDs(outputs, at, output.ID), required(at+".label", output.Label, 40), optional(at+".unit", output.Unit, 16),
			validateFormat(at, output.Format, output.Currency)); err != nil {
			return err
		}
		if output.Digits != nil && (*output.Digits < 0 || *output.Digits > MaxExplorerDigits) {
			return fmt.Errorf("%s.digits must be 0-%d", at, MaxExplorerDigits)
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
	if input.Default == nil {
		return fmt.Errorf("%s.default is required", at)
	}
	if err := finiteNumber(at+".default", *input.Default); err != nil {
		return err
	}
	switch input.Kind {
	case "toggle":
		if input.Min != nil || input.Max != nil || input.Step != nil {
			return fmt.Errorf("%s is a toggle and takes no min, max or step", at)
		}
		if *input.Default != 0 && *input.Default != 1 {
			return fmt.Errorf("%s.default must be 0 (off) or 1 (on) for a toggle", at)
		}
		return nil
	case "slider", "stepper":
	default:
		return fmt.Errorf("%s.kind %q is not one of %s", at, input.Kind, strings.Join(ExplorerInputKinds, ", "))
	}
	if input.Min == nil || input.Max == nil || input.Step == nil {
		return fmt.Errorf("%s is a %s and needs min, max and step", at, input.Kind)
	}
	if err := firstError(finiteNumber(at+".min", *input.Min), finiteNumber(at+".max", *input.Max), finiteNumber(at+".step", *input.Step)); err != nil {
		return err
	}
	if *input.Min >= *input.Max {
		return fmt.Errorf("%s.min must be below max", at)
	}
	if *input.Step <= 0 {
		return fmt.Errorf("%s.step must be above 0", at)
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
	if err := required(field, source, MaxExplorerExprRunes); err != nil {
		return nil, err
	}
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
	if err := bounded("explorer.chart.series", len(chart.Series), 1, 3); err != nil {
		return err
	}
	if chart.Points != nil && (*chart.Points < 2 || *chart.Points > MaxExplorerPoints) {
		return fmt.Errorf("explorer.chart.points must be 2-%d", MaxExplorerPoints)
	}
	seriesTypes := types
	seriesEnv := env
	if chart.X == ExplorerTime {
		t := chart.T
		if t == nil {
			return errors.New(`explorer.chart.t {from, to} is required when x is "t"`)
		}
		if err := firstError(finiteNumber("explorer.chart.t.from", t.From), finiteNumber("explorer.chart.t.to", t.To), optional("explorer.chart.t.label", t.Label, 30)); err != nil {
			return err
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
		if err := required(at+".label", series.Label, 30); err != nil {
			return err
		}
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

func validateRoutine(_ *validator, c Component, _ string) error {
	if c.Routine == nil {
		return nil
	}
	if err := bounded("routine.items", len(c.Routine.Items), 0, 14); err != nil {
		return err
	}
	for index, item := range c.Routine.Items {
		at := fmt.Sprintf("routine.items[%d]", index)
		if err := firstError(required(at+".label", item.Label, 40), required(at+".text", item.Text, 300)); err != nil {
			return err
		}
	}
	return nil
}

// Tasks points at work handed to helpers in this chat. Each row is bound to
// a conversation task; assignee, status and result come from the task.
type Tasks struct {
	Rows []TaskRow `json:"rows"`
}

type TaskRow struct {
	Binding Binding `json:"binding"`
	Label   string  `json:"label"`
}

func validateTasks(v *validator, c Component, _ string) error {
	if c.Tasks == nil {
		return payloadRequired("tasks")
	}
	if err := bounded("tasks.rows", len(c.Tasks.Rows), 1, 12); err != nil {
		return err
	}
	seen := map[string]bool{}
	for index, row := range c.Tasks.Rows {
		at := fmt.Sprintf("tasks.rows[%d]", index)
		if err := required(at+".label", row.Label, 80); err != nil {
			return err
		}
		if row.Binding.Kind != BindingConversationTask {
			return fmt.Errorf("%s.binding.kind %q is not allowed; each row is bound to a conversation_task", at, row.Binding.Kind)
		}
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

func validateAgentCard(_ *validator, c Component, _ string) error {
	if c.Agent == nil {
		return payloadRequired("agent")
	}
	if err := firstError(required("agent.headline", c.Agent.Headline, 80), bounded("agent.points", len(c.Agent.Points), 0, 4)); err != nil {
		return err
	}
	for index, point := range c.Agent.Points {
		if err := required(fmt.Sprintf("agent.points[%d]", index), point, 80); err != nil {
			return err
		}
	}
	return nil
}

// formatNumber writes a number in its shortest form without an exponent for
// everyday sizes.
func formatNumber(value float64) string {
	if math.Abs(value) >= 1e15 || value != 0 && math.Abs(value) < 1e-6 {
		return strconv.FormatFloat(value, 'g', -1, 64)
	}
	return strconv.FormatFloat(value, 'f', -1, 64)
}
