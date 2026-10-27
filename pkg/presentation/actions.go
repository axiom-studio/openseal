package presentation

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
)

// Action is one button in an actions component.
type Action struct {
	ID    string     `json:"id"`
	Label string     `json:"label"`
	Kind  ActionKind `json:"kind"`
	Icon  string     `json:"icon,omitempty"`
	// Value is the reply a respond action sends; it defaults to Label.
	Value string `json:"value,omitempty"`
	// Target is the id of the draft component an edit action opens.
	Target string `json:"target,omitempty"`
	// Decision is approve or reject for an approval action.
	Decision string `json:"decision,omitempty"`
	// Command is the entity command an entity action sends (EntityCommands).
	Command string `json:"command,omitempty"`
	// Binding overrides the component binding for approval and entity actions.
	Binding *Binding `json:"binding,omitempty"`
}

type ActionKind string

const (
	ActionRespond  ActionKind = "respond"
	ActionEdit     ActionKind = "edit"
	ActionApproval ActionKind = "approval"
	ActionEntity   ActionKind = "entity"
)

// ActionKinds lists every action kind.
var ActionKinds = []ActionKind{ActionRespond, ActionEdit, ActionApproval, ActionEntity}

// EditableTypes are the component types an edit action can target.
var EditableTypes = []string{"draft"}

const MaxActions = 4

func validateActions(v *validator, c Component, _ string) error {
	if len(c.Actions) == 0 || len(c.Actions) > MaxActions {
		return fmt.Errorf("actions needs 1-%d actions (got %d)", MaxActions, len(c.Actions))
	}
	ids := map[string]bool{}
	for index, action := range c.Actions {
		at := fmt.Sprintf("actions[%d]", index)
		if !opaqueID.MatchString(action.ID) || ids[action.ID] {
			return fmt.Errorf("%s.id %q must be a unique id matching %s", at, action.ID, opaqueIDPattern)
		}
		ids[action.ID] = true
		if err := required(at+".label", action.Label, 40); err != nil {
			return err
		}
		if err := v.icon(at+".icon", c.ID, action.Icon); err != nil {
			return err
		}
		if err := v.action(c, action, at); err != nil {
			return err
		}
	}
	return nil
}

func (v *validator) action(c Component, action Action, at string) error {
	only := func(field string, set bool) error {
		if set {
			return fmt.Errorf("%s.%s is not used by %s actions", at, field, action.Kind)
		}
		return nil
	}
	binding := action.Binding
	if binding == nil {
		binding = c.Binding
	}
	switch action.Kind {
	case ActionRespond:
		if err := optional(at+".value", action.Value, 500); err != nil {
			return err
		}
		return firstError(only("target", action.Target != ""), only("decision", action.Decision != ""), only("command", action.Command != ""), only("binding", action.Binding != nil))
	case ActionEdit:
		if err := firstError(only("value", action.Value != ""), only("decision", action.Decision != ""), only("command", action.Command != ""), only("binding", action.Binding != nil)); err != nil {
			return err
		}
		target := v.ids[action.Target]
		if target == nil || !contains(EditableTypes, target.Type) {
			return fmt.Errorf("%s.target must be the id of a draft component in this visual", at)
		}
		return nil
	case ActionApproval:
		if err := firstError(only("value", action.Value != ""), only("target", action.Target != ""), only("command", action.Command != "")); err != nil {
			return err
		}
		if binding == nil || binding.Kind != BindingApproval {
			return fmt.Errorf("%s is an approval action and needs an approval binding (on the action or the component)", at)
		}
		if action.Decision != "approve" && action.Decision != "reject" {
			return fmt.Errorf("%s.decision must be approve or reject", at)
		}
	case ActionEntity:
		if err := firstError(only("value", action.Value != ""), only("target", action.Target != ""), only("decision", action.Decision != "")); err != nil {
			return err
		}
		if binding == nil {
			return fmt.Errorf("%s is an entity action and needs a binding (on the action or the component)", at)
		}
		commands := EntityCommands[binding.Kind]
		if !contains(commands, action.Command) {
			if len(commands) == 0 {
				return fmt.Errorf("%s: a %s binding has no entity commands", at, binding.Kind)
			}
			return fmt.Errorf("%s.command %q is not one of %s for a %s binding", at, action.Command, strings.Join(commands, ", "), binding.Kind)
		}
	default:
		return fmt.Errorf("%s.kind %q is not one of respond, edit, approval, entity", at, action.Kind)
	}
	if action.Binding != nil {
		if err := v.binding(*action.Binding); err != nil {
			return fmt.Errorf("%s.binding: %w", at, err)
		}
	}
	return nil
}

