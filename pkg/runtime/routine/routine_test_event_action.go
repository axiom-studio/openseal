package routine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// maximumRoutineTestEventPayload bounds a send_test_event payload.
const maximumRoutineTestEventPayload = 16 << 10

type routineTestEventArguments struct {
	ActivationID string                 `json:"activationId"`
	Payload      map[string]interface{} `json:"payload,omitempty"`
}

// routineTestEventAction sends one test event to an event routine. Only the
// routine's creator can do it (E17).
func routineTestEventAction() skill.Action {
	return skill.Action{
		Name:        RunbookActionSendTestEvent,
		Description: "Send a test event to an event routine, for example right after creating a webhook routine, so the person sees one run. Only the person who created the routine can do this. The run follows the routine's brief and grants.",
		Risk:        skill.RiskLevelWrite, SideEffect: skill.SideEffectWrite, Idempotency: skill.IdempotencyRequired,
		InputSchema: map[string]interface{}{
			"type": "object", "additionalProperties": false, "required": []interface{}{"activationId"},
			"properties": map[string]interface{}{
				"activationId": map[string]interface{}{"type": "string", "minLength": 1},
				"payload":      map[string]interface{}{"type": "object", "description": "Optional JSON object the test event carries, at most 16 KiB. Defaults to {\"test\": true}."},
			},
		},
		OutputSchema: map[string]interface{}{
			"type": "object", "additionalProperties": false,
			"properties": map[string]interface{}{
				"activationId": map[string]interface{}{"type": "string"},
				"eventId":      map[string]interface{}{"type": "string"},
				"offered":      map[string]interface{}{"type": "object"},
			},
			"required": []interface{}{"activationId", "eventId", "offered"},
		},
	}
}

// resolveRoutineTestEvent returns the active event routine of this chat the
// test goes to and the payload to send. The acting person must be the
// routine's recorded creator; an empty creator is never adopted here.
func resolveRoutineTestEvent(ctx context.Context, store RunbookActionStore, events routineEventActions, run *models.AgentRun, arguments map[string]interface{}) (*models.RunbookActivation, json.RawMessage, error) {
	if events.offerer == nil {
		return nil, nil, ErrRoutineEventsUnavailable
	}
	var args routineTestEventArguments
	if err := decodeScheduledTaskArguments(arguments, &args); err != nil {
		return nil, nil, err
	}
	args.ActivationID = strings.TrimSpace(args.ActivationID)
	if args.ActivationID == "" {
		return nil, nil, errors.New("send_test_event requires an activationId")
	}
	var payload json.RawMessage
	if args.Payload != nil {
		encoded, err := json.Marshal(args.Payload)
		if err != nil {
			return nil, nil, err
		}
		if len(encoded) > maximumRoutineTestEventPayload {
			return nil, nil, errors.New("a test event payload can be at most 16 KiB")
		}
		payload = encoded
	}
	a, err := store.GetRunbookActivation(ctx, run.Scope, args.ActivationID)
	if err != nil {
		return nil, nil, err
	}
	if a == nil || a.Owner != run.Owner {
		return nil, nil, errors.New("routine is not owned by this chat")
	}
	if a.Task == nil || a.Task.Event == nil || a.Trigger.Kind != runbook.TriggerEvent {
		return nil, nil, errors.New("only a routine that starts on an event can get a test event")
	}
	if strings.TrimSpace(run.ActingUser) == "" || run.ActingUser != a.Task.CreatedBy {
		return nil, nil, ErrRoutineNotCreator
	}
	if a.Status != models.RunbookActivationActive {
		return nil, nil, ErrRunbookActivationInactive
	}
	return a, payload, nil
}

func dispatchRoutineTestEvent(ctx context.Context, store RunbookActionStore, events routineEventActions, run *models.AgentRun, arguments map[string]interface{}) (map[string]interface{}, error) {
	a, payload, err := resolveRoutineTestEvent(ctx, store, events, run, arguments)
	if err != nil {
		return nil, err
	}
	eventID, offered, err := events.offerer.OfferRoutineTestEvent(ctx, a.Scope, a.ID, run.ActingUser, payload)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{"activationId": a.ID, "eventId": eventID, "offered": offered}, nil
}
