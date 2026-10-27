// Package presentation defines the portable, declarative contract used when an
// Agent presents rich content in a conversation. The contract deliberately has
// no HTML, JavaScript, iframe, colour, or remote-code escape hatch; hosts render
// the same immutable Artifact bytes on every supported surface.
//
// A Surface is a tree of typed components drawn from a closed catalogue. Every
// component shares one envelope (id, type, summary, title, tone, binding,
// children) and carries at most one payload whose JSON key equals its type.
package presentation

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

const (
	SkillID      = "openseal.presentation"
	SkillVersion = "4.0.0"
	// UpdateVisual replaces a visual already shown in the chat with a newer
	// version. Visuals that answer a turn ride on the answer instead.
	UpdateVisual = "update_visual"
	MediaType    = "application/vnd.openseal.surface+json"
	ArtifactType = "interactive_surface"
	Schema       = "openseal.dev/presentation/v2"
)

// Surface-wide limits.
const (
	MaxComponents   = 64        // every component, including nested ones
	MaxDepth        = 3         // top-level components are depth 1
	MaxSurfaceBytes = 64 * 1024 // encoded JSON
	MaxSummaryRunes = 280
	MaxTitleRunes   = 300
)

type State string

const (
	StateStreaming State = "streaming"
	StateReady     State = "ready"
)

type Surface struct {
	Schema     string      `json:"schema"`
	Title      string      `json:"title"`
	State      State       `json:"state"`
	Components []Component `json:"components"`
}

// Component is a closed tagged union. Only the payload belonging to Type may
// be populated; Validate is the exact semantic gate.
type Component struct {
	ID       string      `json:"id"`
	Type     string      `json:"type"`
	Summary  string      `json:"summary,omitempty"`
	Title    string      `json:"title,omitempty"`
	Tone     Tone        `json:"tone,omitempty"`
	Binding  *Binding    `json:"binding,omitempty"`
	Children []Component `json:"children,omitempty"`

	// Text is the payload of text, caption and (optionally) divider.
	Text       string      `json:"text,omitempty"`
	Chart      *Chart      `json:"chart,omitempty"`
	Diagram    *Diagram    `json:"diagram,omitempty"`
	Form       *Form       `json:"form,omitempty"`
	Table      *Table      `json:"table,omitempty"`
	StatRow    *StatRow    `json:"stat_row,omitempty"`
	Grid       *Grid       `json:"grid,omitempty"`
	Tabs       []Tab       `json:"tabs,omitempty"`
	Actions    []Action    `json:"actions,omitempty"`
	Callout    *Callout    `json:"callout,omitempty"`
	Steps      *Steps      `json:"steps,omitempty"`
	Comparison *Comparison `json:"comparison,omitempty"`
	Timeline   *Timeline   `json:"timeline,omitempty"`
	Image      *ImageBlock `json:"image,omitempty"`
	Gallery    *Gallery    `json:"gallery,omitempty"`
	Choices    *Choices    `json:"choices,omitempty"`
	Checklist  *Checklist  `json:"checklist,omitempty"`
	Quiz       *Quiz       `json:"quiz,omitempty"`

	LabeledImage   *LabeledImage   `json:"labeled_image,omitempty"`
	QuantityVisual *QuantityVisual `json:"quantity_visual,omitempty"`
	Math           *Math           `json:"math,omitempty"`
	Definition     *Definition     `json:"definition,omitempty"`
	Flashcards     *Flashcards     `json:"flashcards,omitempty"`
	BeforeAfter    *BeforeAfter    `json:"before_after,omitempty"`
	Draft          *Draft          `json:"draft,omitempty"`
	Recipe         *Recipe         `json:"recipe,omitempty"`
	Item           *Item           `json:"item,omitempty"`
	Analogy        *Analogy        `json:"analogy,omitempty"`
}

type Tone string

const (
	ToneNeutral  Tone = "neutral"
	ToneAccent   Tone = "accent"
	TonePositive Tone = "positive"
	ToneNegative Tone = "negative"
	ToneWarning  Tone = "warning"
	ToneInfo     Tone = "info"
)

// Tones lists every semantic tone. Hosts map tones to design tokens.
var Tones = []Tone{ToneNeutral, ToneAccent, TonePositive, ToneNegative, ToneWarning, ToneInfo}

var opaqueID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

const opaqueIDPattern = `^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`

// Decode parses stored surface bytes and validates their structure. Host
// checks (binding scope, link provenance) belong to publish time and are
// skipped here.
func Decode(raw []byte) (*Surface, error) {
	if len(raw) == 0 || len(raw) > MaxSurfaceBytes {
		return nil, fmt.Errorf("presentation surface must be between 1 byte and %d bytes", MaxSurfaceBytes)
	}
	var surface Surface
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&surface); err != nil {
		return nil, fmt.Errorf("decode presentation surface: %w", err)
	}
	if _, err := Validate(context.Background(), surface, ValidateOptions{SkipHostChecks: true}); err != nil {
		return nil, err
	}
	return &surface, nil
}