func firstError(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// InteractionKind names a stored surface interaction.
type InteractionKind string

const (
	InteractionSubmit  InteractionKind = "submit"  // form submit
	InteractionRespond InteractionKind = "respond" // actions respond button
)

// InteractionRule describes how the host stores one interaction.
type InteractionRule struct {
	Kind InteractionKind
	// SingleUse interactions are unique per (surface, version, component,
	// actor); others are keyed by idempotency key.
	SingleUse bool
	// NotifyAgent interactions post an answer that wakes the agent.
	NotifyAgent bool
}

// Interaction reports which stored interaction a component accepts. actionID
// selects the button of an actions component and must be empty otherwise.
// Approval and entity buttons use their existing endpoints and edit buttons
// are client-only, so none of them is a surface interaction.
func Interaction(component Component, actionID string) (InteractionRule, error) {
	switch component.Type {
	case "form":
		if actionID != "" {
			return InteractionRule{}, errors.New("form submissions do not take an action id")
		}
		return InteractionRule{Kind: InteractionSubmit, SingleUse: true, NotifyAgent: true}, nil
	case "actions":
		for _, action := range component.Actions {
			if action.ID != actionID {
				continue
			}
			if action.Kind != ActionRespond {
				return InteractionRule{}, fmt.Errorf("action %q is a %s action and is not recorded as a card interaction", actionID, action.Kind)
			}
			return InteractionRule{Kind: InteractionRespond, SingleUse: true, NotifyAgent: true}, nil
		}
		return InteractionRule{}, fmt.Errorf("component %q has no action %q", component.ID, actionID)
	default:
		return InteractionRule{}, fmt.Errorf("component %q (%s) does not accept interactions", component.ID, component.Type)
	}
}

var isoDate = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// ValidateInteractionValues checks the values of an interaction against the
// component. Form values are keyed by field id: strings for text, textarea,
// date (YYYY-MM-DD), select and radio (an option value); numbers for number;
// booleans for checkbox. Respond interactions carry no values.
func ValidateInteractionValues(component Component, actionID string, values map[string]interface{}) error {
	rule, err := Interaction(component, actionID)
	if err != nil {
		return err
	}
	if rule.Kind == InteractionRespond {
		if len(values) > 0 {
			return errors.New("respond interactions carry no values")
		}
		return nil
	}
	fields := map[string]FormField{}
	for _, field := range component.Form.Fields {
		fields[field.ID] = field
	}
	for id := range values {
		if _, ok := fields[id]; !ok {
			return fmt.Errorf("form has no field %q", id)
		}
	}
	for _, field := range component.Form.Fields {
		value, present := values[field.ID]
		if !present || value == nil {
			if field.Required {
				return fmt.Errorf("field %q is required", field.ID)
			}
			continue
		}
		if err := validateFieldValue(field, value); err != nil {
			return fmt.Errorf("field %q: %w", field.ID, err)
		}
	}
	return nil
}

func validateFieldValue(field FormField, value interface{}) error {
	switch field.Type {
	case "checkbox":
		checked, ok := value.(bool)
		if !ok {
			return errors.New("must be true or false")
		}
		if field.Required && !checked {
			return errors.New("must be checked")
		}
	case "number":
		number, ok := value.(float64)
		if !ok || math.IsNaN(number) || math.IsInf(number, 0) {
			return errors.New("must be a number")
		}
		if field.Min != nil && number < *field.Min || field.Max != nil && number > *field.Max {
			return errors.New("is out of range")
		}
	default:
		text, ok := value.(string)
		if !ok {
			return errors.New("must be a string")
		}
		if field.Required && strings.TrimSpace(text) == "" {
			return errors.New("is required")
		}
		limit := 2000
		if field.Type == "textarea" {
			limit = 20000
		}
		if len([]rune(text)) > limit {
			return fmt.Errorf("must be at most %d characters", limit)
		}
		switch field.Type {
		case "date":
			if text != "" && !isoDate.MatchString(text) {
				return errors.New("must be a date as YYYY-MM-DD")
			}
		case "select", "radio":
			if text == "" {
				return nil
			}
			for _, option := range field.Options {
				if option.Value == text {
					return nil
				}
			}
			return fmt.Errorf("%q is not one of the field's options", text)
		}
	}
	return nil
}
