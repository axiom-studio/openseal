package models

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/axiom-studio/openseal/pkg/validation"
)

// RoutineGrant is one action a routine's runs may perform without asking.
// It exists only because the user approved a routine brief listing it: the
// grant is copied from the approved create_task arguments (bound by the
// approval's digest) and never added later.
type RoutineGrant struct {
	// Action is the model-visible action identity: skillId + "." + action, or
	// a binding tool's model action name <modelPrefix>.<bindingSlug>.<tool>.
	Action string `json:"action"`
	// Label is the plain-language line the user approved.
	Label string `json:"label"`
	// ToOwnerOnly limits a message-sending grant to sends whose recipients
	// are all the routine owner's own addresses.
	ToOwnerOnly bool `json:"toOwnerOnly,omitempty"`
	// ToDestinations limits a grant of an action that posts to a channel or
	// conversation (for example a Slack channel) to those destinations: the
	// exact channel values its runs pass, normalized without a leading '#'.
	// A posting action is granted only with destinations, and a call is
	// covered only when every destination argument it names is one of them.
	ToDestinations []string `json:"toDestinations,omitempty"`
	// ToolArguments pins a connected tool grant to the exact tool the user
	// approved: the binding tool's fixed arguments (for an MCP tool, its
	// upstream name and pinned hash), copied from the catalog when the brief
	// is resolved. A tool re-pinned or replaced under the same name no longer
	// matches.
	ToolArguments map[string]interface{} `json:"toolArguments,omitempty"`
}

const (
	MaximumRoutineGrants     = 12
	MaximumRoutineGrantLabel = 120
	// MaximumRoutineGrantToolArguments bounds a connected tool pin.
	MaximumRoutineGrantToolArguments      = 8
	maximumRoutineGrantToolArgumentsBytes = 2048
	// MaximumRoutineGrantDestinations bounds a destination pin.
	MaximumRoutineGrantDestinations     = 8
	MaximumRoutineGrantDestinationBytes = 256
)

var routineGrantActionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,254}\.[A-Za-z0-9_-][A-Za-z0-9_.:-]{0,127}$`)

// RoutineBriefText collapses whitespace and bounds plain approval prose.
func RoutineBriefText(value string, limit int) (string, error) {
	text := strings.Join(strings.Fields(value), " ")
	if utf8.RuneCountInString(text) > limit || strings.Contains(strings.ToUpper(text), "[REDACTED]") || strings.ContainsAny(text, "<>`") {
		return "", fmt.Errorf("routine brief text must be plain prose of at most %d characters", limit)
	}
	if text != "" {
		if err := validation.ValidateNoSensitiveInput(text); err != nil {
			return "", err
		}
	}
	return text, nil
}

func NormalizeRoutineGrants(values []RoutineGrant, allowEmptyLabels bool) ([]RoutineGrant, error) {
	if len(values) > MaximumRoutineGrants {
		return nil, fmt.Errorf("a routine brief lists at most %d actions", MaximumRoutineGrants)
	}
	grants := make([]RoutineGrant, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		action := strings.TrimSpace(value.Action)
		if !routineGrantActionPattern.MatchString(action) {
			return nil, fmt.Errorf("routine grant action %q must be an exact action name: skillId.action, or a connected tool's own action name", action)
		}
		if seen[action] {
			return nil, fmt.Errorf("routine grant action %s is listed twice", action)
		}
		seen[action] = true
		label, err := RoutineBriefText(value.Label, MaximumRoutineGrantLabel)
		if err != nil {
			return nil, err
		}
		if label == "" && !allowEmptyLabels {
			return nil, errors.New("routine grant label is required")
		}
		if len(value.ToolArguments) > MaximumRoutineGrantToolArguments {
			return nil, fmt.Errorf("routine grant %s pins at most %d tool arguments", action, MaximumRoutineGrantToolArguments)
		}
		if value.ToolArguments != nil {
			if encoded, err := json.Marshal(value.ToolArguments); err != nil || len(encoded) > maximumRoutineGrantToolArgumentsBytes {
				return nil, fmt.Errorf("routine grant %s tool pin must encode to at most %d bytes", action, maximumRoutineGrantToolArgumentsBytes)
			}
		}
		destinations, err := normalizeRoutineGrantDestinations(action, value.ToDestinations)
		if err != nil {
			return nil, err
		}
		if value.ToOwnerOnly && len(destinations) > 0 {
			return nil, fmt.Errorf("routine grant %s is limited either to the owner or to destinations, not both", action)
		}
		grants = append(grants, RoutineGrant{Action: action, Label: label, ToOwnerOnly: value.ToOwnerOnly, ToDestinations: destinations, ToolArguments: maps.Clone(value.ToolArguments)})
	}
	return grants, nil
}

// NormalizeRoutineGrantDestination trims a destination and drops one leading
// '#', so "#ai-news" and "ai-news" pin the same channel name.
func NormalizeRoutineGrantDestination(value string) string {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(value), "#"))
}

func normalizeRoutineGrantDestinations(action string, values []string) ([]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	if len(values) > MaximumRoutineGrantDestinations {
		return nil, fmt.Errorf("routine grant %s names at most %d destinations", action, MaximumRoutineGrantDestinations)
	}
	destinations := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		destination := NormalizeRoutineGrantDestination(value)
		if destination == "" || len(destination) > MaximumRoutineGrantDestinationBytes || strings.ContainsAny(destination, " \t\r\n<>`#") ||
			strings.ContainsFunc(destination, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			return nil, fmt.Errorf("routine grant %s destination %q must be a channel ID or name", action, value)
		}
		if !seen[destination] {
			seen[destination] = true
			destinations = append(destinations, destination)
		}
	}
	return destinations, nil
}

func CloneRoutineGrants(values []RoutineGrant) []RoutineGrant {
	if values == nil {
		return nil
	}
	grants := append([]RoutineGrant(nil), values...)
	for index := range grants {
		grants[index].ToolArguments = maps.Clone(grants[index].ToolArguments)
		grants[index].ToDestinations = append([]string(nil), grants[index].ToDestinations...)
		if len(grants[index].ToDestinations) == 0 {
			grants[index].ToDestinations = nil
		}
	}
	return grants
}
