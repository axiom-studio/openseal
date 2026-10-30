package presentation

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"strings"
)

// Binding ties a card to a living entity whose state the host overlays.
type Binding struct {
	Kind BindingKind `json:"kind"`
	ID   string      `json:"id"`
}

type BindingKind string

const (
	BindingRunbook          BindingKind = "runbook"
	BindingConversationTask BindingKind = "conversation_task"
	BindingRun              BindingKind = "run"
	BindingApproval         BindingKind = "approval"
	BindingAgent            BindingKind = "agent"
)

// BindingKinds lists every binding kind.
var BindingKinds = []BindingKind{BindingRunbook, BindingConversationTask, BindingRun, BindingApproval, BindingAgent}

// EntityCommands lists the commands an entity action may send per binding
// kind. Approval decisions use approval actions, not entity commands.
var EntityCommands = map[BindingKind][]string{
	BindingRunbook:          {"pause", "resume", "run_now", "stop"},
	BindingConversationTask: {"cancel"},
	BindingAgent:            {"open_chat"},
}

// BindingResolver is implemented by the host. ResolveBinding returns nil when
// the entity exists, is in the publishing conversation's workspace, and is
// visible from that conversation; otherwise an error the agent can act on,
// e.g. "conversation_task/X is not part of this conversation".
//
// An approval binding (on a draft, or on approval actions) must name an
// approval that exists, belongs to the publishing conversation, and is for
// an action proposed by the publishing agent; otherwise the resolver
// rejects it, e.g. "approval/X was not proposed by you in this chat". A
// draft accepts only approval bindings; Validate rejects other kinds before
// calling the resolver.
//
// A routine must be bound to a runbook activation (a routine) in the
// publishing conversation's workspace that is visible from that
// conversation; an agent card to an agent of the workspace the viewer can
// chat with; and every row of a tasks card to a conversation_task of the
// publishing conversation (one call per row). Validate rejects a missing or
// wrong binding kind on these types before calling the resolver.
type BindingResolver interface {
	ResolveBinding(ctx context.Context, binding Binding) error
}

// BindingResolverFunc adapts a function to BindingResolver.
type BindingResolverFunc func(ctx context.Context, binding Binding) error

func (f BindingResolverFunc) ResolveBinding(ctx context.Context, binding Binding) error {
	return f(ctx, binding)
}

func (v *validator) binding(binding Binding) error {
	if !validBindingKind(binding.Kind) {
		return fmt.Errorf("kind %q is not one of runbook, conversation_task, run, approval, agent", binding.Kind)
	}
	if !opaqueID.MatchString(binding.ID) {
		return fmt.Errorf("id %q must match %s", binding.ID, opaqueIDPattern)
	}
	if v.opts.SkipHostChecks {
		return nil
	}
	if v.opts.Bindings == nil {
		return errors.New("bindings cannot be checked here; remove the binding")
	}
	if err := v.opts.Bindings.ResolveBinding(v.ctx, binding); err != nil {
		return fmt.Errorf("%s/%s: %w", binding.Kind, binding.ID, err)
	}
	return nil
}

// bindingNoun names what a binding kind points at, in the words people use.
func bindingNoun(kind BindingKind) string {
	switch kind {
	case BindingRunbook:
		return "routine"
	case BindingConversationTask:
		return "task"
	default:
		return string(kind)
	}
}

func containsBindingKind(kinds []BindingKind, kind BindingKind) bool {
	for _, candidate := range kinds {
		if kind == candidate {
			return true
		}
	}
	return false
}

func joinBindingKinds(kinds []BindingKind) string {
	names := make([]string, len(kinds))
	for index, kind := range kinds {
		names[index] = string(kind)
	}
	return strings.Join(names, ", ")
}

func validBindingKind(kind BindingKind) bool {
	for _, candidate := range BindingKinds {
		if kind == candidate {
			return true
		}
	}
	return false
}

// LinkProvenance is supplied by the host. It reports whether this exact URL
// appeared in a tool result or browser session of the publishing
// conversation. Only provenance-checked links are ever published.
type LinkProvenance func(ctx context.Context, rawURL string) (bool, error)

// MaxLinkBytes bounds a link URL.
const MaxLinkBytes = 2048

