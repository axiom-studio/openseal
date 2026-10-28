package presentation

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// An app in a chat reports a result to its agent (the app SDK's
// postInteraction) as a card interaction on the app itself: the
// interaction's surfaceArtifactId and surfaceId are both the app artifact's
// id, its version the app version that reported, componentId
// AppReportComponentID and actionId AppReportAction. Its stored values are {"values": {...},
// "summary"?: "..."}.
const (
	AppReportComponentID = "app"
	AppReportAction      = "report"
	// MaxAppReportValuesBytes bounds the compact JSON of a report's values.
	MaxAppReportValuesBytes = 4 * 1024
	// MaxAppReportSummaryRunes bounds a report's plain-text summary.
	MaxAppReportSummaryRunes = 200
	// MaxAppReportTextBytes bounds the line the agent reads for a report.
	MaxAppReportTextBytes = 4608
)

// InteractionReport is an app reporting a result to its agent.
const InteractionReport InteractionKind = "report"

// AppReport is a validated app report: Values is the compact JSON object
// the app sent and Summary its optional plain-text line.
type AppReport struct {
	Values  json.RawMessage `json:"values"`
	Summary string          `json:"summary,omitempty"`
}

// AppInteraction reports how the host stores an interaction on an app
// artifact. Only reports are accepted: componentId AppReportComponentID,
// actionId AppReportAction and no item key. A report is repeatable per
// actor, not shared, and wakes the agent.
func AppInteraction(componentID, actionID, itemKey string) (InteractionRule, error) {
	if componentID != AppReportComponentID {
		return InteractionRule{}, fmt.Errorf("app interactions take the component id %q (got %q)", AppReportComponentID, componentID)
	}
	if actionID != AppReportAction {
		return InteractionRule{}, fmt.Errorf("app interactions take the action id %q (got %q)", AppReportAction, actionID)
	}
	if itemKey != "" {
		return InteractionRule{}, errors.New("app reports do not take an item key")
	}
	return InteractionRule{Kind: InteractionReport, NotifyAgent: true}, nil
}

// ValidateAppReport checks the stored values of an app report,
// {"values": {...}, "summary"?: "..."}: values is a JSON object of at most
// MaxAppReportValuesBytes compact bytes, summary plain text of at most
// MaxAppReportSummaryRunes characters without control characters. Unknown
// keys are rejected. The returned report holds compact values and the
// trimmed summary.
func ValidateAppReport(values json.RawMessage) (AppReport, error) {
	var fields map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(values))
	if err := decoder.Decode(&fields); err != nil || fields == nil || decoder.More() {
		return AppReport{}, errors.New("an app report is an object {values, summary?}")
	}
	for name := range fields {
		if name != "values" && name != "summary" {
			return AppReport{}, fmt.Errorf("unexpected value %q; an app report takes only values and summary", name)
		}
	}
	raw, ok := fields["values"]
	if !ok {
		return AppReport{}, errors.New("values is required")
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return AppReport{}, errors.New("values must be a JSON object")
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, trimmed); err != nil {
		return AppReport{}, errors.New("values must be a JSON object")
	}
	if compact.Len() > MaxAppReportValuesBytes {
		return AppReport{}, fmt.Errorf("values are %d bytes; at most %d are allowed", compact.Len(), MaxAppReportValuesBytes)
	}
	report := AppReport{Values: compact.Bytes()}
	if rawSummary, ok := fields["summary"]; ok {
		var summary string
		if err := json.Unmarshal(rawSummary, &summary); err != nil {
			return AppReport{}, errors.New("summary must be a string")
		}
		summary = strings.TrimSpace(summary)
		if utf8.RuneCountInString(summary) > MaxAppReportSummaryRunes {
			return AppReport{}, fmt.Errorf("summary must be at most %d characters", MaxAppReportSummaryRunes)
		}
		if strings.IndexFunc(summary, unicode.IsControl) >= 0 {
			return AppReport{}, errors.New("summary must be one line of plain text")
		}
		report.Summary = summary
	}
	return report, nil
}

// AppReportText is the line the agent reads for an app report, e.g.
// `App result ("Times tables"): Scored 8 out of 10 {"score":8,"of":10}`:
// the summary first when present, then the compact values. For a report
// from ValidateAppReport the line is at most MaxAppReportTextBytes: the
// title is cut at 80 characters and the summary shortened to fit.
func AppReportText(appTitle string, report AppReport) string {
	title := shorten(strings.Join(strings.Fields(appTitle), " "), 80)
	prefix := "App result"
	if title != "" {
		prefix += fmt.Sprintf(" (%q)", title)
	}
	prefix += ": "
	values := string(report.Values)
	if values == "" {
		values = "{}"
	}
	summary := report.Summary
	if summary == "" {
		return prefix + values
	}
	if room := MaxAppReportTextBytes - len(prefix) - len(values) - 1; len(summary) > room {
		summary = shortenBytes(summary, room)
	}
	if summary == "" {
		return prefix + values
	}
	return prefix + summary + " " + values
}

// shorten cuts text to at most limit runes, ending in "…" when cut.
func shorten(text string, limit int) string {
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	runes := []rune(text)
	return strings.TrimSpace(string(runes[:limit-1])) + "…"
}

// shortenBytes cuts text to at most limit bytes on a rune boundary, ending
// in "…" when cut, or returns "" when not even that fits.
func shortenBytes(text string, limit int) string {
	const ellipsis = "…"
	if len(text) <= limit {
		return text
	}
	if limit < len(ellipsis)+1 {
		return ""
	}
	cut := limit - len(ellipsis)
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return strings.TrimSpace(text[:cut]) + ellipsis
}
