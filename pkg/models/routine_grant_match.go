package models

import (
	"context"
	"encoding/json"
	"errors"
	"net/mail"
	"strings"

	"github.com/axiom-studio/openseal/pkg/skill"
)

// RoutineOwnerAddressResolver returns the verified email addresses of a
// routine's owner: the user who asked for it in the chat. Hosts that cannot
// prove a user's addresses leave it unset, and owner-only grants are then
// neither offered nor applied.
type RoutineOwnerAddressResolver interface {
	RoutineOwnerAddresses(ctx context.Context, scope Scope, owner ConversationParticipant) ([]string, error)
}

// computerSkillID is pkg/computer's SkillID, repeated because runtime does
// not import it.
const computerSkillID = "openseal.computer"

// Kernel management Skills govern the Agent's own authority, work and
// routines; a routine brief never waives their review. Commands and writes on
// a person's own computer are never granted standing authority either.
var routineUngrantableSkills = map[string]bool{
	SkillManagementSkillID: true, AgentManagementSkillID: true, TeamManagementSkillID: true,
	RunbookManagementSkillID: true, RunManagementSkillID: true, ObjectiveManagementSkillID: true,
	computerSkillID: true,
}

// RoutineUngrantableSkill reports whether a routine may never be granted
// actions of skillID: the management skills and the computer skill.
func RoutineUngrantableSkill(skillID string) bool {
	return routineUngrantableSkills[skillID]
}

func RoutineGrantIdentity(skillID, action string) string {
	return skillID + "." + action
}

// routineGrantToolPinned reports whether a grant's pin is exactly the fixed
// arguments of the tool being called.
func routineGrantToolPinned(grant RoutineGrant, tool *skill.BindingTool) bool {
	if tool == nil || len(grant.ToolArguments) == 0 || len(grant.ToolArguments) != len(tool.Arguments) {
		return false
	}
	pinned, err := json.Marshal(grant.ToolArguments)
	if err != nil {
		return false
	}
	current, err := json.Marshal(tool.Arguments)
	return err == nil && string(pinned) == string(current)
}

// IsRoutineRecipientKey reports whether an argument names message recipients.
func IsRoutineRecipientKey(name string) bool {
	key := strings.ToLower(strings.TrimSpace(name))
	switch key {
	case "to", "cc", "bcc", "recipient", "recipients", "replyto", "reply_to":
		return true
	}
	return strings.Contains(key, "recipient") || strings.Contains(key, "email") || strings.Contains(key, "address") ||
		strings.Contains(key, "attendee")
}

// routineDestinationKey reports whether an argument names the channel,
// conversation, chat, user, queue or webhook a message is posted to. A
// plain url argument is not one: an outward action that takes only a url
// cannot be pinned, so an event routine refuses it.
func routineDestinationKey(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "channel", "channel_id", "channelid", "channels", "conversation", "conversation_id", "conversationid", "destination", "destinations",
		"chat", "chat_id", "chatid", "userid", "user_id", "webhookurl", "webhook_url", "queueurl", "queue_url":
		return true
	}
	return false
}

// RoutineActionDestinationKeys returns the destination arguments of an
// action's input schema.
func RoutineActionDestinationKeys(schema map[string]interface{}) []string {
	properties, _ := schema["properties"].(map[string]interface{})
	keys := []string{}
	for name := range properties {
		if routineDestinationKey(name) {
			keys = append(keys, name)
		}
	}
	return keys
}

// RoutineActionPostsToDestination reports whether an action has side effects
// in a channel or conversation it takes as an argument.
func RoutineActionPostsToDestination(action skill.Action) bool {
	return action.SideEffect != skill.SideEffectRead && action.SideEffect != skill.SideEffectNone && len(RoutineActionDestinationKeys(action.InputSchema)) > 0
}

