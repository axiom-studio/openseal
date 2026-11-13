package presentation

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Fallback text limits. A chat that cannot draw a component, or shows only
// a notification, reads this text instead; Slack, for one, recommends at most
// 4000 characters of message text.
const (
	MaxFallbackRunes     = 3000
	maxFallbackFacts     = 6
	maxFallbackFactRunes = 160
)

// FallbackText is a plain-markdown reading of a visual for chats that cannot
// draw it: the title, then each component's title, text or summary and a few
// facts from its payload as bullets. It never reads quiz answer keys.
func FallbackText(surface Surface) string {
	var lines []string
	if title := strings.TrimSpace(surface.Title); title != "" {
		lines = append(lines, "**"+title+"**")
	}
	var visit func(list []Component)
	visit = func(list []Component) {
		for _, component := range list {
			lines = append(lines, componentFallbackLines(component)...)
			visit(component.Children)
			for _, tab := range component.Tabs {
				lines = append(lines, "**"+tab.Label+"**")
				visit(tab.Children)
			}
		}
	}
	visit(surface.Components)
	return shorten(strings.Join(lines, "\n"), MaxFallbackRunes)
}

// componentFallbackLines reads one component, without its children, as
// markdown lines: its title, its text or summary, then up to a few facts.
func componentFallbackLines(component Component) []string {
	var lines []string
	if title := strings.TrimSpace(component.Title); title != "" {
		lines = append(lines, "**"+title+"**")
	}
	switch {
	case component.Type == "text" || component.Type == "caption" || component.Type == "divider":
		if text := strings.TrimSpace(component.Text); text != "" {
			lines = append(lines, text)
		}
	case strings.TrimSpace(component.Summary) != "":
		lines = append(lines, strings.TrimSpace(component.Summary))
	}
	facts := componentFacts(component)
	for index, fact := range facts {
		if index == maxFallbackFacts {
			lines = append(lines, fmt.Sprintf("- …and %d more", len(facts)-index))
			break
		}
		lines = append(lines, "- "+shorten(fact, maxFallbackFactRunes))
	}
	return lines
}

// componentFacts lists the payload facts a text reading of a component
// shows: option labels, table rows, stat values and the like. Prose types
// and layouts have none.
func componentFacts(c Component) []string {
	var facts []string
	add := func(parts ...string) {
		if fact := strings.TrimSpace(strings.Join(nonEmpty(parts), " ")); fact != "" {
			facts = append(facts, fact)
		}
	}
	switch {
	case c.Callout != nil:
		add(c.Callout.Body)
	case c.Choices != nil:
		for _, chip := range c.Choices.Chips {
			add(chip.Label)
		}
	case c.Quiz != nil:
		add(c.Quiz.Question)
		for _, option := range c.Quiz.Options {
			add(option.Label)
		}
	case c.Poll != nil:
		add(c.Poll.Question)
		for _, option := range c.Poll.Options {
			add(option.Label)
		}
	case c.Checklist != nil:
		for _, item := range c.Checklist.Items {
			add(item.Label)
		}
	case c.Steps != nil:
		for index, step := range c.Steps.Items {
			add(strconv.Itoa(index+1)+".", step.Title)
		}
	case c.Timeline != nil:
		for _, event := range c.Timeline.Events {
			add(event.When+":", event.Title)
		}
	case c.Table != nil:
		facts = tableFacts(c.Table)
	case c.StatRow != nil:
		for _, stat := range c.StatRow.Stats {
			add(stat.Label+":", amountText(Amount{Value: stat.Value, Format: stat.Format, Currency: stat.Currency, Unit: stat.Unit}), parenthesized(stat.Delta))
		}
	case c.Chart != nil:
		for _, series := range c.Chart.Series {
			prefix := ""
			if len(c.Chart.Series) > 1 {
				prefix = series.Name + " "
			}
			for _, point := range series.Points {
				add(prefix+point.Label+":", amountText(Amount{Value: point.Value, Format: c.Chart.Format, Currency: c.Chart.Currency, Unit: c.Chart.Unit}))
			}
		}
	case c.Breakdown != nil:
		for _, item := range c.Breakdown.Items {
			add(item.Label+":", amountText(Amount{Value: item.Value, Format: c.Breakdown.Format, Currency: c.Breakdown.Currency, Unit: c.Breakdown.Unit}))
		}
	case c.Progress != nil:
		for _, item := range c.Progress.Items {
			amount := func(value float64) string {
				return amountText(Amount{Value: value, Format: item.Format, Currency: item.Currency, Unit: item.Unit})
			}
			add(item.Label+":", amount(item.Value), "of", amount(item.Target))
		}
	case c.Comparison != nil:
		for _, option := range c.Comparison.Options {
			add(option.Name, priceText(option.Price))
		}
	case c.Item != nil:
		add(c.Item.Name, priceText(c.Item.Price), ratingText(c.Item.Rating))
		for _, fact := range c.Item.Facts {
			add(fact)
		}
	case c.Place != nil:
		add(c.Place.Name, ratingText(c.Place.Rating))
		add(c.Place.Address)
		for _, fact := range c.Place.Facts {
			add(fact)
		}
	case c.Definition != nil:
		add(c.Definition.Term+":", c.Definition.Meaning)
	case c.Agenda != nil:
		for _, day := range c.Agenda.Days {
			for _, item := range day.Items {
				add(day.Date, item.Start, item.Title)
			}
		}
	case c.Itinerary != nil:
		for _, day := range c.Itinerary.Days {
			for _, item := range day.Items {
				add(day.Label+":", item.Time, item.Title)
			}
		}
	case c.Recipe != nil:
		for _, ingredient := range c.Recipe.Ingredients {
			add(quantityText(ingredient.Qty), ingredient.Unit, ingredient.Name)
		}
	case c.Tasks != nil:
		for _, row := range c.Tasks.Rows {
			add(row.Label)
		}
	case c.Map != nil:
		for _, pin := range c.Map.Pins {
			add(firstText(pin.Label, pin.Place))
		}
	case c.Diagram != nil:
		for _, node := range c.Diagram.Nodes {
			add(node.Label)
		}
	case c.Form != nil:
		for _, field := range c.Form.Fields {
			add(field.Label)
		}
	case c.Flashcards != nil:
		for _, card := range c.Flashcards.Cards {
			add(card.Front)
		}
	case c.Explorer != nil:
		for _, input := range c.Explorer.Inputs {
			add(input.Label)
		}
	case c.Draft != nil:
		add(c.Draft.Subject)
		add(c.Draft.Body)
	case c.Analogy != nil:
		for _, pair := range c.Analogy.Mapping {
			add(pair.A, "↔", pair.B)
		}
	case c.BeforeAfter != nil:
		add(firstText(labelOf(c.BeforeAfter.Labels, true), "Before")+":", c.BeforeAfter.Before.Text)
		add(firstText(labelOf(c.BeforeAfter.Labels, false), "After")+":", c.BeforeAfter.After.Text)
	case c.Math != nil:
		for _, step := range c.Math.Steps {
			add(step.LaTeX)
		}
	case c.Image != nil:
		add(c.Image.Caption)
	case c.Gallery != nil:
		for _, image := range c.Gallery.Images {
			add(image.Caption)
		}
	case c.LabeledImage != nil:
		for _, marker := range c.LabeledImage.Markers {
			add(marker.Label)
		}
	case c.Agent != nil:
		add(c.Agent.Headline)
		for _, point := range c.Agent.Points {
			add(point)
		}
	case c.Routine != nil:
		for _, item := range c.Routine.Items {
			add(item.Label+":", item.Text)
		}
	case len(c.Actions) > 0:
		for _, action := range c.Actions {
			add(action.Label)
		}
	}
	return facts
}

