package models

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strings"
	"time"
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

func (e *EventEnvelope) Validate() error {
	if e == nil {
		return errors.New("event is required")
	}
	if err := e.Scope.Validate(); err != nil {
		return err
	}
	if !IsValidOpaqueIdentifier(e.ID, 512) || !validateEventSelector(e.Type) || e.Type == "*" || strings.TrimSpace(e.Source) == "" || len(e.Source) > 256 {
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

func validateEventSelector(value string) bool {
	value = strings.TrimSpace(value)
	return value == "*" || IsValidOpaqueIdentifier(value, 256)
}

func validateEventAttributes(attributes map[string]interface{}) error {
	keys := make([]string, 0, len(attributes))
	for key := range attributes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if !IsValidOpaqueIdentifier(key, 128) {
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

func IsValidAgentReference(value string, maxLength int) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maxLength || strings.Contains(value, "://") {
		return false
	}
	parts := strings.Split(value, "/")
	if len(parts) != 1 && len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if !IsValidOpaqueIdentifier(part, maxLength) {
			return false
		}
	}
	return true
}

// eventContext is the run context view of an event. Its last caller is the
// event-wait subsystem, which is deleted next; it goes with it.
func eventContext(event EventEnvelope) map[string]interface{} {
	return map[string]interface{}{
		"id": event.ID, "type": event.Type, "source": event.Source, "subject": event.Subject,
		"severity": event.Severity, "occurredAt": event.OccurredAt.UTC().Format(time.RFC3339Nano),
		"attributes": maps.Clone(event.Attributes), "payload": maps.Clone(event.Payload),
	}
}
