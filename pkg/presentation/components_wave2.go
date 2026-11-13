package presentation

import (
	"fmt"
	"strings"
)

// Diagram shows how ideas connect. The host lays it out from Layout; the
// model never gives coordinates.
type Diagram struct {
	Layout string `json:"layout"`
	// Direction (down or right, default down) orients flow and hierarchy
	// layouts.
	Direction string         `json:"direction,omitempty"`
	Nodes     []DiagramNode  `json:"nodes"`
	Edges     []DiagramEdge  `json:"edges,omitempty"`
	Groups    []DiagramGroup `json:"groups,omitempty"`
}

type DiagramNode struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Icon  string `json:"icon,omitempty"`
	// Group is the id of a DiagramGroup.
	Group string `json:"group,omitempty"`
	// Detail (markdown) shows when the node is tapped.
	Detail string `json:"detail,omitempty"`
}

type DiagramEdge struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Label string `json:"label,omitempty"`
}

type DiagramGroup struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// DiagramLayouts lists the values of Diagram.Layout: flow (steps with
// arrows), cycle (nodes in order around a ring; edges implied if omitted),
// hierarchy (a tree or forest of parent -> child edges), mindmap (the first
// node is the centre) and network (any graph).
var DiagramLayouts = []string{"flow", "cycle", "hierarchy", "mindmap", "network"}

// DiagramDirections lists the values of Diagram.Direction.
var DiagramDirections = []string{"down", "right"}

// Diagram limits.
const (
	MaxDiagramNodes  = 30
	MaxDiagramEdges  = 40
	MaxDiagramGroups = 6

	maxNodeLabelRunes  = 40
	maxNodeDetailRunes = 300
	maxEdgeLabelRunes  = 30
	maxGroupLabelRunes = 30
)

const (
	ruleDiagramLayouts = "flow: steps with arrows; cycle: nodes in order around a ring (at least 3; edges optional); " +
		"hierarchy: parent to child edges, one parent each, no loops; mindmap: the first node is the centre and every node connects to it; network: any links"
	ruleDiagramDirection = "flow and hierarchy only; down by default"
	ruleDiagramGroup     = "the id of one of groups"
	ruleDiagramEdges     = "edges join node ids"
)

func validateDiagram(v *validator, c Component, _ string) error {
	d := c.Diagram
	if d.Direction != "" && d.Layout != "flow" && d.Layout != "hierarchy" {
		return fmt.Errorf("diagram.direction is only used by flow and hierarchy layouts")
	}
	groups := map[string]bool{}
	for index, group := range d.Groups {
		if err := itemIDs(groups, fmt.Sprintf("diagram.groups[%d]", index), group.ID); err != nil {
			return err
		}
	}
	nodes := map[string]bool{}
	for index, node := range d.Nodes {
		at := fmt.Sprintf("diagram.nodes[%d]", index)
		v.icon(at+".icon", c.ID, node.Icon)
		if err := firstError(itemIDs(nodes, at, node.ID), markdown(at+".detail", node.Detail)); err != nil {
			return err
		}
		if node.Group != "" && !groups[node.Group] {
			return fmt.Errorf("%s.group %q is not the id of a group in diagram.groups", at, node.Group)
		}
	}
	for index, edge := range d.Edges {
		at := fmt.Sprintf("diagram.edges[%d]", index)
		if !nodes[edge.From] {
			return fmt.Errorf("%s.from %q is not the id of a node", at, edge.From)
		}
		if !nodes[edge.To] {
			return fmt.Errorf("%s.to %q is not the id of a node", at, edge.To)
		}
	}
	switch d.Layout {
	case "cycle":
		if len(d.Nodes) < 3 {
			return fmt.Errorf("a cycle diagram needs at least 3 nodes (got %d); use flow for two", len(d.Nodes))
		}
	case "hierarchy":
		return hierarchyIsForest(d)
	case "mindmap":
		if len(d.Edges) == 0 {
			return fmt.Errorf("a mindmap diagram needs edges from the first node (the centre) out to the others")
		}
		depth := diagramDepths(d)
		for index, node := range d.Nodes {
			if _, reached := depth[node.ID]; !reached {
				return fmt.Errorf("diagram.nodes[%d] %q is not connected to the centre %q; in a mindmap every node must be reachable from the first", index, node.ID, d.Nodes[0].ID)
			}
		}
	}
	return nil
}

