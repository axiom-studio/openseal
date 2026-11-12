package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/runtime/externalconversation"
	"github.com/google/uuid"
)

// Caps of waiting events and test events (SPEC §0a, §3.1).
const (
	// RoutineEventMaxPendingPerRoutine is the most events that wait for one
	// routine; later ones are stored as dropped rows without payload.
	RoutineEventMaxPendingPerRoutine = 500
	// RoutineEventMaxPendingPerTenant is the same cap for a whole scope.
	RoutineEventMaxPendingPerTenant = 20_000
	// RoutineTestEventsPerHour is the most test events one routine accepts in
	// a rolling hour.
	RoutineTestEventsPerHour = 5
)

var (
	ErrRoutineNotEventRoutine     = errors.New("this routine does not start on an event")
	ErrRoutineTestEventLimit      = errors.New("this routine already got 5 test events in the last hour")
	ErrRoutineEventsWaitingIsFull = errors.New("too many events are already waiting for this routine")
)

type routineEventMatcherStore interface {
	models.RoutineEventStore
	GetRunbookActivation(context.Context, models.Scope, string) (*models.RunbookActivation, error)
}

type routineCallbackRegistrationReader interface {
	GetCallbackRegistration(ctx context.Context, scope models.Scope, id string) (*models.CallbackRegistration, error)
}

// RoutineEventMatcher matches events to event routines and stores them for
// the dispatcher. It never starts a Run or calls a provider. An event whose
// (scope, kind, SourceRef) has no active routine costs one map lookup.
type RoutineEventMatcher struct {
	store      routineEventMatcherStore
	index      routineEventIndexHolder
	stats      routineEventStatsCounters
	nudge      chan struct{}
	publisher  RoutineIndexPublisher
	now        func() time.Time
	unregister func()
}

var _ externalconversation.RoutineEventOfferer = (*RoutineEventMatcher)(nil)

// NewRoutineEventMatcher builds a matcher with an empty index and registers it
// for activation writes made through RunbookActivationService on this store.
// Call RebuildRoutineIndex once before serving.
//
// Activation writes refresh the index only when they go through the very
// same store value, so the host must pass the store its
// RunbookActivationService writes to (not a wrapper of it). A store that
// cannot be a map key could never be followed, so it panics here.
func NewRoutineEventMatcher(store routineEventMatcherStore, opts ...models.ServiceOption) *RoutineEventMatcher {
	if _, ok := routineActivationObserverKey(store); !ok {
		panic("routine event matcher needs a comparable store (a pointer) so activation writes refresh its index")
	}
	matcher := &RoutineEventMatcher{store: store, nudge: make(chan struct{}, 1), now: models.ClockFrom(opts)}
	matcher.index.current.Store(&routineEventIndex{scopes: map[models.Scope]routineEventScopeKeys{}})
	matcher.unregister = registerRoutineActivationObserver(store, matcher.activationWritten)
	return matcher
}

// Close stops the matcher from following activation writes.
func (m *RoutineEventMatcher) Close() {
	if m != nil && m.unregister != nil {
		m.unregister()
	}
}

// SetRoutineIndexPublisher sets the host's signal to other processes. Nil in
// single-process hosts.
func (m *RoutineEventMatcher) SetRoutineIndexPublisher(publisher RoutineIndexPublisher) {
	if m != nil {
		m.publisher = publisher
	}
}

// Listening reports whether any active event routine of the scope listens to
// this kind and SourceRef. It is a lookup in the in-memory index: no I/O and
// no allocation.
func (m *RoutineEventMatcher) Listening(scope models.Scope, kind models.RoutineEventKind, sourceRef string) bool {
	return m != nil && m.index.listening(scope, kind, sourceRef)
}

// RefreshRoutineIndex rereads one scope's index keys from the store. Hosts
// call it when another process signals that the scope's routines changed.
func (m *RoutineEventMatcher) RefreshRoutineIndex(ctx context.Context, scope models.Scope) error {
	if m == nil || m.store == nil {
		return errors.New("routine event matcher is not configured")
	}
	return m.index.refreshScope(ctx, m.store, scope)
}

// RebuildRoutineIndex rereads the whole index: at start, when the host's
// signal subscription reconnects, and as the dispatcher's backstop.
func (m *RoutineEventMatcher) RebuildRoutineIndex(ctx context.Context) error {
	if m == nil || m.store == nil {
		return errors.New("routine event matcher is not configured")
	}
	return m.index.rebuild(ctx, m.store)
}

// RoutineEventStats returns a snapshot of the counters.
func (m *RoutineEventMatcher) RoutineEventStats() RoutineEventStats {
	if m == nil {
		return RoutineEventStats{}
	}
	return m.stats.snapshot()
}

