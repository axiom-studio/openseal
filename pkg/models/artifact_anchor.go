package models

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"unicode"
	"unicode/utf8"
)

// An ArtifactAnchor points a comment at one spot of an agent output: a text
// selection, a paragraph, a sheet cell, a page element or a point. A comment
// is an ordinary channel message whose single artifact reference carries the
// anchor; the model sees it as a <comment-anchor> block rendered by the
// conversation package.

type ArtifactAnchorType string

const (
	ArtifactAnchorText      ArtifactAnchorType = "text"
	ArtifactAnchorParagraph ArtifactAnchorType = "paragraph"
	ArtifactAnchorLayout    ArtifactAnchorType = "layout"
	ArtifactAnchorCell      ArtifactAnchorType = "cell"
	ArtifactAnchorElement   ArtifactAnchorType = "element"
	ArtifactAnchorPoint     ArtifactAnchorType = "point"
)

// AnchorRect and AnchorPoint are fractions 0..1 of the commented surface.
type AnchorRect struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

type AnchorPoint struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

// AnchorAppState is where an app was when the person commented on it.
type AnchorAppState struct {
	Screen  string `json:"screen,omitempty"`
	Heading string `json:"heading,omitempty"`
	Dialog  string `json:"dialog,omitempty"`
}

type ArtifactAnchor struct {
	Type     ArtifactAnchorType `json:"type"`
	Quote    string             `json:"quote,omitempty"`
	Page     int                `json:"page,omitempty"`
	Sheet    string             `json:"sheet,omitempty"`
	Range    string             `json:"range,omitempty"`
	Selector string             `json:"selector,omitempty"`
	Label    string             `json:"label,omitempty"`
	Tag      string             `json:"tag,omitempty"`
	Snippet  string             `json:"snippet,omitempty"`
	Styles   map[string]string  `json:"styles,omitempty"`
	Rect     *AnchorRect        `json:"rect,omitempty"`
	Point    *AnchorPoint       `json:"point,omitempty"`
	AppState *AnchorAppState    `json:"appState,omitempty"`
}

// Anchor limits. The client trims to them; the server rejects, never trims,
// so what the person saw in the chip is exactly what the model gets.
const (
	maxArtifactAnchorBytes = 4096
	maxAnchorQuoteBytes    = 1000
	maxAnchorSnippetBytes  = 1500
	maxAnchorSelectorBytes = 512
	maxAnchorLabelBytes    = 120
	maxAnchorSheetBytes    = 64
	maxAnchorPage          = 10000
	maxAnchorStyles        = 12
	maxAnchorStyleValue    = 120
	maxAnchorAppStateBytes = 200
)

var (
	anchorTagPattern   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	anchorRangePattern = regexp.MustCompile(`^(?:[A-Z]{1,3}[0-9]{1,7}(?::[A-Z]{1,3}[0-9]{1,7})?|[0-9]{1,7}(?:-[0-9]{1,7})?)$`)
	anchorStyleKeys    = map[string]bool{
		"background": true, "background-color": true, "color": true, "font-family": true, "font-size": true,
		"font-weight": true, "display": true, "grid-template-columns": true, "flex-direction": true, "gap": true,
		"padding": true, "width": true, "height": true,
	}
)

func invalidAnchor(format string, args ...any) error {
	return fmt.Errorf("%w: anchor "+format, append([]any{ErrInvalidConversation}, args...)...)
}

