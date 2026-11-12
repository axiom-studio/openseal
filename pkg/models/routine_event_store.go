package models

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/google/uuid"
)

// RoutineEventState is where one stored event is on its way to a Run.
type RoutineEventState string

const (
	RoutineEventPending RoutineEventState = "pending" // waiting for a batch
	RoutineEventBatched RoutineEventState = "batched" // in the open batch, its Run not started yet
	RoutineEventStarted RoutineEventState = "started" // its batch started a Run
	RoutineEventDropped RoutineEventState = "dropped" // never runs; kept without payload for dedupe and counts
)

// Why an event was dropped.
const (
	RoutineEventDroppedOverflow  = "overflow"
	RoutineEventDroppedPaused    = "paused"
	RoutineEventDroppedRetired   = "retired"
	RoutineEventDroppedExhausted = "exhausted"
)

// Store limits and windows of routine events (SPEC §0a, §4).
const (
	// RoutineEventRetention is how long started and dropped events stay
	// (the dedupe window for producer IDs), and how long batch rows of a
	// retired or deleted routine stay.
	RoutineEventRetention = 7 * 24 * time.Hour
	// RoutineEventActiveRunWindow bounds CountActiveRoutineEventRuns to
	// batches started this recently.
	RoutineEventActiveRunWindow = 24 * time.Hour
	// RoutineEventMaxBatchEvents is the most events one batch holds.
	RoutineEventMaxBatchEvents = 50

	routineEventBatchIDPrefix         = "reb-"
	RoutineEventSubscriptionPageLimit = 500
	routineEventActivityMaxIDs        = 500
)

var (
	ErrRoutineEventBatchNotFound = errors.New("routine event batch not found")
	ErrRoutineEventBatchStarted  = errors.New("routine event batch already started another Run")
	ErrHookNotFound              = errors.New("hook not found")
	ErrHookRevision              = errors.New("hook changed; reload it and try again")
	ErrHookConflict              = errors.New("this routine already has a different hook")
)

// RoutineEventRecord is one event stored for one routine.
type RoutineEventRecord struct {
	Scope         Scope
	ActivationID  string
	Event         RoutineEvent
	State         RoutineEventState
	BatchID       string
	DroppedReason string // overflow | paused | retired | exhausted
	ReceivedAt    time.Time
}

// RoutineEventBatch is the set of events one occurrence Run handles.
type RoutineEventBatch struct {
	Scope        Scope
	ID           string // "reb-" + uuid
	ActivationID string
	EventIDs     []string // ordered oldest first, at most 50
	Dropped      int      // overflow drops since the previous batch
	Test         bool     // all events are test events; real and test events are never mixed
	RunID        string   // set when started
	CreatedAt    time.Time
	StartedAt    *time.Time
}

// Hook holds no secret: cortex derives it from the platform key, the ID and Revision (§11).
type Hook struct {
	ID               string
	Scope            Scope
	ActivationID     string
	Revision         int64 // bumped by rotation; the old secret and path token stop working at once
	PathTokenEnabled bool  // off by default
	CreatedAt        time.Time
	RotatedAt        time.Time
}

// RoutineEventActivity is what the routines menu shows about an event routine.
type RoutineEventActivity struct {
	ActivationID   string     `json:"activationId"`
	LastFiredAt    *time.Time `json:"lastFiredAt,omitempty"`
	FiredLast24h   int        `json:"firedLast24h"`
	FiredLastHour  int        `json:"firedLastHour"`       // started real batches in the last 60 minutes
	NextRunAt      *time.Time `json:"nextRunAt,omitempty"` // only when FiredLastHour ≥ MaxPerHour and Pending > 0: oldest start in the window + 1h
	Pending        int        `json:"pending"`
	DroppedLast24h int        `json:"droppedLast24h"`
}

// RoutineEventSourceKey is one matcher index key.
type RoutineEventSourceKey struct {
	Scope     Scope
	Kind      RoutineEventKind
	SourceRef string
}

// RoutineEventSubscription is one active event routine, listed across scopes
// for hosts (Google sync) and the matcher index rebuild.
type RoutineEventSubscription struct {
	Scope        Scope
	ActivationID string
	Owner        ObjectiveOwner
	CreatedBy    string
	Kind         RoutineEventKind
	SourceRef    string
	Calendar     *RoutineCalendarTiming
	// Channels are the Slack channels or Telegram chats the routine names
	// (E14); each must stay an active endpoint conversation.
	Channels []string
}