// Nudges is the channel the dispatcher loop wakes on. It holds at most one
// pending nudge.
func (m *RoutineEventMatcher) Nudges() <-chan struct{} {
	if m == nil {
		return nil
	}
	return m.nudge
}

func (m *RoutineEventMatcher) wake() {
	select {
	case m.nudge <- struct{}{}:
	default:
	}
}

// activationWritten keeps the index in step with a committed activation
// write and tells other processes.
func (m *RoutineEventMatcher) activationWritten(ctx context.Context, previous, next *models.RunbookActivation) {
	if !isRoutineEventActivation(previous) && !isRoutineEventActivation(next) {
		return
	}
	scope := models.Scope{}
	if next != nil {
		scope = next.Scope
	} else {
		scope = previous.Scope
	}
	// A failed refresh is repaired by the next rebuild.
	_ = m.index.refreshScope(ctx, m.store, scope)
	if m.publisher != nil {
		m.publisher.PublishRoutineIndexChange(scope)
	}
}

func isRoutineEventActivation(activation *models.RunbookActivation) bool {
	kind, _ := models.RoutineEventIndexOf(activation)
	return kind != ""
}

// OfferRoutineEvent stores the event for every active routine it matches.
func (m *RoutineEventMatcher) OfferRoutineEvent(ctx context.Context, event *models.RoutineEvent) (externalconversation.RoutineEventOffer, error) {
	if m == nil || event == nil {
		return externalconversation.RoutineEventOffer{}, nil
	}
	m.stats.countIn(event.Kind)
	if !m.index.listening(event.Scope, event.Kind, event.SourceRef) {
		return externalconversation.RoutineEventOffer{}, nil
	}
	started := time.Now()
	offer, err := m.offer(ctx, event)
	m.stats.observeLatency(time.Since(started))
	m.stats.countMatched(event.Kind, offer.Matched, offer.Dropped)
	if offer.Inserted > 0 {
		m.wake()
	}
	return offer, err
}

func (m *RoutineEventMatcher) offer(ctx context.Context, event *models.RoutineEvent) (externalconversation.RoutineEventOffer, error) {
	offer := externalconversation.RoutineEventOffer{}
	if err := event.Validate(); err != nil {
		return offer, err
	}
	if event.Test {
		return offer, errors.New("test events are offered with OfferRoutineTestEvent")
	}
	activations, err := m.store.ListRoutineEventActivations(ctx, event.Scope, event.Kind, event.SourceRef)
	if err != nil {
		return offer, err
	}
	scopePending := -1
	now := m.now().UTC()
	for _, activation := range activations {
		if activation == nil || activation.Status != models.RunbookActivationActive || activation.Task == nil || activation.Task.Event == nil {
			continue
		}
		owned, err := m.ownsSource(ctx, activation, event)
		if err != nil {
			return offer, err
		}
		if !owned || !activation.Task.Event.Matches(event) {
			continue
		}
		offer.Matched++
		full, err := m.waitingIsFull(ctx, activation, &scopePending)
		if err != nil {
			return offer, err
		}
		record := &models.RoutineEventRecord{Scope: event.Scope, ActivationID: activation.ID, Event: *event, State: models.RoutineEventPending, ReceivedAt: now}
		if full {
			record.State, record.DroppedReason = models.RoutineEventDropped, models.RoutineEventDroppedOverflow
			record.Event = models.RoutineEvent{ID: event.ID, OccurredAt: event.OccurredAt}
		}
		inserted, err := m.store.InsertRoutineEvent(ctx, record)
		if err != nil {
			return offer, err
		}
		switch {
		case !inserted:
			offer.Duplicates++
		case full:
			offer.Dropped++
		default:
			offer.Inserted++
			if scopePending >= 0 {
				scopePending++
			}
		}
	}
	return offer, nil
}

// waitingIsFull applies the per-routine and per-scope caps on waiting
// events. The scope count is read once per offer.
func (m *RoutineEventMatcher) waitingIsFull(ctx context.Context, activation *models.RunbookActivation, scopePending *int) (bool, error) {
	pending, err := m.store.CountPendingRoutineEvents(ctx, activation.Scope, activation.ID)
	if err != nil {
		return false, err
	}
	if pending >= RoutineEventMaxPendingPerRoutine {
		return true, nil
	}
	if *scopePending < 0 {
		count, err := m.store.CountPendingRoutineEventsInScope(ctx, activation.Scope)
		if err != nil {
			return false, err
		}
		*scopePending = count
	}
	return *scopePending >= RoutineEventMaxPendingPerTenant, nil
}