// tableFacts is the header and one line per row, cells joined by " | ".
func tableFacts(table *Table) []string {
	labels := make([]string, 0, len(table.Columns))
	for _, column := range table.Columns {
		labels = append(labels, column.Label)
	}
	facts := []string{strings.Join(labels, " | ")}
	for _, row := range table.Rows {
		cells := make([]string, 0, len(table.Columns))
		for _, column := range table.Columns {
			cells = append(cells, tableCellText(column, row[column.Key]))
		}
		facts = append(facts, strings.Join(cells, " | "))
	}
	return facts
}

// tableCellText is a table cell as text in its column's format.
func tableCellText(column TableColumn, cell TableCell) string {
	switch {
	case cell.Text != nil:
		return *cell.Text
	case cell.Number != nil:
		format := StatNumber
		switch column.Kind {
		case "currency":
			format = StatCurrency
		case "percent":
			format = StatPercent
		}
		return amountText(Amount{Value: *cell.Number, Format: format, Currency: column.Currency})
	case cell.Image != nil:
		return "(image)"
	}
	return ""
}

// amountText writes a number in its stat format, as formatAmount does for
// the agent, with a duration in hours, minutes and seconds.
func amountText(amount Amount) string {
	if amount.Format == StatDuration {
		text := durationText(amount.Value)
		if amount.Unit != "" {
			text += " " + amount.Unit
		}
		return text
	}
	return formatAmount(amount.Value, amount.Format, amount.Currency, amount.Unit, -1)
}

func durationText(seconds float64) string {
	total := int64(math.Round(seconds))
	hours, minutes, rest := total/3600, total%3600/60, total%60
	var parts []string
	if hours > 0 {
		parts = append(parts, fmt.Sprintf("%dh", hours))
	}
	if minutes > 0 {
		parts = append(parts, fmt.Sprintf("%dm", minutes))
	}
	if rest > 0 || len(parts) == 0 {
		parts = append(parts, fmt.Sprintf("%ds", rest))
	}
	return strings.Join(parts, " ")
}

func priceText(price *Price) string {
	if price == nil {
		return ""
	}
	text := amountText(price.Amount)
	if price.Per != "" && price.Per != "once" {
		text += "/" + price.Per
	}
	return text
}

func ratingText(rating *ItemRating) string {
	if rating == nil {
		return ""
	}
	return "★ " + strconv.FormatFloat(rating.Value, 'f', -1, 64)
}

func quantityText(quantity *float64) string {
	if quantity == nil {
		return ""
	}
	return amountText(Amount{Value: *quantity})
}

func labelOf(labels *BeforeAfterLabels, before bool) string {
	if labels == nil {
		return ""
	}
	if before {
		return labels.Before
	}
	return labels.After
}

func parenthesized(text string) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	return "(" + strings.TrimSpace(text) + ")"
}

func firstText(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func nonEmpty(values []string) []string {
	out := values[:0:0]
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			out = append(out, strings.TrimSpace(value))
		}
	}
	return out
}
