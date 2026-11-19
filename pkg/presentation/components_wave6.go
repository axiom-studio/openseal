package presentation

import (
	"fmt"
	"net/url"
)

// Cards shows a few things side by side, each with one headline value
// (weather by city, stocks, people, plans). Read-only.
type Cards struct {
	Items []Card `json:"items"`
}

// Card is one thing: a title with an optional photo, headline value, line,
// detail and status chip.
type Card struct {
	Title  string    `json:"title"`
	Image  *ImageRef `json:"image,omitempty"`
	Value  string    `json:"value,omitempty"`
	Line   string    `json:"line,omitempty"`
	Detail string    `json:"detail,omitempty"`
	Chip   *CardChip `json:"chip,omitempty"`
}

// CardChip is a short status on a card; its tone is neutral when unset.
type CardChip struct {
	Text string `json:"text"`
	Tone Tone   `json:"tone,omitempty"`
}

// Cards limits, in characters.
const (
	maxCards           = 8
	maxCardTitleRunes  = 80
	maxCardValueRunes  = 24
	maxCardLineRunes   = 120
	maxCardChipRunes   = 40
	maxConverterCode   = 12
	maxConverterLabel  = 40
	maxConverterSymbol = 4
	maxConverterNote   = 160
)

func validateCards(v *validator, c Component, _ string) error {
	for index, card := range c.Cards.Items {
		if err := v.image(fmt.Sprintf("cards.items[%d].image", index), card.Image); err != nil {
			return err
		}
	}
	return nil
}

// Converter converts between two units or currencies both ways: 1 From
// is Rate To. People type in either box.
type Converter struct {
	From ConverterUnit `json:"from"`
	To   ConverterUnit `json:"to"`
	Rate float64       `json:"rate"`
	// Amount is the From amount shown first; 1 when unset.
	Amount *float64 `json:"amount,omitempty"`
	// AsOf is the YYYY-MM-DD the rate is from.
	AsOf string `json:"asOf,omitempty"`
	Note string `json:"note,omitempty"`
}

// ConverterUnit is one side of a converter. An ISO 4217 code makes it a
// currency, which the app shows with its flag; any other code is a unit
// such as km, mi, kg or lb.
type ConverterUnit struct {
	Code   string `json:"code"`
	Label  string `json:"label"`
	Symbol string `json:"symbol,omitempty"`
}

func validateConverter(_ *validator, c Component, _ string) error {
	return calendarDate("converter.asOf", c.Converter.AsOf)
}

// Source names a page a component's facts came from; the app shows the
// sources as one chip after the component.
type Source struct {
	URL   string `json:"url"`
	Title string `json:"title"`
}

// Source limits.
const (
	maxSources          = 6
	maxSourceTitleRunes = 80
	sourceURLPattern    = `^https?://\S+$`
)

// ruleSources is checked by Go for every component's sources.
const ruleSources = "source urls are absolute http or https URLs without credentials"

// validateSources checks the URLs the schema admitted: each must parse as
// an absolute http(s) URL with a host and no credentials.
func validateSources(sources []Source) error {
	for index, source := range sources {
		parsed, err := url.Parse(source.URL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.Opaque != "" {
			return fmt.Errorf("sources[%d].url %q must be an absolute http or https URL", index, source.URL)
		}
		if parsed.User != nil {
			return fmt.Errorf("sources[%d].url %q must not contain credentials", index, source.URL)
		}
	}
	return nil
}