// ownsSource is the owner check of SPEC §1.3: the routine may only hear
// sources its owner or creator holds.
func (m *RoutineEventMatcher) ownsSource(ctx context.Context, activation *models.RunbookActivation, event *models.RoutineEvent) (bool, error) {
	trigger := activation.Task.Event
	if activation.Scope != event.Scope || trigger.SourceRef != event.SourceRef {
		return false, nil
	}
	switch trigger.Kind {
	case models.RoutineEventSlackMessage, models.RoutineEventTelegramMessage:
		// The SourceRef hashes the deployment, binding, adapter and
		// installation. create_task builds it only from the routine owner's
		// own connection, and the hourly SourceChecker (given the owner)
		// pauses the routine when that connection is gone, so an equal
		// SourceRef is the owner check (SPEC §1.3, Review log).
		return true, nil
	case models.RoutineEventGitHub:
		reader, ok := m.store.(routineCallbackRegistrationReader)
		if !ok {
			return false, nil
		}
		registrationID := strings.TrimPrefix(trigger.SourceRef, models.RoutineCallbackSourcePrefix)
		registration, err := reader.GetCallbackRegistration(ctx, event.Scope, registrationID)
		if err != nil {
			return false, err
		}
		return registration != nil && registration.Scope == event.Scope && registration.Status == models.CallbackRegistrationActive &&
			registration.DeploymentID == activation.Owner.ID, nil
	case models.RoutineEventGmailMessage, models.RoutineEventCalendar:
		rest := strings.TrimPrefix(trigger.SourceRef, models.RoutineGoogleSourcePrefix)
		userID, _, found := strings.Cut(rest, ":")
		return found && userID != "" && userID == activation.Task.CreatedBy, nil
	case models.RoutineEventWebhook:
		if trigger.Webhook == nil || models.HookSourceRef(trigger.Webhook.HookID) != trigger.SourceRef {
			return false, nil
		}
		hook, err := m.store.GetHook(ctx, trigger.Webhook.HookID)
		if err != nil {
			return false, err
		}
		return hook != nil && hook.Scope == activation.Scope && hook.ActivationID == activation.ID, nil
	}
	return false, nil
}

// OfferRoutineTestEvent stores one test event for one routine and returns
// its event ID. Only the routine's creator may send it; the filter is skipped
// for it.
func (m *RoutineEventMatcher) OfferRoutineTestEvent(ctx context.Context, scope models.Scope, activationID, testBy string, payload json.RawMessage) (string, externalconversation.RoutineEventOffer, error) {
	offer := externalconversation.RoutineEventOffer{}
	if m == nil || m.store == nil {
		return "", offer, errors.New("routine event matcher is not configured")
	}
	if err := models.ValidateRoutineEventScopeActivation(scope, activationID); err != nil {
		return "", offer, err
	}
	activation, err := m.store.GetRunbookActivation(ctx, scope, strings.TrimSpace(activationID))
	if err != nil {
		return "", offer, err
	}
	if activation == nil {
		return "", offer, models.ErrRunbookActivationNotFound
	}
	if activation.Task == nil || activation.Task.Event == nil || activation.Trigger.Kind != runbook.TriggerEvent {
		return "", offer, ErrRoutineNotEventRoutine
	}
	if activation.Status != models.RunbookActivationActive {
		return "", offer, ErrRunbookActivationInactive
	}
	testBy = strings.TrimSpace(testBy)
	if testBy == "" || testBy != activation.Task.CreatedBy {
		return "", offer, ErrRoutineNotCreator
	}
	now := m.now().UTC()
	recent, err := m.store.CountRoutineTestEventsSince(ctx, scope, activation.ID, now.Add(-time.Hour))
	if err != nil {
		return "", offer, err
	}
	if recent >= RoutineTestEventsPerHour {
		return "", offer, ErrRoutineTestEventLimit
	}
	scopePending := -1
	full, err := m.waitingIsFull(ctx, activation, &scopePending)
	if err != nil {
		return "", offer, err
	}
	if full {
		return "", offer, ErrRoutineEventsWaitingIsFull
	}
	if len(payload) == 0 {
		payload = json.RawMessage(`{"test":true}`)
	}
	event := models.RoutineEvent{
		Scope: scope, ID: "test:" + uuid.NewString(), Kind: activation.Task.Event.Kind, SourceRef: activation.Task.Event.SourceRef,
		OccurredAt: now, Summary: "Test event", Payload: append(json.RawMessage(nil), payload...), Test: true, TestBy: testBy,
	}
	if err := event.Validate(); err != nil {
		return "", offer, err
	}
	inserted, err := m.store.InsertRoutineEvent(ctx, &models.RoutineEventRecord{Scope: scope, ActivationID: activation.ID, Event: event, State: models.RoutineEventPending, ReceivedAt: now})
	if err != nil {
		return "", offer, err
	}
	offer.Matched = 1
	if inserted {
		offer.Inserted = 1
		m.wake()
	} else {
		offer.Duplicates = 1
	}
	return event.ID, offer, nil
}
