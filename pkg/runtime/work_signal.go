package runtime

import (
	"context"
	"sort"
	"sync"
	"time"
)

// WorkFamily names the class of durable work whose claimability changed.
// Families keep wake fan-out narrow: an ActionCall change never wakes Run
// schedulers, and a Run change never wakes action claimers.
type WorkFamily string

const (
	// WorkFamilyAction covers action_calls and Runs leaving an action pause.
	WorkFamilyAction WorkFamily = "action"
	// WorkFamilyRun covers agent_runs and agent_requests.
	WorkFamilyRun WorkFamily = "run"
	// WorkFamilyMaintenance covers Skill runtime maintenance, which gates both
	// ActionCalls and Runs waiting for the maintenance to finish.
	WorkFamilyMaintenance WorkFamily = "maintenance"
	// WorkFamilyApproval covers approval_checkpoints.
	WorkFamilyApproval WorkFamily = "approval"
	// WorkFamilyConversation covers conversations, channel messages and the
	// in-chat Skill setup and credential requests.
	WorkFamilyConversation WorkFamily = "conversation"
	// WorkFamilyExternal covers the external conversation inbox, deliveries,
	// endpoints and callback events.
	WorkFamilyExternal WorkFamily = "external"
	// WorkFamilyActivity covers channel-visible Run commentary.
	WorkFamilyActivity WorkFamily = "activity"
	// WorkFamilyAuthoring covers workforce change sets and workforce
	// authoring Runs, which host authoring workers claim.
	WorkFamilyAuthoring WorkFamily = "authoring"

	// Due families are scheduled by the loops themselves for time-based work:
	// retries, lease expiries, timers and spaced or backed-off passes. Each
	// wakes only the loop that scheduled it.

	// WorkFamilyRunDue wakes Run pools only.
	WorkFamilyRunDue WorkFamily = "run-due"
	// WorkFamilyConversationDue wakes the conversation Run reconciler only.
	WorkFamilyConversationDue WorkFamily = "conversation-due"
	// WorkFamilyExternalDue wakes the external conversation supervisor only.
	WorkFamilyExternalDue WorkFamily = "external-due"
)

// WorkSignalKind distinguishes a scoped change from queue lifecycle.
type WorkSignalKind int

const (
	// WorkSignalChanged reports due work in Scope for Family.
	WorkSignalChanged WorkSignalKind = iota
	// WorkSignalConnected reports that the work queue is consuming. With
	// Rebuild set the queue lost its schedule, and every family's pending
	// work must be republished from the store.
	WorkSignalConnected
	// WorkSignalDisconnected reports a lost queue. Loops fall back to their
	// configured poll intervals until the next WorkSignalConnected.
	WorkSignalDisconnected
)

// WorkSignal is one wake hint from the work queue. Signals are hints only:
// claims, leases and fairness are always decided by the store.
type WorkSignal struct {
	Kind    WorkSignalKind
	Family  WorkFamily
	Scope   Scope
	Rebuild bool
}

// ActionDeadlineStore reports when persisted ActionCalls next become
// claimable without any further write: a retry's available_at or a running
// attempt's lease expiry. Nil means no time-based work is pending.
type ActionDeadlineStore interface {
	NextActionDeadline(ctx context.Context, scope Scope, now time.Time) (*time.Time, error)
}

// AgentRunDeadlineStore reports when persisted Runs of one pool next become
// claimable without any further write: a queued Run's available_at or a
// running Run's lease expiry. Sleeping timers are reported by WakeDueTimers.
type AgentRunDeadlineStore interface {
	NextAgentRunDeadline(ctx context.Context, scope Scope, kind RunKind, assignedAgentID string, now time.Time) (*time.Time, error)
}

// ExternalConversationDeadlineStore reports when external conversation work
// next becomes due: inbox, delivery and callback retries or lease expiries,
// reply retries and pending approval expiries.
type ExternalConversationDeadlineStore interface {
	NextExternalConversationDeadline(ctx context.Context, scope Scope, now time.Time) (*time.Time, error)
}

// WorkSignalHealth is a point-in-time view of the work queue consumer.
type WorkSignalHealth struct {
	Healthy        bool      `json:"healthy"`
	ConnectedAt    time.Time `json:"connectedAt,omitempty"`
	DisconnectedAt time.Time `json:"disconnectedAt,omitempty"`
	LastSignalAt   time.Time `json:"lastSignalAt,omitempty"`
	Connects       int64     `json:"connects"`
	Disconnects    int64     `json:"disconnects"`
	Signals        int64     `json:"signals"`
}