// routineDestinationsPinned reports whether arguments name at least one
// destination under keys, and every one of them is pinned. A value it cannot
// read as a destination fails closed.
func routineDestinationsPinned(arguments map[string]interface{}, keys []string, pinned []string) bool {
	allowed := make(map[string]bool, len(pinned))
	for _, destination := range pinned {
		if destination = NormalizeRoutineGrantDestination(destination); destination != "" {
			allowed[destination] = true
		}
	}
	count := 0
	check := func(value string) bool {
		destination := NormalizeRoutineGrantDestination(value)
		if destination == "" {
			return true
		}
		count++
		return allowed[destination]
	}
	for _, key := range keys {
		switch typed := arguments[key].(type) {
		case nil:
		case string:
			if !check(typed) {
				return false
			}
		case []interface{}:
			for _, nested := range typed {
				value, ok := nested.(string)
				if !ok || !check(value) {
					return false
				}
			}
		case []string:
			for _, value := range typed {
				if !check(value) {
					return false
				}
			}
		default:
			return false
		}
	}
	return count > 0
}

// routineRecipientsOwned reports whether every recipient in arguments, at any
// depth, is one of owned, and there is at least one. Anything it cannot read
// as an address under a recipient key fails closed.
func routineRecipientsOwned(arguments map[string]interface{}, owned []string) bool {
	allowed := make(map[string]bool, len(owned))
	for _, address := range owned {
		if address = strings.ToLower(strings.TrimSpace(address)); address != "" {
			allowed[address] = true
		}
	}
	if len(allowed) == 0 {
		return false
	}
	count := 0
	var visit func(value interface{}, recipient bool) bool
	visit = func(value interface{}, recipient bool) bool {
		switch typed := value.(type) {
		case nil:
			return true
		case map[string]interface{}:
			for key, nested := range typed {
				lower := strings.ToLower(key)
				if recipient && !IsRoutineRecipientKey(key) {
					if lower == "name" || lower == "displayname" || lower == "display_name" {
						continue
					}
					return false
				}
				if !visit(nested, recipient || IsRoutineRecipientKey(key)) {
					return false
				}
			}
			return true
		case []interface{}:
			for _, nested := range typed {
				if !visit(nested, recipient) {
					return false
				}
			}
			return true
		case []string:
			for _, nested := range typed {
				if !visit(nested, recipient) {
					return false
				}
			}
			return true
		case string:
			if !recipient || strings.TrimSpace(typed) == "" {
				return true
			}
			addresses, err := mail.ParseAddressList(typed)
			if err != nil {
				return false
			}
			for _, address := range addresses {
				if !allowed[strings.ToLower(address.Address)] {
					return false
				}
				count++
			}
			return true
		default:
			return !recipient
		}
	}
	return visit(arguments, false) && count > 0
}

type RoutineGrantStore interface {
	GetRunbookActivation(context.Context, Scope, string) (*RunbookActivation, error)
	GetChannelMessage(context.Context, Scope, string, string) (*ChannelMessage, error)
}

