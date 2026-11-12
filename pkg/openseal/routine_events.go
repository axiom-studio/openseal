package openseal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/externalconversation"
	"github.com/axiom-studio/openseal/pkg/runtime/routine"
)

// Event routines: types the host uses with the Engine API below.
type (
	RoutineEvent               = models.RoutineEvent
	RoutineEventKind           = models.RoutineEventKind
	RoutineEventOffer          = externalconversation.RoutineEventOffer
	RoutineEventTrigger        = models.RoutineEventTrigger
	RoutineEventCondition      = models.RoutineEventCondition
	RoutineFilterOp            = models.RoutineFilterOp
	RoutineCalendarTiming      = models.RoutineCalendarTiming
	RoutineWebhook             = models.RoutineWebhook
	RoutinePausedReason        = models.RoutinePausedReason
	RoutineEventSourceCatalog  = routine.RoutineEventSourceCatalog
	RoutineEventSourceOption   = routine.RoutineEventSourceOption
	RoutineIndexPublisher      = routine.RoutineIndexPublisher
	RoutineEventStats          = routine.RoutineEventStats
	RoutineEventBatchView      = routine.RoutineEventBatchView
	RoutineEventView           = routine.RoutineEventView
	RoutineEventActivity       = models.RoutineEventActivity
	RoutineEventSubscription   = models.RoutineEventSubscription
	RoutineEventDispatchResult = routine.RoutineEventDispatchResult
	RoutineEventFileKeeper     = routine.RoutineEventFileKeeper
	Hook                       = models.Hook

	RoutineSourceUnavailableError = routine.RoutineSourceUnavailableError
)

const (
	RoutineEventSlackMessage    = models.RoutineEventSlackMessage
	RoutineEventTelegramMessage = models.RoutineEventTelegramMessage
	RoutineEventGitHub          = models.RoutineEventGitHub
	RoutineEventGmailMessage    = models.RoutineEventGmailMessage
	RoutineEventCalendar        = models.RoutineEventCalendar
	RoutineEventWebhook         = models.RoutineEventWebhook

	RoutinePausedNeedsSignIn   = models.RoutinePausedNeedsSignIn
	RoutinePausedSourceRemoved = models.RoutinePausedSourceRemoved
	RoutinePausedUnverified    = models.RoutinePausedUnverified

	RoutineEventSourceNeedsSignIn            = routine.RoutineEventSourceNeedsSignIn
	RoutineEventSourceNotConfigured          = routine.RoutineEventSourceNotConfigured
	RoutineEventSourceSharedSignInNotAllowed = routine.RoutineEventSourceSharedSignInNotAllowed

	RoutineEventBatchContextKey  = models.RoutineEventBatchContextKey
	RoutineEventCountContextKey  = models.RoutineEventCountContextKey
	RoutineEventKindContextKey   = models.RoutineEventKindContextKey
	RoutineEventHeaderContextKey = models.RoutineEventHeaderContextKey
	RoutineEventTestContextKey   = models.RoutineEventTestContextKey

	RoutineEventTenantRunCap     = routine.RoutineEventTenantRunCap
	RoutineEventDispatchInterval = routine.RoutineEventDispatchInterval
	RoutineEventPayloadMaxBytes  = models.RoutineEventPayloadMaxBytes
)

var (
	ErrHookNotFound               = models.ErrHookNotFound
	ErrHookRevision               = models.ErrHookRevision
	ErrRoutineNotCreator          = routine.ErrRoutineNotCreator
	ErrRoutineNotEventRoutine     = routine.ErrRoutineNotEventRoutine
	ErrRoutineTestEventLimit      = routine.ErrRoutineTestEventLimit
	ErrRoutineEventsWaitingIsFull = routine.ErrRoutineEventsWaitingIsFull
	ErrRoutineEventBatchNotFound  = models.ErrRoutineEventBatchNotFound
	ErrRoutineEventsUnavailable   = routine.ErrRoutineEventsUnavailable
	ErrRoutineSourceUnavailable   = routine.ErrRoutineSourceUnavailable
)

var (
	RoutineGoogleSourceRef   = models.RoutineGoogleSourceRef
	RoutineCallbackSourceRef = models.RoutineCallbackSourceRef
	HookSourceRef            = models.HookSourceRef
	HookID                   = models.HookID
)

// The matcher is what actions and ingress offer events to.
var _ externalconversation.RoutineEventOfferer = (*routine.RoutineEventMatcher)(nil)

// routineEventRuntime is the Engine's matcher and dispatcher plus what the
// host set for the routine actions.
type routineEventRuntime struct {
	matcher        *routine.RoutineEventMatcher
	dispatcher     *routine.RoutineEventDispatcher
	tenantRunCap   int
	sources        routine.RoutineEventSourceCatalog
	hooksAvailable bool
	validators     []*routine.RunbookActionValidator
	dispatchers    []*routine.RunbookActionDispatcher
}