// Encode validates the structure of a surface and returns its canonical bytes.
// Publishing hosts call Validate with their hooks first.
func Encode(surface Surface) ([]byte, error) {
	if _, err := Validate(context.Background(), surface, ValidateOptions{SkipHostChecks: true}); err != nil {
		return nil, err
	}
	return json.Marshal(surface)
}

// Find returns the component with id anywhere in the surface tree.
func (s Surface) Find(id string) (*Component, bool) {
	var found *Component
	walk(s.Components, func(component *Component, _ string, _ int) bool {
		if component.ID == id {
			found = component
			return false
		}
		return true
	})
	return found, found != nil
}

// walk visits every component depth first, including tab children. visit
// returns false to stop.
func walk(components []Component, visit func(component *Component, path string, depth int) bool) {
	var recurse func(list []Component, prefix string, depth int) bool
	recurse = func(list []Component, prefix string, depth int) bool {
		for index := range list {
			component := &list[index]
			path := fmt.Sprintf("%s[%d]", prefix, index)
			if !visit(component, path, depth) {
				return false
			}
			if !recurse(component.Children, path+".children", depth+1) {
				return false
			}
			for tab := range component.Tabs {
				if !recurse(component.Tabs[tab].Children, fmt.Sprintf("%s.tabs[%d].children", path, tab), depth+1) {
					return false
				}
			}
		}
		return true
	}
	recurse(components, "components", 1)
}

// ValidationError names the component and JSON path that failed.
type ValidationError struct {
	Path        string
	ComponentID string
	Message     string
}

func (e *ValidationError) Error() string {
	if e.ComponentID != "" {
		return fmt.Sprintf("%s (component %q): %s", e.Path, e.ComponentID, e.Message)
	}
	if e.Path != "" {
		return e.Path + ": " + e.Message
	}
	return e.Message
}

// Warning is a non-blocking finding from Validate or Lint.
type Warning struct {
	Code        string `json:"code"`
	Path        string `json:"path"`
	ComponentID string `json:"componentId,omitempty"`
	Message     string `json:"message"`
}

// ValidateOptions carries host-supplied checks. A zero value fails closed:
// bindings, links and images are rejected unless their hook is supplied or
// SkipHostChecks is set.
type ValidateOptions struct {
	// Bindings confirms that a bound entity exists and is in scope.
	Bindings BindingResolver
	// LinkProvenance confirms that a link URL appeared in this conversation.
	LinkProvenance LinkProvenance
	// Images confirms that an image reference names an image visible from
	// this conversation.
	Images ImageResolver
	// SkipHostChecks skips binding, link provenance and image checks. Use it only
	// for stored content that already passed publish validation and for
	// pre-validation whose host checks run later.
	SkipHostChecks bool
}

type validator struct {
	ctx      context.Context
	opts     ValidateOptions
	warnings []Warning
	surface  Surface
	ids      map[string]*Component
}

func (v *validator) warn(code, path, id, format string, args ...interface{}) {
	v.warnings = append(v.warnings, Warning{Code: code, Path: path, ComponentID: id, Message: fmt.Sprintf(format, args...)})
}

// Validate checks a surface against the v2 contract. It returns non-blocking
// warnings (such as unknown icons) and the first blocking error.
func Validate(ctx context.Context, surface Surface, opts ValidateOptions) ([]Warning, error) {
	fail := func(path, format string, args ...interface{}) ([]Warning, error) {
		return nil, &ValidationError{Path: path, Message: fmt.Sprintf(format, args...)}
	}
	if surface.Schema != Schema {
		return fail("schema", "must be %q", Schema)
	}
	if err := boundedText(surface.Title, 1, MaxTitleRunes); err != nil {
		return fail("title", "%v", err)
	}
	if surface.State != StateStreaming && surface.State != StateReady {
		return fail("state", "must be streaming or ready")
	}
	if len(surface.Components) == 0 {
		return fail("components", "at least one component is required")
	}
	encoded, err := json.Marshal(surface)
	if err != nil {
		return fail("", "encode visual: %v", err)
	}
	if len(encoded) > MaxSurfaceBytes {
		return fail("", "the visual is %d bytes of JSON; the limit is %d. Use fewer or smaller components", len(encoded), MaxSurfaceBytes)
	}
	v := &validator{ctx: ctx, opts: opts, surface: surface, ids: map[string]*Component{}}
	count := 0
	var failure error
	walk(surface.Components, func(component *Component, path string, depth int) bool {
		count++
		switch {
		case count > MaxComponents:
			failure = &ValidationError{Path: path, ComponentID: component.ID, Message: fmt.Sprintf("a visual can hold at most %d components including nested ones", MaxComponents)}
		case depth > MaxDepth:
			failure = &ValidationError{Path: path, ComponentID: component.ID, Message: fmt.Sprintf("components can nest at most %d levels deep", MaxDepth)}
		case !opaqueID.MatchString(component.ID):
			failure = &ValidationError{Path: path + ".id", ComponentID: component.ID, Message: "id must match " + opaqueIDPattern}
		case v.ids[component.ID] != nil:
			failure = &ValidationError{Path: path + ".id", ComponentID: component.ID, Message: "id is already used by another component"}
		}
		if failure != nil {
			return false
		}
		v.ids[component.ID] = component
		return true
	})
	if failure != nil {
		return nil, failure
	}
	walk(surface.Components, func(component *Component, path string, _ int) bool {
		if err := v.component(*component, path); err != nil {
			failure = &ValidationError{Path: path, ComponentID: component.ID, Message: err.Error()}
			return false
		}
		return true
	})
	if failure != nil {
		return nil, failure
	}
	return v.warnings, nil
}

