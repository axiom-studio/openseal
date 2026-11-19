package presentation

import (
	"strings"
	"testing"
)

func cardsOf(items ...Card) Component {
	return Component{ID: "wx", Type: "cards", Summary: "Weather now in three cities.", Cards: &Cards{Items: items}}
}

func converterOf(change func(*Converter)) Component {
	amount := 100.0
	converter := &Converter{
		From: ConverterUnit{Code: "USD", Label: "US dollar", Symbol: "$"}, To: ConverterUnit{Code: "EUR", Label: "Euro", Symbol: "€"},
		Rate: 0.9117, Amount: &amount, AsOf: "2026-11-19", Note: "Mid-market reference. Banks charge more.",
	}
	if change != nil {
		change(converter)
	}
	return Component{ID: "fx", Type: "converter", Summary: "1 USD is 0.9117 EUR.", Converter: converter}
}

func TestValidateWave6(t *testing.T) {
	tokyo := Card{Title: "Tokyo, Japan", Image: img("tokyo"), Value: "14°C", Line: "Clear skies", Detail: "High 19°C · Low 11°C",
		Chip: &CardChip{Text: "14% rain chance", Tone: TonePositive}}
	many := func(count int) []Card {
		list := make([]Card, count)
		for index := range list {
			list[index] = Card{Title: "City"}
		}
		return list
	}
	withSources := func(c Component, sources ...Source) Component {
		c.Sources = sources
		return c
	}
	wttr := Source{URL: "https://wttr.in/Tokyo", Title: "wttr.in"}
	zero, negative := 0.0, -1.0
	skip := ValidateOptions{SkipHostChecks: true}
	cases := []struct {
		name      string
		component Component
		opts      ValidateOptions
		err       string
	}{
		{"cards", cardsOf(tokyo, Card{Title: "Paris"}), images("tokyo"), ""},
		{"eight cards", cardsOf(many(8)...), skip, ""},
		{"nine cards", cardsOf(many(9)...), skip, "cards.items needs 1-8 (got 9)"},
		{"no cards", cardsOf(), skip, "cards.items needs 1-8 (got 0)"},
		{"no title", cardsOf(Card{Value: "1"}), skip, "cards.items[0].title"},
		{"long title", cardsOf(Card{Title: strings.Repeat("é", 81)}), skip, "cards.items[0].title must contain between 1 and 80 characters (got 81)"},
		{"title at limit", cardsOf(Card{Title: strings.Repeat("é", 80)}), skip, ""},
		{"long value", cardsOf(Card{Title: "x", Value: strings.Repeat("9", 25)}), skip, "cards.items[0].value"},
		{"long line", cardsOf(Card{Title: "x", Line: strings.Repeat("x", 121)}), skip, "cards.items[0].line"},
		{"long detail", cardsOf(Card{Title: "x", Detail: strings.Repeat("x", 121)}), skip, "cards.items[0].detail"},
		{"empty chip", cardsOf(Card{Title: "x", Chip: &CardChip{}}), skip, "cards.items[0].chip.text"},
		{"long chip", cardsOf(Card{Title: "x", Chip: &CardChip{Text: strings.Repeat("x", 41)}}), skip, "cards.items[0].chip.text"},
		{"bad chip tone", cardsOf(Card{Title: "x", Chip: &CardChip{Text: "ok", Tone: "loud"}}), skip, "cards.items[0].chip.tone"},
		{"unknown image", cardsOf(tokyo), images("paris"), "cards.items[0].image (image tokyo version 1): is not an image in this chat"},
		{"no image checks", cardsOf(tokyo), ValidateOptions{}, "cards.items[0].image: this host cannot show images yet"},

		{"converter", converterOf(nil), skip, ""},
		{"converter bare", converterOf(func(c *Converter) { c.Amount, c.AsOf, c.Note, c.From.Symbol, c.To.Symbol = nil, "", "", "", "" }), skip, ""},
		{"units", converterOf(func(c *Converter) { c.From, c.To, c.Rate = ConverterUnit{Code: "km", Label: "Kilometres"}, ConverterUnit{Code: "mi", Label: "Miles"}, 0.621371 }), skip, ""},
		{"zero rate", converterOf(func(c *Converter) { c.Rate = 0 }), skip, "converter.rate"},
		{"negative rate", converterOf(func(c *Converter) { c.Rate = -2 }), skip, "converter.rate"},
		{"zero amount", converterOf(func(c *Converter) { c.Amount = &zero }), skip, "converter.amount"},
		{"negative amount", converterOf(func(c *Converter) { c.Amount = &negative }), skip, "converter.amount"},
		{"no from code", converterOf(func(c *Converter) { c.From.Code = "" }), skip, "converter.from.code"},
		{"long code", converterOf(func(c *Converter) { c.To.Code = strings.Repeat("x", 13) }), skip, "converter.to.code"},
		{"no label", converterOf(func(c *Converter) { c.To.Label = "" }), skip, "converter.to.label"},
		{"long label", converterOf(func(c *Converter) { c.From.Label = strings.Repeat("x", 41) }), skip, "converter.from.label"},
		{"long symbol", converterOf(func(c *Converter) { c.From.Symbol = "US$$$" }), skip, "converter.from.symbol"},
		{"bad date", converterOf(func(c *Converter) { c.AsOf = "19 Nov" }), skip, "converter.asOf"},
		{"not a day", converterOf(func(c *Converter) { c.AsOf = "2026-02-30" }), skip, `converter.asOf "2026-02-30" is not a calendar date`},
		{"long note", converterOf(func(c *Converter) { c.Note = strings.Repeat("x", 161) }), skip, "converter.note"},

		{"sources", withSources(cardsOf(Card{Title: "x"}), wttr, Source{URL: "http://example.com/a?b=c", Title: "Example"}), skip, ""},
		{"sources on any type", withSources(converterOf(nil), wttr), skip, ""},
		{"nested sources", Component{ID: "s", Type: "section", Children: []Component{withSources(text("t", "Hi"), Source{URL: "ftp://x", Title: "x"})}}, skip,
			"sources[0].url"},
		{"six sources", withSources(cardsOf(Card{Title: "x"}), wttr, wttr, wttr, wttr, wttr, wttr), skip, ""},
		{"seven sources", withSources(cardsOf(Card{Title: "x"}), wttr, wttr, wttr, wttr, wttr, wttr, wttr), skip, "sources needs 1-6 (got 7)"},
		{"no title", withSources(cardsOf(Card{Title: "x"}), Source{URL: wttr.URL}), skip, "sources[0].title"},
		{"long source title", withSources(cardsOf(Card{Title: "x"}), Source{URL: wttr.URL, Title: strings.Repeat("x", 81)}), skip, "sources[0].title"},
		{"not a url", withSources(cardsOf(Card{Title: "x"}), Source{URL: "wttr.in", Title: "wttr"}), skip, "sources[0].url"},
		{"javascript url", withSources(cardsOf(Card{Title: "x"}), Source{URL: "javascript:alert(1)", Title: "x"}), skip, "sources[0].url"},
		{"no host", withSources(cardsOf(Card{Title: "x"}), Source{URL: "https:///path", Title: "x"}), skip, "must be an absolute http or https URL"},
		{"credentials", withSources(cardsOf(Card{Title: "x"}), Source{URL: "https://user:pw@example.com/", Title: "x"}), skip, "must not contain credentials"},
		{"long url", withSources(cardsOf(Card{Title: "x"}), Source{URL: "https://example.com/" + strings.Repeat("a", MaxLinkBytes), Title: "x"}), skip, "sources[0].url"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateOne(tc.component, tc.opts)
			if tc.err == "" {
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("Validate error = %v, want %q", err, tc.err)
			}
		})
	}
}

