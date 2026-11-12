package models

import (
	"errors"
	"regexp"
	"strings"

	"github.com/axiom-studio/openseal/pkg/validation"
)

// ScheduledAgentTask is the cognitive execution method of an Objective-owned
// routine. It pins work and its reporting destination, never credentials or
// model-generated execution authority. Every occurrence resolves the current
// deployed Agent and its currently authorized tools through the normal worker.
type ScheduledAgentTask struct {
	Goal            string `json:"goal"`
	ConversationID  string `json:"conversationId"`
	SourceMessageID string `json:"sourceMessageId"`
	// AppID is the app this routine keeps up to date. It belongs to the same
	// conversation and owner; each occurrence is told to save the app's data.
	AppID string `json:"appId,omitempty"`
	// Grants are the actions the user approved in the routine's brief; its
	// runs perform them without asking. They come only from the approved
	// create_task arguments and can only be cleared afterwards.
	Grants []RoutineGrant `json:"grants,omitempty"`
	// Script, when set, is the saved chat script every occurrence's model
	// runs first with run_command, pinned to the version the user approved.
	// Only an approved create_task or set_script sets it.
	Script *RoutineScript `json:"script,omitempty"`
	// CreatedBy is the person who created the routine: the creating run's
	// ActingUser. Its occurrences act for that person, and only that person
	// changes its script or resumes it. Empty means no recorded creator.
	CreatedBy string `json:"createdBy,omitempty"`
	// Event, when set, makes this an event routine: its occurrences start
	// when a matching event arrives instead of on a schedule.
	Event *RoutineEventTrigger `json:"event,omitempty"`
}

// scheduledTaskAppID matches host-assigned app identifiers.
var scheduledTaskAppID = regexp.MustCompile(`^app-[a-f0-9]{64}$`)

func (t *ScheduledAgentTask) Validate() error {
	if t == nil || strings.TrimSpace(t.Goal) == "" || len(t.Goal) > 16000 || !IsValidOpaqueIdentifier(t.ConversationID, 256) || !IsValidOpaqueIdentifier(t.SourceMessageID, 256) {
		return errors.New("scheduled task requires bounded work and an originating conversation message")
	}
	if t.CreatedBy != "" && !IsValidOpaqueIdentifier(t.CreatedBy, 256) {
		return errors.New("scheduled task creator is invalid")
	}
	if t.AppID != "" && !scheduledTaskAppID.MatchString(t.AppID) {
		return errors.New("scheduled task app must be an app ID from this chat")
	}
	if _, err := NormalizeRoutineGrants(t.Grants, false); err != nil {
		return err
	}
	if t.Script != nil {
		if err := t.Script.Validate(); err != nil {
			return err
		}
	}
	if t.Event != nil {
		if err := t.Event.Validate(); err != nil {
			return err
		}
	}
	return validation.ValidateNoSensitiveInput(t.Goal)
}

func CloneScheduledAgentTask(t *ScheduledAgentTask) *ScheduledAgentTask {
	if t == nil {
		return nil
	}
	copy := *t
	copy.Grants = CloneRoutineGrants(t.Grants)
	copy.Script = CloneRoutineScript(t.Script)
	copy.Event = cloneRoutineEventTrigger(t.Event)
	return &copy
}

func cloneRoutineEventTrigger(t *RoutineEventTrigger) *RoutineEventTrigger {
	if t == nil {
		return nil
	}
	copy := *t
	if t.Filter != nil {
		copy.Filter = make([]RoutineEventCondition, len(t.Filter))
		for i, condition := range t.Filter {
			condition.Values = append([]string(nil), condition.Values...)
			copy.Filter[i] = condition
		}
	}
	if t.Calendar != nil {
		calendar := *t.Calendar
		copy.Calendar = &calendar
	}
	if t.Webhook != nil {
		webhook := *t.Webhook
		copy.Webhook = &webhook
	}
	return &copy
}

const (
	ScheduledTaskContextKey = "scheduledAgentTask"
)