// hierarchyIsForest checks that every node has at most one parent and that
// following parents never loops.
func hierarchyIsForest(d *Diagram) error {
	parent := map[string]string{}
	for index, edge := range d.Edges {
		if edge.From == edge.To {
			return fmt.Errorf("diagram.edges[%d] joins %q to itself; a hierarchy has no loops", index, edge.From)
		}
		if existing, ok := parent[edge.To]; ok {
			return fmt.Errorf("diagram.edges[%d]: %q already has the parent %q; in a hierarchy each node has one parent (edges go parent -> child)", index, edge.To, existing)
		}
		parent[edge.To] = edge.From
	}
	for _, node := range d.Nodes {
		seen := map[string]bool{node.ID: true}
		for at := node.ID; parent[at] != ""; {
			at = parent[at]
			if seen[at] {
				return fmt.Errorf("diagram.edges loop back through %q; a hierarchy has no loops", at)
			}
			seen[at] = true
		}
	}
	return nil
}

// diagramDepths returns the distance of every node reachable from the first
// node, following edges in either direction.
func diagramDepths(d *Diagram) map[string]int {
	if len(d.Nodes) == 0 {
		return nil
	}
	neighbours := map[string][]string{}
	for _, edge := range d.Edges {
		neighbours[edge.From] = append(neighbours[edge.From], edge.To)
		neighbours[edge.To] = append(neighbours[edge.To], edge.From)
	}
	depth := map[string]int{d.Nodes[0].ID: 0}
	queue := []string{d.Nodes[0].ID}
	for len(queue) > 0 {
		at := queue[0]
		queue = queue[1:]
		for _, next := range neighbours[at] {
			if _, seen := depth[next]; !seen {
				depth[next] = depth[at] + 1
				queue = append(queue, next)
			}
		}
	}
	return depth
}

// LabeledImage puts numbered markers on an image from the chat.
type LabeledImage struct {
	Image   ImageRef      `json:"image"`
	Markers []ImageMarker `json:"markers"`
}

// ImageMarker sits at X, Y: fractions of the image width and height from its
// top-left corner.
type ImageMarker struct {
	ID     string  `json:"id"`
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Label  string  `json:"label"`
	Detail string  `json:"detail,omitempty"`
}

// Labeled image limits.
const (
	maxMarkers           = 12
	maxMarkerLabelRunes  = 40
	maxMarkerDetailRunes = 300
)

func validateLabeledImage(v *validator, c Component, _ string) error {
	l := c.LabeledImage
	if err := v.image("labeled_image.image", &l.Image); err != nil {
		return err
	}
	seen := map[string]bool{}
	for index, marker := range l.Markers {
		at := fmt.Sprintf("labeled_image.markers[%d]", index)
		if err := firstError(itemIDs(seen, at, marker.ID), markdown(at+".detail", marker.Detail)); err != nil {
			return err
		}
	}
	return nil
}

// QuantityVisual makes numbers tangible for learners. Kind picks which of
// the QuantityValues fields are used; Steps let the viewer step through
// transformations, each replacing those fields.
type QuantityVisual struct {
	Kind string `json:"kind"`
	QuantityValues
	Steps []QuantityStep `json:"steps,omitempty"`
}

// QuantityStep is one stage of a quantity visual.
type QuantityStep struct {
	Label string `json:"label"`
	QuantityValues
}

// QuantityValues holds the per-kind fields of a quantity visual:
// fraction_bars uses Fractions; number_line uses Min, Max, Ticks, Points and
// Ranges; unit_blocks uses Groups; percent_waffle uses Parts; scale_compare
// uses Items.
type QuantityValues struct {
	Fractions []Fraction    `json:"fractions,omitempty"`
	Min       *float64      `json:"min,omitempty"`
	Max       *float64      `json:"max,omitempty"`
	Ticks     *float64      `json:"ticks,omitempty"`
	Points    []NumberPoint `json:"points,omitempty"`
	Ranges    []NumberRange `json:"ranges,omitempty"`
	Groups    []UnitGroup   `json:"groups,omitempty"`
	Parts     []WafflePart  `json:"parts,omitempty"`
	Items     []ScaleItem   `json:"items,omitempty"`
}

type Fraction struct {
	Numerator   int    `json:"numerator"`
	Denominator int    `json:"denominator"`
	Label       string `json:"label,omitempty"`
}

type NumberPoint struct {
	Value float64 `json:"value"`
	Label string  `json:"label,omitempty"`
}

