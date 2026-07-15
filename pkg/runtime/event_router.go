package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"
)

const objectiveEventRulesVersion = "1"

var ErrInvalidObjectiveEventRules = errors.New("objective event rules are invalid")

// ObjectiveEventRules is the versioned, product-neutral event routing contract
// stored by an Objective. Hosts normalize Kubernetes, webhook, message,
// analytics, finance, and other domain events into EventEnvelope values.
type ObjectiveEventRules struct {
	Version string               `json:"version"`
	Rules   []ObjectiveEventRule `json:"rules"`
}

// ObjectiveEventRule selects normalized events and projects each match into a
// canonical durable Run. Credentials and transport configuration never belong
// in this portable contract.
type ObjectiveEventRule struct {
	ID              string                 `json:"id"`
	EventType       string                 `json:"eventType"`
	Source          string                 `json:"source,omitempty"`
	Subject         string                 `json:"subject,omitempty"`
	Severities      []string               `json:"severities,omitempty"`
	Attributes      map[string]interface{} `json:"attributes,omitempty"`
	AssignedAgentID string                 `json:"assignedAgentId,omitempty"`
	RunBudget       *BudgetPolicy          `json:"runBudget,omitempty"`
	RunTemplate     *ObjectiveRunTemplate  `json:"runTemplate,omitempty"`
}

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
	RuleID      string         `json:"ruleId"`
	Run         *AgentRun      `json:"run"`
	Event       *ActivityEvent `json:"event,omitempty"`
	Created     bool           `json:"created"`
}

type EventRouteResult struct {
	Routes []EventRoute `json:"routes"`
}

type ObjectiveEventRouter struct {
	store RunCommandStore
}

func NewObjectiveEventRouter(store RunCommandStore) *ObjectiveEventRouter {
	return &ObjectiveEventRouter{store: store}
}

func DecodeObjectiveEventRules(value map[string]interface{}) (*ObjectiveEventRules, error) {
	if len(value) == 0 {
		return nil, nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("%w: encode rules: %v", ErrInvalidObjectiveEventRules, err)
	}
	var rules ObjectiveEventRules
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&rules); err != nil {
		return nil, fmt.Errorf("%w: decode rules: %v", ErrInvalidObjectiveEventRules, err)
	}
	if err := rules.Validate(); err != nil {
		return nil, err
	}
	return &rules, nil
}

func (r *ObjectiveEventRules) Validate() error {
	if r == nil {
		return nil
	}
	if r.Version != objectiveEventRulesVersion {
		return fmt.Errorf("%w: version must be %q", ErrInvalidObjectiveEventRules, objectiveEventRulesVersion)
	}
	if len(r.Rules) == 0 || len(r.Rules) > 64 {
		return fmt.Errorf("%w: between 1 and 64 rules are required", ErrInvalidObjectiveEventRules)
	}
	seen := make(map[string]struct{}, len(r.Rules))
	for index := range r.Rules {
		rule := &r.Rules[index]
		if err := rule.Validate(); err != nil {
			return fmt.Errorf("%w: rule %d: %w", ErrInvalidObjectiveEventRules, index, err)
		}
		if _, exists := seen[rule.ID]; exists {
			return fmt.Errorf("%w: duplicate rule id %q", ErrInvalidObjectiveEventRules, rule.ID)
		}
		seen[rule.ID] = struct{}{}
	}
	return nil
}

func (r *ObjectiveEventRule) Validate() error {
	if r == nil || !validOpaqueIdentifier(r.ID, 128) {
		return errors.New("id is required")
	}
	if !validateEventSelector(r.EventType) {
		return errors.New("eventType is required")
	}
	if len(strings.TrimSpace(r.Source)) > 256 || len(strings.TrimSpace(r.Subject)) > 512 {
		return errors.New("source or subject selector is too long")
	}
	if len(r.Severities) > 32 || len(r.Attributes) > 64 {
		return errors.New("severity or attribute selector limit exceeded")
	}
	for _, severity := range r.Severities {
		if strings.TrimSpace(severity) == "" || len(severity) > 64 {
			return errors.New("severity selectors must be non-empty and at most 64 characters")
		}
	}
	if err := validateEventAttributes(r.Attributes); err != nil {
		return err
	}
	if r.AssignedAgentID != "" && !validAgentReference(r.AssignedAgentID, 256) {
		return errors.New("assignedAgentId is invalid")
	}
	if r.RunBudget != nil {
		if err := r.RunBudget.Validate(); err != nil {
			return fmt.Errorf("run budget: %w", err)
		}
	}
	if err := r.RunTemplate.Validate(); err != nil {
		return err
	}
	return nil
}