// WorkSignalStatus tracks whether the work queue is consuming, and schedules
// loop passes into it. While healthy, loops hold no timers and issue no
// reads between passes: they schedule their next durable deadline into the
// queue and sleep until it, or a commit, wakes them. A nil or unhealthy
// status keeps every loop on its configured poll interval.
type WorkSignalStatus struct {
	mu        sync.Mutex
	health    WorkSignalHealth
	changed   chan struct{}
	now       func() time.Time
	publisher *workPublisher
}

func NewWorkSignalStatus() *WorkSignalStatus {
	return &WorkSignalStatus{changed: make(chan struct{}), now: time.Now}
}

// bindPublisher connects the queue that scheduled passes go to.
func (s *WorkSignalStatus) bindPublisher(publisher *workPublisher) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.publisher = publisher
}

// queueBound reports whether a work queue drives this status, so loops wait
// for it at startup instead of listing every scope.
func (s *WorkSignalStatus) queueBound() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.publisher != nil
}

// schedule asks the queue for a pass of family in scope at due. A nil due
// schedules nothing: the scope has no time-based work.
func (s *WorkSignalStatus) schedule(family WorkFamily, scope Scope, due *time.Time) {
	if s == nil || due == nil {
		return
	}
	s.mu.Lock()
	publisher := s.publisher
	s.mu.Unlock()
	publisher.schedule(WorkItem{Family: family, Scope: scope, DueAt: due.UTC()})
}

// scheduleIn schedules a pass after delay, for spacing and backoff.
func (s *WorkSignalStatus) scheduleIn(family WorkFamily, scope Scope, delay time.Duration) {
	if s == nil {
		return
	}
	s.schedule(family, scope, new(s.now().Add(delay)))
}

// Healthy reports whether loops may rely on signals instead of polling.
func (s *WorkSignalStatus) Healthy() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.health.Healthy
}