func (v *validator) component(c Component, path string) error {
	spec, ok := catalogue[c.Type]
	if !ok {
		return fmt.Errorf("unsupported type %q; use one of %s", c.Type, strings.Join(TypeNames(), ", "))
	}
	if spec.summary == summaryRequired && strings.TrimSpace(c.Summary) == "" {
		return fmt.Errorf("summary is required for %s: one plain sentence saying what it shows", c.Type)
	}
	if err := boundedText(c.Summary, 0, MaxSummaryRunes); err != nil {
		return fmt.Errorf("summary %v", err)
	}
	if err := boundedText(c.Title, 0, MaxTitleRunes); err != nil {
		return fmt.Errorf("title %v", err)
	}
	if c.Tone != "" && !validTone(c.Tone) {
		return fmt.Errorf("tone %q is not one of neutral, accent, positive, negative, warning, info", c.Tone)
	}
	if c.Binding != nil {
		if spec.bindings != nil && !containsBindingKind(spec.bindings, c.Binding.Kind) {
			return fmt.Errorf("binding.kind %q is not allowed on %s; use %s", c.Binding.Kind, c.Type, joinBindingKinds(spec.bindings))
		}
		if err := v.binding(*c.Binding); err != nil {
			return fmt.Errorf("binding: %w", err)
		}
	}
	if len(c.Children) > 0 && !spec.children {
		return fmt.Errorf("%s cannot have children; only section, grid and carousel can", c.Type)
	}
	if err := exactPayload(c); err != nil {
		return err
	}
	return spec.validate(v, c, path)
}

// exactPayload enforces that exactly the payload matching Type is present.
func exactPayload(c Component) error {
	present := map[string]bool{
		"text": c.Text != "", "chart": c.Chart != nil, "diagram": c.Diagram != nil, "form": c.Form != nil,
		"table": c.Table != nil, "stat_row": c.StatRow != nil, "grid": c.Grid != nil, "tabs": len(c.Tabs) > 0, "actions": len(c.Actions) > 0,
		"callout": c.Callout != nil, "steps": c.Steps != nil, "comparison": c.Comparison != nil, "timeline": c.Timeline != nil,
		"image": c.Image != nil, "gallery": c.Gallery != nil, "choices": c.Choices != nil, "checklist": c.Checklist != nil, "quiz": c.Quiz != nil,
		"labeled_image": c.LabeledImage != nil, "quantity_visual": c.QuantityVisual != nil, "math": c.Math != nil, "definition": c.Definition != nil,
		"flashcards": c.Flashcards != nil, "before_after": c.BeforeAfter != nil, "draft": c.Draft != nil, "recipe": c.Recipe != nil,
		"item": c.Item != nil, "analogy": c.Analogy != nil,
	}
	allowed := catalogue[c.Type].payload
	for key, set := range present {
		if set && key != allowed {
			return fmt.Errorf("%s cannot carry a %q payload", c.Type, key)
		}
	}
	return nil
}

func validTone(tone Tone) bool {
	for _, candidate := range Tones {
		if tone == candidate {
			return true
		}
	}
	return false
}

func boundedText(value string, minimum, maximum int) error {
	length := len([]rune(strings.TrimSpace(value)))
	if length < minimum || length > maximum {
		if minimum == 0 {
			return fmt.Errorf("must be at most %d characters (got %d)", maximum, length)
		}
		return fmt.Errorf("must contain between %d and %d characters (got %d)", minimum, maximum, length)
	}
	return nil
}

func required(field, value string, maximum int) error {
	if err := boundedText(value, 1, maximum); err != nil {
		return fmt.Errorf("%s %w", field, err)
	}
	return nil
}

func optional(field, value string, maximum int) error {
	if err := boundedText(value, 0, maximum); err != nil {
		return fmt.Errorf("%s %w", field, err)
	}
	return nil
}