// WithRoutineEventTenantRunCap sets how many event Runs may be unfinished in
// one scope at once (default 10); further batches wait.
func WithRoutineEventTenantRunCap(n int) Option {
	return func(e *Engine) error {
		if n < 1 || n > 1000 {
			return fmt.Errorf("routine event tenant run cap must be between 1 and 1000")
		}
		e.routineEvents.tenantRunCap = n
		return nil
	}
}

// buildRoutineEvents makes the matcher and dispatcher on the Engine's own
// store. The matcher follows activation writes through that exact store
// value, so it must not be a wrapper of it.
func (e *Engine) buildRoutineEvents() {
	matcher := routine.NewRoutineEventMatcher(e.store)
	dispatcher := routine.NewRoutineEventDispatcher(e.store, matcher)
	dispatcher.SetAcceptedRunExecutionPreparer(e)
	dispatcher.TenantRunCap = e.routineEvents.tenantRunCap
	dispatcher.SourceChecker = routine.NewRoutineEventSourceChecker(e.store)
	e.routineEvents.matcher = matcher
	e.routineEvents.dispatcher = dispatcher
}

// attachRoutineEventValidator gives the routine actions the matcher and the host's
// sources, and keeps them so later host setters reach them.
func (e *Engine) attachRoutineEventValidator(validator *routine.RunbookActionValidator) {
	e.routineEvents.validators = append(e.routineEvents.validators, validator)
	validator.SetRoutineEvents(e.routineEvents.matcher, e.routineEvents.sources, e.routineEvents.hooksAvailable)
}

func (e *Engine) attachRoutineEventDispatcher(dispatcher *routine.RunbookActionDispatcher) {
	e.routineEvents.dispatchers = append(e.routineEvents.dispatchers, dispatcher)
	dispatcher.SetRoutineEvents(e.routineEvents.matcher, e.routineEvents.sources, e.routineEvents.hooksAvailable)
}

func (e *Engine) applyRoutineEvents() {
	for _, validator := range e.routineEvents.validators {
		validator.SetRoutineEvents(e.routineEvents.matcher, e.routineEvents.sources, e.routineEvents.hooksAvailable)
	}
	for _, dispatcher := range e.routineEvents.dispatchers {
		dispatcher.SetRoutineEvents(e.routineEvents.matcher, e.routineEvents.sources, e.routineEvents.hooksAvailable)
	}
}

// SetRoutineEventSourceCatalog sets the host's own sources (Google) for
// event_sources and create_task. Call it before Start.
func (e *Engine) SetRoutineEventSourceCatalog(catalog routine.RoutineEventSourceCatalog) {
	e.routineEvents.sources = catalog
	e.applyRoutineEvents()
}

// SetHooksAvailable says whether the host can serve webhook routines (its
// platform hook key is configured). Call it before Start.
func (e *Engine) SetHooksAvailable(available bool) {
	e.routineEvents.hooksAvailable = available
	e.applyRoutineEvents()
}

// SetRoutineIndexPublisher sets the signal that tells other processes a
// scope's event routines changed. Call it before Start.
func (e *Engine) SetRoutineIndexPublisher(publisher routine.RoutineIndexPublisher) {
	e.routineEvents.matcher.SetRoutineIndexPublisher(publisher)
}

// OfferRoutineEvent hands one event from a host source (Google, webhooks) to
// the routines listening on its source. It never starts a Run.
func (e *Engine) OfferRoutineEvent(ctx context.Context, event *models.RoutineEvent) (externalconversation.RoutineEventOffer, error) {
	return e.routineEvents.matcher.OfferRoutineEvent(ctx, event)
}

// SendRoutineTestEvent stores one test event for a routine and returns its
// event ID. Only the routine's creator may send it (ErrRoutineNotCreator
// otherwise).
func (e *Engine) SendRoutineTestEvent(ctx context.Context, scope models.Scope, activationID, testBy string, payload json.RawMessage) (string, externalconversation.RoutineEventOffer, error) {
	return e.routineEvents.matcher.OfferRoutineTestEvent(ctx, scope, activationID, testBy, payload)
}

// RoutineEventListening reports whether any active event routine listens to
// this source. It is a map lookup, for host producers to call before they
// build an event.
func (e *Engine) RoutineEventListening(scope models.Scope, kind models.RoutineEventKind, sourceRef string) bool {
	return e.routineEvents.matcher.Listening(scope, kind, sourceRef)
}

// RefreshRoutineIndex rereads one scope's index after another process said
// its routines changed.
func (e *Engine) RefreshRoutineIndex(ctx context.Context, scope models.Scope) error {
	return e.routineEvents.matcher.RefreshRoutineIndex(ctx, scope)
}

// RebuildRoutineIndex rereads the whole index, for example when the host's
// change signal reconnects.
func (e *Engine) RebuildRoutineIndex(ctx context.Context) error {
	return e.routineEvents.matcher.RebuildRoutineIndex(ctx)
}

// RoutineEventStats returns the matcher's and dispatcher's counters.
func (e *Engine) RoutineEventStats() routine.RoutineEventStats {
	return e.routineEvents.matcher.RoutineEventStats()
}