func TestWave6Guidance(t *testing.T) {
	rubric := VisualRubric()
	for _, want := range []string{
		"A few things, each with one headline number → `cards`.",
		"Unit or currency conversion → `converter`.",
		"Cite pages you fetched in the component's sources instead of a caption line; only real URLs from this chat.",
	} {
		if !strings.Contains(rubric, want) {
			t.Fatalf("rubric lacks %q", want)
		}
	}
	if strings.Index(rubric, "`cards`") > strings.Index(rubric, "`stat_row`") {
		t.Fatal("cards comes after stat_row in the rubric")
	}
	format := VisualFormat()
	for _, want := range []string{"\ncards: 1-8 things", "\nconverter: Two units and a rate", "sources?:"} {
		if !strings.Contains(format, want) {
			t.Fatalf("format lacks %q", want)
		}
	}
}

func TestWave6FallbackText(t *testing.T) {
	surface := one(
		cardsOf(Card{Title: "Tokyo", Value: "14°C", Line: "Clear", Chip: &CardChip{Text: "14% rain"}}),
		converterOf(nil),
	)
	text := FallbackText(surface)
	for _, want := range []string{"- Tokyo: 14°C Clear (14% rain)", "- 1 USD = 0.9117 EUR (2026-11-19)"} {
		if !strings.Contains(text, want) {
			t.Fatalf("fallback text lacks %q:\n%s", want, text)
		}
	}
}