func (a *ArtifactAnchor) Validate() error {
	if a == nil {
		return nil
	}
	encoded, err := json.Marshal(a)
	if err != nil {
		return invalidAnchor("cannot be encoded: %v", err)
	}
	if len(encoded) > maxArtifactAnchorBytes {
		return invalidAnchor("exceeds %d bytes", maxArtifactAnchorBytes)
	}
	if err := a.validateRequired(); err != nil {
		return err
	}
	for _, field := range []struct {
		name, value string
		limit       int
		newlines    bool
	}{
		{"quote", a.Quote, maxAnchorQuoteBytes, true},
		{"snippet", a.Snippet, maxAnchorSnippetBytes, true},
		{"selector", a.Selector, maxAnchorSelectorBytes, false},
		{"label", a.Label, maxAnchorLabelBytes, false},
		{"sheet", a.Sheet, maxAnchorSheetBytes, false},
	} {
		if err := validateAnchorText(field.name, field.value, field.limit, field.newlines); err != nil {
			return err
		}
	}
	if a.Tag != "" && !anchorTagPattern.MatchString(a.Tag) {
		return invalidAnchor("tag %q is not a lowercase tag name", a.Tag)
	}
	if a.Range != "" && !anchorRangePattern.MatchString(a.Range) {
		return invalidAnchor("range %q is neither a cell range nor a line range", a.Range)
	}
	if a.Page < 0 || a.Page > maxAnchorPage {
		return invalidAnchor("page must be within 0..%d", maxAnchorPage)
	}
	if err := a.validateStyles(); err != nil {
		return err
	}
	if r := a.Rect; r != nil && !anchorFractions(r.X, r.Y, r.W, r.H) {
		return invalidAnchor("rect values must be finite fractions 0..1")
	}
	if p := a.Point; p != nil && !anchorFractions(p.X, p.Y) {
		return invalidAnchor("point values must be finite fractions 0..1")
	}
	if s := a.AppState; s != nil {
		for name, value := range map[string]string{"screen": s.Screen, "heading": s.Heading, "dialog": s.Dialog} {
			if err := validateAnchorText("appState."+name, value, maxAnchorAppStateBytes, false); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *ArtifactAnchor) validateRequired() error {
	switch a.Type {
	case ArtifactAnchorText, ArtifactAnchorParagraph:
		if a.Quote == "" {
			return invalidAnchor("of type %s needs a quote", a.Type)
		}
	case ArtifactAnchorCell:
		if a.Sheet == "" || a.Range == "" {
			return invalidAnchor("of type cell needs a sheet and a range")
		}
	case ArtifactAnchorElement:
		if a.Selector == "" || a.Label == "" {
			return invalidAnchor("of type element needs a selector and a label")
		}
	case ArtifactAnchorPoint:
		if a.Point == nil {
			return invalidAnchor("of type point needs a point")
		}
	case ArtifactAnchorLayout:
	default:
		return invalidAnchor("type %q is not supported", a.Type)
	}
	return nil
}

func (a *ArtifactAnchor) validateStyles() error {
	if len(a.Styles) > maxAnchorStyles {
		return invalidAnchor("has more than %d styles", maxAnchorStyles)
	}
	for key, value := range a.Styles {
		if !anchorStyleKeys[key] {
			return invalidAnchor("style %q is not allowed", key)
		}
		if err := validateAnchorText("style "+key, value, maxAnchorStyleValue, false); err != nil {
			return err
		}
	}
	return nil
}

func validateAnchorText(name, value string, limit int, newlines bool) error {
	if len(value) > limit {
		return invalidAnchor("%s exceeds %d bytes", name, limit)
	}
	if !utf8.ValidString(value) {
		return invalidAnchor("%s is not valid UTF-8", name)
	}
	for _, r := range value {
		if unicode.IsControl(r) && !(newlines && r == '\n') {
			return invalidAnchor("%s contains a control character", name)
		}
	}
	return nil
}

func anchorFractions(values ...float64) bool {
	for _, v := range values {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1 {
			return false
		}
	}
	return true
}

func (a *ArtifactAnchor) Clone() *ArtifactAnchor {
	if a == nil {
		return nil
	}
	out := *a
	if a.Styles != nil {
		out.Styles = make(map[string]string, len(a.Styles))
		for key, value := range a.Styles {
			out.Styles[key] = value
		}
	}
	if a.Rect != nil {
		rect := *a.Rect
		out.Rect = &rect
	}
	if a.Point != nil {
		point := *a.Point
		out.Point = &point
	}
	if a.AppState != nil {
		state := *a.AppState
		out.AppState = &state
	}
	return &out
}
