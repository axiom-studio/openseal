package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
)

// Dispatcher limits and intervals (SPEC §4).
const (
	// RoutineEventTenantRunCap is the default most event Runs that are not
	// finished in one scope at once; further batches wait.
	RoutineEventTenantRunCap = 10
	// RoutineEventDispatchInterval is the default loop tick.
	RoutineEventDispatchInterval = 10 * time.Second
	// RoutineEventHeaderMaxBytes bounds the routineEventHeader context value.
	RoutineEventHeaderMaxBytes = 200

	routineEventSourceCheckInterval = time.Hour
	routineEventPruneInterval       = 24 * time.Hour
	routineEventPruneBatch          = 10_000
	routineEventPruneRounds         = 20
	routineHookPruneGrace           = time.Hour
	routineEventIdempotencyPrefix   = "routine-event:"
	routineEventDispatchWorkLimit   = 200
	// routineEventMinPassGap is the least time between two dispatcher
	// passes, so a stream of matched events coalesces into one pass a second.
	routineEventMinPassGap = time.Second
)

// RoutineEventSourceChecker tells the dispatcher whether a routine's
// conversation or callback source, and each channel or chat it names, still
// exists. Nil skips the check.
type RoutineEventSourceChecker interface {
	SourceExists(ctx context.Context, subscription RoutineEventSubscription) (bool, error)
}

// RoutineEventDispatchResult counts what one dispatcher pass did.
type RoutineEventDispatchResult struct {
	Scopes       int `json:"scopes"`
	Examined     int `json:"examined"`
	Started      int `json:"started"`
	TestStarted  int `json:"testStarted"`
	Replayed     int `json:"replayed"`
	Waiting      int `json:"waiting"`
	RateLimited  int `json:"rateLimited"`
	TenantCapped int `json:"tenantCapped"`
	Retired      int `json:"retired"`
	Paused       int `json:"paused"`
	Dropped      int `json:"dropped"`
	// Failed counts routines whose dispatch failed this pass; the pass goes
	// on with the next routine and retries the failed one on the next tick.
	Failed int `json:"failed"`
}

func (r *RoutineEventDispatchResult) add(other *RoutineEventDispatchResult) {
	r.Examined += other.Examined
	r.Started += other.Started
	r.TestStarted += other.TestStarted
	r.Replayed += other.Replayed
	r.Waiting += other.Waiting
	r.RateLimited += other.RateLimited
	r.TenantCapped += other.TenantCapped
	r.Retired += other.Retired
	r.Paused += other.Paused
	r.Dropped += other.Dropped
	r.Failed += other.Failed
}

// RoutineEventDispatcher turns stored routine events into occurrence Runs:
// one active Run per routine, at most MaxPerHour real batches an hour, at
// most TenantRunCap event Runs per scope. It runs on one process (the host's
// sweep leader), so its check-then-claim cannot race. It keeps no goroutine
// or timer per routine.
type RoutineEventDispatcher struct {
	store             KernelStore
	matcher           *RoutineEventMatcher
	reportingStore    ConversationStore
	executionPreparer AcceptedRunExecutionPreparer
	// TenantRunCap overrides RoutineEventTenantRunCap when positive.
	TenantRunCap int
	// SourceChecker checks conversation and callback sources hourly. Nil
	// skips the check.
	SourceChecker RoutineEventSourceChecker
	now           func() time.Time
	minPassGap    time.Duration

	// workCursors keeps, per scope, the last routine a capped pass examined;
	// the next pass starts after it, so routines that sort late get the
	// freed slots in turn.
	cursorMu    sync.Mutex
	workCursors map[Scope]string

	lastRebuild     time.Time
	lastSourceCheck time.Time
	lastPrune       time.Time
}

// NewRoutineEventDispatcher builds a dispatcher on the matcher's store.
func NewRoutineEventDispatcher(store KernelStore, matcher *RoutineEventMatcher, opts ...models.ServiceOption) *RoutineEventDispatcher {
	dispatcher := &RoutineEventDispatcher{store: store, matcher: matcher, now: models.ClockFrom(opts), minPassGap: routineEventMinPassGap, workCursors: map[Scope]string{}}
	if reportingStore, ok := store.(ConversationStore); ok {
		dispatcher.reportingStore = reportingStore
	}
	return dispatcher
}