func (s *WorkSignalStatus) Snapshot() WorkSignalHealth {
	if s == nil {
		return WorkSignalHealth{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.health
}

// Observe records one queue signal. Lifecycle signals flip health and
// release every loop waiting on the previous health state.
func (s *WorkSignalStatus) Observe(signal WorkSignal) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	switch signal.Kind {
	case WorkSignalConnected:
		s.health.Connects++
		s.health.ConnectedAt = now
		s.setHealthyLocked(true)
	case WorkSignalDisconnected:
		s.health.Disconnects++
		s.health.DisconnectedAt = now
		s.setHealthyLocked(false)
	default:
		s.health.Signals++
		s.health.LastSignalAt = now
	}
}

func (s *WorkSignalStatus) setHealthyLocked(healthy bool) {
	if s.health.Healthy == healthy {
		return
	}
	s.health.Healthy = healthy
	close(s.changed)
	s.changed = make(chan struct{})
}

// watch returns the current health and a channel closed on its next change.
// A nil status is permanently unhealthy and never changes.
func (s *WorkSignalStatus) watch() (bool, <-chan struct{}) {
	if s == nil {
		return false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.health.Healthy, s.changed
}

// idleWait is how long an idle polling loop sleeps before its next probe.
type idleWait struct {
	delay   time.Duration
	forever bool
}

func pollWait(delay time.Duration) idleWait { return idleWait{delay: delay} }

// waitForever sleeps until a wake: the next pass is scheduled in the queue.
func waitForever() idleWait { return idleWait{forever: true} }

// waitIdle returns false only when ctx ends. A closed health channel ends the
// wait so the loop re-evaluates whether it may still rely on the queue.
func waitIdle(ctx context.Context, wake, health <-chan struct{}, wait idleWait) bool {
	var timeout <-chan time.Time
	if !wait.forever {
		timer := time.NewTimer(wait.delay)
		defer timer.Stop()
		timeout = timer.C
	}
	select {
	case <-ctx.Done():
		return false
	case <-wake:
	case <-health:
	case <-timeout:
	}
	return true
}

func earliestDeadline(values ...*time.Time) *time.Time {
	var result *time.Time
	for _, value := range values {
		if value != nil && (result == nil || value.Before(*result)) {
			next := *value
			result = &next
		}
	}
	return result
}

// scopeWakeQueue multiplexes queue-driven reconciliation over many scopes in
// one loop. A scope idle for the spacing interval runs at once; a scope that
// ran within it is deferred to the end of the interval, so a busy scope never
// runs more often than the fixed tick it replaces.
type scopeWakeQueue struct {
	mu       sync.Mutex
	dirty    map[Scope]struct{}
	lastRun  map[Scope]time.Time
	failures map[Scope]int
	signal   chan struct{}
}

func newScopeWakeQueue() *scopeWakeQueue {
	return &scopeWakeQueue{dirty: make(map[Scope]struct{}), lastRun: make(map[Scope]time.Time), failures: make(map[Scope]int), signal: make(chan struct{}, 1)}
}

// outcome returns when a failed scope should be retried, with exponential
// backoff from base, or nil once the scope succeeds.
func (q *scopeWakeQueue) outcome(scope Scope, err error, base time.Duration, now time.Time) *time.Time {
	q.mu.Lock()
	defer q.mu.Unlock()
	if err == nil {
		delete(q.failures, scope)
		return nil
	}
	q.failures[scope]++
	return new(now.Add(workerPollDelay(base, q.failures[scope]-1, scope.Kind+":"+scope.ID)))
}

func (q *scopeWakeQueue) wakeScope(scope Scope) {
	if scope.Validate() != nil {
		return
	}
	q.mu.Lock()
	q.dirty[scope] = struct{}{}
	q.mu.Unlock()
	select {
	case q.signal <- struct{}{}:
	default:
	}
}

// ran records a full pass over scopes.
func (q *scopeWakeQueue) ran(scopes []Scope, now time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, scope := range scopes {
		q.lastRun[scope] = now
	}
}

// take drains signalled scopes. A scope that ran within spacing is returned
// in deferred with the time its next pass may start.
func (q *scopeWakeQueue) take(now time.Time, spacing time.Duration) (ready []Scope, deferred map[Scope]time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()
	ready = make([]Scope, 0, len(q.dirty))
	for scope := range q.dirty {
		if last, ran := q.lastRun[scope]; ran && spacing > 0 && now.Sub(last) < spacing {
			if deferred == nil {
				deferred = make(map[Scope]time.Time)
			}
			deferred[scope] = last.Add(spacing)
			continue
		}
		q.lastRun[scope] = now
		ready = append(ready, scope)
	}
	clear(q.dirty)
	// Forget scopes idle beyond any spacing window so the map tracks only
	// recently active scopes, not every scope ever seen.
	if spacing > 0 && len(q.lastRun) > 1024 {
		for scope, last := range q.lastRun {
			if now.Sub(last) >= spacing {
				delete(q.lastRun, scope)
			}
		}
	}
	sort.Slice(ready, func(i, j int) bool {
		if ready[i].Kind != ready[j].Kind {
			return ready[i].Kind < ready[j].Kind
		}
		return ready[i].ID < ready[j].ID
	})
	return ready, deferred
}

// knownWorkerScopes filters signalled scopes to the scope source's inventory
// for sources without a point lookup. An unknown scope refreshes the
// inventory at most once per refresh interval.
type knownWorkerScopes struct {
	source         WorkerScopeSource
	refresh        time.Duration
	known          map[Scope]struct{}
	unknownChecked map[Scope]time.Time
}

func newKnownWorkerScopes(source WorkerScopeSource, refresh time.Duration) *knownWorkerScopes {
	return &knownWorkerScopes{source: source, refresh: refresh, known: make(map[Scope]struct{}), unknownChecked: make(map[Scope]time.Time)}
}

func (k *knownWorkerScopes) list(ctx context.Context) ([]Scope, error) {
	scopes, err := k.source.ListWorkerScopes(ctx)
	if err != nil {
		return nil, err
	}
	scopes = normalizedConversationRunScopes(scopes)
	clear(k.known)
	for _, scope := range scopes {
		k.known[scope] = struct{}{}
	}
	return scopes, nil
}

func (k *knownWorkerScopes) filter(ctx context.Context, now time.Time, scopes []Scope) ([]Scope, error) {
	refreshed := false
	result := make([]Scope, 0, len(scopes))
	for _, scope := range scopes {
		if _, ok := k.known[scope]; !ok && !refreshed {
			if checked, seen := k.unknownChecked[scope]; !seen || now.Sub(checked) >= k.refresh {
				k.unknownChecked[scope] = now
				if _, err := k.list(ctx); err != nil {
					return result, err
				}
				refreshed = true
			}
		}
		if _, ok := k.known[scope]; ok {
			delete(k.unknownChecked, scope)
			result = append(result, scope)
		}
	}
	return result, nil
}