// ValidateLink checks a link field value: an absolute https URL without
// credentials that the host's LinkProvenance has seen in this conversation.
// Link fields exist only on item and place components.
func ValidateLink(ctx context.Context, rawURL string, opts ValidateOptions) error {
	if rawURL == "" || len(rawURL) > MaxLinkBytes || strings.TrimSpace(rawURL) != rawURL {
		return fmt.Errorf("link must be a URL of 1-%d bytes without surrounding spaces", MaxLinkBytes)
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.Opaque != "" {
		return fmt.Errorf("link %q must be an absolute https URL", rawURL)
	}
	if parsed.User != nil {
		return fmt.Errorf("link %q must not contain credentials", rawURL)
	}
	if opts.SkipHostChecks {
		return nil
	}
	if opts.LinkProvenance == nil {
		return fmt.Errorf("link %q cannot be verified by this host; leave it out", rawURL)
	}
	seen, err := opts.LinkProvenance(ctx, rawURL)
	if err != nil {
		return fmt.Errorf("check link %q: %w", rawURL, err)
	}
	if !seen {
		return fmt.Errorf("link %q did not appear in a tool result or browser page in this conversation; only use links you found", rawURL)
	}
	return nil
}

// PlaceFix is what find_place returned for a place id in this
// conversation: the place's name, coordinates and, when known, its address
// and category in plain words.
type PlaceFix struct {
	Name     string  `json:"name"`
	Lat      float64 `json:"lat"`
	Lon      float64 `json:"lon"`
	Address  string  `json:"address,omitempty"`
	Category string  `json:"category,omitempty"`
}

// PlaceProvenance is supplied by the host. It returns what find_place
// returned for placeID in the publishing conversation, and false when
// find_place never returned that id there. Visuals refer to places only by
// these ids; the host stamps coordinates in with StampPlaces.
type PlaceProvenance func(ctx context.Context, placeID string) (PlaceFix, bool, error)

// PlaceIDPattern matches a place id from find_place: "place:" and 12
// lowercase hex characters the host derives from its geocoder's own id, so
// the same place always gets the same id.
const PlaceIDPattern = `^place:[0-9a-f]{12}$`

var placeIDRegexp = regexp.MustCompile(PlaceIDPattern)

// placeRef checks a place id field (a map pin's place, a location) and,
// with host checks, that find_place returned it in this conversation.
func (v *validator) placeRef(field, id string) error {
	if !placeIDRegexp.MatchString(id) {
		return fmt.Errorf("%s %q must be a place id returned by find_place, such as place:4b2f9c0e1a77", field, id)
	}
	if v.opts.SkipHostChecks {
		return nil
	}
	if v.opts.PlaceProvenance == nil {
		return fmt.Errorf("%s %s cannot be checked here; leave places out", field, id)
	}
	fix, found, err := v.opts.PlaceProvenance(v.ctx, id)
	if err != nil {
		return fmt.Errorf("check %s %s: %w", field, id, err)
	}
	if !found {
		return fmt.Errorf("%s %s was not found by find_place in this chat; look the place up with find_place and use the id it returns", field, id)
	}
	if err := validPlaceFix(fix); err != nil {
		return fmt.Errorf("%s %s: %w", field, id, err)
	}
	return nil
}

// stampedPoint checks the output-only lat and lon at field (".lat" and
// ".lon" are appended). The host fills them in; a visual from the model
// must leave them out, and stored ones must be a pair on a place id.
func (v *validator) stampedPoint(field, placeID string, lat, lon *float64) error {
	if lat == nil && lon == nil {
		return nil
	}
	if !v.opts.SkipHostChecks {
		return fmt.Errorf("%s.lat and .lon are filled in from find_place; leave them out and never type coordinates", field)
	}
	if lat == nil || lon == nil || placeID == "" {
		return fmt.Errorf("%s.lat and .lon come as a pair with a place id", field)
	}
	return validCoordinates(field, *lat, *lon)
}

// stampedText checks an output-only text field the host fills in.
func (v *validator) stampedText(field, value string, maximum int) error {
	if value == "" {
		return nil
	}
	if !v.opts.SkipHostChecks {
		return fmt.Errorf("%s is filled in from find_place; leave it out", field)
	}
	return optional(field, value, maximum)
}

func validCoordinates(field string, lat, lon float64) error {
	if math.IsNaN(lat) || lat < -90 || lat > 90 || math.IsNaN(lon) || lon < -180 || lon > 180 {
		return fmt.Errorf("%s coordinates %v, %v are out of range", field, lat, lon)
	}
	return nil
}

func validPlaceFix(fix PlaceFix) error {
	if strings.TrimSpace(fix.Name) == "" {
		return errors.New("the place has no name")
	}
	return validCoordinates("the place's", fix.Lat, fix.Lon)
}
