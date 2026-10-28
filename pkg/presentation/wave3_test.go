package presentation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func agendaOf(days ...AgendaDay) Component {
	return Component{ID: "a", Type: "agenda", Summary: "x", Agenda: &Agenda{Days: days}}
}

func agendaDay(date string, items ...AgendaItem) AgendaDay {
	return AgendaDay{Date: date, Items: items}
}

func slot(id, start, end string) AgendaItem {
	return AgendaItem{ID: id, Start: start, End: end, Title: strings.ToUpper(id)}
}

func itineraryOf(days ...ItineraryDay) Component {
	return Component{ID: "i", Type: "itinerary", Summary: "x", Itinerary: &Itinerary{Days: days}}
}

func stops(day string, ids ...string) ItineraryDay {
	items := make([]ItineraryItem, len(ids))
	for index, id := range ids {
		items[index] = ItineraryItem{ID: id, Title: strings.ToUpper(id)}
		if index > 0 {
			items[index].Travel = &Travel{Mode: "walk", Minutes: 10}
		}
	}
	return ItineraryDay{ID: day, Label: strings.ToUpper(day), Items: items}
}

func breakdownOf(values ...float64) Component {
	items := make([]BreakdownItem, len(values))
	for index, value := range values {
		items[index] = BreakdownItem{ID: fmt.Sprintf("b%d", index), Label: fmt.Sprintf("Part %d", index), Value: value}
	}
	return Component{ID: "b", Type: "breakdown", Summary: "x", Breakdown: &Breakdown{Kind: "treemap", Items: items}}
}

func progressOf(items ...ProgressItem) Component {
	return Component{ID: "p", Type: "progress", Summary: "x", Progress: &Progress{Kind: "ring", Items: items}}
}

func pollOf(multiple bool, ids ...string) Component {
	options := make([]PollOption, len(ids))
	for index, id := range ids {
		options[index] = PollOption{ID: id, Label: strings.ToUpper(id[:1]) + id[1:]}
	}
	return Component{ID: "vote", Type: "poll", Title: "Dinner", Summary: "x", Poll: &Poll{Question: "Where shall we eat?", Options: options, Multiple: multiple}}
}

func slider(id string, low, high, step, value float64) ExplorerInput {
	return ExplorerInput{ID: id, Label: strings.ToUpper(id), Kind: "slider", Min: num(low), Max: num(high), Step: num(step), Default: num(value)}
}

func explorerOf(inputs []ExplorerInput, outputs ...ExplorerOutput) Component {
	return Component{ID: "e", Type: "explorer", Title: "Savings", Summary: "x", Explorer: &Explorer{Inputs: inputs, Outputs: outputs}}
}

func output(id, expression string) ExplorerOutput {
	return ExplorerOutput{ID: id, Label: strings.ToUpper(id[:1]) + id[1:], Expr: expression}
}

func interest() Component {
	return explorerOf([]ExplorerInput{slider("deposit", 100, 20000, 100, 10000), slider("rate", 0.5, 10, 0.5, 5), {ID: "years", Label: "Years", Kind: "stepper", Min: num(1), Max: num(40), Step: num(1), Default: num(10)}},
		ExplorerOutput{ID: "balance", Label: "Final balance", Expr: "deposit * (1 + rate / 100) ^ years", Digits: minutes(2)})
}

func routineOf(binding *Binding, items ...RoutineItem) Component {
	return Component{ID: "r", Type: "routine", Title: "Morning news", Summary: "x", Binding: binding, Routine: &Routine{Items: items}}
}

func tasksOf(ids ...string) Component {
	rows := make([]TaskRow, len(ids))
	for index, id := range ids {
		rows[index] = TaskRow{Binding: Binding{Kind: BindingConversationTask, ID: id}, Label: "Task " + id}
	}
	return Component{ID: "t", Type: "tasks", Summary: "x", Tasks: &Tasks{Rows: rows}}
}

func agentOf(binding *Binding) Component {
	return Component{ID: "g", Type: "agent", Summary: "x", Binding: binding, Agent: &AgentCard{Headline: "Plans trips", Points: []string{"Finds trains"}}}
}

// resolver allows every binding and records what it was asked.
type resolver struct{ seen []Binding }

func (r *resolver) ResolveBinding(_ context.Context, binding Binding) error {
	r.seen = append(r.seen, binding)
	if binding.ID == "elsewhere" {
		return errors.New("is not part of this conversation")
	}
	return nil
}

