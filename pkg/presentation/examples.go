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
			Component{ID: "parts", Type: "labeled_image", Summary: "The cell illustration with the cytoplasm, mitochondria and nucleus marked.", LabeledImage: &LabeledImage{
				Image: ImageRef{ArtifactID: "img-cell", Version: 1},
				Markers: []ImageMarker{
					{ID: "cytoplasm", X: 0.25, Y: 0.6, Label: "Cytoplasm", Detail: "Where glucose is first split (glycolysis)."},
					{ID: "mitochondria", X: 0.68, Y: 0.32, Label: "Mitochondria", Detail: "The cell's power stations, where most ATP is made."},
					{ID: "nucleus", X: 0.48, Y: 0.5, Label: "Nucleus", Detail: "Holds the DNA; it makes no energy."},
				},
			}},
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
		Name: "fractions", Prompt: "Is 3/4 bigger than 2/3?",
		Lead: "Yes. Cut both into twelfths and 3/4 is 9/12 while 2/3 is only 8/12.",
		Visual: visual("3/4 against 2/3",
			Component{ID: "bars", Type: "quantity_visual", Summary: "Bars for 3/4 and 2/3, then both cut into twelfths: 9/12 is more than 8/12.", QuantityVisual: &QuantityVisual{
				Kind:           "fraction_bars",
				QuantityValues: QuantityValues{Fractions: []Fraction{{Numerator: 3, Denominator: 4, Label: "3/4"}, {Numerator: 2, Denominator: 3, Label: "2/3"}}},
				Steps: []QuantityStep{{Label: "Cut both into twelfths", QuantityValues: QuantityValues{Fractions: []Fraction{
					{Numerator: 9, Denominator: 12, Label: "9/12"}, {Numerator: 8, Denominator: 12, Label: "8/12"},
				}}}},
			}},
		),
	}, {
		Name: "french-words", Prompt: "Teach me the French words for food",
		Lead: "Here are six everyday food words. Flip each card, mark how you did, and tap Done when you're through.",
		Visual: visual("French food words",
			Component{ID: "deck", Type: "flashcards", Title: "French food words", Summary: "Six French words for everyday food.", Flashcards: &Flashcards{Cards: []Flashcard{
				{ID: "pain", Front: "le pain", Back: "bread"}, {ID: "fromage", Front: "le fromage", Back: "cheese"},
				{ID: "pomme", Front: "la pomme", Back: "apple"}, {ID: "poulet", Front: "le poulet", Back: "chicken"},
				{ID: "eau", Front: "l'eau", Back: "water"}, {ID: "oeuf", Front: "l'œuf", Back: "egg"},
			}, Shuffle: true}},
		),
	}, {
		Name: "email-tone", Prompt: "Fix the tone of this email: \"Sam, I still haven't got the report. I need it by Friday.\"",
		Lead: "Here's a warmer version that still asks for the report by Friday.",
		Visual: visual("A warmer email",
			Component{ID: "changes", Type: "before_after", Summary: "The new version greets Sam, asks rather than demands, and says thanks.", BeforeAfter: &BeforeAfter{
				Mode:   "diff",
				Before: BeforeAfterSide{Text: "Sam, I still haven't got the report. I need it by Friday."},
				After:  BeforeAfterSide{Text: "Hi Sam, hope your week's going well! Could you send the report over by Friday? Thanks so much."},
				Labels: &BeforeAfterLabels{Before: "Yours", After: "Warmer"},
			}},
			Component{ID: "email", Type: "draft", Summary: "The warmer email to Sam, ready to copy or change.", Draft: &Draft{
				Channel: "email", To: "Sam", Subject: "The report",
				Body: "Hi Sam,\n\nHope your week's going well! Could you send the report over by Friday?\n\nThanks so much,\nAlex",
			}},
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
