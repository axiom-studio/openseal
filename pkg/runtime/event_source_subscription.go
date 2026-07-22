package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	MaximumEventSourceSubscriptionParametersBytes = 64 * 1024
	MaximumEventSourceSubscriptionEventTypes      = 100
)

var (
	ErrEventSourceSubscriptionNotFound = errors.New("event source subscription not found")
	ErrEventSourceSubscriptionConflict = errors.New("event source subscription revision conflict")
	ErrInvalidEventSourceSubscription  = errors.New("invalid event source subscription")
	ErrInvalidEventSourceHealth        = errors.New("invalid event source health report")
)

type EventSourceSubscriptionStatus string

const (
	EventSourceSubscriptionActive  EventSourceSubscriptionStatus = "active"
	EventSourceSubscriptionPaused  EventSourceSubscriptionStatus = "paused"
	EventSourceSubscriptionRetired EventSourceSubscriptionStatus = "retired"
)

type EventSourceConnectorKind string

const (
	EventSourceConnectorHost  EventSourceConnectorKind = "host"
	EventSourceConnectorSkill EventSourceConnectorKind = "skill"
)

// EventSourceConnector identifies the deterministic adapter that acquires and
// normalizes events. Skill connectors resolve credentials through the owning
// deployment's governed Skill binding; subscription state never contains
// credential values or references.
type EventSourceConnector struct {
	Kind      EventSourceConnectorKind `json:"kind"`
	ID        string                   `json:"id"`
	Version   string                   `json:"version,omitempty"`
	BindingID string                   `json:"bindingId,omitempty"`
	Action    string                   `json:"action,omitempty"`
}

// EventSourceSubscription is desired state for a long-running host connector.
// Source is the exact normalized EventEnvelope source emitted by the adapter.
// Objective event rules remain the authority for deciding which work wakes.
type EventSourceSubscription struct {
	ID                  string                        `json:"id"`
	Scope               Scope                         `json:"scope"`
	Owner               ObjectiveOwner                `json:"owner"`
	DisplayName         string                        `json:"displayName"`
	Description         string                        `json:"description,omitempty"`
	Source              string                        `json:"source"`
	Connector           EventSourceConnector          `json:"connector"`
	Status              EventSourceSubscriptionStatus `json:"status"`
	EventTypes          []string                      `json:"eventTypes"`
	Parameters          map[string]interface{}        `json:"parameters,omitempty"`
	PollIntervalSeconds int64                         `json:"pollIntervalSeconds,omitempty"`
	Revision            int64                         `json:"revision"`
	CreatedAt           time.Time                     `json:"createdAt"`
	UpdatedAt           time.Time                     `json:"updatedAt"`
}

type EventSourceHealthState string

const (
	EventSourceHealthUnknown   EventSourceHealthState = "unknown"
	EventSourceHealthHealthy   EventSourceHealthState = "healthy"
	EventSourceHealthDegraded  EventSourceHealthState = "degraded"
	EventSourceHealthUnhealthy EventSourceHealthState = "unhealthy"
)

// EventSourceHealth is operational state reported by a connector. Summary is
// required to be a safe operator-facing message, never a raw provider body.
type EventSourceHealth struct {
	Scope                Scope                  `json:"scope"`
	SubscriptionID       string                 `json:"subscriptionId"`
	SubscriptionRevision int64                  `json:"subscriptionRevision"`
	State                EventSourceHealthState `json:"state"`
	LastHeartbeatAt      time.Time              `json:"lastHeartbeatAt"`
	LastSuccessAt        time.Time              `json:"lastSuccessAt,omitempty"`
	LastEventAt          time.Time              `json:"lastEventAt,omitempty"`
	ConsecutiveFailures  int                    `json:"consecutiveFailures"`
	ErrorCode            string                 `json:"errorCode,omitempty"`
	Summary              string                 `json:"summary,omitempty"`
	Revision             int64                  `json:"revision"`
	UpdatedAt            time.Time              `json:"updatedAt"`
}

type EventSourceSubscriptionDetail struct {
	Subscription *EventSourceSubscription `json:"subscription"`
	Health       *EventSourceHealth       `json:"health,omitempty"`
	Checkpoint   *EventSourceCheckpoint   `json:"checkpoint,omitempty"`
}

type EventSourceSubscriptionFilter struct {
	Scope         Scope
	Owner         *ObjectiveOwner
	Statuses      []EventSourceSubscriptionStatus
	ConnectorKind EventSourceConnectorKind
	Limit         int
	Offset        int
}

