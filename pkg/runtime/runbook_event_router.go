package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/runbook"
)

// EventEnvelope is a credential-free event accepted at the kernel boundary.
// ID must be stable at the source so delivery retries are exactly idempotent.
type EventEnvelope struct {
	ID         string                 `json:"id"`
	Scope      Scope                  `json:"scope"`
	Type       string                 `json:"type"`
	Source     string                 `json:"source"`
	Subject    string                 `json:"subject,omitempty"`
	Severity   string                 `json:"severity,omitempty"`
	OccurredAt time.Time              `json:"occurredAt"`
	Attributes map[string]interface{} `json:"attributes,omitempty"`
	Payload    map[string]interface{} `json:"payload,omitempty"`
	Actor      ActivityActor          `json:"actor,omitempty"`
}

type EventRoute struct {
	ObjectiveID string         `json:"objectiveId"`
	RunbookID   string         `json:"runbookId"`
	TriggerID   string         `json:"triggerId"`
	Run         *AgentRun      `json:"run"`
	Event       *ActivityEvent `json:"event,omitempty"`
	Created     bool           `json:"created"`
}

type EventRouteResult struct {
	Routes []EventRoute `json:"routes"`
}

type RunbookEventRouter struct {
	store interface {
		RunCommandStore
		RunbookActivationStore
		PortfolioStore
	}
}

func NewRunbookEventRouter(store interface {
	RunCommandStore
	RunbookActivationStore
	PortfolioStore
}) *RunbookEventRouter {
	return &RunbookEventRouter{store: store}
}

func (e *EventEnvelope) Validate() error {
	if e == nil {
		return errors.New("event is required")
	}
	if err := e.Scope.Validate(); err != nil {
		return err
	}
	if !validOpaqueIdentifier(e.ID, 512) || !validateEventSelector(e.Type) || e.Type == "*" || strings.TrimSpace(e.Source) == "" || len(e.Source) > 256 {
		return errors.New("event requires a stable id, concrete type, and source")
	}
	if len(e.Subject) > 512 || len(e.Severity) > 64 || len(e.Attributes) > 64 || e.OccurredAt.IsZero() {
		return errors.New("event subject, severity, attributes, or occurredAt are invalid")
	}
	if err := validateEventAttributes(e.Attributes); err != nil {
		return err
	}
	if err := ValidateCredentialFreeContext(e.Payload); err != nil {
		return fmt.Errorf("event payload: %w", err)
	}
	encoded, err := json.Marshal(struct {
		Attributes map[string]interface{} `json:"attributes,omitempty"`
		Payload    map[string]interface{} `json:"payload,omitempty"`
	}{e.Attributes, e.Payload})
	if err != nil || len(encoded) > 1<<20 {
		return errors.New("event data must be valid JSON no larger than 1 MiB")
	}
	return nil
}

func (r *RunbookEventRouter) Route(ctx context.Context, event EventEnvelope) (*EventRouteResult, error) {
	if r == nil || r.store == nil {
		return nil, errors.New("Runbook event router is not configured")
	}
	if err := event.Validate(); err != nil {
		return nil, err
	}
	activations, err := r.store.ListRunbookActivations(ctx, RunbookActivationFilter{
		Scope: event.Scope, Statuses: []RunbookActivationStatus{RunbookActivationActive},
		TriggerKinds: []runbook.TriggerKind{runbook.TriggerEvent}, Limit: 500,
	})
	if err != nil {
		return nil, err
	}
	result := &EventRouteResult{Routes: make([]EventRoute, 0)}
	commands := NewRunCommandService(r.store)
	for _, activation := range activations {
		if !strings.EqualFold(activation.Trigger.EventType, event.Type) {
			continue
		}
		objective, err := r.store.GetObjective(ctx, event.Scope, activation.ObjectiveID)
		if err != nil {
			return nil, err
		}
		if objective == nil || objective.Status != ObjectiveStatusActive || objective.Owner != activation.Owner {
			continue
		}
		contextValues := cloneMap(activation.Input)
		if contextValues == nil {
			contextValues = map[string]interface{}{}
		}
		contextValues["event"] = eventContext(event)
		contextValues["runbookActivationId"] = activation.ID
		contextValues["runbookDefinitionId"] = activation.DefinitionID
		contextValues["runbookDefinitionVersion"] = activation.DefinitionVersion
		contextValues["runbookTriggerId"] = activation.TriggerID
		actor := event.Actor
		if strings.TrimSpace(actor.Type) == "" {
			actor = ActivityActor{Type: "event", ID: event.Source}
		}
		created, err := commands.CreateAgentRun(ctx, CreateAgentRunRequest{
			Scope: event.Scope, ObjectiveID: activation.ObjectiveID, Owner: activation.Owner,
			AssignedAgentID: activation.AssignedAgentID, Entrypoint: activation.Trigger.Entrypoint,
			ConcurrencyKey: "runbook:" + activation.ID, Goal: objective.Goal, Priority: objective.Priority,
			Source: RunSourceEvent, Context: contextValues,
			Policy: cloneMap(activation.Policy), Budget: cloneBudgetPolicy(activation.Budget),
			IdempotencyKey: runbookEventRouteKey(activation.ID, event.Source, event.ID), Actor: actor,
			Visibility: ActivityVisibilityScope,
		})
		if err != nil {
			return nil, err
		}
		result.Routes = append(result.Routes, EventRoute{
			ObjectiveID: activation.ObjectiveID, RunbookID: activation.ID, TriggerID: activation.TriggerID,
			Run: created.Run, Event: created.Event, Created: created.Event != nil,
		})
	}
	return result, nil
}

func eventContext(event EventEnvelope) map[string]interface{} {
	return map[string]interface{}{
		"id": event.ID, "type": event.Type, "source": event.Source, "subject": event.Subject,
		"severity": event.Severity, "occurredAt": event.OccurredAt.UTC().Format(time.RFC3339Nano),
		"attributes": cloneMap(event.Attributes), "payload": cloneMap(event.Payload),
	}
}

func runbookEventRouteKey(runbookID, source, eventID string) string {
	digest := sha256.Sum256([]byte(source + "\x00" + eventID))
	return "runbook-event:" + runbookID + ":" + hex.EncodeToString(digest[:16])
}

func validateEventSelector(value string) bool {
	value = strings.TrimSpace(value)
	return value == "*" || validOpaqueIdentifier(value, 256)
}

func validateEventAttributes(attributes map[string]interface{}) error {
	keys := make([]string, 0, len(attributes))
	for key := range attributes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if !validOpaqueIdentifier(key, 128) {
			return fmt.Errorf("event attribute key %q is invalid", key)
		}
		switch attributes[key].(type) {
		case nil, string, bool, float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, json.Number:
		default:
			return fmt.Errorf("event attribute %q must be a JSON scalar", key)
		}
	}
	return ValidateCredentialFreeContext(attributes)
}

func validAgentReference(value string, maxLength int) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maxLength || strings.Contains(value, "://") {
		return false
	}
	parts := strings.Split(value, "/")
	if len(parts) != 1 && len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if !validOpaqueIdentifier(part, maxLength) {
			return false
		}
	}
	return true
}
