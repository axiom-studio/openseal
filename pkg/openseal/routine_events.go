package openseal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime"
)

// Event routines: types the host uses with the Engine API below.
type (
	RoutineEvent               = runtime.RoutineEvent
	RoutineEventKind           = runtime.RoutineEventKind
	RoutineEventOffer          = runtime.RoutineEventOffer
	RoutineEventTrigger        = runtime.RoutineEventTrigger
	RoutineEventCondition      = runtime.RoutineEventCondition
	RoutineFilterOp            = runtime.RoutineFilterOp
	RoutineCalendarTiming      = runtime.RoutineCalendarTiming
	RoutineWebhook             = runtime.RoutineWebhook
	RoutinePausedReason        = runtime.RoutinePausedReason
	RoutineEventSourceCatalog  = runtime.RoutineEventSourceCatalog
	RoutineEventSourceOption   = runtime.RoutineEventSourceOption
	RoutineIndexPublisher      = runtime.RoutineIndexPublisher
	RoutineEventStats          = runtime.RoutineEventStats
	RoutineEventBatchView      = runtime.RoutineEventBatchView
	RoutineEventView           = runtime.RoutineEventView
	RoutineEventActivity       = runtime.RoutineEventActivity
	RoutineEventSubscription   = runtime.RoutineEventSubscription
	RoutineEventDispatchResult = runtime.RoutineEventDispatchResult
	RoutineEventFileKeeper     = runtime.RoutineEventFileKeeper
	Hook                       = runtime.Hook
)

const (
	RoutineEventSlackMessage    = runtime.RoutineEventSlackMessage
	RoutineEventTelegramMessage = runtime.RoutineEventTelegramMessage
	RoutineEventGitHub          = runtime.RoutineEventGitHub
	RoutineEventGmailMessage    = runtime.RoutineEventGmailMessage
	RoutineEventCalendar        = runtime.RoutineEventCalendar
	RoutineEventWebhook         = runtime.RoutineEventWebhook

	RoutinePausedNeedsSignIn   = runtime.RoutinePausedNeedsSignIn
	RoutinePausedSourceRemoved = runtime.RoutinePausedSourceRemoved
	RoutinePausedUnverified    = runtime.RoutinePausedUnverified

	RoutineEventSourceNeedsSignIn            = runtime.RoutineEventSourceNeedsSignIn
	RoutineEventSourceNotConfigured          = runtime.RoutineEventSourceNotConfigured
	RoutineEventSourceSharedSignInNotAllowed = runtime.RoutineEventSourceSharedSignInNotAllowed

	RoutineEventBatchContextKey  = runtime.RoutineEventBatchContextKey
	RoutineEventCountContextKey  = runtime.RoutineEventCountContextKey
	RoutineEventKindContextKey   = runtime.RoutineEventKindContextKey
	RoutineEventHeaderContextKey = runtime.RoutineEventHeaderContextKey
	RoutineEventTestContextKey   = runtime.RoutineEventTestContextKey

	RoutineEventTenantRunCap     = runtime.RoutineEventTenantRunCap
	RoutineEventDispatchInterval = runtime.RoutineEventDispatchInterval
	RoutineEventPayloadMaxBytes  = runtime.RoutineEventPayloadMaxBytes
)

var (
	ErrHookNotFound               = runtime.ErrHookNotFound
	ErrHookRevision               = runtime.ErrHookRevision
	ErrRoutineNotCreator          = runtime.ErrRoutineNotCreator
	ErrRoutineNotEventRoutine     = runtime.ErrRoutineNotEventRoutine
	ErrRoutineTestEventLimit      = runtime.ErrRoutineTestEventLimit
	ErrRoutineEventsWaitingIsFull = runtime.ErrRoutineEventsWaitingIsFull
	ErrRoutineEventBatchNotFound  = runtime.ErrRoutineEventBatchNotFound
	ErrRoutineEventsUnavailable   = runtime.ErrRoutineEventsUnavailable
)

var (
	RoutineGoogleSourceRef   = runtime.RoutineGoogleSourceRef
	RoutineCallbackSourceRef = runtime.RoutineCallbackSourceRef
	HookSourceRef            = runtime.HookSourceRef
	HookID                   = runtime.HookID
)

