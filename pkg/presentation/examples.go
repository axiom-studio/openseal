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

func number64(value float64) *float64 { return &value }

func digits(value int) *int { return &value }

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
			Component{ID: "split", Type: "breakdown", Summary: "Rent is 48% of spending, food 22%, streaming 4%.", Breakdown: &Breakdown{
				Kind: "treemap", Format: StatCurrency, Currency: "GBP", Items: []BreakdownItem{
					{ID: "rent", Label: "Rent", Value: 950, Icon: "house"}, {ID: "food", Label: "Food", Value: 430}, {ID: "transport", Label: "Transport", Value: 180},
					{ID: "bills", Label: "Bills", Value: 210}, {ID: "streaming", Label: "Streaming", Value: 75}, {ID: "other", Label: "Other", Value: 140},
				},
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
		Name: "compound-interest", Prompt: "How does compound interest work?",
		Lead: "Each year you earn interest on your interest too, so the growth speeds up. Move the sliders to see it.",
		Visual: visual("Compound interest",
			Component{ID: "growth", Type: "explorer", Summary: "Sliders for deposit, rate and years show the final balance and how it grows year by year.", Explorer: &Explorer{
				Inputs: []ExplorerInput{
					{ID: "deposit", Label: "Deposit", Kind: "slider", Min: number64(100), Max: number64(20000), Step: number64(100), Default: number64(10000), Format: StatCurrency, Currency: "GBP"},
					{ID: "rate", Label: "Interest rate", Kind: "slider", Min: number64(0.5), Max: number64(10), Step: number64(0.5), Default: number64(5), Format: StatPercent},
					{ID: "years", Label: "Years", Kind: "stepper", Min: number64(1), Max: number64(40), Step: number64(1), Default: number64(10), Unit: "years"},
				},
				Outputs: []ExplorerOutput{
					{ID: "balance", Label: "Final balance", Expr: "deposit * (1 + rate / 100) ^ years", Format: StatCurrency, Currency: "GBP", Digits: digits(2)},
					{ID: "interest", Label: "Interest earned", Expr: "deposit * (1 + rate / 100) ^ years - deposit", Format: StatCurrency, Currency: "GBP", Digits: digits(2)},
				},
				Chart: &ExplorerChart{X: "t", T: &ExplorerRange{From: 0, To: 40, Label: "Year"}, Series: []ExplorerSeries{
					{Label: "With interest", Expr: "deposit * (1 + rate / 100) ^ t"}, {Label: "Deposit only", Expr: "deposit"},
				}},
			}},
		),
	}, {
		Name: "weekend", Prompt: "Plan our long weekend in Brighton, four of us",
		Lead: "Here's a relaxed three days by the sea, with short walks between stops.",
		Visual: visual("Brighton weekend",
			Component{ID: "plan", Type: "itinerary", Summary: "Three days: the pier and beach, the Lanes and a cliff walk, then brunch and home.", Itinerary: &Itinerary{Days: []ItineraryDay{
				{ID: "fri", Label: "Friday", Items: []ItineraryItem{
					{ID: "arrive", Time: "15:00", Title: "Check in", Place: "Seafront flat"},
					{ID: "pier", Time: "16:30", Title: "Palace Pier and the beach", Travel: &Travel{Mode: "walk", Minutes: 10}},
					{ID: "chips", Time: "19:00", Title: "Fish and chips on the front", Travel: &Travel{Mode: "walk", Minutes: 5}},
				}},
				{ID: "sat", Label: "Saturday", Items: []ItineraryItem{
					{ID: "lanes", Time: "10:00", Title: "The Lanes", Detail: "Small shops and cafés; good for a slow morning."},
					{ID: "cliffs", Time: "14:00", Title: "Cliff walk to Rottingdean", Travel: &Travel{Mode: "transit", Minutes: 25}},
				}},
				{ID: "sun", Label: "Sunday", Items: []ItineraryItem{
					{ID: "brunch", Time: "10:30", Title: "Brunch in Kemptown"},
					{ID: "home", Time: "14:00", Title: "Train home", Travel: &Travel{Mode: "walk", Minutes: 20}},
				}},
			}}},
			Component{ID: "pack", Type: "checklist", Title: "Packing", Summary: "Five things to pack for a seaside weekend.", Checklist: &Checklist{Items: []ChecklistItem{
				{ID: "swimwear", Label: "Swimwear"}, {ID: "towels", Label: "Beach towels"}, {ID: "jackets", Label: "Windproof jackets"},
				{ID: "suncream", Label: "Sun cream"}, {ID: "shoes", Label: "Walking shoes"},
			}}},
		),
	}, {
		Name: "family-dinner", Prompt: "Can you ask everyone where we should go for dinner on Saturday?",
		Lead: "Here's a quick vote. I'll book once everyone has picked.",
		Visual: visual("Saturday dinner",
			Component{ID: "vote", Type: "poll", Title: "Saturday dinner", Summary: "A vote between Thai, pizza and the Sunday roast pub.", Poll: &Poll{
				Question: "Where should we go for dinner on Saturday?",
				Options:  []PollOption{{ID: "thai", Label: "Thai Garden"}, {ID: "pizza", Label: "Pizza place"}, {ID: "pub", Label: "The Red Lion"}},
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