type NumberRange struct {
	From  float64 `json:"from"`
	To    float64 `json:"to"`
	Label string  `json:"label,omitempty"`
}

type UnitGroup struct {
	Count int    `json:"count"`
	Label string `json:"label,omitempty"`
}

type WafflePart struct {
	Value float64 `json:"value"`
	Label string  `json:"label"`
}

type ScaleItem struct {
	Label string `json:"label"`
	Value Amount `json:"value"`
}

// QuantityKinds lists the values of QuantityVisual.Kind.
var QuantityKinds = []string{"fraction_bars", "number_line", "unit_blocks", "percent_waffle", "scale_compare"}

// quantityKindFields are the fields each quantity kind takes.
var quantityKindFields = map[string][]string{
	"fraction_bars": {"fractions"}, "number_line": {"min", "max", "ticks", "points", "ranges"},
	"unit_blocks": {"groups"}, "percent_waffle": {"parts"}, "scale_compare": {"items"},
}

// Quantity visual limits.
const (
	MaxQuantitySteps     = 4
	MaxDenominator       = 24
	MaxUnitBlocks        = 100
	MaxNumberLineTicks   = 100
	quantityLabelRunes   = 40
	quantityStepRunes    = 60
	maxNumberLinePoints  = 6
	maxNumberLineRanges  = 3
	maxQuantityGroupings = 4
	maxPercent           = 100
)

// Quantity rules the schema cannot state. The tick limit is
// MaxNumberLineTicks.
const (
	ruleQuantityFields     = "each kind takes only its own fields: fraction_bars fractions; number_line min and max (required), ticks, points, ranges; unit_blocks groups; percent_waffle parts; scale_compare items"
	ruleQuantityNumerator  = "at most the denominator"
	ruleQuantityNumberLine = "a number_line has min below max, at most 100 ticks between them, points and ranges inside them, and each range's from below its to"
	ruleQuantityPercent    = "percent_waffle parts total at most 100"
	ruleQuantityScale      = "scale_compare sizes are 0 or more"
)

func validateQuantityVisual(_ *validator, c Component, _ string) error {
	q := c.QuantityVisual
	if err := quantityValues("quantity_visual", q.Kind, q.QuantityValues); err != nil {
		return err
	}
	for index, step := range q.Steps {
		if err := quantityValues(fmt.Sprintf("quantity_visual.steps[%d]", index), q.Kind, step.QuantityValues); err != nil {
			return err
		}
	}
	return nil
}

// quantityValues checks the fields of one kind and that no other kind's
// fields are set.
func quantityValues(at, kind string, q QuantityValues) error {
	used := map[string]bool{
		"fractions": len(q.Fractions) > 0, "min": q.Min != nil, "max": q.Max != nil, "ticks": q.Ticks != nil, "points": len(q.Points) > 0,
		"ranges": len(q.Ranges) > 0, "groups": len(q.Groups) > 0, "parts": len(q.Parts) > 0, "items": len(q.Items) > 0,
	}
	fields := quantityKindFields[kind]
	for _, name := range []string{"fractions", "min", "max", "ticks", "points", "ranges", "groups", "parts", "items"} {
		if used[name] && !contains(fields, name) {
			return fmt.Errorf("%s.%s is not used by %s (it takes %s)", at, name, kind, strings.Join(fields, ", "))
		}
	}
	switch kind {
	case "fraction_bars":
		if len(q.Fractions) == 0 {
			return fmt.Errorf("%s.fractions needs 1-%d (got 0)", at, maxQuantityGroupings)
		}
		for index, fraction := range q.Fractions {
			if fraction.Numerator > fraction.Denominator {
				return fmt.Errorf("%s.fractions[%d].numerator must be 0-%d, at most the denominator (got %d); show a whole as its own bar", at, index, fraction.Denominator, fraction.Numerator)
			}
		}
	case "number_line":
		return numberLine(at, q)
	case "unit_blocks":
		if len(q.Groups) == 0 {
			return fmt.Errorf("%s.groups needs 1-%d (got 0)", at, maxQuantityGroupings)
		}
	case "percent_waffle":
		if len(q.Parts) == 0 {
			return fmt.Errorf("%s.parts needs 1-%d (got 0)", at, maxQuantityGroupings)
		}
		total := 0.0
		for _, part := range q.Parts {
			total += part.Value
		}
		if total > maxPercent+1e-9 {
			return fmt.Errorf("%s.parts add up to %g%%; they must total at most 100", at, total)
		}
	case "scale_compare":
		if len(q.Items) < 2 {
			return fmt.Errorf("%s.items needs 2-%d (got %d)", at, maxQuantityGroupings, len(q.Items))
		}
		for index, item := range q.Items {
			itemAt := fmt.Sprintf("%s.items[%d]", at, index)
			if err := validateAmount(itemAt+".value", item.Value); err != nil {
				return err
			}
			if item.Value.Value < 0 {
				return fmt.Errorf("%s.value.value must be 0 or more; sizes are drawn in proportion", itemAt)
			}
		}
	}
	return nil
}