func TestValidateWave3(t *testing.T) {
	bindings := ValidateOptions{Bindings: &resolver{}}
	withChart := func(chart *ExplorerChart) Component {
		component := interest()
		component.Explorer.Chart = chart
		return component
	}
	cases := []struct {
		name      string
		component Component
		opts      ValidateOptions
		err       string
	}{
		{"summary", Component{ID: "a", Type: "agenda", Agenda: agendaOf(agendaDay("2026-11-02")).Agenda}, ValidateOptions{}, "summary is required for agenda"},
		{"payload", Component{ID: "a", Type: "poll", Summary: "x"}, ValidateOptions{}, "poll payload is required"},
		{"foreign payload", Component{ID: "a", Type: "place", Summary: "x", Place: &Place{Name: "Cafe"}, Poll: pollOf(false, "a", "b").Poll}, ValidateOptions{}, `place cannot carry a "poll" payload`},
		// agenda
		{"agenda", agendaOf(agendaDay("2026-11-02", slot("gym", "07:00", "08:00"), slot("work", "09:00", "")), agendaDay("2026-11-03")), ValidateOptions{}, ""},
		{"agenda timezone", func() Component {
			c := agendaOf(agendaDay("2026-11-02"))
			c.Agenda.Timezone = "Europe/London"
			return c
		}(), ValidateOptions{}, ""},
		{"agenda bad timezone", func() Component { c := agendaOf(agendaDay("2026-11-02")); c.Agenda.Timezone = "Mars/Base"; return c }(), ValidateOptions{}, `agenda.timezone "Mars/Base" must be an IANA time zone`},
		{"agenda local timezone", func() Component { c := agendaOf(agendaDay("2026-11-02")); c.Agenda.Timezone = "Local"; return c }(), ValidateOptions{}, "agenda.timezone"},
		{"agenda no days", agendaOf(), ValidateOptions{}, "agenda.days needs 1-7 (got 0)"},
		{"agenda 8 days", agendaOf(agendaDay("2026-11-01"), agendaDay("2026-11-02"), agendaDay("2026-11-03"), agendaDay("2026-11-04"), agendaDay("2026-11-05"), agendaDay("2026-11-06"), agendaDay("2026-11-07"), agendaDay("2026-11-08")), ValidateOptions{}, "agenda.days needs 1-7 (got 8)"},
		{"agenda date", agendaOf(agendaDay("2 Nov")), ValidateOptions{}, `agenda.days[0].date "2 Nov" must be a date`},
		{"agenda days order", agendaOf(agendaDay("2026-11-03"), agendaDay("2026-11-02")), ValidateOptions{}, "agenda.days[1].date 2026-11-02 must come after 2026-11-03"},
		{"agenda duplicate day", agendaOf(agendaDay("2026-11-02"), agendaDay("2026-11-02")), ValidateOptions{}, "agenda.days[1].date"},
		{"agenda time", agendaOf(agendaDay("2026-11-02", slot("gym", "7am", ""))), ValidateOptions{}, `agenda.days[0].items[0].start "7am" must be a 24-hour time as HH:MM`},
		{"agenda 24:00", agendaOf(agendaDay("2026-11-02", slot("gym", "24:00", ""))), ValidateOptions{}, "must be a 24-hour time"},
		{"agenda end before start", agendaOf(agendaDay("2026-11-02", slot("gym", "08:00", "07:30"))), ValidateOptions{}, "agenda.days[0].items[0].end 07:30 must be after start 08:00"},
		{"agenda order", agendaOf(agendaDay("2026-11-02", slot("work", "09:00", ""), slot("gym", "07:00", ""))), ValidateOptions{}, "agenda.days[0].items[1].start 07:00 is before the item above it"},
		{"agenda same start", agendaOf(agendaDay("2026-11-02", slot("a", "09:00", ""), slot("b", "09:00", ""))), ValidateOptions{}, ""},
		{"agenda duplicate id", agendaOf(agendaDay("2026-11-02", slot("a", "09:00", "")), agendaDay("2026-11-03", slot("a", "09:00", ""))), ValidateOptions{}, `agenda.days[1].items[0].id "a" is used twice`},
		{"agenda 13 items", agendaOf(agendaDay("2026-11-02", func() []AgendaItem {
			items := make([]AgendaItem, 13)
			for index := range items {
				items[index] = slot(fmt.Sprintf("i%d", index), fmt.Sprintf("%02d:00", index+1), "")
			}
			return items
		}()...)), ValidateOptions{}, "agenda.days[0].items allows at most 12 (got 13)"},
		{"agenda tone", agendaOf(agendaDay("2026-11-02", AgendaItem{ID: "a", Start: "09:00", Title: "A", Tone: "red"})), ValidateOptions{}, `agenda.days[0].items[0].tone "red"`},
		{"agenda free slot", func() Component {
			c := agendaOf(agendaDay("2026-11-02"))
			c.Agenda.FreeSlots = []AgendaFreeSlot{{Date: "2026-11-02", Start: "12:00", End: "13:00"}}
			return c
		}(), ValidateOptions{}, ""},
		{"agenda free slot day", func() Component {
			c := agendaOf(agendaDay("2026-11-02"))
			c.Agenda.FreeSlots = []AgendaFreeSlot{{Date: "2026-11-05", Start: "12:00", End: "13:00"}}
			return c
		}(), ValidateOptions{}, "agenda.freeSlots[0].date 2026-11-05 is not the date of one of agenda.days"},
		{"agenda free slot times", func() Component {
			c := agendaOf(agendaDay("2026-11-02"))
			c.Agenda.FreeSlots = []AgendaFreeSlot{{Date: "2026-11-02", Start: "13:00", End: "12:00"}}
			return c
		}(), ValidateOptions{}, "agenda.freeSlots[0].end 12:00 must be after start 13:00"},
		// itinerary
		{"itinerary", itineraryOf(stops("sat", "pier", "lanes"), stops("sun", "brunch")), ValidateOptions{}, ""},
		{"itinerary no days", itineraryOf(), ValidateOptions{}, "itinerary.days needs 1-10 (got 0)"},
		{"itinerary empty day", itineraryOf(ItineraryDay{ID: "sat", Label: "Sat"}), ValidateOptions{}, "itinerary.days[0].items needs 1-12 (got 0)"},
		{"itinerary label", itineraryOf(ItineraryDay{ID: "sat", Items: stops("x", "a").Items}), ValidateOptions{}, "itinerary.days[0].label"},
		{"itinerary first travel", func() Component {
			c := itineraryOf(stops("sat", "pier", "lanes"))
			c.Itinerary.Days[0].Items[0].Travel = &Travel{Mode: "walk", Minutes: 5}
			return c
		}(), ValidateOptions{}, "itinerary.days[0].items[0].travel: the first item of a day has no travel leg"},
		{"itinerary mode", func() Component {
			c := itineraryOf(stops("sat", "pier", "lanes"))
			c.Itinerary.Days[0].Items[1].Travel.Mode = "teleport"
			return c
		}(), ValidateOptions{}, `itinerary.days[0].items[1].travel.mode "teleport" is not one of walk`},
		{"itinerary minutes", func() Component {
			c := itineraryOf(stops("sat", "pier", "lanes"))
			c.Itinerary.Days[0].Items[1].Travel.Minutes = 1441
			return c
		}(), ValidateOptions{}, "itinerary.days[0].items[1].travel.minutes must be 1-1440 (got 1441)"},
		{"itinerary time", func() Component {
			c := itineraryOf(stops("sat", "pier"))
			c.Itinerary.Days[0].Items[0].Time = "9:00"
			return c
		}(), ValidateOptions{}, `itinerary.days[0].items[0].time "9:00"`},
		{"itinerary detail", func() Component {
			c := itineraryOf(stops("sat", "pier"))
			c.Itinerary.Days[0].Items[0].Detail = strings.Repeat("d", 201)
			return c
		}(), ValidateOptions{}, "itinerary.days[0].items[0].detail must be at most 200"},
		{"itinerary image", func() Component {
			c := itineraryOf(stops("sat", "pier"))
			c.Itinerary.Days[0].Items[0].Image = img("pier")
			return c
		}(), images("beach"), "itinerary.days[0].items[0].image (image pier version 1): is not an image in this chat"},
		{"itinerary duplicate item", itineraryOf(stops("sat", "pier"), stops("sun", "pier")), ValidateOptions{}, `itinerary.days[1].items[0].id "pier" is used twice`},
		// place
		{"place", Component{ID: "p", Type: "place", Summary: "x", Place: &Place{Name: "Cafe", Category: "Café", Address: "1 High St", Rating: &ItemRating{Value: 4.5, Count: minutes(120)}, PriceLevel: 2, Hours: []string{"Mon-Fri 8-18"}, Facts: []string{"Dog friendly"}}}, ValidateOptions{}, ""},
		{"place name", Component{ID: "p", Type: "place", Summary: "x", Place: &Place{}}, ValidateOptions{}, "place.name must contain between 1 and 80"},
		{"place price level", Component{ID: "p", Type: "place", Summary: "x", Place: &Place{Name: "Cafe", PriceLevel: 5}}, ValidateOptions{}, "place.priceLevel must be 1-4 (got 5)"},
		{"place rating", Component{ID: "p", Type: "place", Summary: "x", Place: &Place{Name: "Cafe", Rating: &ItemRating{Value: 6}}}, ValidateOptions{}, "place.rating.value must be 0-5"},
		{"place hours", Component{ID: "p", Type: "place", Summary: "x", Place: &Place{Name: "Cafe", Hours: make([]string, 8)}}, ValidateOptions{}, "place.hours allows at most 7 (got 8)"},
		{"place facts", Component{ID: "p", Type: "place", Summary: "x", Place: &Place{Name: "Cafe", Facts: []string{strings.Repeat("f", 61)}}}, ValidateOptions{}, "place.facts[0] must contain between 1 and 60"},
		// breakdown
		{"breakdown", breakdownOf(3, 0, 1), ValidateOptions{}, ""},
		{"breakdown one item", breakdownOf(3), ValidateOptions{}, "breakdown.items needs 2-12 (got 1)"},
		{"breakdown kind", func() Component { c := breakdownOf(1, 2); c.Breakdown.Kind = "pie"; return c }(), ValidateOptions{}, `breakdown.kind "pie" is not one of treemap, waffle, stacked_bar`},
		{"breakdown negative", breakdownOf(1, -2), ValidateOptions{}, "breakdown.items[1].value must be 0 or more"},
		{"breakdown all zero", breakdownOf(0, 0), ValidateOptions{}, "at least one value must be above 0"},
		{"breakdown currency", func() Component { c := breakdownOf(1, 2); c.Breakdown.Format = StatCurrency; return c }(), ValidateOptions{}, "breakdown.currency is required with format currency"},
		// progress
		{"progress", progressOf(ProgressItem{ID: "run", Label: "Running", Value: 120, Target: 100, Unit: "km"}), ValidateOptions{}, ""},
		{"progress kind", func() Component {
			c := progressOf(ProgressItem{ID: "a", Label: "A", Target: 1})
			c.Progress.Kind = "gauge"
			return c
		}(), ValidateOptions{}, `progress.kind "gauge"`},
		{"progress target", progressOf(ProgressItem{ID: "a", Label: "A", Value: 1}), ValidateOptions{}, "progress.items[0].target must be above 0"},
		{"progress value", progressOf(ProgressItem{ID: "a", Label: "A", Value: -1, Target: 1}), ValidateOptions{}, "progress.items[0].value must be 0 or more"},
		{"progress items", progressOf(), ValidateOptions{}, "progress.items needs 1-6 (got 0)"},
		{"progress currency", progressOf(ProgressItem{ID: "a", Label: "A", Target: 1, Currency: "GBP"}), ValidateOptions{}, "progress.items[0].currency is only used with format currency"},
		// poll
		{"poll", pollOf(false, "thai", "pizza"), ValidateOptions{}, ""},
		{"poll one option", pollOf(false, "thai"), ValidateOptions{}, "poll.options needs 2-8 (got 1)"},
		{"poll nine options", pollOf(false, "a", "b", "c", "d", "e", "f", "g", "h", "i"), ValidateOptions{}, "poll.options needs 2-8 (got 9)"},
		{"poll closes", func() Component { c := pollOf(false, "a", "b"); c.Poll.ClosesAt = "2026-11-01T18:00:00Z"; return c }(), ValidateOptions{}, ""},
		{"poll closes format", func() Component { c := pollOf(false, "a", "b"); c.Poll.ClosesAt = "Saturday 6pm"; return c }(), ValidateOptions{}, `poll.closesAt "Saturday 6pm" must be an RFC 3339 time`},
		{"poll question", func() Component { c := pollOf(false, "a", "b"); c.Poll.Question = strings.Repeat("q", 201); return c }(), ValidateOptions{}, "poll.question must contain between 1 and 200"},
		// explorer
		{"explorer", interest(), ValidateOptions{}, ""},
		{"explorer toggle", explorerOf([]ExplorerInput{slider("price", 1, 10, 1, 5), {ID: "member", Label: "Member", Kind: "toggle", Default: num(1)}}, output("total", "if(member, price * 0.9, price)")), ValidateOptions{}, ""},
		{"explorer toggle range", explorerOf([]ExplorerInput{{ID: "member", Label: "Member", Kind: "toggle", Default: num(1), Max: num(1)}}, output("total", "if(member, 1, 2)")), ValidateOptions{}, "explorer.inputs[0] is a toggle and takes no min, max or step"},
		{"explorer toggle default", explorerOf([]ExplorerInput{{ID: "member", Label: "Member", Kind: "toggle", Default: num(2)}}, output("total", "if(member, 1, 2)")), ValidateOptions{}, "explorer.inputs[0].default must be 0 (off) or 1 (on)"},
		{"explorer no inputs", explorerOf(nil, output("total", "1")), ValidateOptions{}, "explorer.inputs needs 1-4 (got 0)"},
		{"explorer five outputs", explorerOf([]ExplorerInput{slider("x", 0, 1, 1, 0)}, output("a", "x"), output("b", "x"), output("c", "x"), output("d", "x"), output("e", "x")), ValidateOptions{}, "explorer.outputs needs 1-4 (got 5)"},
		{"explorer id", explorerOf([]ExplorerInput{slider("my-rate", 0, 1, 1, 0)}, output("a", "1")), ValidateOptions{}, `explorer.inputs[0].id "my-rate" must match ^[a-z][a-z0-9_]{0,39}$`},
		{"explorer reserved id", explorerOf([]ExplorerInput{slider("t", 0, 1, 1, 0)}, output("a", "1")), ValidateOptions{}, `explorer.inputs[0].id "t" is reserved`},
		{"explorer function id", explorerOf([]ExplorerInput{slider("max", 0, 1, 1, 0)}, output("a", "1")), ValidateOptions{}, `explorer.inputs[0].id "max" is reserved`},
		{"explorer duplicate id", explorerOf([]ExplorerInput{slider("x", 0, 1, 1, 0), slider("x", 0, 1, 1, 0)}, output("a", "x")), ValidateOptions{}, `explorer.inputs[1].id "x" is used twice`},
		{"explorer kind", explorerOf([]ExplorerInput{{ID: "x", Label: "X", Kind: "dial", Default: num(0)}}, output("a", "x")), ValidateOptions{}, `explorer.inputs[0].kind "dial"`},
		{"explorer default missing", explorerOf([]ExplorerInput{{ID: "x", Label: "X", Kind: "slider", Min: num(0), Max: num(1), Step: num(1)}}, output("a", "x")), ValidateOptions{}, "explorer.inputs[0].default is required"},
		{"explorer range", explorerOf([]ExplorerInput{slider("x", 5, 5, 1, 5)}, output("a", "x")), ValidateOptions{}, "explorer.inputs[0].min must be below max"},
		{"explorer step", explorerOf([]ExplorerInput{slider("x", 0, 5, 0, 0)}, output("a", "x")), ValidateOptions{}, "explorer.inputs[0].step must be above 0"},
		{"explorer missing step", explorerOf([]ExplorerInput{{ID: "x", Label: "X", Kind: "stepper", Min: num(0), Max: num(1), Default: num(0)}}, output("a", "x")), ValidateOptions{}, "explorer.inputs[0] is a stepper and needs min, max and step"},
		{"explorer default range", explorerOf([]ExplorerInput{slider("x", 0, 5, 1, 6)}, output("a", "x")), ValidateOptions{}, "explorer.inputs[0].default 6 is outside 0-5"},
		{"explorer default step", explorerOf([]ExplorerInput{slider("x", 0, 5, 2, 3)}, output("a", "x")), ValidateOptions{}, "explorer.inputs[0].default 3 is not on a step of 2 from 0"},
		{"explorer decimal step", explorerOf([]ExplorerInput{slider("x", 0.1, 1, 0.1, 0.7)}, output("a", "x")), ValidateOptions{}, ""},
		{"explorer syntax", explorerOf([]ExplorerInput{slider("x", 0, 5, 1, 1)}, output("a", "x +")), ValidateOptions{}, "explorer.outputs[0].expr: syntax error"},
		{"explorer unknown name", explorerOf([]ExplorerInput{slider("x", 0, 5, 1, 1)}, output("a", "y * 2")), ValidateOptions{}, `explorer.outputs[0].expr: type error at 0: unknown name "y"`},
		{"explorer bool output", explorerOf([]ExplorerInput{slider("x", 0, 5, 1, 1)}, output("a", "x > 2")), ValidateOptions{}, "explorer.outputs[0].expr must give a number"},
		{"explorer long expr", explorerOf([]ExplorerInput{slider("x", 0, 5, 1, 1)}, output("a", "x"+strings.Repeat(" + x", 75))), ValidateOptions{}, "explorer.outputs[0].expr must contain between 1 and 300"},
		{"explorer not finite", explorerOf([]ExplorerInput{slider("x", 0, 5, 1, 0)}, output("a", "1 / x")), ValidateOptions{}, "explorer.outputs[0].expr cannot be worked out at the inputs' defaults"},
		{"explorer digits", func() Component { c := interest(); c.Explorer.Outputs[0].Digits = minutes(7); return c }(), ValidateOptions{}, "explorer.outputs[0].digits must be 0-6"},
		{"explorer output id", explorerOf([]ExplorerInput{slider("x", 0, 5, 1, 1)}, output("A", "x")), ValidateOptions{}, `explorer.outputs[0].id "A" must match`},
		{"explorer chart t", withChart(&ExplorerChart{X: "t", T: &ExplorerRange{From: 0, To: 40}, Series: []ExplorerSeries{{Label: "Balance", Expr: "deposit * (1 + rate / 100) ^ t"}}}), ValidateOptions{}, ""},
		{"explorer chart input", withChart(&ExplorerChart{X: "years", Points: minutes(40), Series: []ExplorerSeries{{Label: "Balance", Expr: "deposit * (1 + rate / 100) ^ years"}}}), ValidateOptions{}, ""},
		{"explorer chart t missing", withChart(&ExplorerChart{X: "t", Series: []ExplorerSeries{{Label: "B", Expr: "t"}}}), ValidateOptions{}, `explorer.chart.t {from, to} is required when x is "t"`},
		{"explorer chart t range", withChart(&ExplorerChart{X: "t", T: &ExplorerRange{From: 5, To: 5}, Series: []ExplorerSeries{{Label: "B", Expr: "t"}}}), ValidateOptions{}, "explorer.chart.t.from must be below to"},
		{"explorer chart t unused", withChart(&ExplorerChart{X: "years", T: &ExplorerRange{From: 0, To: 5}, Series: []ExplorerSeries{{Label: "B", Expr: "years"}}}), ValidateOptions{}, `explorer.chart.t is only used when x is "t"`},
		{"explorer chart t without t", withChart(&ExplorerChart{X: "years", Series: []ExplorerSeries{{Label: "B", Expr: "t"}}}), ValidateOptions{}, `explorer.chart.series[0].expr: type error at 0: unknown name "t"`},
		{"explorer chart x", withChart(&ExplorerChart{X: "month", Series: []ExplorerSeries{{Label: "B", Expr: "1"}}}), ValidateOptions{}, `explorer.chart.x "month" must be the id of an input or "t"`},
		{"explorer chart toggle", func() Component {
			c := explorerOf([]ExplorerInput{slider("price", 1, 10, 1, 5), {ID: "member", Label: "Member", Kind: "toggle", Default: num(1)}}, output("total", "price"))
			c.Explorer.Chart = &ExplorerChart{X: "member", Series: []ExplorerSeries{{Label: "B", Expr: "price"}}}
			return c
		}(), ValidateOptions{}, `explorer.chart.x "member" is a toggle`},
		{"explorer chart points", withChart(&ExplorerChart{X: "years", Points: minutes(62), Series: []ExplorerSeries{{Label: "B", Expr: "years"}}}), ValidateOptions{}, "explorer.chart.points must be 2-61"},
		{"explorer chart series", withChart(&ExplorerChart{X: "years"}), ValidateOptions{}, "explorer.chart.series needs 1-3 (got 0)"},
		// routine
		{"routine", routineOf(&Binding{Kind: BindingRunbook, ID: "act-1"}, RoutineItem{Label: "Top story", Text: "Rates held"}), bindings, ""},
		{"routine without payload", Component{ID: "r", Type: "routine", Summary: "x", Binding: &Binding{Kind: BindingRunbook, ID: "act-1"}}, bindings, ""},
		{"routine unbound", routineOf(nil), bindings, "binding is required: {kind: runbook, id} names the routine this card shows"},
		{"routine wrong kind", routineOf(&Binding{Kind: BindingAgent, ID: "a"}), bindings, `binding.kind "agent" is not allowed on routine; use runbook`},
		{"routine not visible", routineOf(&Binding{Kind: BindingRunbook, ID: "elsewhere"}), bindings, "binding: runbook/elsewhere: is not part of this conversation"},
		{"routine items", routineOf(&Binding{Kind: BindingRunbook, ID: "act-1"}, make([]RoutineItem, 15)...), bindings, "routine.items allows at most 14 (got 15)"},
		{"routine item text", routineOf(&Binding{Kind: BindingRunbook, ID: "act-1"}, RoutineItem{Label: "A"}), bindings, "routine.items[0].text"},
		// tasks
		{"tasks", tasksOf("task-1", "task-2"), bindings, ""},
		{"tasks rows", tasksOf(), bindings, "tasks.rows needs 1-12 (got 0)"},
		{"tasks 13 rows", tasksOf("1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11", "12", "13"), bindings, "tasks.rows needs 1-12 (got 13)"},
		{"tasks row kind", func() Component { c := tasksOf("task-1"); c.Tasks.Rows[0].Binding.Kind = BindingRun; return c }(), bindings, `tasks.rows[0].binding.kind "run" is not allowed; each row is bound to a conversation_task`},
		{"tasks duplicate", tasksOf("task-1", "task-1"), bindings, `tasks.rows[1].binding.id "task-1" is listed twice`},
		{"tasks not visible", tasksOf("task-1", "elsewhere"), bindings, "tasks.rows[1].binding: conversation_task/elsewhere: is not part of this conversation"},
		{"tasks unchecked", tasksOf("task-1"), ValidateOptions{}, "tasks.rows[0].binding: bindings cannot be checked here"},
		{"tasks label", func() Component { c := tasksOf("task-1"); c.Tasks.Rows[0].Label = ""; return c }(), bindings, "tasks.rows[0].label"},
		{"tasks component binding", func() Component {
			c := tasksOf("task-1")
			c.Binding = &Binding{Kind: BindingConversationTask, ID: "x"}
			return c
		}(), bindings, "tasks takes no component binding"},
		// agent
		{"agent", agentOf(&Binding{Kind: BindingAgent, ID: "agent-7"}), bindings, ""},
		{"agent unbound", agentOf(nil), bindings, "binding is required: {kind: agent, id} names the agent this card shows"},
		{"agent wrong kind", agentOf(&Binding{Kind: BindingRun, ID: "r"}), bindings, `binding.kind "run" is not allowed on agent; use agent`},
		{"agent headline", Component{ID: "g", Type: "agent", Summary: "x", Binding: &Binding{Kind: BindingAgent, ID: "a"}, Agent: &AgentCard{}}, bindings, "agent.headline"},
		{"agent points", Component{ID: "g", Type: "agent", Summary: "x", Binding: &Binding{Kind: BindingAgent, ID: "a"}, Agent: &AgentCard{Headline: "h", Points: make([]string, 5)}}, bindings, "agent.points allows at most 4 (got 5)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Validate(context.Background(), one(tc.component), tc.opts)
			if tc.err == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.err)
			}
			if !strings.Contains(err.Error(), `components[0] (component "`) {
				t.Fatalf("error %q does not name the component path", err)
			}
		})
	}
}

