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
)

// WorkSignalKind distinguishes a scoped change from listener lifecycle.
type WorkSignalKind int

const (
	// WorkSignalChanged reports committed work in Scope for Family.
	WorkSignalChanged WorkSignalKind = iota
	// WorkSignalConnected reports a (re)established listener. Changes may have
	// been missed while it was down, so every loop must probe once.
	WorkSignalConnected
	// WorkSignalDisconnected reports a lost listener. Loops fall back to their
	// configured poll intervals until the next WorkSignalConnected.
	WorkSignalDisconnected
)

// WorkSignal is one wake hint from durable storage. Signals are hints only:
// claims, leases and fairness are always decided by the store.
type WorkSignal struct {
	Kind   WorkSignalKind
	Family WorkFamily
	Scope  Scope
}

// WorkSignalSource streams committed work changes, for example PostgreSQL
// LISTEN/NOTIFY. ListenForWork blocks until ctx ends and calls handle
// serially. It reports WorkSignalConnected each time the listener connects.
type WorkSignalSource interface {
	ListenForWork(ctx context.Context, handle func(WorkSignal)) error
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

// WorkSignalHealth is a point-in-time view of a work signal listener.
type WorkSignalHealth struct {
	Healthy        bool      `json:"healthy"`
	ConnectedAt    time.Time `json:"connectedAt,omitempty"`
	DisconnectedAt time.Time `json:"disconnectedAt,omitempty"`
	LastSignalAt   time.Time `json:"lastSignalAt,omitempty"`
	Connects       int64     `json:"connects"`
	Disconnects    int64     `json:"disconnects"`
	Signals        int64     `json:"signals"`
}

// WorkSignalStatus tracks whether a work signal listener is connected. While
// healthy, idle worker loops sleep until a wake or their next durable
// deadline instead of polling. A nil or unhealthy status keeps every loop on
// its configured poll interval.
type WorkSignalStatus struct {
	mu      sync.Mutex
	health  WorkSignalHealth
	changed chan struct{}
	now     func() time.Time
}

func NewWorkSignalStatus() *WorkSignalStatus {
	return &WorkSignalStatus{changed: make(chan struct{}), now: time.Now}
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

// Observe records one listener signal. Lifecycle signals flip health and
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

// idleWait is how long an idle loop sleeps before its next durable probe.
type idleWait struct {
	delay   time.Duration
	forever bool
}

func pollWait(delay time.Duration) idleWait { return idleWait{delay: delay} }

// deadlineWait sleeps until deadline, or until a wake when there is none.
func deadlineWait(now time.Time, deadline *time.Time) idleWait {
	if deadline == nil {
		return idleWait{forever: true}
	}
	return idleWait{delay: max(deadline.Sub(now), 0)}
}

func (w idleWait) earliest(now time.Time, deadline *time.Time) idleWait {
	if deadline == nil {
		return w
	}
	candidate := max(deadline.Sub(now), 0)
	if w.forever || candidate < w.delay {
		return idleWait{delay: candidate}
	}
	return w
}

// waitIdle returns false only when ctx ends. A closed health channel ends the
// wait so the loop re-evaluates whether it may still rely on signals.
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

// scopeWakeQueue multiplexes signal-driven reconciliation over many scopes in
// one loop: a scope is reconciled when signalled or when its own deadline is
// due, and every listed scope is reconciled on a full pass. A scope idle for
// the spacing interval runs at once; signals within the interval coalesce
// into one pass at its end, so a busy scope never runs more often than the
// fixed tick it replaces.
type scopeWakeQueue struct {
	mu        sync.Mutex
	full      bool
	dirty     map[Scope]struct{}
	deadlines map[Scope]time.Time
	lastRun   map[Scope]time.Time
	failures  map[Scope]int
	signal    chan struct{}
}

func newScopeWakeQueue() *scopeWakeQueue {
	return &scopeWakeQueue{dirty: make(map[Scope]struct{}), deadlines: make(map[Scope]time.Time), lastRun: make(map[Scope]time.Time), failures: make(map[Scope]int), signal: make(chan struct{}, 1)}
}

// outcome schedules a failed scope's retry with exponential backoff from
// base, and returns whether the scope succeeded.
func (q *scopeWakeQueue) outcome(scope Scope, err error, base time.Duration, now time.Time) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if err == nil {
		delete(q.failures, scope)
		return true
	}
	q.failures[scope]++
	retry := now.Add(workerPollDelay(base, q.failures[scope]-1, scope.Kind+":"+scope.ID))
	if current, scheduled := q.deadlines[scope]; !scheduled || retry.Before(current) {
		q.deadlines[scope] = retry
	}
	return false
}

// ran records a full pass over scopes.
func (q *scopeWakeQueue) ran(scopes []Scope, now time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, scope := range scopes {
		q.lastRun[scope] = now
	}
}

func (q *scopeWakeQueue) notify() {
	select {
	case q.signal <- struct{}{}:
	default:
	}
}

func (q *scopeWakeQueue) wakeAll() {
	q.mu.Lock()
	q.full = true
	q.mu.Unlock()
	q.notify()
}

func (q *scopeWakeQueue) wakeScope(scope Scope) {
	if scope.Validate() != nil {
		return
	}
	q.mu.Lock()
	q.dirty[scope] = struct{}{}
	q.mu.Unlock()
	q.notify()
}

// setDeadline replaces the scope's durable deadline.
func (q *scopeWakeQueue) setDeadline(scope Scope, deadline *time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if deadline == nil {
		delete(q.deadlines, scope)
		return
	}
	q.deadlines[scope] = *deadline
}

func (q *scopeWakeQueue) clearDeadlines() {
	q.mu.Lock()
	defer q.mu.Unlock()
	clear(q.deadlines)
}

func (q *scopeWakeQueue) nextDeadline() *time.Time {
	q.mu.Lock()
	defer q.mu.Unlock()
	var result *time.Time
	for _, deadline := range q.deadlines {
		if result == nil || deadline.Before(*result) {
			next := deadline
			result = &next
		}
	}
	return result
}

// take drains pending work. Due deadlines join the signalled scopes, and a
// scope that ran within spacing is deferred to the end of that interval.
func (q *scopeWakeQueue) take(now time.Time, spacing time.Duration) (bool, []Scope) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for scope, deadline := range q.deadlines {
		if !deadline.After(now) {
			q.dirty[scope] = struct{}{}
			delete(q.deadlines, scope)
		}
	}
	full := q.full
	q.full = false
	scopes := make([]Scope, 0, len(q.dirty))
	for scope := range q.dirty {
		if last, ran := q.lastRun[scope]; ran && spacing > 0 && now.Sub(last) < spacing {
			next := last.Add(spacing)
			if current, scheduled := q.deadlines[scope]; !scheduled || next.Before(current) {
				q.deadlines[scope] = next
			}
			continue
		}
		q.lastRun[scope] = now
		scopes = append(scopes, scope)
	}
	clear(q.dirty)
	sort.Slice(scopes, func(i, j int) bool {
		if scopes[i].Kind != scopes[j].Kind {
			return scopes[i].Kind < scopes[j].Kind
		}
		return scopes[i].ID < scopes[j].ID
	})
	return full, scopes
}

