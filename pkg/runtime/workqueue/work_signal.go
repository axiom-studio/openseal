package workqueue

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
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
	Family  models.WorkFamily
	Scope   models.Scope
	Rebuild bool
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

func NewWorkSignalStatus(opts ...models.ServiceOption) *WorkSignalStatus {
	return &WorkSignalStatus{changed: make(chan struct{}), now: models.ClockFrom(opts)}
}

// bindPublisher connects the queue that scheduled passes go to.
func (s *WorkSignalStatus) bindPublisher(publisher *workPublisher) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.publisher = publisher
}

// queueBound reports whether a work queue drives this status, so loops wait
// for it at startup instead of listing every scope.
func (s *WorkSignalStatus) QueueBound() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.publisher != nil
}

// schedule asks the queue for a pass of family in scope at due. A nil due
// schedules nothing: the scope has no time-based work.
func (s *WorkSignalStatus) Schedule(family models.WorkFamily, scope models.Scope, due *time.Time) {
	if s == nil || due == nil {
		return
	}
	s.mu.Lock()
	publisher := s.publisher
	s.mu.Unlock()
	publisher.schedule(models.WorkItem{Family: family, Scope: scope, DueAt: due.UTC()})
}

// scheduleIn schedules a pass after delay, for spacing and backoff.
func (s *WorkSignalStatus) ScheduleIn(family models.WorkFamily, scope models.Scope, delay time.Duration) {
	if s == nil {
		return
	}
	s.Schedule(family, scope, new(s.now().Add(delay)))
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
func (s *WorkSignalStatus) Watch() (bool, <-chan struct{}) {
	if s == nil {
		return false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.health.Healthy, s.changed
}

// idleWait is how long an idle polling loop sleeps before its next probe.
type IdleWait struct {
	delay   time.Duration
	forever bool
}

func PollWait(delay time.Duration) IdleWait { return IdleWait{delay: delay} }

// waitForever sleeps until a wake: the next pass is scheduled in the queue.
func WaitForever() IdleWait { return IdleWait{forever: true} }

// waitIdle returns false only when ctx ends. A closed health channel ends the
// wait so the loop re-evaluates whether it may still rely on the queue.
func WaitIdle(ctx context.Context, wake, health <-chan struct{}, wait IdleWait) bool {
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

func EarliestDeadline(values ...*time.Time) *time.Time {
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
type ScopeWakeQueue struct {
	mu       sync.Mutex
	dirty    map[models.Scope]struct{}
	lastRun  map[models.Scope]time.Time
	failures map[models.Scope]int
	signal   chan struct{}
}

// signals receives once after one or more wakeScope calls.
func (q *ScopeWakeQueue) Signals() <-chan struct{} { return q.signal }

func NewScopeWakeQueue() *ScopeWakeQueue {
	return &ScopeWakeQueue{dirty: make(map[models.Scope]struct{}), lastRun: make(map[models.Scope]time.Time), failures: make(map[models.Scope]int), signal: make(chan struct{}, 1)}
}

// outcome returns when a failed scope should be retried, with exponential
// backoff from base, or nil once the scope succeeds.
func (q *ScopeWakeQueue) Outcome(scope models.Scope, err error, base time.Duration, now time.Time) *time.Time {
	q.mu.Lock()
	defer q.mu.Unlock()
	if err == nil {
		delete(q.failures, scope)
		return nil
	}
	q.failures[scope]++
	return new(now.Add(WorkerPollDelay(base, q.failures[scope]-1, scope.Kind+":"+scope.ID)))
}

func (q *ScopeWakeQueue) WakeScope(scope models.Scope) {
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
func (q *ScopeWakeQueue) Ran(scopes []models.Scope, now time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, scope := range scopes {
		q.lastRun[scope] = now
	}
}

// take drains signalled scopes. A scope that ran within spacing is returned
// in deferred with the time its next pass may start.
func (q *ScopeWakeQueue) Take(now time.Time, spacing time.Duration) (ready []models.Scope, deferred map[models.Scope]time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()
	ready = make([]models.Scope, 0, len(q.dirty))
	for scope := range q.dirty {
		if last, ran := q.lastRun[scope]; ran && spacing > 0 && now.Sub(last) < spacing {
			if deferred == nil {
				deferred = make(map[models.Scope]time.Time)
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
	known          map[models.Scope]struct{}
	unknownChecked map[models.Scope]time.Time
}

func NewKnownWorkerScopes(source WorkerScopeSource, refresh time.Duration) *knownWorkerScopes {
	return &knownWorkerScopes{source: source, refresh: refresh, known: make(map[models.Scope]struct{}), unknownChecked: make(map[models.Scope]time.Time)}
}

func (k *knownWorkerScopes) List(ctx context.Context) ([]models.Scope, error) {
	scopes, err := k.source.ListWorkerScopes(ctx)
	if err != nil {
		return nil, err
	}
	scopes = NormalizeScopes(scopes)
	clear(k.known)
	for _, scope := range scopes {
		k.known[scope] = struct{}{}
	}
	return scopes, nil
}

func (k *knownWorkerScopes) filter(ctx context.Context, now time.Time, scopes []models.Scope) ([]models.Scope, error) {
	refreshed := false
	result := make([]models.Scope, 0, len(scopes))
	for _, scope := range scopes {
		if _, ok := k.known[scope]; !ok && !refreshed {
			if checked, seen := k.unknownChecked[scope]; !seen || now.Sub(checked) >= k.refresh {
				k.unknownChecked[scope] = now
				if _, err := k.List(ctx); err != nil {
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

func NormalizeScopes(scopes []models.Scope) []models.Scope {
	seen := make(map[models.Scope]struct{}, len(scopes))
	result := make([]models.Scope, 0, len(scopes))
	for _, scope := range scopes {
		if scope.Validate() != nil {
			continue
		}
		if _, exists := seen[scope]; exists {
			continue
		}
		seen[scope] = struct{}{}
		result = append(result, scope)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Kind != result[j].Kind {
			return result[i].Kind < result[j].Kind
		}
		return result[i].ID < result[j].ID
	})
	return result
}