func TestBindingKindsAreCheckedBeforeTheResolver(t *testing.T) {
	for name, component := range map[string]Component{
		"routine": routineOf(&Binding{Kind: BindingAgent, ID: "a"}),
		"agent":   agentOf(&Binding{Kind: BindingRunbook, ID: "a"}),
		"tasks": func() Component {
			c := tasksOf("task-1")
			c.Tasks.Rows[0].Binding.Kind = BindingApproval
			return c
		}(),
		"routine unbound": routineOf(nil),
	} {
		hosts := &resolver{}
		if _, err := Validate(context.Background(), one(component), ValidateOptions{Bindings: hosts}); err == nil || len(hosts.seen) > 0 {
			t.Fatalf("%s: err %v, resolver saw %v", name, err, hosts.seen)
		}
	}
	hosts := &resolver{}
	if _, err := Validate(context.Background(), one(tasksOf("task-1", "task-2")), ValidateOptions{Bindings: hosts}); err != nil || len(hosts.seen) != 2 ||
		hosts.seen[1] != (Binding{Kind: BindingConversationTask, ID: "task-2"}) {
		t.Fatalf("each row goes through the resolver: err %v, seen %v", err, hosts.seen)
	}
}

func TestPlaceHasNoLink(t *testing.T) {
	raw := `{"schema":"` + Schema + `","title":"x","state":"ready","components":[{"id":"p","type":"place","summary":"x","place":{"name":"Cafe","link":"https://cafe.example"}}]}`
	if _, err := Decode([]byte(raw)); err == nil || !strings.Contains(err.Error(), `unknown field "link"`) {
		t.Fatalf("place link decoded: %v", err)
	}
	if place := componentJSONSchema()["properties"].(map[string]interface{})["place"].(map[string]interface{})["properties"].(map[string]interface{}); place["link"] != nil {
		t.Fatal("place schema has a link")
	}
}