type EventSourceSubscriptionStore interface {
	CreateEventSourceSubscription(context.Context, *EventSourceSubscription) error
	GetEventSourceSubscription(context.Context, Scope, string) (*EventSourceSubscription, error)
	ListEventSourceSubscriptions(context.Context, EventSourceSubscriptionFilter) ([]*EventSourceSubscription, error)
	UpdateEventSourceSubscription(context.Context, *EventSourceSubscription, int64) error
	GetEventSourceHealth(context.Context, Scope, string) (*EventSourceHealth, error)
	SaveEventSourceHealth(context.Context, *EventSourceHealth, int64) error
}

type CreateEventSourceSubscriptionRequest struct {
	ID                  string
	Scope               Scope
	Owner               ObjectiveOwner
	DisplayName         string
	Description         string
	Source              string
	Connector           EventSourceConnector
	Status              EventSourceSubscriptionStatus
	EventTypes          []string
	Parameters          map[string]interface{}
	PollIntervalSeconds int64
}

type UpdateEventSourceSubscriptionRequest struct {
	ExpectedRevision    int64
	DisplayName         *string
	Description         *string
	Connector           *EventSourceConnector
	Status              *EventSourceSubscriptionStatus
	EventTypes          *[]string
	Parameters          map[string]interface{}
	ReplaceParameters   bool
	PollIntervalSeconds *int64
}

type ReportEventSourceHealthRequest struct {
	Scope                        Scope
	SubscriptionID               string
	ExpectedHealthRevision       int64
	ObservedSubscriptionRevision int64
	State                        EventSourceHealthState
	LastEventAt                  time.Time
	ErrorCode                    string
	Summary                      string
}

type EventSourceSubscriptionService struct {
	store       EventSourceSubscriptionStore
	checkpoints EventSourceCheckpointStore
	now         func() time.Time
}

func NewEventSourceSubscriptionService(store EventSourceSubscriptionStore, checkpoints EventSourceCheckpointStore) *EventSourceSubscriptionService {
	return &EventSourceSubscriptionService{store: store, checkpoints: checkpoints, now: time.Now}
}

func (s *EventSourceSubscriptionService) Create(ctx context.Context, req CreateEventSourceSubscriptionRequest) (*EventSourceSubscription, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("event source subscription service is not configured")
	}
	status := req.Status
	if status == "" {
		status = EventSourceSubscriptionPaused
	}
	now := s.now().UTC()
	subscription := &EventSourceSubscription{
		ID: strings.TrimSpace(req.ID), Scope: req.Scope, Owner: req.Owner,
		DisplayName: strings.TrimSpace(req.DisplayName), Description: strings.TrimSpace(req.Description), Source: strings.TrimSpace(req.Source),
		Connector: normalizeEventSourceConnector(req.Connector), Status: status, EventTypes: cloneEventSourceStrings(req.EventTypes),
		Parameters: cloneMap(req.Parameters), PollIntervalSeconds: req.PollIntervalSeconds, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	if subscription.ID == "" {
		subscription.ID = "event-source:" + uuid.NewString()
	}
	if err := subscription.Validate(); err != nil {
		return nil, err
	}
	if err := s.store.CreateEventSourceSubscription(ctx, subscription); err != nil {
		return nil, err
	}
	return cloneEventSourceSubscription(subscription), nil
}

func (s *EventSourceSubscriptionService) Get(ctx context.Context, scope Scope, id string) (*EventSourceSubscriptionDetail, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("event source subscription service is not configured")
	}
	subscription, err := s.store.GetEventSourceSubscription(ctx, scope, strings.TrimSpace(id))
	if err != nil || subscription == nil {
		return nil, err
	}
	health, err := s.store.GetEventSourceHealth(ctx, scope, subscription.ID)
	if err != nil {
		return nil, err
	}
	var checkpoint *EventSourceCheckpoint
	if s.checkpoints != nil {
		checkpoint, err = s.checkpoints.GetEventSourceCheckpoint(ctx, scope, subscription.Source, subscription.ID)
		if err != nil {
			return nil, err
		}
	}
	return &EventSourceSubscriptionDetail{Subscription: subscription, Health: health, Checkpoint: checkpoint}, nil
}

func (s *EventSourceSubscriptionService) List(ctx context.Context, filter EventSourceSubscriptionFilter) ([]*EventSourceSubscription, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("event source subscription service is not configured")
	}
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	if filter.Offset < 0 {
		return nil, ErrInvalidEventSourceSubscription
	}
	if filter.Owner != nil && filter.Owner.Validate() != nil {
		return nil, ErrInvalidEventSourceSubscription
	}
	if filter.ConnectorKind != "" && filter.ConnectorKind != EventSourceConnectorHost && filter.ConnectorKind != EventSourceConnectorSkill {
		return nil, ErrInvalidEventSourceSubscription
	}
	for _, status := range filter.Statuses {
		if status != EventSourceSubscriptionActive && status != EventSourceSubscriptionPaused && status != EventSourceSubscriptionRetired {
			return nil, ErrInvalidEventSourceSubscription
		}
	}
	if filter.Limit <= 0 || filter.Limit > 500 {
		filter.Limit = 50
	}
	return s.store.ListEventSourceSubscriptions(ctx, filter)
}