// knownWorkerScopes filters signalled scopes to the scope source's inventory
// so signal-driven passes never touch a scope a full pass would skip. An
// unknown scope refreshes the inventory at most once per refresh interval.
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

// signalScopeReconcileInterval is how often a supervisor still lists worker
// scopes while work signals are healthy. New scopes are discovered from
// their first signal; this slow pass only retires pools of scopes removed
// from the scope source, which emits no database signal.
const signalScopeReconcileInterval = 10 * time.Minute

// scopeDiscovery decides when a dynamic supervisor lists its worker scopes.
type scopeDiscovery struct {
	mu             sync.Mutex
	lastReconcile  time.Time
	unknownChecked map[string]time.Time
	requests       chan struct{}
}

func newScopeDiscovery() *scopeDiscovery {
	return &scopeDiscovery{unknownChecked: make(map[string]time.Time), requests: make(chan struct{}, 1)}
}

func (d *scopeDiscovery) request() {
	select {
	case d.requests <- struct{}{}:
	default:
	}
}

func (d *scopeDiscovery) reconciled(now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.lastReconcile = now
}

// tickDue keeps every tick while polling and skips ticks while signals are
// healthy until the slow retirement interval has elapsed.
func (d *scopeDiscovery) tickDue(healthy bool, now time.Time) bool {
	if !healthy {
		return true
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return now.Sub(d.lastReconcile) >= signalScopeReconcileInterval
}

// unknown requests a scope listing for a signalled scope without a pool, at
// most once per scope per minimum interval so foreign scopes cannot turn
// every commit into a scope listing.
func (d *scopeDiscovery) unknown(key string, now time.Time, minimum time.Duration) {
	d.mu.Lock()
	checked, seen := d.unknownChecked[key]
	due := !seen || now.Sub(checked) >= minimum
	if due {
		d.unknownChecked[key] = now
	}
	d.mu.Unlock()
	if due {
		d.request()
	}
}

// deadlineTimer fires at a durable deadline while signals are healthy.
type deadlineTimer struct{ timer *time.Timer }

func newDeadlineTimer(healthy bool, deadline *time.Time) deadlineTimer {
	if !healthy || deadline == nil {
		return deadlineTimer{}
	}
	return deadlineTimer{timer: time.NewTimer(max(time.Until(*deadline), 0))}
}

func (t deadlineTimer) channel() <-chan time.Time {
	if t.timer == nil {
		return nil
	}
	return t.timer.C
}

func (t deadlineTimer) stop() {
	if t.timer != nil {
		t.timer.Stop()
	}
}
