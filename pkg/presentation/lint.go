package presentation

import (
	"fmt"

	"strconv"
	"strings"
	"unicode"

	"github.com/axiom-studio/openseal/pkg/presentation/expr"
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
	LintDiagramWithoutEdges     = "diagram_without_edges"
	LintMindmapTooDeep          = "mindmap_too_deep"
	LintLabeledImageOneMarker   = "labeled_image_one_marker"
	LintFlashcardFrontIsBack    = "flashcard_front_is_back"
	LintMathSingleStep          = "math_single_step"
	LintDraftLongMessage        = "draft_long_message"
	LintRecipeThinSteps         = "recipe_thin_steps"
	LintAgendaOneItem           = "agenda_one_item"
	LintItineraryThinDay        = "itinerary_thin_day"
	LintBreakdownOneDominant    = "breakdown_one_dominant"
	LintPollTwoMultiple         = "poll_two_multiple"
	LintExplorerStaticOutputs   = "explorer_static_outputs"
	LintRoutineWithoutItems     = "routine_without_items"
	LintTasksOneRow             = "tasks_one_row"
	LintMapOnePin               = "map_one_pin"
	LintItineraryOneLocation    = "itinerary_one_location"
	dominantShare               = 0.95
	maxMindmapDepth             = 3
	longMessageRunes            = 1500
	thinRecipeStepRunes         = 10
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
		case "diagram":
			if c.Diagram == nil {
				break
			}
			if len(c.Diagram.Edges) == 0 && c.Diagram.Layout != "cycle" {
				add(LintDiagramWithoutEdges, path+".diagram.edges", c.ID, "a diagram without edges shows no connections; add edges or use a list")
			}
			if c.Diagram.Layout == "mindmap" {
				deepest := 0
				for _, depth := range diagramDepths(c.Diagram) {
					deepest = max(deepest, depth)
				}
				if deepest > maxMindmapDepth {
					add(LintMindmapTooDeep, path+".diagram", c.ID, "a mindmap more than 3 levels deep is hard to read; keep it to 3 or use a hierarchy")
				}
			}
		case "labeled_image":
			if c.LabeledImage != nil && len(c.LabeledImage.Markers) == 1 {
				add(LintLabeledImageOneMarker, path+".labeled_image.markers", c.ID, "one marker reads better as an image with a caption")
			}
		case "flashcards":
			if c.Flashcards == nil {
				break
			}
			for index, card := range c.Flashcards.Cards {
				if normalized(card.Front) == normalized(card.Back) {
					add(LintFlashcardFrontIsBack, fmt.Sprintf("%s.flashcards.cards[%d]", path, index), c.ID, "the back repeats the front; put the answer on the back")
					break
				}
			}
		case "math":
			if c.Math != nil && len(c.Math.Steps) == 1 && strings.TrimSpace(c.Math.Steps[0].Note) == "" {
				add(LintMathSingleStep, path+".math.steps", c.ID, "one step without a note reads better in text; show the working or add a note")
			}
		case "draft":
			if c.Draft != nil && c.Draft.Channel == "message" && len([]rune(strings.TrimSpace(c.Draft.Body))) > longMessageRunes {
				add(LintDraftLongMessage, path+".draft.body", c.ID, "a message over 1,500 characters is long to read on a phone; shorten it or make it an email")
			}
		case "recipe":
			if c.Recipe != nil && recipeStepsThin(c.Recipe.Steps) {
				add(LintRecipeThinSteps, path+".recipe.steps", c.ID, "the steps are too short to cook from; say what to do in each")
			}
		case "agenda":
			if c.Agenda != nil && agendaItems(c.Agenda) == 1 {
				add(LintAgendaOneItem, path+".agenda", c.ID, "an agenda with one item reads better as a sentence")
			}
		case "itinerary":
			if c.Itinerary == nil {
				break
			}
			located := 0
			for _, day := range c.Itinerary.Days {
				for _, item := range day.Items {
					if item.Location != "" {
						located++
					}
				}
			}
			if located == 1 {
				add(LintItineraryOneLocation, path+".itinerary", c.ID, "one stop with a location draws no map; give the other stops locations too or leave it out")
			}
			for index, day := range c.Itinerary.Days {
				if len(day.Items) == 1 {
					add(LintItineraryThinDay, fmt.Sprintf("%s.itinerary.days[%d]", path, index), c.ID, "a day with one stop and no travel adds little; merge it or say it in text")
					break
				}
			}
		case "breakdown":
			if c.Breakdown != nil && breakdownDominated(c.Breakdown.Items) {
				add(LintBreakdownOneDominant, path+".breakdown.items", c.ID, "one part is over 95% of the whole, so the rest are invisible; say it in a sentence or a stat_row")
			}
		case "poll":
			if c.Poll != nil && c.Poll.Multiple && len(c.Poll.Options) == 2 {
				add(LintPollTwoMultiple, path+".poll.multiple", c.ID, "with two options, voting for both says nothing; leave multiple off")
			}
		case "explorer":
			if c.Explorer != nil && explorerOutputsStatic(c.Explorer) {
				add(LintExplorerStaticOutputs, path+".explorer.outputs", c.ID, "no output uses an input, so moving the controls changes nothing; use the input ids in expr")
			}
		case "routine":
			if c.Routine == nil || len(c.Routine.Items) == 0 {
				add(LintRoutineWithoutItems, path+".routine.items", c.ID, "show the latest highlights from the routine in items")
			}
		case "tasks":
			if c.Tasks != nil && len(c.Tasks.Rows) == 1 {
				add(LintTasksOneRow, path+".tasks.rows", c.ID, "one task reads better as a sentence")
			}
		case "map":
			if c.Map != nil && len(c.Map.Pins) == 1 {
				add(LintMapOnePin, path+".map.pins", c.ID, "a map of one place reads better as a place card with its location")
			}
		}
		return true
	})
	return warnings
}

func agendaItems(agenda *Agenda) int {
	count := 0
	for _, day := range agenda.Days {
		count += len(day.Items)
	}
	return count
}

// breakdownDominated reports one item over dominantShare of the total.
func breakdownDominated(items []BreakdownItem) bool {
	total, largest := 0.0, 0.0
	for _, item := range items {
		total += item.Value
		largest = max(largest, item.Value)
	}
	return total > 0 && largest/total > dominantShare
}

// explorerOutputsStatic reports outputs that all compile and read no input.
func explorerOutputsStatic(explorer *Explorer) bool {
	types := explorerTypes(explorer.Inputs)
	for _, output := range explorer.Outputs {
		program, err := expr.Compile(output.Expr, types)
		if err != nil || len(program.Uses()) > 0 {
			return false
		}
	}
	return len(explorer.Outputs) > 0
}

// recipeStepsThin reports steps that are all under thinRecipeStepRunes.
func recipeStepsThin(steps []RecipeStep) bool {
	if len(steps) == 0 {
		return false
	}
	for _, step := range steps {
		if len([]rune(strings.TrimSpace(step.Text))) >= thinRecipeStepRunes {
			return false
		}
	}
	return true
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