func numberLine(at string, q QuantityValues) error {
	if q.Min == nil || q.Max == nil {
		return fmt.Errorf("%s needs min and max for a number_line", at)
	}
	low, high := *q.Min, *q.Max
	if low >= high {
		return fmt.Errorf("%s.min must be a number below max", at)
	}
	if q.Ticks != nil && (high-low)/(*q.Ticks) > MaxNumberLineTicks {
		return fmt.Errorf("%s.ticks is the gap between ticks and must give at most %d ticks between min and max", at, MaxNumberLineTicks)
	}
	inside := func(field string, value float64) error {
		if value < low || value > high {
			return fmt.Errorf("%s must be between min %g and max %g", field, low, high)
		}
		return nil
	}
	for index, point := range q.Points {
		if err := inside(fmt.Sprintf("%s.points[%d].value", at, index), point.Value); err != nil {
			return err
		}
	}
	for index, span := range q.Ranges {
		item := fmt.Sprintf("%s.ranges[%d]", at, index)
		if err := firstError(inside(item+".from", span.From), inside(item+".to", span.To)); err != nil {
			return err
		}
		if span.From >= span.To {
			return fmt.Errorf("%s.from must be below to", item)
		}
	}
	return nil
}

// Math is a worked solution, one LaTeX line per step.
type Math struct {
	Steps []MathStep `json:"steps"`
	// Reveal is all (default) or stepwise ("Show next step").
	Reveal string `json:"reveal,omitempty"`
}

type MathStep struct {
	LaTeX string `json:"latex"`
	Note  string `json:"note,omitempty"`
}

// MathReveals lists the values of Math.Reveal.
var MathReveals = []string{"all", "stepwise"}

// MaxLaTeXRunes bounds one math step.
// Math limits.
const (
	MaxLaTeXRunes    = 300
	maxMathSteps     = 12
	maxMathNoteRunes = 200
)

// deniedLaTeX lists macros that could fetch, embed or define code. KaTeX
// also runs with trust off, but such macros never reach a viewer.
var deniedLaTeX = map[string]bool{
	"href": true, "url": true, "includegraphics": true, "def": true, "newcommand": true,
	"renewcommand": true, "providecommand": true, "gdef": true, "edef": true, "xdef": true, "let": true,
}

const ruleLaTeX = "LaTeX without $ delimiters and without \\href, \\url, \\includegraphics, \\html… or new macros"

// ValidateLaTeX checks the rules of one math expression the schema cannot: no $
// delimiters, and none of the macros that can fetch or define (\href, \url,
// \includegraphics, \html…, \def, \newcommand and their kin).
func ValidateLaTeX(field, latex string) error {
	trimmed := strings.TrimSpace(latex)
	if strings.HasPrefix(trimmed, "$") || strings.HasSuffix(trimmed, "$") {
		return fmt.Errorf("%s is LaTeX without $ delimiters, e.g. \\frac{3}{4}", field)
	}
	for index := 0; index < len(latex); index++ {
		if latex[index] != '\\' {
			continue
		}
		end := index + 1
		for end < len(latex) && (latex[end] >= 'a' && latex[end] <= 'z' || latex[end] >= 'A' && latex[end] <= 'Z') {
			end++
		}
		name := latex[index+1 : end]
		if deniedLaTeX[name] || strings.HasPrefix(name, "html") {
			return fmt.Errorf("%s must not use \\%s; links, images, HTML and new macros are not allowed", field, name)
		}
		if end == index+1 {
			end++ // an escaped symbol such as \\ or \{
		}
		index = end - 1
	}
	return nil
}

func validateMath(_ *validator, c Component, _ string) error {
	for index, step := range c.Math.Steps {
		if err := ValidateLaTeX(fmt.Sprintf("math.steps[%d].latex", index), step.LaTeX); err != nil {
			return err
		}
	}
	return nil
}

