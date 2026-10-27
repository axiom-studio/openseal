package presentation

import (
	"fmt"
	"math"
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
)

func validateDiagram(v *validator, c Component, _ string) error {
	d := c.Diagram
	if d == nil {
		return payloadRequired("diagram")
	}
	if !contains(DiagramLayouts, d.Layout) {
		return fmt.Errorf("diagram.layout %q is not one of %s", d.Layout, strings.Join(DiagramLayouts, ", "))
	}
	if d.Direction != "" {
		if d.Layout != "flow" && d.Layout != "hierarchy" {
			return fmt.Errorf("diagram.direction is only used by flow and hierarchy layouts")
		}
		if err := oneOf("diagram.direction", d.Direction, DiagramDirections); err != nil {
			return err
		}
	}
	if err := firstError(bounded("diagram.nodes", len(d.Nodes), 2, MaxDiagramNodes), bounded("diagram.edges", len(d.Edges), 0, MaxDiagramEdges),
		bounded("diagram.groups", len(d.Groups), 0, MaxDiagramGroups)); err != nil {
		return err
	}
	groups := map[string]bool{}
	for index, group := range d.Groups {
		at := fmt.Sprintf("diagram.groups[%d]", index)
		if err := firstError(itemIDs(groups, at, group.ID), required(at+".label", group.Label, 30)); err != nil {
			return err
		}
	}
	nodes := map[string]bool{}
	for index, node := range d.Nodes {
		at := fmt.Sprintf("diagram.nodes[%d]", index)
		if err := firstError(itemIDs(nodes, at, node.ID), required(at+".label", node.Label, 40), v.icon(at+".icon", c.ID, node.Icon),
			markdown(at+".detail", node.Detail, 0, 300)); err != nil {
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
		if err := optional(at+".label", edge.Label, 30); err != nil {
			return err
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

func validateLabeledImage(v *validator, c Component, _ string) error {
	l := c.LabeledImage
	if l == nil {
		return payloadRequired("labeled_image")
	}
	if err := firstError(v.image("labeled_image.image", &l.Image), bounded("labeled_image.markers", len(l.Markers), 1, 12)); err != nil {
		return err
	}
	seen := map[string]bool{}
	for index, marker := range l.Markers {
		at := fmt.Sprintf("labeled_image.markers[%d]", index)
		if err := firstError(itemIDs(seen, at, marker.ID), unitInterval(at+".x", marker.X), unitInterval(at+".y", marker.Y),
			required(at+".label", marker.Label, 40), markdown(at+".detail", marker.Detail, 0, 300)); err != nil {
			return err
		}
	}
	return nil
}

func unitInterval(field string, value float64) error {
	if math.IsNaN(value) || value < 0 || value > 1 {
		return fmt.Errorf("%s must be between 0 and 1 (a fraction of the image size from the top-left)", field)
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
)

func validateQuantityVisual(_ *validator, c Component, _ string) error {
	q := c.QuantityVisual
	if q == nil {
		return payloadRequired("quantity_visual")
	}
	if !contains(QuantityKinds, q.Kind) {
		return fmt.Errorf("quantity_visual.kind %q is not one of %s", q.Kind, strings.Join(QuantityKinds, ", "))
	}
	if err := quantityValues("quantity_visual", q.Kind, q.QuantityValues); err != nil {
		return err
	}
	if err := bounded("quantity_visual.steps", len(q.Steps), 0, MaxQuantitySteps); err != nil {
		return err
	}
	for index, step := range q.Steps {
		at := fmt.Sprintf("quantity_visual.steps[%d]", index)
		if err := firstError(required(at+".label", step.Label, quantityStepRunes), quantityValues(at, q.Kind, step.QuantityValues)); err != nil {
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
	fields := map[string][]string{
		"fraction_bars": {"fractions"}, "number_line": {"min", "max", "ticks", "points", "ranges"},
		"unit_blocks": {"groups"}, "percent_waffle": {"parts"}, "scale_compare": {"items"},
	}[kind]
	for _, name := range []string{"fractions", "min", "max", "ticks", "points", "ranges", "groups", "parts", "items"} {
		if used[name] && !contains(fields, name) {
			return fmt.Errorf("%s.%s is not used by %s (it takes %s)", at, name, kind, strings.Join(fields, ", "))
		}
	}
	switch kind {
	case "fraction_bars":
		if err := bounded(at+".fractions", len(q.Fractions), 1, maxQuantityGroupings); err != nil {
			return err
		}
		for index, fraction := range q.Fractions {
			item := fmt.Sprintf("%s.fractions[%d]", at, index)
			if fraction.Denominator < 1 || fraction.Denominator > MaxDenominator {
				return fmt.Errorf("%s.denominator must be 1-%d (got %d)", item, MaxDenominator, fraction.Denominator)
			}
			if fraction.Numerator < 0 || fraction.Numerator > fraction.Denominator {
				return fmt.Errorf("%s.numerator must be 0-%d, at most the denominator (got %d); show a whole as its own bar", item, fraction.Denominator, fraction.Numerator)
			}
			if err := optional(item+".label", fraction.Label, quantityLabelRunes); err != nil {
				return err
			}
		}
	case "number_line":
		return numberLine(at, q)
	case "unit_blocks":
		if err := bounded(at+".groups", len(q.Groups), 1, maxQuantityGroupings); err != nil {
			return err
		}
		for index, group := range q.Groups {
			item := fmt.Sprintf("%s.groups[%d]", at, index)
			if group.Count < 0 || group.Count > MaxUnitBlocks {
				return fmt.Errorf("%s.count must be 0-%d (got %d)", item, MaxUnitBlocks, group.Count)
			}
			if err := optional(item+".label", group.Label, quantityLabelRunes); err != nil {
				return err
			}
		}
	case "percent_waffle":
		if err := bounded(at+".parts", len(q.Parts), 1, maxQuantityGroupings); err != nil {
			return err
		}
		total := 0.0
		for index, part := range q.Parts {
			item := fmt.Sprintf("%s.parts[%d]", at, index)
			if math.IsNaN(part.Value) || part.Value < 0 || part.Value > 100 {
				return fmt.Errorf("%s.value must be a percentage from 0 to 100", item)
			}
			if err := required(item+".label", part.Label, quantityLabelRunes); err != nil {
				return err
			}
			total += part.Value
		}
		if total > 100+1e-9 {
			return fmt.Errorf("%s.parts add up to %g%%; they must total at most 100", at, total)
		}
	case "scale_compare":
		if err := bounded(at+".items", len(q.Items), 2, maxQuantityGroupings); err != nil {
			return err
		}
		for index, item := range q.Items {
			itemAt := fmt.Sprintf("%s.items[%d]", at, index)
			if err := firstError(required(itemAt+".label", item.Label, quantityLabelRunes), validateAmount(itemAt+".value", item.Value)); err != nil {
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
	if math.IsNaN(low) || math.IsInf(low, 0) || math.IsNaN(high) || math.IsInf(high, 0) || low >= high {
		return fmt.Errorf("%s.min must be a number below max", at)
	}
	if q.Ticks != nil {
		step := *q.Ticks
		if math.IsNaN(step) || step <= 0 || (high-low)/step > MaxNumberLineTicks {
			return fmt.Errorf("%s.ticks is the gap between ticks and must give at most %d ticks between min and max", at, MaxNumberLineTicks)
		}
	}
	inside := func(field string, value float64) error {
		if math.IsNaN(value) || value < low || value > high {
			return fmt.Errorf("%s must be between min %g and max %g", field, low, high)
		}
		return nil
	}
	if err := firstError(bounded(at+".points", len(q.Points), 0, maxNumberLinePoints), bounded(at+".ranges", len(q.Ranges), 0, maxNumberLineRanges)); err != nil {
		return err
	}
	for index, point := range q.Points {
		item := fmt.Sprintf("%s.points[%d]", at, index)
		if err := firstError(inside(item+".value", point.Value), optional(item+".label", point.Label, quantityLabelRunes)); err != nil {
			return err
		}
	}
	for index, span := range q.Ranges {
		item := fmt.Sprintf("%s.ranges[%d]", at, index)
		if err := firstError(inside(item+".from", span.From), inside(item+".to", span.To), optional(item+".label", span.Label, quantityLabelRunes)); err != nil {
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
const MaxLaTeXRunes = 300

// deniedLaTeX lists macros that could fetch, embed or define code. KaTeX
// also runs with trust off, but such macros never reach a viewer.
var deniedLaTeX = map[string]bool{
	"href": true, "url": true, "includegraphics": true, "def": true, "newcommand": true,
	"renewcommand": true, "providecommand": true, "gdef": true, "edef": true, "xdef": true, "let": true,
}

// ValidateLaTeX checks one math expression: at most MaxLaTeXRunes, no $
// delimiters, and none of the macros that can fetch or define (\href, \url,
// \includegraphics, \html…, \def, \newcommand and their kin).
func ValidateLaTeX(field, latex string) error {
	if err := required(field, latex, MaxLaTeXRunes); err != nil {
		return err
	}
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
	m := c.Math
	if m == nil {
		return payloadRequired("math")
	}
	if err := firstError(bounded("math.steps", len(m.Steps), 1, 12), oneOf("math.reveal", m.Reveal, MathReveals)); err != nil {
		return err
	}
	for index, step := range m.Steps {
		at := fmt.Sprintf("math.steps[%d]", index)
		if err := firstError(ValidateLaTeX(at+".latex", step.LaTeX), optional(at+".note", step.Note, 200)); err != nil {
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

func validateDefinition(v *validator, c Component, _ string) error {
	d := c.Definition
	if d == nil {
		return payloadRequired("definition")
	}
	return firstError(required("definition.term", d.Term, 60), optional("definition.phonetic", d.Phonetic, 60),
		optional("definition.partOfSpeech", d.PartOfSpeech, 20), required("definition.meaning", d.Meaning, 300),
		optional("definition.example", d.Example, 200), v.image("definition.image", d.Image))
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

func validateFlashcards(v *validator, c Component, _ string) error {
	f := c.Flashcards
	if f == nil {
		return payloadRequired("flashcards")
	}
	if err := bounded("flashcards.cards", len(f.Cards), 2, 30); err != nil {
		return err
	}
	seen := map[string]bool{}
	for index, card := range f.Cards {
		at := fmt.Sprintf("flashcards.cards[%d]", index)
		if err := firstError(itemIDs(seen, at, card.ID), required(at+".front", card.Front, 200), required(at+".back", card.Back, 400),
			v.image(at+".image", card.Image)); err != nil {
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

func validateBeforeAfter(v *validator, c Component, _ string) error {
	b := c.BeforeAfter
	if b == nil {
		return payloadRequired("before_after")
	}
	if !contains(BeforeAfterModes, b.Mode) {
		return fmt.Errorf("before_after.mode %q is not one of %s", b.Mode, strings.Join(BeforeAfterModes, ", "))
	}
	side := func(at string, s BeforeAfterSide) (string, error) {
		switch {
		case s.Text != "" && s.Image != nil:
			return "", fmt.Errorf("%s takes text or image, not both", at)
		case s.Image != nil:
			return "image", v.image(at+".image", s.Image)
		default:
			return "text", required(at+".text", s.Text, 4000)
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
	if b.Labels != nil {
		return firstError(optional("before_after.labels.before", b.Labels.Before, 20), optional("before_after.labels.after", b.Labels.After, 20))
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

func validateDraft(_ *validator, c Component, _ string) error {
	d := c.Draft
	if d == nil {
		return payloadRequired("draft")
	}
	if !contains(DraftChannels, d.Channel) {
		return fmt.Errorf("draft.channel %q is not one of %s", d.Channel, strings.Join(DraftChannels, ", "))
	}
	return firstError(optional("draft.to", d.To, 200), optional("draft.subject", d.Subject, 200), markdown("draft.body", d.Body, 1, 4000))
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
const MaxRecipeMinutes = 7 * 24 * 60

func validateRecipe(v *validator, c Component, _ string) error {
	r := c.Recipe
	if r == nil {
		return payloadRequired("recipe")
	}
	if r.Servings < 1 || r.Servings > 24 {
		return fmt.Errorf("recipe.servings must be 1-24 (got %d)", r.Servings)
	}
	if r.Time != nil {
		for index, minutes := range []*int{r.Time.Prep, r.Time.Cook} {
			if minutes != nil && (*minutes < 0 || *minutes > MaxRecipeMinutes) {
				return fmt.Errorf("recipe.time.%s is in minutes and must be 0-%d", []string{"prep", "cook"}[index], MaxRecipeMinutes)
			}
		}
	}
	if err := firstError(bounded("recipe.ingredients", len(r.Ingredients), 1, 30), bounded("recipe.steps", len(r.Steps), 1, 15), v.image("recipe.image", r.Image)); err != nil {
		return err
	}
	seen := map[string]bool{}
	for index, ingredient := range r.Ingredients {
		at := fmt.Sprintf("recipe.ingredients[%d]", index)
		if err := firstError(itemIDs(seen, at, ingredient.ID), optional(at+".unit", ingredient.Unit, 16), required(at+".name", ingredient.Name, 60),
			optional(at+".group", ingredient.Group, 30)); err != nil {
			return err
		}
		if qty := ingredient.Qty; qty != nil && (math.IsNaN(*qty) || math.IsInf(*qty, 0) || *qty <= 0) {
			return fmt.Errorf("%s.qty must be a number above 0; leave it out for \"to taste\"", at)
		}
	}
	steps := map[string]bool{}
	for index, step := range r.Steps {
		at := fmt.Sprintf("recipe.steps[%d]", index)
		if err := firstError(itemIDs(steps, at, step.ID), required(at+".text", step.Text, 300)); err != nil {
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

func validateItem(v *validator, c Component, _ string) error {
	item := c.Item
	if item == nil {
		return payloadRequired("item")
	}
	if err := firstError(required("item.name", item.Name, 80), v.image("item.image", item.Image), optional("item.badge", item.Badge, 24),
		bounded("item.facts", len(item.Facts), 0, 4)); err != nil {
		return err
	}
	if item.Price != nil {
		if err := firstError(validateAmount("item.price", item.Price.Amount), oneOf("item.price.per", item.Price.Per, PricePeriods)); err != nil {
			return err
		}
	}
	if rating := item.Rating; rating != nil {
		if math.IsNaN(rating.Value) || rating.Value < 0 || rating.Value > 5 {
			return fmt.Errorf("item.rating.value must be 0-5")
		}
		if rating.Count != nil && *rating.Count < 0 {
			return fmt.Errorf("item.rating.count must be 0 or more")
		}
	}
	for index, fact := range item.Facts {
		if err := required(fmt.Sprintf("item.facts[%d]", index), fact, 60); err != nil {
			return err
		}
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

func validateAnalogy(v *validator, c Component, _ string) error {
	a := c.Analogy
	if a == nil {
		return payloadRequired("analogy")
	}
	if err := firstError(required("analogy.from.label", a.From.Label, 40), v.icon("analogy.from.icon", c.ID, a.From.Icon),
		required("analogy.to.label", a.To.Label, 40), v.icon("analogy.to.icon", c.ID, a.To.Icon),
		bounded("analogy.mapping", len(a.Mapping), 1, 5)); err != nil {
		return err
	}
	for index, pair := range a.Mapping {
		at := fmt.Sprintf("analogy.mapping[%d]", index)
		if err := firstError(required(at+".a", pair.A, 60), required(at+".b", pair.B, 60)); err != nil {
			return err
		}
	}
	return nil
}
