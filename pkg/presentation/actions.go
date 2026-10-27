package presentation

import (
	"fmt"
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