// Definition explains one term simply.
type Definition struct {
	Term         string    `json:"term"`
	Phonetic     string    `json:"phonetic,omitempty"`
	PartOfSpeech string    `json:"partOfSpeech,omitempty"`
	Meaning      string    `json:"meaning"`
	Example      string    `json:"example,omitempty"`
	Image        *ImageRef `json:"image,omitempty"`
}

// Definition limits.
const (
	maxTermRunes         = 60
	maxPhoneticRunes     = 60
	maxPartOfSpeechRunes = 20
	maxMeaningRunes      = 300
	maxExampleRunes      = 200
)

func validateDefinition(v *validator, c Component, _ string) error {
	return v.image("definition.image", c.Definition.Image)
}

// Flashcards is a practice deck. Viewers flip cards, mark each "got" or
// "again" (mark), and finish with done, which answers you with their tally.
type Flashcards struct {
	Cards   []Flashcard `json:"cards"`
	Shuffle bool        `json:"shuffle,omitempty"`
}

type Flashcard struct {
	ID    string    `json:"id"`
	Front string    `json:"front"`
	Back  string    `json:"back"`
	Image *ImageRef `json:"image,omitempty"`
}

// Flashcard limits.
const (
	minFlashcards     = 2
	maxFlashcards     = 30
	maxCardFrontRunes = 200
	maxCardBackRunes  = 400
)

func validateFlashcards(v *validator, c Component, _ string) error {
	seen := map[string]bool{}
	for index, card := range c.Flashcards.Cards {
		at := fmt.Sprintf("flashcards.cards[%d]", index)
		if err := firstError(itemIDs(seen, at, card.ID), v.image(at+".image", card.Image)); err != nil {
			return err
		}
	}
	return nil
}

// BeforeAfter shows a change: two texts or two images.
type BeforeAfter struct {
	Mode   string             `json:"mode"`
	Before BeforeAfterSide    `json:"before"`
	After  BeforeAfterSide    `json:"after"`
	Labels *BeforeAfterLabels `json:"labels,omitempty"`
}

// BeforeAfterSide is a text or an image, never both.
type BeforeAfterSide struct {
	Text  string    `json:"text,omitempty"`
	Image *ImageRef `json:"image,omitempty"`
}

type BeforeAfterLabels struct {
	Before string `json:"before,omitempty"`
	After  string `json:"after,omitempty"`
}

// BeforeAfterModes lists the values of BeforeAfter.Mode: diff (texts, with
// changed words highlighted), split (side by side) and slider (images).
var BeforeAfterModes = []string{"diff", "split", "slider"}

// Before and after limits.
const (
	maxSideTextRunes  = 4000
	maxSideLabelRunes = 20
)

const (
	ruleBeforeAfterSides = "text or an image, not both; both sides the same kind"
	ruleBeforeAfterModes = "diff highlights changed words (texts); split shows both side by side; slider wipes between images"
)

func validateBeforeAfter(v *validator, c Component, _ string) error {
	b := c.BeforeAfter
	side := func(at string, s BeforeAfterSide) (string, error) {
		switch {
		case s.Text != "" && s.Image != nil:
			return "", fmt.Errorf("%s takes text or image, not both", at)
		case s.Image != nil:
			return "image", v.image(at+".image", s.Image)
		case strings.TrimSpace(s.Text) == "":
			return "", fmt.Errorf("%s needs text or an image", at)
		default:
			return "text", nil
		}
	}
	before, err := side("before_after.before", b.Before)
	if err != nil {
		return err
	}
	after, err := side("before_after.after", b.After)
	if err != nil {
		return err
	}
	if before != after {
		return fmt.Errorf("before_after.before is %s but after is %s; both sides must be texts or both images", before, after)
	}
	if b.Mode == "diff" && before != "text" {
		return fmt.Errorf("before_after.mode diff compares texts; use slider or split for images")
	}
	if b.Mode == "slider" && before != "image" {
		return fmt.Errorf("before_after.mode slider compares images; use diff or split for texts")
	}
	return nil
}

// Draft is a message the agent wrote for the user. Unbound it offers "Looks
// good", "Make it shorter" and edit; bound to the pending approval of the
// send the agent proposed, its buttons decide that approval.
type Draft struct {
	Channel string `json:"channel"`
	// To is shown only; it does not address anything.
	To      string `json:"to,omitempty"`
	Subject string `json:"subject,omitempty"`
	Body    string `json:"body"`
}