func (d *RoutineEventDispatcher) SetAcceptedRunExecutionPreparer(preparer AcceptedRunExecutionPreparer) {
	if d != nil {
		d.executionPreparer = preparer
	}
}

func (d *RoutineEventDispatcher) tenantRunCap() int {
	if d.TenantRunCap > 0 {
		return d.TenantRunCap
	}
	return RoutineEventTenantRunCap
}

// Run is the dispatcher loop: it wakes on the matcher's nudge and on a
// ticker, at most once per minPassGap, so a stream of inserts coalesces.
// Every process keeps its index fresh (rebuild at start and every 15
// minutes); only a process for which leads returns true (nil = always)
// starts Runs and does the hourly source check and daily prune.
func (d *RoutineEventDispatcher) Run(ctx context.Context, interval time.Duration, leads func() bool) error {
	if d == nil || d.store == nil {
		return errors.New("routine event dispatcher is not configured")
	}
	if interval <= 0 {
		interval = RoutineEventDispatchInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	nudges := d.matcher.Nudges()
	for {
		d.pass(ctx, leads)
		last := time.Now()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		case <-nudges:
		}
		if wait := d.minPassGap - time.Since(last); wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
}

// pass is one loop iteration. Errors are retried on the next tick.
func (d *RoutineEventDispatcher) pass(ctx context.Context, leads func() bool) {
	now := d.now()
	if d.matcher != nil && (d.lastRebuild.IsZero() || now.Sub(d.lastRebuild) >= RoutineEventIndexRebuildInterval) {
		if d.matcher.RebuildRoutineIndex(ctx) == nil {
			d.lastRebuild = now
		}
	}
	if leads != nil && !leads() {
		return
	}
	_, _ = d.ReconcileAll(ctx, routineEventDispatchWorkLimit)
	if d.SourceChecker != nil && (d.lastSourceCheck.IsZero() || now.Sub(d.lastSourceCheck) >= routineEventSourceCheckInterval) {
		if _, err := d.CheckRoutineSources(ctx); err == nil {
			d.lastSourceCheck = now
		}
	}
	if d.lastPrune.IsZero() || now.Sub(d.lastPrune) >= routineEventPruneInterval {
		if _, err := d.PruneRoutineEventData(ctx); err == nil {
			d.lastPrune = now
		}
	}
}

// ReconcileAll runs one dispatch pass over every scope with waiting events.
// One query finds those scopes, so idle tenants cost nothing.
func (d *RoutineEventDispatcher) ReconcileAll(ctx context.Context, limitPerScope int) (*RoutineEventDispatchResult, error) {
	if d == nil || d.store == nil {
		return nil, errors.New("routine event dispatcher is not configured")
	}
	scopes, err := d.store.ListRoutineEventWorkScopes(ctx)
	if err != nil {
		return nil, err
	}
	d.keepWorkCursors(scopes)
	total := &RoutineEventDispatchResult{Scopes: len(scopes)}
	queueDepth := 0
	var firstErr error
	for _, scope := range scopes {
		result, depth, err := d.reconcileScope(ctx, scope, limitPerScope)
		total.add(result)
		queueDepth += depth
		if err != nil && firstErr == nil {
			// A failure in one scope, or of one routine, does not stall the
			// rest of the pass.
			firstErr = fmt.Errorf("dispatch routine events in %s/%s: %w", scope.Kind, scope.ID, err)
		}
	}
	if d.matcher != nil {
		d.matcher.stats.queueDepth.Store(int64(queueDepth))
	}
	return total, firstErr
}

// ReconcileScope runs one dispatch pass over one scope.
func (d *RoutineEventDispatcher) ReconcileScope(ctx context.Context, scope Scope, limit int) (*RoutineEventDispatchResult, error) {
	if d == nil || d.store == nil {
		return nil, errors.New("routine event dispatcher is not configured")
	}
	result, _, err := d.reconcileScope(ctx, scope, limit)
	return result, err
}

type routineEventScopePass struct {
	activeRuns int
	capped     bool
}

func (d *RoutineEventDispatcher) reconcileScope(ctx context.Context, scope Scope, limit int) (*RoutineEventDispatchResult, int, error) {
	result := &RoutineEventDispatchResult{}
	if err := scope.Validate(); err != nil {
		return result, 0, err
	}
	if limit <= 0 || limit > routineEventSubscriptionPageLimit {
		limit = routineEventDispatchWorkLimit
	}
	ids, err := d.routineEventWork(ctx, scope, limit)
	if err != nil || len(ids) == 0 {
		return result, 0, err
	}
	// One scope-wide count gives the queue depth; nothing per routine.
	depth, err := d.store.CountPendingRoutineEventsInScope(ctx, scope)
	if err != nil {
		return result, 0, err
	}
	active, err := d.store.CountActiveRoutineEventRuns(ctx, scope)
	if err != nil {
		return result, depth, err
	}
	pass := &routineEventScopePass{activeRuns: active}
	var firstErr error
	examined := ""
	for index, id := range ids {
		if !pass.capped && pass.activeRuns >= d.tenantRunCap() {
			pass.capped = true
			result.TenantCapped++
		}
		if pass.capped {
			// The tenant cap skips the rest of the scope's work this pass,
			// with no store reads per routine (SPEC §4).
			result.Waiting += len(ids) - index
			break
		}
		result.Examined++
		examined = id
		if err := d.dispatchActivation(ctx, scope, id, pass, result); err != nil {
			// One routine's failure stays local: the routines sorted after
			// it still start. It is retried on the next tick.
			result.Failed++
			if firstErr == nil {
				firstErr = fmt.Errorf("routine %s: %w", id, err)
			}
		}
	}
	d.advanceWorkCursor(scope, examined, pass.capped || len(ids) >= limit)
	return result, depth, firstErr
}

// routineEventWork lists the scope's routines with waiting events, starting
// after the scope's cursor and wrapping around to the start.
func (d *RoutineEventDispatcher) routineEventWork(ctx context.Context, scope Scope, limit int) ([]string, error) {
	d.cursorMu.Lock()
	after := d.workCursors[scope]
	d.cursorMu.Unlock()
	ids, err := d.store.ListRoutineEventWork(ctx, scope, after, limit)
	if err != nil || after == "" || len(ids) >= limit {
		return ids, err
	}
	head, err := d.store.ListRoutineEventWork(ctx, scope, "", limit-len(ids))
	if err != nil {
		return nil, err
	}
	for _, id := range head {
		if id > after {
			break
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// advanceWorkCursor moves the scope's cursor past the last routine examined
// when the pass left work unexamined (tenant cap or list limit); otherwise
// every routine had its turn and the next pass starts from the beginning.
func (d *RoutineEventDispatcher) advanceWorkCursor(scope Scope, examined string, unfinished bool) {
	d.cursorMu.Lock()
	defer d.cursorMu.Unlock()
	switch {
	case !unfinished:
		delete(d.workCursors, scope)
	case examined != "":
		if d.workCursors == nil {
			d.workCursors = map[Scope]string{}
		}
		d.workCursors[scope] = examined
	}
}

// keepWorkCursors forgets the cursors of scopes that no longer have work.
func (d *RoutineEventDispatcher) keepWorkCursors(scopes []Scope) {
	d.cursorMu.Lock()
	defer d.cursorMu.Unlock()
	if len(d.workCursors) == 0 {
		return
	}
	keep := make(map[Scope]bool, len(scopes))
	for _, scope := range scopes {
		keep[scope] = true
	}
	for scope := range d.workCursors {
		if !keep[scope] {
			delete(d.workCursors, scope)
		}
	}
}

func routineEventIdempotencyKey(batchID string) string {
	return routineEventIdempotencyPrefix + batchID
}

func (d *RoutineEventDispatcher) dispatchActivation(ctx context.Context, scope Scope, activationID string, pass *routineEventScopePass, result *RoutineEventDispatchResult) error {
	activation, err := d.store.GetRunbookActivation(ctx, scope, activationID)
	if err != nil {
		return err
	}
	if activation == nil || activation.Task == nil || activation.Task.Event == nil || activation.Trigger.Kind != runbook.TriggerEvent {
		dropped, err := d.closeOut(ctx, scope, activationID, RoutineEventDroppedRetired)
		result.Dropped += dropped
		return err
	}
	if activation.Status != RunbookActivationActive {
		reason := RoutineEventDroppedPaused
		if activation.Status == RunbookActivationRetired {
			reason = RoutineEventDroppedRetired
		}
		dropped, err := d.closeOut(ctx, scope, activation.ID, reason)
		result.Dropped += dropped
		return err
	}
	now := d.now().UTC()

	batch, err := d.store.GetOpenRoutineEventBatch(ctx, scope, activation.ID)
	if err != nil {
		return err
	}
	if batch != nil {
		// A Run may already exist for the open batch: the process stopped
		// between creating it and marking the batch. Finish that start.
		existing, err := d.store.GetAgentRun(ctx, scope, agentrun.RunIDForIdempotencyKey(scope, routineEventIdempotencyKey(batch.ID)))
		if err != nil {
			return err
		}
		if existing != nil {
			if err := d.store.MarkRoutineEventBatchStarted(ctx, scope, batch.ID, existing.ID, now); err != nil {
				return err
			}
			result.Replayed++
			if !isTerminalAgentRunStatus(existing.Status) {
				pass.activeRuns++
			}
			if !batch.Test {
				return d.retireWhenExhausted(ctx, activation, result)
			}
			return nil
		}
	}
	if pass.activeRuns >= d.tenantRunCap() {
		pass.capped = true
		result.TenantCapped++
		result.Waiting++
		return nil
	}
	backpressured, err := activationBackpressured(ctx, d.store, activation)
	if err != nil {
		return err
	}
	if backpressured {
		result.Waiting++
		return nil
	}
	if batch == nil || !batch.Test {
		exhausted, err := d.exhausted(ctx, activation)
		if err != nil {
			return err
		}
		if exhausted {
			return d.retire(ctx, activation, result)
		}
	}
	limited, err := d.hourlyLimitReached(ctx, activation, now)
	if err != nil {
		return err
	}
	if batch == nil {
		if limited {
			// Only test events may start now. Claim only when some arrived
			// recently, so a real batch is rarely formed just to wait.
			tests, err := d.store.CountRoutineTestEventsSince(ctx, scope, activation.ID, now.Add(-RoutineEventActiveRunWindow))
			if err != nil {
				return err
			}
			if tests == 0 {
				result.RateLimited++
				return nil
			}
		}
		batch, err = d.store.ClaimRoutineEventBatch(ctx, scope, activation.ID, RoutineEventMaxBatchEvents, now)
		if err != nil {
			return err
		}
		if batch == nil {
			return nil
		}
	}
	if limited && !batch.Test {
		result.RateLimited++
		return nil
	}
	return d.startBatch(ctx, activation, batch, pass, result)
}

func (d *RoutineEventDispatcher) startBatch(ctx context.Context, activation *RunbookActivation, batch *RoutineEventBatch, pass *routineEventScopePass, result *RoutineEventDispatchResult) error {
	scope := activation.Scope
	if err := verifyRunbookActivationExecution(ctx, d.store, scope, activation.ID); err != nil {
		if !errors.Is(err, ErrRunbookActivationUnverified) {
			return err
		}
		return d.pause(ctx, activation, RoutinePausedUnverified, result)
	}
	objective, err := d.store.GetObjective(ctx, scope, activation.ObjectiveID)
	if err != nil {
		return err
	}
	if objective == nil || objective.Owner != activation.Owner || objective.Status != ObjectiveStatusActive {
		result.Waiting++
		return nil
	}
	stored, records, err := d.store.GetRoutineEventBatch(ctx, scope, batch.ID)
	if err != nil {
		return err
	}
	if stored == nil {
		return ErrRoutineEventBatchNotFound
	}
	extra := map[string]interface{}{
		RoutineEventBatchContextKey:  stored.ID,
		RoutineEventCountContextKey:  len(stored.EventIDs),
		RoutineEventKindContextKey:   string(activation.Task.Event.Kind),
		RoutineEventHeaderContextKey: routineEventBatchHeader(activation.Task.Event.Kind, stored, records),
	}
	if stored.Test {
		extra[RoutineEventTestContextKey] = true
	}
	created, err := startActivationOccurrence(ctx, d.store, d.reportingStore, activation, objective, RunSourceEvent,
		routineEventIdempotencyKey(stored.ID), extra, ActivityActor{Type: "service", ID: "routine-event-dispatcher"},
		runbookScheduleVisibility(activation), d.executionPreparer)
	if errors.Is(err, ErrRoutineReportingUnavailable) {
		// The routine's chat was archived or its request message is gone.
		// Retrying cannot help: pause it so the person sees why, and close
		// out its events.
		return d.pause(ctx, activation, RoutinePausedUnverified, result)
	}
	if err != nil {
		return fmt.Errorf("start routine %s for event batch %s: %w", activation.ID, stored.ID, err)
	}
	if err := d.store.MarkRoutineEventBatchStarted(ctx, scope, stored.ID, created.Run.ID, d.now().UTC()); err != nil {
		return err
	}
	if !isTerminalAgentRunStatus(created.Run.Status) {
		pass.activeRuns++
	}
	if created.Event == nil {
		result.Replayed++
	} else {
		if stored.Test {
			result.TestStarted++
		} else {
			result.Started++
		}
		if d.matcher != nil {
			d.matcher.stats.countRunStarted(stored.Test)
		}
	}
	if stored.Test {
		return nil
	}
	return d.retireWhenExhausted(ctx, activation, result)
}

// exhausted reports whether the routine already started its maximum of real
// batches. The count comes from durable batch rows, never the activation.
func (d *RoutineEventDispatcher) exhausted(ctx context.Context, activation *RunbookActivation) (bool, error) {
	maximum := activation.Task.Event.MaximumOccurrences
	if maximum <= 0 {
		return false, nil
	}
	count, _, err := d.store.CountRoutineEventBatchesSince(ctx, activation.Scope, activation.ID, time.Time{}, false)
	if err != nil {
		return false, err
	}
	return int64(count) >= maximum, nil
}

func (d *RoutineEventDispatcher) retireWhenExhausted(ctx context.Context, activation *RunbookActivation, result *RoutineEventDispatchResult) error {
	exhausted, err := d.exhausted(ctx, activation)
	if err != nil || !exhausted {
		return err
	}
	return d.retire(ctx, activation, result)
}

func (d *RoutineEventDispatcher) hourlyLimitReached(ctx context.Context, activation *RunbookActivation, now time.Time) (bool, error) {
	maximum := activation.Task.Event.MaxPerHour
	if maximum <= 0 {
		maximum = RoutineEventDefaultMaxPerHour
	}
	count, _, err := d.store.CountRoutineEventBatchesSince(ctx, activation.Scope, activation.ID, now.Add(-time.Hour), false)
	if err != nil {
		return false, err
	}
	return count >= maximum, nil
}

func (d *RoutineEventDispatcher) retire(ctx context.Context, activation *RunbookActivation, result *RoutineEventDispatchResult) error {
	_, err := NewRunbookActivationService(d.store, models.WithClock(d.now)).Update(ctx, activation.Scope, activation.ID, UpdateRunbookActivationRequest{
		ExpectedRevision: activation.Revision, Status: RunbookActivationRetired, Exhausted: true,
	})
	if errors.Is(err, ErrRunbookActivationRevision) {
		return nil
	}
	if err != nil {
		return err
	}
	result.Retired++
	dropped, err := d.closeOut(ctx, activation.Scope, activation.ID, RoutineEventDroppedExhausted)
	result.Dropped += dropped
	return err
}

func (d *RoutineEventDispatcher) pause(ctx context.Context, activation *RunbookActivation, reason RoutinePausedReason, result *RoutineEventDispatchResult) error {
	_, err := NewRunbookActivationService(d.store, models.WithClock(d.now)).Update(ctx, activation.Scope, activation.ID, UpdateRunbookActivationRequest{
		ExpectedRevision: activation.Revision, Status: RunbookActivationPaused, PausedReason: reason,
	})
	if errors.Is(err, ErrRunbookActivationRevision) {
		return nil
	}
	if err != nil {
		return err
	}
	result.Paused++
	dropped, err := d.closeOut(ctx, activation.Scope, activation.ID, RoutineEventDroppedPaused)
	result.Dropped += dropped
	return err
}

// closeOut ends a routine's waiting work when it is paused, retired or used
// up. An open batch whose Run already started is marked started first; the
// rest is dropped, and the store deletes the open batch row.
func (d *RoutineEventDispatcher) closeOut(ctx context.Context, scope Scope, activationID, reason string) (int, error) {
	open, err := d.store.GetOpenRoutineEventBatch(ctx, scope, activationID)
	if err != nil {
		return 0, err
	}
	if open != nil {
		existing, err := d.store.GetAgentRun(ctx, scope, agentrun.RunIDForIdempotencyKey(scope, routineEventIdempotencyKey(open.ID)))
		if err != nil {
			return 0, err
		}
		if existing != nil {
			if err := d.store.MarkRoutineEventBatchStarted(ctx, scope, open.ID, existing.ID, d.now().UTC()); err != nil {
				return 0, err
			}
		}
	}
	return d.store.DropRoutineEvents(ctx, scope, activationID, reason)
}

// CheckRoutineSources pauses conversation and callback routines whose source,
// or a channel or chat they name, is gone (source_removed). It runs hourly on
// the leader.
func (d *RoutineEventDispatcher) CheckRoutineSources(ctx context.Context) (int, error) {
	if d == nil || d.store == nil || d.SourceChecker == nil {
		return 0, nil
	}
	kinds := []RoutineEventKind{RoutineEventSlackMessage, RoutineEventTelegramMessage, RoutineEventGitHub}
	paused := 0
	after := ""
	for {
		page, cursor, err := d.store.ListRoutineEventSubscriptions(ctx, kinds, after, routineEventSubscriptionPageLimit)
		if err != nil {
			return paused, err
		}
		for _, subscription := range page {
			exists, err := d.SourceChecker.SourceExists(ctx, subscription)
			if err != nil || exists {
				continue
			}
			activation, err := d.store.GetRunbookActivation(ctx, subscription.Scope, subscription.ActivationID)
			if err != nil {
				return paused, err
			}
			if activation == nil || activation.Status != RunbookActivationActive {
				continue
			}
			result := &RoutineEventDispatchResult{}
			if err := d.pause(ctx, activation, RoutinePausedSourceRemoved, result); err != nil {
				return paused, err
			}
			paused += result.Paused
		}
		if cursor == "" || len(page) == 0 {
			return paused, nil
		}
		after = cursor
	}
}

// PruneRoutineEventData removes started and dropped events older than the
// retention, batch rows of routines gone that long, and hooks of retired or
// missing routines. It runs daily on the leader.
func (d *RoutineEventDispatcher) PruneRoutineEventData(ctx context.Context) (int, error) {
	if d == nil || d.store == nil {
		return 0, nil
	}
	now := d.now().UTC()
	removed := 0
	for round := 0; round < routineEventPruneRounds; round++ {
		count, err := d.store.PruneRoutineEvents(ctx, now.Add(-RoutineEventRetention), routineEventPruneBatch)
		removed += count
		if err != nil {
			return removed, err
		}
		if count < routineEventPruneBatch {
			break
		}
	}
	count, err := d.store.PruneHooks(ctx, now.Add(-routineHookPruneGrace))
	return removed + count, err
}

// routineEventBatchHeader is the "what started it" line: one event's
// Summary, else a kernel-made count. It is cut to 200 bytes.
func routineEventBatchHeader(kind RoutineEventKind, batch *RoutineEventBatch, records []*RoutineEventRecord) string {
	count := len(batch.EventIDs)
	var header string
	switch {
	case batch.Test && count <= 1:
		header = "Test event"
	case batch.Test:
		header = fmt.Sprintf("%d test events", count)
	case count == 1:
		header = "New event"
		for _, record := range records {
			if record != nil && record.Event.ID == batch.EventIDs[0] && strings.TrimSpace(record.Event.Summary) != "" {
				header = strings.TrimSpace(record.Event.Summary)
			}
		}
	default:
		noun := "events"
		switch kind {
		case RoutineEventGmailMessage:
			noun = "emails"
		case RoutineEventSlackMessage, RoutineEventTelegramMessage:
			noun = "messages"
		}
		header = fmt.Sprintf("%d new %s", count, noun)
	}
	return models.TruncateUTF8(header, RoutineEventHeaderMaxBytes)
}

// PauseRoutine pauses one event routine for a system reason (needs_sign_in,
// source_removed, unverified). A routine that is retired, or already paused
// for the same reason, is left as it is; it returns whether it paused.
//
// It runs on the caller's goroutine (for example Google sync), beside the
// dispatcher loop, so it only changes the status and nudges the dispatcher.
// The dispatcher closes out the waiting events on its next pass, on its own
// goroutine, so the close-out never deletes the batch of a Run it is
// creating at that moment.
func (d *RoutineEventDispatcher) PauseRoutine(ctx context.Context, scope Scope, activationID string, reason RoutinePausedReason) (bool, error) {
	if d == nil || d.store == nil {
		return false, errors.New("routine event dispatcher is not configured")
	}
	if !validRoutinePausedReason(reason) {
		return false, fmt.Errorf("unknown pause reason %q", reason)
	}
	if err := validateRoutineEventScopeActivation(scope, activationID); err != nil {
		return false, err
	}
	activationID = strings.TrimSpace(activationID)
	for attempt := 0; attempt < 3; attempt++ {
		activation, err := d.store.GetRunbookActivation(ctx, scope, activationID)
		if err != nil {
			return false, err
		}
		if activation == nil {
			return false, ErrRunbookActivationNotFound
		}
		if activation.Task == nil || activation.Task.Event == nil {
			return false, ErrRoutineNotEventRoutine
		}
		if activation.Status == RunbookActivationRetired ||
			(activation.Status == RunbookActivationPaused && activation.PausedReason == reason) {
			return false, nil
		}
		_, err = NewRunbookActivationService(d.store, models.WithClock(d.now)).Update(ctx, scope, activation.ID, UpdateRunbookActivationRequest{
			ExpectedRevision: activation.Revision, Status: RunbookActivationPaused, PausedReason: reason,
		})
		if errors.Is(err, ErrRunbookActivationRevision) {
			continue
		}
		if err != nil {
			return false, err
		}
		if d.matcher != nil {
			d.matcher.wake()
		}
		return true, nil
	}
	return false, ErrRunbookActivationRevision
}

// PauseRoutinesForSource pauses every active event routine of the scope that
// listens to this kind and SourceRef, for example when a person's Google
// sign-in is revoked. It returns how many it paused.
func (d *RoutineEventDispatcher) PauseRoutinesForSource(ctx context.Context, scope Scope, kind RoutineEventKind, sourceRef string, reason RoutinePausedReason) (int, error) {
	if d == nil || d.store == nil {
		return 0, errors.New("routine event dispatcher is not configured")
	}
	activations, err := d.store.ListRoutineEventActivations(ctx, scope, kind, sourceRef)
	if err != nil {
		return 0, err
	}
	paused := 0
	for _, activation := range activations {
		if activation == nil {
			continue
		}
		done, err := d.PauseRoutine(ctx, scope, activation.ID, reason)
		if err != nil {
			return paused, err
		}
		if done {
			paused++
		}
	}
	return paused, nil
}