// RoutineEventStore keeps routine events, their batches and hooks.
type RoutineEventStore interface {
	ListRoutineEventActivations(ctx context.Context, scope Scope, kind RoutineEventKind, sourceRef string) ([]*RunbookActivation, error)              // active only
	ListRoutineEventSubscriptions(ctx context.Context, kinds []RoutineEventKind, after string, limit int) ([]RoutineEventSubscription, string, error) // active, all scopes, keyset by (scope, id); nil kinds = all
	ListRoutineEventSourceKeysInScope(ctx context.Context, scope Scope) ([]RoutineEventSourceKey, error)                                              // distinct keys of active event routines; index refresh
	CountActiveRoutineEventRuns(ctx context.Context, scope Scope) (int, error)                                                                        // non-terminal Runs of batches started in the last 24h (tenant run cap, §4)
	InsertRoutineEvent(ctx context.Context, record *RoutineEventRecord) (inserted bool, err error)                                                    // ON CONFLICT DO NOTHING; a dropped record stores no payload
	CountPendingRoutineEvents(ctx context.Context, scope Scope, activationID string) (int, error)                                                     // pending + unstarted batched, test included
	CountPendingRoutineEventsInScope(ctx context.Context, scope Scope) (int, error)                                                                   // tenant cap, §3.1
	ListRoutineEventWorkScopes(ctx context.Context) ([]Scope, error)                                                                                  // scopes with pending or batched events (partial pending index)
	ListRoutineEventWork(ctx context.Context, scope Scope, after string, limit int) ([]string, error)                                                 // activation IDs with pending or batched events, keyset after `after`
	ClaimRoutineEventBatch(ctx context.Context, scope Scope, activationID string, maxEvents int, now time.Time) (*RoutineEventBatch, error)
	MarkRoutineEventBatchStarted(ctx context.Context, scope Scope, batchID, runID string, now time.Time) error
	GetRoutineEventBatch(ctx context.Context, scope Scope, batchID string) (*RoutineEventBatch, []*RoutineEventRecord, error)
	GetOpenRoutineEventBatch(ctx context.Context, scope Scope, activationID string) (*RoutineEventBatch, error)
	CountRoutineEventBatchesSince(ctx context.Context, scope Scope, activationID string, since time.Time, test bool) (count int, oldest *time.Time, err error) // started batches; zero since = all
	CountRoutineTestEventsSince(ctx context.Context, scope Scope, activationID string, since time.Time) (int, error)
	DropRoutineEvents(ctx context.Context, scope Scope, activationID string, reason string) (int, error) // pending and unstarted batched; deletes the open batch row in the same transaction
	RoutineEventActivity(ctx context.Context, scope Scope, activationIDs []string, now time.Time) (map[string]RoutineEventActivity, error)
	PruneRoutineEvents(ctx context.Context, before time.Time, limit int) (int, error)                                                                      // started/dropped events received before `before` (now-7d); batch rows only of activations retired or missing before `before`
	CreateHook(ctx context.Context, hook *Hook) (*Hook, error)                                                                                             // idempotent on ID
	GetHook(ctx context.Context, hookID string) (*Hook, error)                                                                                             // global lookup
	UpdateHook(ctx context.Context, scope Scope, hookID string, expectedRevision int64, rotate bool, pathTokenEnabled *bool, now time.Time) (*Hook, error) // CAS; rotate bumps Revision
	PruneHooks(ctx context.Context, createdBefore time.Time) (int, error)                                                                                  // hooks, in every scope, whose activation is retired or does not exist
}

// RoutineEventIndexOf returns the event index columns of an activation: the
// kind and SourceRef of an event Task, empty for everything else.
func RoutineEventIndexOf(activation *RunbookActivation) (RoutineEventKind, string) {
	if activation == nil || activation.Task == nil || activation.Task.Event == nil || activation.Trigger.Kind != runbook.TriggerEvent {
		return "", ""
	}
	return activation.Task.Event.Kind, activation.Task.Event.SourceRef
}

func RoutineEventSubscriptionOf(activation *RunbookActivation) (RoutineEventSubscription, bool) {
	kind, sourceRef := RoutineEventIndexOf(activation)
	if kind == "" {
		return RoutineEventSubscription{}, false
	}
	subscription := RoutineEventSubscription{
		Scope: activation.Scope, ActivationID: activation.ID, Owner: activation.Owner,
		CreatedBy: activation.Task.CreatedBy, Kind: kind, SourceRef: sourceRef,
	}
	if timing := activation.Task.Event.Calendar; timing != nil {
		copied := *timing
		subscription.Calendar = &copied
	}
	subscription.Channels = RoutineEventNamedChannels(activation.Task.Event)
	return subscription, true
}