// DraftChannels lists the values of Draft.Channel.
var DraftChannels = []string{"email", "message", "post", "note"}

// Draft limits.
const (
	maxDraftHeaderRunes = 200 // to and subject
	maxDraftBodyRunes   = 4000
)

func validateDraft(_ *validator, c Component, _ string) error {
	return markdown("draft.body", c.Draft.Body)
}

// Recipe is a dish with scalable ingredients and steps.
type Recipe struct {
	Servings    int                `json:"servings"`
	Time        *RecipeTime        `json:"time,omitempty"`
	Ingredients []RecipeIngredient `json:"ingredients"`
	Steps       []RecipeStep       `json:"steps"`
	Image       *ImageRef          `json:"image,omitempty"`
	// Checkable lets people tick ingredients off together.
	Checkable bool `json:"checkable,omitempty"`
}

// RecipeTime is in minutes.
type RecipeTime struct {
	Prep *int `json:"prep,omitempty"`
	Cook *int `json:"cook,omitempty"`
}

type RecipeIngredient struct {
	ID string `json:"id"`
	// Qty is for Servings; viewers rescale it.
	Qty   *float64 `json:"qty,omitempty"`
	Unit  string   `json:"unit,omitempty"`
	Name  string   `json:"name"`
	Group string   `json:"group,omitempty"`
}

type RecipeStep struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// MaxRecipeMinutes bounds a recipe's prep and cook times (a week).
// Recipe limits.
const (
	MaxRecipeMinutes        = 7 * 24 * 60
	maxServings             = 24
	maxIngredients          = 30
	maxRecipeSteps          = 15
	maxIngredientUnitRunes  = 16
	maxIngredientNameRunes  = 60
	maxIngredientGroupRunes = 30
	maxRecipeStepRunes      = 300
)

func validateRecipe(v *validator, c Component, _ string) error {
	r := c.Recipe
	if err := v.image("recipe.image", r.Image); err != nil {
		return err
	}
	seen := map[string]bool{}
	for index, ingredient := range r.Ingredients {
		if err := itemIDs(seen, fmt.Sprintf("recipe.ingredients[%d]", index), ingredient.ID); err != nil {
			return err
		}
	}
	steps := map[string]bool{}
	for index, step := range r.Steps {
		if err := itemIDs(steps, fmt.Sprintf("recipe.steps[%d]", index), step.ID); err != nil {
			return err
		}
	}
	return nil
}

// Item is one product, option or listing. It has no link yet.
type Item struct {
	Name   string      `json:"name"`
	Image  *ImageRef   `json:"image,omitempty"`
	Price  *Price      `json:"price,omitempty"`
	Rating *ItemRating `json:"rating,omitempty"`
	Facts  []string    `json:"facts,omitempty"`
	Badge  string      `json:"badge,omitempty"`
}

type ItemRating struct {
	Value float64 `json:"value"`
	Count *int    `json:"count,omitempty"`
}

// Item limits.
const (
	maxItemNameRunes = 80
	maxFacts         = 4 // facts of an item or a place
	maxFactRunes     = 60
)

func validateItem(v *validator, c Component, _ string) error {
	item := c.Item
	if err := v.image("item.image", item.Image); err != nil {
		return err
	}
	if item.Price != nil {
		return validateAmount("item.price", item.Price.Amount)
	}
	return nil
}

// Analogy pairs something new with something familiar.
type Analogy struct {
	From    AnalogySide      `json:"from"`
	To      AnalogySide      `json:"to"`
	Mapping []AnalogyMapping `json:"mapping"`
}

type AnalogySide struct {
	Label string `json:"label"`
	Icon  string `json:"icon,omitempty"`
}

// AnalogyMapping says A (in From) is like B (in To).
type AnalogyMapping struct {
	A string `json:"a"`
	B string `json:"b"`
}

// Analogy limits.
const (
	maxAnalogyLabelRunes = 40
	maxAnalogyPairs      = 5
	maxAnalogyPartRunes  = 60
)

func validateAnalogy(v *validator, c Component, _ string) error {
	v.icon("analogy.from.icon", c.ID, c.Analogy.From.Icon)
	v.icon("analogy.to.icon", c.ID, c.Analogy.To.Icon)
	return nil
}