func (s *EventSourceSubscriptionService) Update(ctx context.Context, scope Scope, id string, req UpdateEventSourceSubscriptionRequest) (*EventSourceSubscription, error) {
	if s == nil || s.store == nil || req.ExpectedRevision < 1 {
		return nil, ErrInvalidEventSourceSubscription
	}
	current, err := s.store.GetEventSourceSubscription(ctx, scope, strings.TrimSpace(id))
	if err != nil {
		return nil, err
	}
	if current.Revision != req.ExpectedRevision {
		return nil, ErrEventSourceSubscriptionConflict
	}
	next := cloneEventSourceSubscription(current)
	if req.DisplayName != nil {
		next.DisplayName = strings.TrimSpace(*req.DisplayName)
	}
	if req.Description != nil {
		next.Description = strings.TrimSpace(*req.Description)
	}
	if req.Connector != nil {
		next.Connector = normalizeEventSourceConnector(*req.Connector)
	}
	if req.Status != nil {
		next.Status = *req.Status
	}
	if req.EventTypes != nil {
		next.EventTypes = cloneEventSourceStrings(*req.EventTypes)
	}
	if req.ReplaceParameters {
		next.Parameters = cloneMap(req.Parameters)
	}
	if req.PollIntervalSeconds != nil {
		next.PollIntervalSeconds = *req.PollIntervalSeconds
	}
	if current.Status == EventSourceSubscriptionRetired && next.Status != EventSourceSubscriptionRetired {
		return nil, ErrInvalidEventSourceSubscription
	}
	next.Revision++
	next.UpdatedAt = s.now().UTC()
	if err := next.Validate(); err != nil {
		return nil, err
	}
	if err := s.store.UpdateEventSourceSubscription(ctx, next, req.ExpectedRevision); err != nil {
		return nil, err
	}
	return cloneEventSourceSubscription(next), nil
}

func (s *EventSourceSubscriptionService) ReportHealth(ctx context.Context, req ReportEventSourceHealthRequest) (*EventSourceHealth, error) {
	if s == nil || s.store == nil || req.ExpectedHealthRevision < 0 || req.ObservedSubscriptionRevision < 1 {
		return nil, ErrInvalidEventSourceHealth
	}
	subscription, err := s.store.GetEventSourceSubscription(ctx, req.Scope, strings.TrimSpace(req.SubscriptionID))
	if err != nil {
		return nil, err
	}
	if subscription.Status != EventSourceSubscriptionActive || subscription.Revision != req.ObservedSubscriptionRevision {
		return nil, ErrEventSourceSubscriptionConflict
	}
	current, err := s.store.GetEventSourceHealth(ctx, req.Scope, subscription.ID)
	if err != nil {
		return nil, err
	}
	currentRevision := int64(0)
	if current != nil {
		currentRevision = current.Revision
	}
	if currentRevision != req.ExpectedHealthRevision {
		return nil, ErrEventSourceSubscriptionConflict
	}
	now := s.now().UTC()
	if !req.LastEventAt.IsZero() && req.LastEventAt.After(now.Add(5*time.Minute)) {
		return nil, ErrInvalidEventSourceHealth
	}
	health := &EventSourceHealth{
		Scope: req.Scope, SubscriptionID: subscription.ID, SubscriptionRevision: subscription.Revision,
		State: req.State, LastHeartbeatAt: now, LastEventAt: req.LastEventAt.UTC(), ErrorCode: strings.TrimSpace(req.ErrorCode),
		Summary: strings.TrimSpace(req.Summary), Revision: currentRevision + 1, UpdatedAt: now,
	}
	if current != nil {
		health.LastSuccessAt = current.LastSuccessAt
		health.ConsecutiveFailures = current.ConsecutiveFailures
		if health.LastEventAt.IsZero() {
			health.LastEventAt = current.LastEventAt
		}
	}
	if health.State == EventSourceHealthHealthy {
		health.LastSuccessAt, health.ConsecutiveFailures, health.ErrorCode, health.Summary = now, 0, "", ""
	} else {
		health.ConsecutiveFailures++
	}
	if err := health.Validate(); err != nil {
		return nil, err
	}
	if err := s.store.SaveEventSourceHealth(ctx, health, currentRevision); err != nil {
		return nil, err
	}
	return cloneEventSourceHealth(health), nil
}