// PrepareRoutineEventRecord checks a record before insert and fills its
// receive time. Only pending and dropped records can be inserted.
func PrepareRoutineEventRecord(record *RoutineEventRecord) error {
	if record == nil {
		return errors.New("routine event record is required")
	}
	if err := record.Scope.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(record.ActivationID) == "" {
		return errors.New("routine event record needs its activation")
	}
	if !ValidRoutineEventID(record.Event.ID) {
		return errors.New("routine event record needs a valid event ID")
	}
	switch record.State {
	case RoutineEventPending:
		if record.DroppedReason != "" {
			return errors.New("a pending routine event has no dropped reason")
		}
	case RoutineEventDropped:
		if !IsValidRoutineEventDropReason(record.DroppedReason) {
			return fmt.Errorf("routine event dropped reason %q is invalid", record.DroppedReason)
		}
	default:
		return fmt.Errorf("a routine event is inserted pending or dropped, not %q", record.State)
	}
	if record.BatchID != "" {
		return errors.New("a new routine event is not in a batch")
	}
	if record.ReceivedAt.IsZero() {
		record.ReceivedAt = time.Now()
	}
	record.ReceivedAt = record.ReceivedAt.UTC()
	if record.Event.OccurredAt.IsZero() {
		record.Event.OccurredAt = record.ReceivedAt
	}
	return nil
}

func IsValidRoutineEventDropReason(reason string) bool {
	switch reason {
	case RoutineEventDroppedOverflow, RoutineEventDroppedPaused, RoutineEventDroppedRetired, RoutineEventDroppedExhausted:
		return true
	}
	return false
}

func NewRoutineEventBatchID() string { return routineEventBatchIDPrefix + uuid.NewString() }

func NormalizeRoutineEventBatchSize(maxEvents int) int {
	if maxEvents <= 0 || maxEvents > RoutineEventMaxBatchEvents {
		return RoutineEventMaxBatchEvents
	}
	return maxEvents
}

func NormalizeRoutineEventLimit(limit, ceiling int) int {
	if limit <= 0 || limit > ceiling {
		return ceiling
	}
	return limit
}

// routineEventSubscriptionCursor is the opaque keyset cursor (scope kind,
// scope ID, activation ID) of ListRoutineEventSubscriptions.
func EncodeRoutineEventSubscriptionCursor(subscription RoutineEventSubscription) string {
	raw := subscription.Scope.Kind + "\x00" + subscription.Scope.ID + "\x00" + subscription.ActivationID
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func DecodeRoutineEventSubscriptionCursor(cursor string) (scopeKind, scopeID, activationID string, err error) {
	if cursor == "" {
		return "", "", "", nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return "", "", "", errors.New("routine event subscription cursor is invalid")
	}
	parts := strings.Split(string(raw), "\x00")
	if len(parts) != 3 {
		return "", "", "", errors.New("routine event subscription cursor is invalid")
	}
	return parts[0], parts[1], parts[2], nil
}

func RoutineEventKindStrings(kinds []RoutineEventKind) ([]string, error) {
	values := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		if !kind.Valid() {
			return nil, fmt.Errorf("unknown routine event kind %q", kind)
		}
		values = append(values, string(kind))
	}
	return values, nil
}

// UniqueRoutineActivationIDs drops blanks and repeats, keeping at most the
// activity limit.
func UniqueRoutineActivationIDs(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	values := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		values = append(values, id)
		if len(values) == routineEventActivityMaxIDs {
			break
		}
	}
	return values
}

// FinishRoutineEventActivity sets NextRunAt when the routine hit its hourly
// limit and still has events waiting.
func FinishRoutineEventActivity(activity *RoutineEventActivity, maxPerHour int, oldestInHour *time.Time) {
	if maxPerHour > 0 && activity.FiredLastHour >= maxPerHour && activity.Pending > 0 && oldestInHour != nil {
		next := oldestInHour.Add(time.Hour).UTC()
		activity.NextRunAt = &next
	}
}

func PrepareHook(hook *Hook) (*Hook, error) {
	if hook == nil {
		return nil, errors.New("hook is required")
	}
	value := *hook
	if !ValidHookID(value.ID) {
		return nil, errors.New("hook ID is invalid")
	}
	if err := value.Scope.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(value.ActivationID) == "" {
		return nil, errors.New("hook needs its activation")
	}
	if value.Revision < 0 {
		return nil, errors.New("hook revision cannot be negative")
	}
	if value.Revision == 0 {
		value.Revision = 1
	}
	if value.CreatedAt.IsZero() {
		value.CreatedAt = time.Now()
	}
	value.CreatedAt = value.CreatedAt.UTC()
	if value.RotatedAt.IsZero() {
		value.RotatedAt = value.CreatedAt
	}
	value.RotatedAt = value.RotatedAt.UTC()
	return &value, nil
}

func SameHookOwner(a, b *Hook) bool {
	return a.Scope == b.Scope && a.ActivationID == b.ActivationID
}

func ValidateRoutineEventScopeActivation(scope Scope, activationID string) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(activationID) == "" {
		return errors.New("routine activation ID is required")
	}
	return nil
}
