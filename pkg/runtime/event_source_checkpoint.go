package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const MaximumEventSourceRecentIDs = 20_000

var (
	ErrEventSourceCheckpointConflict = errors.New("event source checkpoint revision conflict")
	ErrInvalidEventSourceCheckpoint  = errors.New("invalid event source checkpoint")
)

// EventSourceCheckpoint is the product-neutral durable progress record for a
// host event connector. Cursor is opaque to OpenSeal. RecentEventIDs provide a
// bounded replay window for list-only sources that cannot resume from a cursor.
// Checkpoints are internal runtime state and are never projected into prompts.
type EventSourceCheckpoint struct {
	Scope          Scope     `json:"scope"`
	Source         string    `json:"source"`
	SubscriptionID string    `json:"subscriptionId"`
	Cursor         string    `json:"cursor,omitempty"`
	RecentEventIDs []string  `json:"recentEventIds,omitempty"`
	Watermark      time.Time `json:"watermark,omitempty"`
	Revision       int64     `json:"revision"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

type EventSourceCheckpointStore interface {
	GetEventSourceCheckpoint(context.Context, Scope, string, string) (*EventSourceCheckpoint, error)
	SaveEventSourceCheckpoint(context.Context, *EventSourceCheckpoint, int64) error
}

type AdvanceEventSourceCheckpointRequest struct {
	Scope            Scope
	Source           string
	SubscriptionID   string
	ExpectedRevision int64
	Cursor           string
	EventIDs         []string
	Watermark        time.Time
}

type EventSourceCheckpointService struct {
	store EventSourceCheckpointStore
	now   func() time.Time
}

func NewEventSourceCheckpointService(store EventSourceCheckpointStore) *EventSourceCheckpointService {
	return &EventSourceCheckpointService{store: store, now: time.Now}
}

func (s *EventSourceCheckpointService) Get(ctx context.Context, scope Scope, source, subscriptionID string) (*EventSourceCheckpoint, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("event source checkpoint service is not configured")
	}
	if err := validateEventSourceIdentity(scope, source, subscriptionID); err != nil {
		return nil, err
	}
	return s.store.GetEventSourceCheckpoint(ctx, scope, strings.TrimSpace(source), strings.TrimSpace(subscriptionID))
}

func (s *EventSourceCheckpointService) Advance(ctx context.Context, req AdvanceEventSourceCheckpointRequest) (*EventSourceCheckpoint, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("event source checkpoint service is not configured")
	}
	if err := validateEventSourceIdentity(req.Scope, req.Source, req.SubscriptionID); err != nil || req.ExpectedRevision < 0 {
		return nil, ErrInvalidEventSourceCheckpoint
	}
	current, err := s.store.GetEventSourceCheckpoint(ctx, req.Scope, strings.TrimSpace(req.Source), strings.TrimSpace(req.SubscriptionID))
	if err != nil {
		return nil, err
	}
	currentRevision := int64(0)
	if current != nil {
		currentRevision = current.Revision
	}
	if currentRevision != req.ExpectedRevision {
		return nil, ErrEventSourceCheckpointConflict
	}

	recent := make([]string, 0, len(req.EventIDs)+eventSourceRecentCount(current))
	seen := make(map[string]struct{}, cap(recent))
	if current != nil {
		for _, id := range current.RecentEventIDs {
			appendEventSourceID(&recent, seen, id)
		}
	}
	for _, id := range req.EventIDs {
		if !validOpaqueIdentifier(strings.TrimSpace(id), 512) {
			return nil, ErrInvalidEventSourceCheckpoint
		}
		appendEventSourceID(&recent, seen, id)
	}
	if len(recent) > MaximumEventSourceRecentIDs {
		recent = append([]string(nil), recent[len(recent)-MaximumEventSourceRecentIDs:]...)
	}
	watermark := req.Watermark.UTC()
	if current != nil && current.Watermark.After(watermark) {
		watermark = current.Watermark
	}
	next := &EventSourceCheckpoint{
		Scope: req.Scope, Source: strings.TrimSpace(req.Source), SubscriptionID: strings.TrimSpace(req.SubscriptionID),
		Cursor: strings.TrimSpace(req.Cursor), RecentEventIDs: recent, Watermark: watermark,
		Revision: req.ExpectedRevision + 1, UpdatedAt: s.now().UTC(),
	}
	if next.Cursor == "" && current != nil {
		next.Cursor = current.Cursor
	}
	if err := next.Validate(); err != nil {
		return nil, err
	}
	if err := s.store.SaveEventSourceCheckpoint(ctx, next, req.ExpectedRevision); err != nil {
		return nil, err
	}
	return cloneEventSourceCheckpoint(next), nil
}

func (c *EventSourceCheckpoint) Validate() error {
	if c == nil || validateEventSourceIdentity(c.Scope, c.Source, c.SubscriptionID) != nil || c.Revision < 1 || c.UpdatedAt.IsZero() || len(c.Cursor) > 4096 || len(c.RecentEventIDs) > MaximumEventSourceRecentIDs {
		return ErrInvalidEventSourceCheckpoint
	}
	seen := make(map[string]struct{}, len(c.RecentEventIDs))
	for _, id := range c.RecentEventIDs {
		id = strings.TrimSpace(id)
		if !validOpaqueIdentifier(id, 512) {
			return ErrInvalidEventSourceCheckpoint
		}
		if _, duplicate := seen[id]; duplicate {
			return ErrInvalidEventSourceCheckpoint
		}
		seen[id] = struct{}{}
	}
	return nil
}

func validateEventSourceIdentity(scope Scope, source, subscriptionID string) error {
	if scope.Validate() != nil || !validOpaqueIdentifier(strings.TrimSpace(source), 256) || !validOpaqueIdentifier(strings.TrimSpace(subscriptionID), 512) {
		return ErrInvalidEventSourceCheckpoint
	}
	return nil
}

func appendEventSourceID(result *[]string, seen map[string]struct{}, id string) {
	id = strings.TrimSpace(id)
	if _, exists := seen[id]; exists {
		return
	}
	seen[id] = struct{}{}
	*result = append(*result, id)
}

func eventSourceRecentCount(checkpoint *EventSourceCheckpoint) int {
	if checkpoint == nil {
		return 0
	}
	return len(checkpoint.RecentEventIDs)
}

func cloneEventSourceCheckpoint(value *EventSourceCheckpoint) *EventSourceCheckpoint {
	if value == nil {
		return nil
	}
	encoded, _ := json.Marshal(value)
	var cloned EventSourceCheckpoint
	_ = json.Unmarshal(encoded, &cloned)
	return &cloned
}