// validAgentReference accepts both ordinary opaque deployment IDs and the
// scope-qualified definition IDs produced by workforce authoring
// (scope-kind/scope-id/definition-id). Keep URL/query/control delimiters out so
// the reference remains data rather than a transport path.
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
	if len(e.Subject) > 512 || len(e.Severity) > 64 || len(e.Attributes) > 64 {
		return errors.New("event subject, severity, or attribute limit exceeded")
	}
	if e.OccurredAt.IsZero() {
		return errors.New("event occurredAt is required for stable replay")
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

func (r *ObjectiveEventRouter) Route(ctx context.Context, event EventEnvelope) (*EventRouteResult, error) {
	if r == nil || r.store == nil {
		return nil, errors.New("objective event router is not configured")
	}
	if err := event.Validate(); err != nil {
		return nil, err
	}
	objectives, err := r.listActiveObjectives(ctx, event.Scope)
	if err != nil {
		return nil, err
	}
	result := &EventRouteResult{Routes: make([]EventRoute, 0)}
	commands := NewRunCommandService(r.store)
	for _, objective := range objectives {
		rules, decodeErr := DecodeObjectiveEventRules(objective.EventRules)
		if decodeErr != nil {
			return nil, fmt.Errorf("objective %s: %w", objective.ID, decodeErr)
		}
		if rules == nil {
			continue
		}
		for _, rule := range rules.Rules {
			if !rule.matches(event) {
				continue
			}
			request := eventRunRequest(objective, rule, event)
			created, createErr := commands.CreateAgentRun(ctx, request)
			if createErr != nil {
				return nil, createErr
			}
			result.Routes = append(result.Routes, EventRoute{
				ObjectiveID: objective.ID, RuleID: rule.ID, Run: created.Run, Event: created.Event, Created: created.Event != nil,
			})
		}
	}
	return result, nil
}

func (r *ObjectiveEventRouter) listActiveObjectives(ctx context.Context, scope Scope) ([]*Objective, error) {
	const pageSize = 200
	result := make([]*Objective, 0)
	for offset := 0; ; offset += pageSize {
		page, err := r.store.ListObjectives(ctx, ObjectiveFilter{Scope: scope, Statuses: []ObjectiveStatus{ObjectiveStatusActive}, Limit: pageSize, Offset: offset})
		if err != nil {
			return nil, err
		}
		result = append(result, page...)
		if len(page) < pageSize {
			break
		}
	}
	return result, nil
}

func eventRunRequest(objective *Objective, rule ObjectiveEventRule, event EventEnvelope) CreateAgentRunRequest {
	contextValues := map[string]interface{}{}
	entrypoint := ""
	policy := map[string]interface{}(nil)
	if rule.RunTemplate != nil {
		contextValues = cloneMap(rule.RunTemplate.Context)
		if contextValues == nil {
			contextValues = map[string]interface{}{}
		}
		entrypoint = strings.TrimSpace(rule.RunTemplate.Entrypoint)
		policy = cloneMap(rule.RunTemplate.Policy)
		if rule.RunTemplate.Capability != nil {
			contextValues["capabilityInvocation"] = map[string]interface{}{
				"skillId": rule.RunTemplate.Capability.SkillID, "skillVersion": rule.RunTemplate.Capability.SkillVersion,
				"action": rule.RunTemplate.Capability.Action, "inputs": cloneMap(rule.RunTemplate.Capability.Inputs),
			}
		}
	}
	contextValues["event"] = eventContext(event)
	actor := event.Actor
	if strings.TrimSpace(actor.Type) == "" {
		actor = ActivityActor{Type: "event", ID: event.Source}
	}
	return CreateAgentRunRequest{
		Scope: event.Scope, Kind: RunKindAgentWork, ObjectiveID: objective.ID, Owner: objective.Owner,
		AssignedAgentID: rule.AssignedAgentID, Entrypoint: entrypoint, ConcurrencyKey: "objective:" + objective.ID + ":event:" + rule.ID,
		Goal: objective.Goal, Source: RunSourceEvent, Priority: objective.Priority, Context: contextValues,
		Budget: rule.RunBudget, Policy: policy, IdempotencyKey: eventRouteKey(objective.ID, rule.ID, event.Source, event.ID),
		Actor: actor, Visibility: ActivityVisibilityScope,
	}
}

func eventContext(event EventEnvelope) map[string]interface{} {
	return map[string]interface{}{
		"id": event.ID, "type": event.Type, "source": event.Source, "subject": event.Subject,
		"severity": event.Severity, "occurredAt": event.OccurredAt.UTC().Format(time.RFC3339Nano),
		"attributes": cloneMap(event.Attributes), "payload": cloneMap(event.Payload),
	}
}

func eventRouteKey(objectiveID, ruleID, eventSource, eventID string) string {
	digest := sha256.Sum256([]byte(eventSource + "\x00" + eventID))
	return "event:" + objectiveID + ":" + ruleID + ":" + hex.EncodeToString(digest[:16])
}

func (r ObjectiveEventRule) matches(event EventEnvelope) bool {
	if r.EventType != "*" && !strings.EqualFold(r.EventType, event.Type) {
		return false
	}
	if r.Source != "" && r.Source != event.Source {
		return false
	}
	if r.Subject != "" && r.Subject != event.Subject {
		return false
	}
	if len(r.Severities) > 0 {
		found := false
		for _, severity := range r.Severities {
			if strings.EqualFold(severity, event.Severity) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	for key, expected := range r.Attributes {
		if actual, ok := event.Attributes[key]; !ok || !reflect.DeepEqual(expected, actual) {
			return false
		}
	}
	return true
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