// The matcher is what actions and ingress offer events to.
var _ runtime.RoutineEventOfferer = (*runtime.RoutineEventMatcher)(nil)

// routineEventRuntime is the Engine's matcher and dispatcher plus what the
// host set for the routine actions.
type routineEventRuntime struct {
	matcher        *runtime.RoutineEventMatcher
	dispatcher     *runtime.RoutineEventDispatcher
	tenantRunCap   int
	sources        runtime.RoutineEventSourceCatalog
	hooksAvailable bool
	validators     []*runtime.RunbookActionValidator
	dispatchers    []*runtime.RunbookActionDispatcher
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
	matcher := runtime.NewRoutineEventMatcher(e.store)
	dispatcher := runtime.NewRoutineEventDispatcher(e.store, matcher)
	dispatcher.SetAcceptedRunExecutionPreparer(e)
	dispatcher.TenantRunCap = e.routineEvents.tenantRunCap
	dispatcher.SourceChecker = runtime.NewRoutineEventSourceChecker(e.store)
	e.routineEvents.matcher = matcher
	e.routineEvents.dispatcher = dispatcher
}

// attachRoutineEventValidator gives the routine actions the matcher and the host's
// sources, and keeps them so later host setters reach them.
func (e *Engine) attachRoutineEventValidator(validator *runtime.RunbookActionValidator) {
	e.routineEvents.validators = append(e.routineEvents.validators, validator)
	validator.SetRoutineEvents(e.routineEvents.matcher, e.routineEvents.sources, e.routineEvents.hooksAvailable)
}

