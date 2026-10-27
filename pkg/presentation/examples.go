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
	gbp := func(value float64, per string) *Price {
		return &Price{Amount: Amount{Value: value, Format: StatCurrency, Currency: "GBP"}, Per: per}
	}
	return []Example{{
		Name: "phone-plan", Prompt: "Is this phone plan a good deal?",
		Lead: "Not quite. You use about half the data you pay for.",
		Visual: visual("Your phone plan",
			Component{ID: "plans", Type: "comparison", Summary: "Your £25 plan against two cheaper plans with enough data; Sim Plus is the best value.", Comparison: &Comparison{
				Attributes: []ComparisonAttribute{{Key: "data", Label: "Data"}, {Key: "contract", Label: "Contract"}},
				Options: []ComparisonOption{
					{ID: "current", Name: "Current", Price: gbp(25, "month"), Values: map[string]string{"data": "60 GB", "contract": "24 months"}},
					{ID: "sim-plus", Name: "Sim Plus", Badge: "Best value", Price: gbp(17, "month"), Values: map[string]string{"data": "30 GB", "contract": "12 months"}},
					{ID: "flex-20", Name: "Flex 20", Price: gbp(12, "month"), Values: map[string]string{"data": "20 GB", "contract": "30 days"}, Cons: []string{"Tight if you stream on the go"}},
				},
				Recommended: "sim-plus", Pickable: true,
			}},
			Component{ID: "saving", Type: "stat_row", Summary: "Moving to Sim Plus saves £96 a year.", Tone: TonePositive, StatRow: &StatRow{Stats: []Stat{
				{Label: "Yearly saving", Value: 96, Format: StatCurrency, Currency: "GBP", Trend: "down", GoodDirection: "down"}, {Label: "Data you use", Value: 28, Unit: "GB/mo"},
			}}},
		),
	}, {
		Name: "compound-interest", Prompt: "How does compound interest work?",
		Lead: "Your interest earns interest too, so growth speeds up over time.",
		Visual: visual("£1,000 at 5% a year",
			Component{ID: "growth", Type: "chart", Summary: "£1,000 grows to about £2,653 in 20 years at 5%, faster each year.", Chart: &Chart{
				Kind: "line", XLabel: "Years", YLabel: "Balance", Format: StatCurrency, Currency: "GBP",
				Series:    []ChartSeries{series("Compound", years, 1000, 1276, 1629, 2079, 2653), series("Simple", years, 1000, 1250, 1500, 1750, 2000)},
				Highlight: &ChartHighlight{Series: "Compound"},
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
				Kind: "donut", Format: StatCurrency, Currency: "GBP",
				Series: []ChartSeries{series("Spending", []string{"Rent", "Food", "Transport", "Bills", "Streaming", "Other"}, 950, 430, 180, 210, 75, 140)},
			}},
			Component{ID: "note", Type: "callout", Tone: ToneWarning, Summary: "Two of three streaming services go unwatched.", Callout: &Callout{
				Icon: "lightbulb", Body: "You pay for **three** streaming services and watched one last month.",
			}},
			Component{ID: "cancel", Type: "checklist", Title: "To cancel", Summary: "Two unused subscriptions to cancel.", Checklist: &Checklist{Items: []ChecklistItem{
				{ID: "streamflix", Label: "StreamFlix (£10.99/mo)"}, {ID: "boxplus", Label: "BoxPlus (£6.99/mo)"},
			}}},
		),
	}, {
		Name: "cells", Prompt: "Explain how cells make energy",
		Lead: "Cells turn glucose into ATP, mostly inside the mitochondria.",
		Visual: visual("How cells make energy",
			Component{ID: "how", Type: "steps", Summary: "Glucose is split, broken down further, then turned into most of the ATP in the mitochondria.", Steps: &Steps{Items: []Step{
				{ID: "glycolysis", Title: "Glycolysis", Detail: "Glucose splits into two pyruvate, making **2 ATP**.", Icon: "flask-conical"},
				{ID: "krebs", Title: "Krebs cycle", Detail: "Pyruvate is broken down in the mitochondria, making **2 ATP**.", Icon: "refresh-cw"},
				{ID: "chain", Title: "Electron transport chain", Detail: "Oxygen helps turn the rest of the energy into about **34 ATP**.", Icon: "zap"},
			}}},
			Component{ID: "check", Type: "quiz", Summary: "One question: where most ATP is made.", Quiz: &Quiz{
				Question:  "Where is most ATP made?",
				Options:   []QuizOption{{ID: "nucleus", Label: "Nucleus"}, {ID: "mitochondria", Label: "Mitochondria"}, {ID: "wall", Label: "Cell wall"}},
				AnswerKey: &QuizAnswerKey{Correct: []string{"mitochondria"}, Explanation: "The electron transport chain in the **mitochondria** makes about 34 of the 38 ATP."},
			}},
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
			Component{ID: "next", Type: "choices", Summary: "Offers a shopping list or a weekly repeat.", Choices: &Choices{Chips: []Chip{
				{ID: "list", Label: "Make a shopping list"}, {ID: "weekly", Label: "Do this every week"},
			}}},
		),
	}, {
		Name: "weekend", Prompt: "Plan a long weekend somewhere with a beach, four of us",
		Lead: "Three beach towns within a few hours, from cheapest to closest.",
		Visual: visual("Beach weekend options",
			Component{ID: "options", Type: "comparison", Summary: "Tenby is cheapest at £540, Camber closest at 2 hours, Whitby in between.", Comparison: &Comparison{
				Attributes: []ComparisonAttribute{{Key: "drive", Label: "Drive"}, {Key: "beach", Label: "Beach"}},
				Options: []ComparisonOption{
					{ID: "tenby", Name: "Tenby", Badge: "Cheapest", Price: gbp(540, "once"), Values: map[string]string{"drive": "4 hours", "beach": "Sandy, sheltered"}},
					{ID: "whitby", Name: "Whitby", Price: gbp(620, "once"), Values: map[string]string{"drive": "3 hours", "beach": "Sandy, with an abbey above"}},
					{ID: "camber", Name: "Camber", Badge: "Closest", Price: gbp(710, "once"), Values: map[string]string{"drive": "2 hours", "beach": "Big dunes"}},
				},
				Pickable: true,
			}},
		),
	}, {
		Name: "french-revolution", Prompt: "What happened in the French Revolution?",
		Lead: "France overthrew its king and tried to rebuild itself between 1789 and 1799.",
		Visual: visual("The French Revolution",
			Component{ID: "levels", Type: "tabs", Tabs: []Tab{
				{Label: "Simple", Children: []Component{text("simple", "People were hungry and taxed while nobles paid little. They stormed the Bastille, ended the monarchy, and after years of turmoil Napoleon took power.")}},
				{Label: "Key dates", Children: []Component{{ID: "dates", Type: "timeline", Summary: "Five key events from 1789 to 1799.", Timeline: &Timeline{Events: []TimelineEvent{
					{ID: "bastille", When: "1789", Title: "Storming of the Bastille"}, {ID: "varennes", When: "1791", Title: "Royal family's failed escape"},
					{ID: "execution", When: "1793", Title: "Louis XVI executed"}, {ID: "terror", When: "1794", Title: "End of the Terror"},
					{ID: "napoleon", When: "1799", Title: "Napoleon seizes power"},
				}}}}},
			}},
			Component{ID: "why", Type: "callout", Summary: "Why the revolution still matters.", Callout: &Callout{Icon: "lightbulb", Body: "It spread the idea that power comes from the people, not a king."}},
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