func (s *EventSourceSubscription) Validate() error {
	if s == nil || s.Scope.Validate() != nil || s.Owner.Validate() != nil || !validOpaqueIdentifier(s.ID, 512) || !validOpaqueIdentifier(s.Source, 256) ||
		len(strings.TrimSpace(s.DisplayName)) == 0 || len(s.DisplayName) > 160 || len(s.Description) > 2000 || s.Revision < 1 || s.CreatedAt.IsZero() || s.UpdatedAt.IsZero() || s.UpdatedAt.Before(s.CreatedAt) {
		return ErrInvalidEventSourceSubscription
	}
	switch s.Status {
	case EventSourceSubscriptionActive, EventSourceSubscriptionPaused, EventSourceSubscriptionRetired:
	default:
		return ErrInvalidEventSourceSubscription
	}
	if err := s.Connector.Validate(); err != nil {
		return err
	}
	if len(s.EventTypes) == 0 || len(s.EventTypes) > MaximumEventSourceSubscriptionEventTypes || validateUniqueEventSourceValues(s.EventTypes, 256) != nil {
		return ErrInvalidEventSourceSubscription
	}
	if s.PollIntervalSeconds != 0 && (s.PollIntervalSeconds < 5 || s.PollIntervalSeconds > 86400) {
		return ErrInvalidEventSourceSubscription
	}
	if err := validateCredentialFreeContext(s.Parameters); err != nil {
		return fmt.Errorf("%w: parameters must be credential-free", ErrInvalidEventSourceSubscription)
	}
	encoded, err := json.Marshal(s.Parameters)
	if err != nil || len(encoded) > MaximumEventSourceSubscriptionParametersBytes {
		return ErrInvalidEventSourceSubscription
	}
	return nil
}

func (c EventSourceConnector) Validate() error {
	if !validOpaqueIdentifier(c.ID, 256) || len(strings.TrimSpace(c.Version)) > 128 {
		return ErrInvalidEventSourceSubscription
	}
	switch c.Kind {
	case EventSourceConnectorHost:
		if c.BindingID != "" || c.Action != "" {
			return ErrInvalidEventSourceSubscription
		}
	case EventSourceConnectorSkill:
		if !validOpaqueIdentifier(c.BindingID, 512) || !validOpaqueIdentifier(c.Action, 128) || strings.TrimSpace(c.Version) == "" {
			return ErrInvalidEventSourceSubscription
		}
	default:
		return ErrInvalidEventSourceSubscription
	}
	return nil
}

func (h *EventSourceHealth) Validate() error {
	if h == nil || h.Scope.Validate() != nil || !validOpaqueIdentifier(h.SubscriptionID, 512) || h.SubscriptionRevision < 1 || h.Revision < 1 || h.LastHeartbeatAt.IsZero() || h.UpdatedAt.IsZero() || h.ConsecutiveFailures < 0 || len(h.Summary) > 1024 || strings.ContainsAny(h.Summary, "\r\n") {
		return ErrInvalidEventSourceHealth
	}
	switch h.State {
	case EventSourceHealthHealthy, EventSourceHealthDegraded, EventSourceHealthUnhealthy:
	default:
		return ErrInvalidEventSourceHealth
	}
	if h.ErrorCode != "" && !validOpaqueIdentifier(h.ErrorCode, 128) {
		return ErrInvalidEventSourceHealth
	}
	if h.State == EventSourceHealthHealthy && (h.ConsecutiveFailures != 0 || h.ErrorCode != "" || h.Summary != "" || h.LastSuccessAt.IsZero()) {
		return ErrInvalidEventSourceHealth
	}
	if h.State != EventSourceHealthHealthy && h.ConsecutiveFailures == 0 {
		return ErrInvalidEventSourceHealth
	}
	return nil
}

func validateUniqueEventSourceValues(values []string, maxLength int) error {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if !validOpaqueIdentifier(value, maxLength) {
			return ErrInvalidEventSourceSubscription
		}
		if _, exists := seen[value]; exists {
			return ErrInvalidEventSourceSubscription
		}
		seen[value] = struct{}{}
	}
	return nil
}

func cloneEventSourceSubscription(value *EventSourceSubscription) *EventSourceSubscription {
	if value == nil {
		return nil
	}
	encoded, _ := json.Marshal(value)
	var cloned EventSourceSubscription
	_ = json.Unmarshal(encoded, &cloned)
	return &cloned
}

func cloneEventSourceHealth(value *EventSourceHealth) *EventSourceHealth {
	if value == nil {
		return nil
	}
	encoded, _ := json.Marshal(value)
	var cloned EventSourceHealth
	_ = json.Unmarshal(encoded, &cloned)
	return &cloned
}

func cloneEventSourceStrings(values []string) []string {
	result := make([]string, len(values))
	for index, value := range values {
		result[index] = strings.TrimSpace(value)
	}
	return result
}

func normalizeEventSourceConnector(value EventSourceConnector) EventSourceConnector {
	value.ID = strings.TrimSpace(value.ID)
	value.Version = strings.TrimSpace(value.Version)
	value.BindingID = strings.TrimSpace(value.BindingID)
	value.Action = strings.TrimSpace(value.Action)
	return value
}