func TestLintWave3(t *testing.T) {
	bound := &Binding{Kind: BindingRunbook, ID: "act-1"}
	cases := []struct {
		name      string
		component Component
		codes     []string
	}{
		{"agenda one item", agendaOf(agendaDay("2026-11-02", slot("gym", "07:00", ""))), []string{LintAgendaOneItem}},
		{"agenda", agendaOf(agendaDay("2026-11-02", slot("gym", "07:00", "")), agendaDay("2026-11-03", slot("work", "09:00", ""))), nil},
		{"itinerary thin day", itineraryOf(stops("sat", "pier", "lanes"), stops("sun", "home")), []string{LintItineraryThinDay}},
		{"itinerary", itineraryOf(stops("sat", "pier", "lanes")), nil},
		{"breakdown dominated", breakdownOf(96, 4), []string{LintBreakdownOneDominant}},
		{"breakdown", breakdownOf(95, 5), nil},
		{"poll two multiple", pollOf(true, "a", "b"), []string{LintPollTwoMultiple}},
		{"poll three multiple", pollOf(true, "a", "b", "c"), nil},
		{"explorer static", explorerOf([]ExplorerInput{slider("x", 0, 5, 1, 1)}, output("a", "2 * 3"), output("b", "4")), []string{LintExplorerStaticOutputs}},
		{"explorer", explorerOf([]ExplorerInput{slider("x", 0, 5, 1, 1)}, output("a", "2 * 3"), output("b", "x")), nil},
		{"routine without items", routineOf(bound), []string{LintRoutineWithoutItems}},
		{"routine", routineOf(bound, RoutineItem{Label: "A", Text: "B"}), nil},
		{"tasks one row", tasksOf("task-1"), []string{LintTasksOneRow}},
		{"tasks", tasksOf("task-1", "task-2"), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var codes []string
			for _, warning := range Lint(one(tc.component)) {
				codes = append(codes, warning.Code)
			}
			if strings.Join(codes, ",") != strings.Join(tc.codes, ",") {
				t.Fatalf("lint codes = %v, want %v", codes, tc.codes)
			}
		})
	}
}

