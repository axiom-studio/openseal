package presentation

// Example is a worked answer: either a visual answer from the §3 golden set,
// built only from catalogue types, or a reply whose right form is plain text
// (Visual is nil). Tests validate every example; hosts may show them to the
// model, and galleries and evaluations reuse them as fixtures.
type Example struct {
	Name   string  `json:"-"`
	Prompt string  `json:"userAsks"`
	Lead   string  `json:"answer"`
	Visual *Visual `json:"visual,omitempty"`
}

func visual(title string, components ...Component) *Visual {
	return &Visual{Title: title, Components: components}
}

func series(name string, labels []string, values ...float64) ChartSeries {
	points := make([]ChartPoint, len(values))
	for index, value := range values {
		points[index] = ChartPoint{Label: labels[index], Value: value}
	}
	return ChartSeries{Name: name, Points: points}
}

func text(id, value string) Component { return Component{ID: id, Type: "text", Text: value} }

// VisualExamples returns the worked examples, visual answers first, then
// replies that should stay text.
func VisualExamples() []Example {
	years := []string{"0", "5", "10", "15", "20"}
	return []Example{{
		Name: "phone-plan", Prompt: "Is this phone plan a good deal?",
		Lead: "Not quite. You use about half the data you pay for.",
		Visual: visual("Your phone plan",
			Component{ID: "plans", Type: "table", Summary: "Your £25 plan against two cheaper plans with enough data.", Table: &Table{
				Columns: []string{"Plan", "Price", "Data", "Contract"},
				Rows:    [][]string{{"Current", "£25/mo", "60 GB", "24 months"}, {"Sim Plus", "£17/mo", "30 GB", "12 months"}, {"Flex 20", "£12/mo", "20 GB", "30 days"}},
			}},
			Component{ID: "saving", Type: "stat_row", Summary: "Moving to Sim Plus saves £96 a year.", Tone: TonePositive, StatRow: &StatRow{Stats: []Stat{
				{Label: "Yearly saving", Value: 96, Format: StatCurrency, Currency: "GBP", Trend: "down", GoodDirection: "down"}, {Label: "Data you use", Value: 28, Unit: "GB/mo"},
			}}},
			Component{ID: "next", Type: "actions", Actions: []Action{{ID: "switch", Label: "Switch to Sim Plus", Kind: ActionRespond, Icon: "smartphone"}}},
		),
	}, {
		Name: "compound-interest", Prompt: "How does compound interest work?",
		Lead: "Your interest earns interest too, so growth speeds up over time.",
		Visual: visual("£1,000 at 5% a year",
			Component{ID: "growth", Type: "chart", Summary: "£1,000 grows to about £2,653 in 20 years at 5%, faster each year.", Chart: &Chart{
				Kind: "line", XLabel: "Years", YLabel: "Balance (£)",
				Series: []ChartSeries{series("Compound", years, 1000, 1276, 1629, 2079, 2653), series("Simple", years, 1000, 1250, 1500, 1750, 2000)},
			}},
			Component{ID: "result", Type: "stat_row", Summary: "After 20 years compounding earns £653 more than simple interest.", StatRow: &StatRow{Stats: []Stat{
				{Label: "After 20 years", Value: 2653, Format: StatCurrency, Currency: "GBP"}, {Label: "Extra from compounding", Value: 653, Format: StatCurrency, Currency: "GBP", Trend: "up", GoodDirection: "up"},
			}}},
		),
	}, {
		Name: "money", Prompt: "Where is my money going?",
		Lead: "Rent and food are most of it, and three streaming services add up.",
		Visual: visual("Last month's spending",
			Component{ID: "split", Type: "chart", Summary: "Rent is 48% of spending, food 22%, streaming 4%.", Chart: &Chart{
				Kind: "donut", Series: []ChartSeries{series("Spending", []string{"Rent", "Food", "Transport", "Bills", "Streaming", "Other"}, 950, 430, 180, 210, 75, 140)},
			}},
			Component{ID: "note", Type: "text", Tone: ToneWarning, Text: "You pay for **three** streaming services and watched one."},
			Component{ID: "next", Type: "actions", Actions: []Action{
				{ID: "cancel", Label: "Help me cancel two", Kind: ActionRespond, Icon: "trash-2"},
				{ID: "keep", Label: "Keep them", Kind: ActionRespond},
			}},
		),
	}, {
		Name: "cells", Prompt: "Explain how cells make energy",
		Lead: "Cells turn glucose into ATP, mostly inside the mitochondria.",
		Visual: visual("How cells make energy",
			Component{ID: "flow", Type: "diagram", Summary: "Glucose is split, broken down further, then turned into ATP in the mitochondria.", Diagram: &Diagram{
				Nodes: []DiagramNode{{ID: "glucose", Label: "Glucose", X: 0}, {ID: "glycolysis", Label: "Glycolysis (2 ATP)", X: 200}, {ID: "krebs", Label: "Krebs cycle (2 ATP)", X: 400}, {ID: "chain", Label: "Electron chain (~34 ATP)", X: 600}},
				Edges: []DiagramEdge{{From: "glucose", To: "glycolysis"}, {From: "glycolysis", To: "krebs", Label: "pyruvate"}, {From: "krebs", To: "chain"}},
			}},
			Component{ID: "check", Type: "form", Summary: "One question: where most ATP is made.", Form: &Form{SubmitLabel: "Check", Fields: []FormField{{
				ID: "answer", Type: "radio", Label: "Where is most ATP made?", Required: true,
				Options: []FormOption{{Label: "Nucleus", Value: "nucleus"}, {Label: "Mitochondria", Value: "mitochondria"}, {Label: "Cell wall", Value: "wall"}},
			}}}},
		),
	}, {
		Name: "dinners", Prompt: "Five dinners this week, nobody likes mushrooms",
		Lead: "Here are five mushroom-free dinners, each under 40 minutes.",
		Visual: visual("Five dinners",
			Component{ID: "week", Type: "carousel", Children: []Component{
				{ID: "mon", Type: "text", Title: "Monday: Chicken fajitas", Text: "25 min · peppers, onion, wraps"},
				{ID: "tue", Type: "text", Title: "Tuesday: Lentil dal", Text: "35 min · red lentils, spinach, rice"},
				{ID: "wed", Type: "text", Title: "Wednesday: Salmon traybake", Text: "30 min · potatoes, green beans"},
				{ID: "thu", Type: "text", Title: "Thursday: Pesto pasta", Text: "15 min · peas, parmesan"},
				{ID: "fri", Type: "text", Title: "Friday: Homemade pizza", Text: "40 min · tomato, mozzarella, ham"},
			}},
			Component{ID: "next", Type: "actions", Actions: []Action{
				{ID: "list", Label: "Make a shopping list", Kind: ActionRespond, Icon: "shopping-cart"},
				{ID: "weekly", Label: "Do this every week", Kind: ActionRespond, Icon: "repeat"},
			}},
		),
	}, {
		Name: "weekend", Prompt: "Plan a long weekend somewhere with a beach, four of us",
		Lead: "Three beach towns within a few hours, from cheapest to closest.",
		Visual: visual("Beach weekend options",
			Component{ID: "options", Type: "grid", Grid: &Grid{Columns: 3}, Children: []Component{
				{ID: "whitby", Type: "stat_row", Title: "Whitby", Summary: "Whitby: 3h drive, about £620 for four.", StatRow: &StatRow{Stats: []Stat{{Label: "Drive", Value: 10800, Format: StatDuration}, {Label: "Budget", Value: 620, Format: StatCurrency, Currency: "GBP"}}}},
				{ID: "tenby", Type: "stat_row", Title: "Tenby", Summary: "Tenby: 4h drive, about £540 for four.", StatRow: &StatRow{Stats: []Stat{{Label: "Drive", Value: 14400, Format: StatDuration}, {Label: "Budget", Value: 540, Format: StatCurrency, Currency: "GBP"}}}},
				{ID: "camber", Type: "stat_row", Title: "Camber", Summary: "Camber: 2h drive, about £710 for four.", StatRow: &StatRow{Stats: []Stat{{Label: "Drive", Value: 7200, Format: StatDuration}, {Label: "Budget", Value: 710, Format: StatCurrency, Currency: "GBP"}}}},
			}},
			Component{ID: "pick", Type: "actions", Actions: []Action{
				{ID: "whitby", Label: "Whitby", Kind: ActionRespond, Value: "Let's go to Whitby"},
				{ID: "tenby", Label: "Tenby", Kind: ActionRespond, Value: "Let's go to Tenby"},
				{ID: "camber", Label: "Camber", Kind: ActionRespond, Value: "Let's go to Camber"},
			}},
		),
	}, {
		Name: "french-revolution", Prompt: "What happened in the French Revolution?",
		Lead: "France overthrew its king and tried to rebuild itself between 1789 and 1799.",
		Visual: visual("The French Revolution",
			Component{ID: "levels", Type: "tabs", Tabs: []Tab{
				{Label: "Simple", Children: []Component{text("simple", "People were hungry and taxed while nobles paid little. They stormed the Bastille, ended the monarchy, and after years of turmoil Napoleon took power.")}},
				{Label: "Key dates", Children: []Component{{ID: "dates", Type: "table", Summary: "Five key events from 1789 to 1799.", Table: &Table{
					Columns: []string{"Year", "Event"},
					Rows:    [][]string{{"1789", "Storming of the Bastille"}, {"1791", "Royal family's failed escape"}, {"1793", "Louis XVI executed"}, {"1794", "End of the Terror"}, {"1799", "Napoleon seizes power"}},
				}}}},
			}},
			Component{ID: "why", Type: "caption", Text: "It spread the idea that power comes from the people, not a king."},
		),
	}, {
		Name: "text-feelings", Prompt: "I'm really nervous about my exam tomorrow",
		Lead: "That's completely normal, and it usually means you care about doing well. Tonight, skim your notes once, then stop and sleep; " +
			"a rested brain recalls more than a crammed one. Want a quick five-question warm-up for the morning?",
	}, {
		Name: "text-writing", Prompt: "Write a short thank-you note to my neighbour for watering the plants",
		Lead: "Hi Sam, thank you so much for looking after the plants while we were away. They're thriving, which is more than I manage! " +
			"Let me know if I can ever return the favour. Best, Alex",
	}, {
		Name: "text-fact", Prompt: "What's the capital of Australia?",
		Lead: "Canberra.",
	}}
}