// routineActivationForRun returns the routine activation a Run executes for:
// a kernel-scheduled or started occurrence of a scheduled task, or a child of
// one assigned to the same Agent.
func routineActivationForRun(ctx context.Context, runs AgentRunReader, store RoutineGrantStore, run *AgentRun) (*RunbookActivation, error) {
	if run == nil || store == nil {
		return nil, nil
	}
	origin := run
	if origin.Context[ScheduledTaskContextKey] != true {
		rootID := strings.TrimSpace(run.RootRunID)
		if rootID == "" || rootID == run.ID || runs == nil {
			return nil, nil
		}
		root, err := runs.GetAgentRun(ctx, run.Scope, rootID)
		if errors.Is(err, ErrRunNotFound) || err == nil && root == nil {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		origin = root
	}
	activationID, _ := origin.Context["runbookActivationId"].(string)
	if origin.Context[ScheduledTaskContextKey] != true || strings.TrimSpace(activationID) == "" {
		return nil, nil
	}
	activation, err := store.GetRunbookActivation(ctx, run.Scope, strings.TrimSpace(activationID))
	if errors.Is(err, ErrRunbookActivationNotFound) {
		return nil, nil
	}
	if err != nil || activation == nil {
		return nil, err
	}
	if activation.Task == nil || activation.Scope != run.Scope || activation.Owner != run.Owner || origin.Owner != run.Owner ||
		activation.AssignedAgentID == "" || activation.AssignedAgentID != run.AssignedAgentID || origin.AssignedAgentID != run.AssignedAgentID ||
		origin.ObjectiveID != activation.ObjectiveID {
		return nil, nil
	}
	return activation, nil
}

// RoutineOwner is the person who created the routine: its task's CreatedBy.
func RoutineOwner(activation *RunbookActivation) (ConversationParticipant, error) {
	if activation == nil || activation.Task == nil || activation.Task.CreatedBy == "" {
		return ConversationParticipant{}, errors.New("routine owner is unavailable")
	}
	return ConversationParticipant{Type: ConversationParticipantUser, ID: activation.Task.CreatedBy}, nil
}

// ResolveRoutineGrant returns the approved routine grant that covers this
// action, or nil. A review: always action is never covered, and an owner-only
// grant covers a send only when every recipient is the owner's address. A
// call of a binding-tools base action is covered only by a grant naming the
// exact tool its fixed arguments select on the routine umbrella's binding,
// never by the base action, and only while that tool still has the fixed
// arguments (upstream name and pinned hash) the user approved.
func ResolveRoutineGrant(ctx context.Context, runs AgentRunReader, store RoutineGrantStore, owners RoutineOwnerAddressResolver, run *AgentRun, bound *skill.BoundAction, arguments map[string]interface{}) (*RoutineGrant, error) {
	if bound == nil || bound.Definition == nil || bound.Action.AlwaysReview() || RoutineUngrantableSkill(bound.Definition.ID) {
		return nil, nil
	}
	activation, err := routineActivationForRun(ctx, runs, store, run)
	if err != nil || activation == nil || len(activation.Task.Grants) == 0 {
		return nil, err
	}
	identity := RoutineGrantIdentity(bound.Definition.ID, bound.Action.Name)
	recipients := arguments
	var tool *skill.BindingTool
	if spec := bound.Definition.BindingTools; spec.RoutesAction(bound.Action.Name) {
		tool = skill.SelectedBindingTool(bound.Definition, bound.Binding, bound.Action.Name, arguments)
		if tool == nil || bound.Binding.DeploymentID != activation.Owner.ID {
			return nil, nil
		}
		identity = skill.BindingToolModelName(bound.Definition, bound.Binding, tool.Name)
		recipients, _ = arguments[spec.InputArgument].(map[string]interface{})
	}
	for _, grant := range activation.Task.Grants {
		if grant.Action != identity || (tool == nil) != (grant.ToolArguments == nil) || tool != nil && !routineGrantToolPinned(grant, tool) {
			continue
		}
		if grant.ToOwnerOnly {
			if owners == nil {
				return nil, nil
			}
			owner, err := RoutineOwner(activation)
			if err != nil {
				return nil, nil
			}
			addresses, err := owners.RoutineOwnerAddresses(ctx, activation.Scope, owner)
			if err != nil || !routineRecipientsOwned(recipients, addresses) {
				return nil, nil
			}
		}
		action := bound.Action
		if tool != nil {
			action.InputSchema = tool.InputSchema
		}
		if len(grant.ToDestinations) > 0 || RoutineActionPostsToDestination(action) {
			if len(grant.ToDestinations) == 0 || !routineDestinationsPinned(recipients, RoutineActionDestinationKeys(action.InputSchema), grant.ToDestinations) {
				return nil, nil
			}
		}
		matched := grant
		return &matched, nil
	}
	return nil, nil
}
