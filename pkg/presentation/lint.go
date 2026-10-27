package presentation

import (
	"strings"
	"unicode"
)

// Lint codes. Lint findings are quality hints for the activity trail and
// evaluation metrics; they never block publishing.
const (
	LintChartAxisLabels     = "chart_axis_labels"
	LintPieSlices           = "pie_slices"
	LintChartSinglePoint    = "chart_single_point"
	LintSingleChild         = "single_child"
	LintSummaryRepeatsTitle = "summary_repeats_title"
)

// MaxPieSlices is the slice count above which a pie or donut reads poorly.
const MaxPieSlices = 7

// Lint returns quality warnings for a surface. It assumes nothing about
// validity and skips what it cannot inspect.
func Lint(surface Surface) []Warning {
	var warnings []Warning
	add := func(code, path, id, message string) {
		warnings = append(warnings, Warning{Code: code, Path: path, ComponentID: id, Message: message})
	}
	walk(surface.Components, func(c *Component, path string, _ int) bool {
		if c.Summary != "" && c.Title != "" && normalized(c.Summary) == normalized(c.Title) {
			add(LintSummaryRepeatsTitle, path+".summary", c.ID, "summary repeats the title; say what the visual shows")
		}
		switch c.Type {
		case "chart":
			if c.Chart == nil {
				break
			}
			round := c.Chart.Kind == "pie" || c.Chart.Kind == "donut"
			if !round && (strings.TrimSpace(c.Chart.XLabel) == "" || strings.TrimSpace(c.Chart.YLabel) == "") {
				add(LintChartAxisLabels, path+".chart", c.ID, "label both axes and include units, e.g. yLabel \"Cost (£)\"")
			}
			if round {
				for _, series := range c.Chart.Series {
					if len(series.Points) > MaxPieSlices {
						add(LintPieSlices, path+".chart", c.ID, "pie and donut charts read best with at most 7 slices; group the rest as \"Other\" or use a bar chart")
						break
					}
				}
			}
			if len(c.Chart.Series) == 1 && len(c.Chart.Series[0].Points) == 1 {
				add(LintChartSinglePoint, path+".chart", c.ID, "a chart of one value reads better as a stat_row")
			}
		case "section", "grid":
			if len(c.Children) == 1 {
				add(LintSingleChild, path+".children", c.ID, c.Type+" with one child adds nothing; use the child on its own")
			}
		}
		return true
	})
	return warnings
}

func normalized(value string) string {
	return strings.Join(strings.FieldsFunc(strings.ToLower(value), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}), " ")
}