func (e *Engine) attachRoutineEventDispatcher(dispatcher *runtime.RunbookActionDispatcher) {
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
func (e *Engine) SetRoutineEventSourceCatalog(catalog runtime.RoutineEventSourceCatalog) {
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
func (e *Engine) SetRoutineIndexPublisher(publisher runtime.RoutineIndexPublisher) {
	e.routineEvents.matcher.SetRoutineIndexPublisher(publisher)
}

// OfferRoutineEvent hands one event from a host source (Google, webhooks) to
// the routines listening on its source. It never starts a Run.
func (e *Engine) OfferRoutineEvent(ctx context.Context, event *runtime.RoutineEvent) (runtime.RoutineEventOffer, error) {
	return e.routineEvents.matcher.OfferRoutineEvent(ctx, event)
}

// SendRoutineTestEvent stores one test event for a routine. Only the
// routine's creator may send it (ErrRoutineNotCreator otherwise).
func (e *Engine) SendRoutineTestEvent(ctx context.Context, scope runtime.Scope, activationID, testBy string, payload json.RawMessage) (runtime.RoutineEventOffer, error) {
	return e.routineEvents.matcher.OfferRoutineTestEvent(ctx, scope, activationID, testBy, payload)
}

// RoutineEventListening reports whether any active event routine listens to
// this source. It is a map lookup, for host producers to call before they
// build an event.
func (e *Engine) RoutineEventListening(scope runtime.Scope, kind runtime.RoutineEventKind, sourceRef string) bool {
	return e.routineEvents.matcher.Listening(scope, kind, sourceRef)
}

// RefreshRoutineIndex rereads one scope's index after another process said
// its routines changed.
func (e *Engine) RefreshRoutineIndex(ctx context.Context, scope runtime.Scope) error {
	return e.routineEvents.matcher.RefreshRoutineIndex(ctx, scope)
}

// RebuildRoutineIndex rereads the whole index, for example when the host's
// change signal reconnects.
func (e *Engine) RebuildRoutineIndex(ctx context.Context) error {
	return e.routineEvents.matcher.RebuildRoutineIndex(ctx)
}

// RoutineEventStats returns the matcher's and dispatcher's counters.
func (e *Engine) RoutineEventStats() runtime.RoutineEventStats {
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
func (e *Engine) ReconcileAllRoutineEvents(ctx context.Context, limitPerScope int) (*runtime.RoutineEventDispatchResult, error) {
	return e.routineEvents.dispatcher.ReconcileAll(ctx, limitPerScope)
}

// ResolveHook finds a webhook routine by its hook ID. ErrHookNotFound when
// the hook is missing, its routine is retired or gone, or the routine no
// longer uses it. A paused routine is returned; the host answers without
// offering.
func (e *Engine) ResolveHook(ctx context.Context, hookID string) (*runtime.Hook, *runtime.RunbookActivation, error) {
	hookID = strings.TrimSpace(hookID)
	if hookID == "" {
		return nil, nil, runtime.ErrHookNotFound
	}
	hook, err := e.store.GetHook(ctx, hookID)
	if err != nil {
		return nil, nil, err
	}
	if hook == nil {
		return nil, nil, runtime.ErrHookNotFound
	}
	activation, err := e.store.GetRunbookActivation(ctx, hook.Scope, hook.ActivationID)
	if err != nil {
		return nil, nil, err
	}
	if activation == nil || activation.Status == runtime.RunbookActivationRetired || activation.Task == nil ||
		activation.Task.Event == nil || activation.Task.Event.Kind != runtime.RoutineEventWebhook ||
		activation.Task.Event.Webhook == nil || activation.Task.Event.Webhook.HookID != hook.ID {
		return nil, nil, runtime.ErrHookNotFound
	}
	return hook, activation, nil
}

// UpdateHook rotates a hook's secret (rotate) or switches its path token.
// expectedRevision guards against a concurrent change (ErrHookRevision).
// The caller checks that the acting person created the routine (E17).
func (e *Engine) UpdateHook(ctx context.Context, scope runtime.Scope, hookID string, expectedRevision int64, rotate bool, pathTokenEnabled *bool) (*runtime.Hook, error) {
	return e.store.UpdateHook(ctx, scope, strings.TrimSpace(hookID), expectedRevision, rotate, pathTokenEnabled, time.Now().UTC())
}

// GetRoutineEventBatchView returns the facts of the events that started one
// occurrence, never their payloads.
func (e *Engine) GetRoutineEventBatchView(ctx context.Context, scope runtime.Scope, activationID, batchID string) (*runtime.RoutineEventBatchView, error) {
	batch, records, err := e.store.GetRoutineEventBatch(ctx, scope, strings.TrimSpace(batchID))
	if err != nil {
		if errors.Is(err, runtime.ErrRoutineEventBatchNotFound) {
			return nil, runtime.ErrRoutineEventBatchNotFound
		}
		return nil, err
	}
	if batch == nil || batch.ActivationID != strings.TrimSpace(activationID) {
		return nil, runtime.ErrRoutineEventBatchNotFound
	}
	return runtime.NewRoutineEventBatchView(batch, records), nil
}

// ListRoutineEventSubscriptions lists active event routines across scopes,
// for host sources such as the Google sync loop.
func (e *Engine) ListRoutineEventSubscriptions(ctx context.Context, kinds []runtime.RoutineEventKind, after string, limit int) ([]runtime.RoutineEventSubscription, string, error) {
	return e.store.ListRoutineEventSubscriptions(ctx, kinds, after, limit)
}

// PauseRoutinesForSource pauses every active routine of the scope on this
// source, for example with needs_sign_in when a Google sign-in is revoked.
func (e *Engine) PauseRoutinesForSource(ctx context.Context, scope runtime.Scope, kind runtime.RoutineEventKind, sourceRef string, reason runtime.RoutinePausedReason) (int, error) {
	return e.routineEvents.dispatcher.PauseRoutinesForSource(ctx, scope, kind, sourceRef, reason)
}

// PauseRoutine pauses one event routine for a system reason and drops its
// waiting events.
func (e *Engine) PauseRoutine(ctx context.Context, scope runtime.Scope, activationID string, reason runtime.RoutinePausedReason) error {
	_, err := e.routineEvents.dispatcher.PauseRoutine(ctx, scope, activationID, reason)
	return err
}

// RoutineEventActivity returns what the routines menu shows about event
// routines: last fired, counts and waiting events.
func (e *Engine) RoutineEventActivity(ctx context.Context, scope runtime.Scope, activationIDs []string) (map[string]runtime.RoutineEventActivity, error) {
	return e.store.RoutineEventActivity(ctx, scope, activationIDs, time.Now().UTC())
}
