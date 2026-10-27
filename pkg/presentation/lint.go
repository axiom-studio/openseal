package presentation

import (
	"strconv"
	"strings"
	"unicode"
)

// Lint codes. Lint findings are quality hints for the activity trail and
// evaluation metrics; they never block publishing.
const (
	LintChartAxisLabels         = "chart_axis_labels"
	LintChartSinglePoint        = "chart_single_point"
	LintChartFewPoints          = "chart_few_points"
	LintSingleChild             = "single_child"
	LintSummaryRepeatsTitle     = "summary_repeats_title"
	LintComparisonOneAttribute  = "comparison_one_attribute"
	LintStepsThin               = "steps_thin"
	LintTableReadsAsComparison  = "table_reads_as_comparison"
	LintQuizWithoutExplanation  = "quiz_without_explanation"
	LintTimelineLooksUnordered  = "timeline_unordered"
	longTableTextRunes          = 40
	minimumLongTextTableColumns = 3
)

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
			most := 0
			for _, series := range c.Chart.Series {
				most = max(most, len(series.Points))
			}
			switch {
			case len(c.Chart.Series) == 1 && most == 1:
				add(LintChartSinglePoint, path+".chart", c.ID, "a chart of one value reads better as a stat_row")
			case (c.Chart.Kind == "line" || c.Chart.Kind == "area") && most < 3:
				add(LintChartFewPoints, path+".chart", c.ID, "a line or area chart needs at least 3 points to show a trend; use a bar chart or a stat_row")
			}
		case "section", "grid":
			if len(c.Children) == 1 {
				add(LintSingleChild, path+".children", c.ID, c.Type+" with one child adds nothing; use the child on its own")
			}
		case "comparison":
			if c.Comparison != nil && len(c.Comparison.Attributes) == 1 {
				add(LintComparisonOneAttribute, path+".comparison", c.ID, "a comparison on one attribute reads better as a table or a chart; compare at least 2 attributes")
			}
		case "steps":
			if c.Steps != nil && len(c.Steps.Items) <= 2 {
				detailed := false
				for _, step := range c.Steps.Items {
					detailed = detailed || strings.TrimSpace(step.Detail) != ""
				}
				if !detailed {
					add(LintStepsThin, path+".steps", c.ID, "two bare steps read better as a sentence; add details or use text")
				}
			}
		case "table":
			if c.Table != nil && len(c.Table.Rows) <= 4 && longTextColumns(c.Table) >= minimumLongTextTableColumns {
				add(LintTableReadsAsComparison, path+".table", c.ID, "a short table of long text reads better as a comparison")
			}
		case "quiz":
			if c.Quiz != nil && c.Quiz.AnswerKey != nil && strings.TrimSpace(c.Quiz.AnswerKey.Explanation) == "" {
				add(LintQuizWithoutExplanation, path+".quiz.answerKey.explanation", c.ID, "explain the answer; people learn most from why it is right")
			}
		case "timeline":
			if c.Timeline != nil && timelineLooksUnordered(c.Timeline.Events) {
				add(LintTimelineLooksUnordered, path+".timeline", c.ID, "events look out of order; list them in time order or give each a date")
			}
		}
		return true
	})
	return warnings
}

// longTextColumns counts text columns holding a cell longer than
// longTableTextRunes.
func longTextColumns(table *Table) int {
	count := 0
	for _, column := range table.Columns {
		if column.Kind != "text" {
			continue
		}
		for _, row := range table.Rows {
			if cell, ok := row[column.Key]; ok && cell.Text != nil && len([]rune(*cell.Text)) > longTableTextRunes {
				count++
				break
			}
		}
	}
	return count
}

// timelineLooksUnordered reports events without dates whose labels are all
// plain numbers (years) that do not increase.
func timelineLooksUnordered(events []TimelineEvent) bool {
	for _, event := range events {
		if event.Date != "" {
			return false
		}
	}
	previous := 0.0
	for index, event := range events {
		value, err := strconv.ParseFloat(strings.TrimSpace(event.When), 64)
		if err != nil {
			return false
		}
		if index > 0 && value < previous {
			return true
		}
		previous = value
	}
	return false
}

func normalized(value string) string {
	return strings.Join(strings.FieldsFunc(strings.ToLower(value), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}), " ")
}