// RunRoutineEventDispatcher runs the dispatcher loop until ctx ends. It wakes
// on new events and every interval. Only a process for which leads returns
// true (nil means always) starts Runs; every process keeps its index fresh.
func (e *Engine) RunRoutineEventDispatcher(ctx context.Context, interval time.Duration, leads func() bool) {
	err := e.routineEvents.dispatcher.Run(ctx, interval, leads)
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) && e.logger != nil {
		e.logger.Warnw("routine event dispatcher stopped", "error", err)
	}
}

// ReconcileAllRoutineEvents runs one dispatcher pass over every scope.
func (e *Engine) ReconcileAllRoutineEvents(ctx context.Context, limitPerScope int) (*routine.RoutineEventDispatchResult, error) {
	return e.routineEvents.dispatcher.ReconcileAll(ctx, limitPerScope)
}

// ResolveHook finds a webhook routine by its hook ID. ErrHookNotFound when
// the hook is missing, its routine is retired or gone, or the routine no
// longer uses it. A paused routine is returned; the host answers without
// offering.
func (e *Engine) ResolveHook(ctx context.Context, hookID string) (*models.Hook, *models.RunbookActivation, error) {
	hookID = strings.TrimSpace(hookID)
	if hookID == "" {
		return nil, nil, models.ErrHookNotFound
	}
	hook, err := e.store.GetHook(ctx, hookID)
	if err != nil {
		return nil, nil, err
	}
	if hook == nil {
		return nil, nil, models.ErrHookNotFound
	}
	activation, err := e.store.GetRunbookActivation(ctx, hook.Scope, hook.ActivationID)
	if err != nil {
		return nil, nil, err
	}
	if activation == nil || activation.Status == models.RunbookActivationRetired || activation.Task == nil ||
		activation.Task.Event == nil || activation.Task.Event.Kind != models.RoutineEventWebhook ||
		activation.Task.Event.Webhook == nil || activation.Task.Event.Webhook.HookID != hook.ID {
		return nil, nil, models.ErrHookNotFound
	}
	return hook, activation, nil
}

// UpdateHook rotates a hook's secret (rotate) or switches its path token.
// expectedRevision guards against a concurrent change (ErrHookRevision).
// The caller checks that the acting person created the routine (E17).
func (e *Engine) UpdateHook(ctx context.Context, scope models.Scope, hookID string, expectedRevision int64, rotate bool, pathTokenEnabled *bool) (*models.Hook, error) {
	return e.store.UpdateHook(ctx, scope, strings.TrimSpace(hookID), expectedRevision, rotate, pathTokenEnabled, time.Now().UTC())
}

// GetRoutineEventBatchView returns the facts of the events that started one
// occurrence, never their payloads.
func (e *Engine) GetRoutineEventBatchView(ctx context.Context, scope models.Scope, activationID, batchID string) (*routine.RoutineEventBatchView, error) {
	batch, records, err := e.store.GetRoutineEventBatch(ctx, scope, strings.TrimSpace(batchID))
	if err != nil {
		if errors.Is(err, models.ErrRoutineEventBatchNotFound) {
			return nil, models.ErrRoutineEventBatchNotFound
		}
		return nil, err
	}
	if batch == nil || batch.ActivationID != strings.TrimSpace(activationID) {
		return nil, models.ErrRoutineEventBatchNotFound
	}
	return routine.NewRoutineEventBatchView(batch, records), nil
}

// ListRoutineEventSubscriptions lists active event routines across scopes,
// for host sources such as the Google sync loop.
func (e *Engine) ListRoutineEventSubscriptions(ctx context.Context, kinds []models.RoutineEventKind, after string, limit int) ([]models.RoutineEventSubscription, string, error) {
	return e.store.ListRoutineEventSubscriptions(ctx, kinds, after, limit)
}

// PauseRoutinesForSource pauses every active routine of the scope on this
// source, for example with needs_sign_in when a Google sign-in is revoked.
func (e *Engine) PauseRoutinesForSource(ctx context.Context, scope models.Scope, kind models.RoutineEventKind, sourceRef string, reason models.RoutinePausedReason) (int, error) {
	return e.routineEvents.dispatcher.PauseRoutinesForSource(ctx, scope, kind, sourceRef, reason)
}

// PauseRoutine pauses one event routine for a system reason. It only writes
// the status and nudges the dispatcher, which drops the waiting events on
// its next pass (the close-out).
func (e *Engine) PauseRoutine(ctx context.Context, scope models.Scope, activationID string, reason models.RoutinePausedReason) error {
	_, err := e.routineEvents.dispatcher.PauseRoutine(ctx, scope, activationID, reason)
	return err
}

// RoutineEventActivity returns what the routines menu shows about event
// routines: last fired, counts and waiting events.
func (e *Engine) RoutineEventActivity(ctx context.Context, scope models.Scope, activationIDs []string) (map[string]models.RoutineEventActivity, error) {
	return e.store.RoutineEventActivity(ctx, scope, activationIDs, time.Now().UTC())
}