func TestWave3SchemaEntries(t *testing.T) {
	properties := componentJSONSchema()["properties"].(map[string]interface{})
	format := VisualFormat()
	for _, name := range []string{"agenda", "itinerary", "place", "breakdown", "progress", "poll", "explorer", "routine", "tasks", "agent"} {
		if properties[name] == nil {
			t.Fatalf("schema has no %s payload", name)
		}
		spec := catalogue[name]
		if spec.payload != name || !RequiresSummary(name) || spec.sentence == "" {
			t.Fatalf("%s catalogue entry = %#v", name, spec)
		}
		if !strings.Contains(format, "\n"+name+"*: ") {
			t.Fatalf("format has no line for %s", name)
		}
	}
	for name, kind := range map[string]string{"routine": "runbook", "agent": "agent", "draft": "approval"} {
		if !strings.Contains(format, "\n"+name+"*: ") || !strings.Contains(format[strings.Index(format, "\n"+name+"*: "):], "binding.kind:"+kind+"\n") &&
			!strings.HasSuffix(format, "binding.kind:"+kind) {
			t.Fatalf("format does not say %s binds %s", name, kind)
		}
	}
	if strings.Contains(format, "binding.kind:\n") {
		t.Fatal("format lists an empty binding kind")
	}
}

func TestRoutineBindings(t *testing.T) {
	surface := one(
		routineOf(&Binding{Kind: BindingRunbook, ID: "act-1"}),
		Component{ID: "s", Type: "section", Children: []Component{
			{ID: "r2", Type: "routine", Summary: "x", Binding: &Binding{Kind: BindingRunbook, ID: "act-2"}},
			{ID: "r3", Type: "routine", Summary: "x", Binding: &Binding{Kind: BindingRunbook, ID: "act-1"}},
			{ID: "b", Type: "actions", Binding: &Binding{Kind: BindingRunbook, ID: "act-3"}, Actions: []Action{{ID: "p", Label: "Pause", Kind: ActionEntity, Command: "pause"}}},
		}},
	)
	if got := strings.Join(RoutineBindings(surface), ","); got != "act-1,act-2" {
		t.Fatalf("RoutineBindings = %s", got)
	}
	if RoutineBindings(one(agendaOf())) != nil {
		t.Fatal("agenda has routines")
	}
}
