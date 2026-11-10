package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/google/uuid"
)

// routineEventPayloadProbe reports whether the stored row of one event still
// holds its payload. Each backend reads its own storage.
type routineEventPayloadProbe func(t *testing.T, scope Scope, activationID, eventID string) bool

func TestMemoryRoutineEventStore(t *testing.T) {
	runRoutineEventStoreContract(t, func(t *testing.T) (KernelStore, routineEventPayloadProbe) {
		store := NewMemoryStore()
		return store, func(t *testing.T, scope Scope, activationID, eventID string) bool {
			store.mu.RLock()
			defer store.mu.RUnlock()
			record := store.routineEvents[memoryRoutineEventKey{scope: scope, activationID: activationID, eventID: eventID}]
			if record == nil {
				t.Fatalf("event %s is not stored", eventID)
			}
			return len(record.Event.Payload) > 0 || record.Event.Summary != ""
		}
	})
}

func TestSQLiteRoutineEventStore(t *testing.T) {
	runRoutineEventStoreContract(t, func(t *testing.T) (KernelStore, routineEventPayloadProbe) {
		store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "routine-events.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		return store, func(t *testing.T, scope Scope, activationID, eventID string) bool {
			var hasPayload bool
			if err := store.db.QueryRow(`SELECT payload IS NOT NULL FROM routine_events WHERE scope_kind = ? AND scope_id = ? AND activation_id = ? AND event_id = ?`,
				scope.Kind, scope.ID, activationID, eventID).Scan(&hasPayload); err != nil {
				t.Fatalf("read event %s: %v", eventID, err)
			}
			return hasPayload
		}
	})
}

// A SQLite store opened before migration 80 gains the index columns, and an
// event Task written before them is filled in.
func TestSQLiteRoutineEventsMigrationFillsIndexColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kernel.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{Kind: "tenant", ID: "migrate"}
	activation := routineEventTestActivation(scope, "activation-old", time.Now().UTC())
	if err := store.CreateRunbookActivation(t.Context(), activation); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE runbook_activations SET event_kind = NULL, event_source_ref = NULL`); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	reopened, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	keys, err := reopened.ListRoutineEventSourceKeysInScope(t.Context(), scope)
	if err != nil || len(keys) != 1 || keys[0].SourceRef != activation.Task.Event.SourceRef {
		t.Fatalf("keys after reopen = %#v, %v", keys, err)
	}
}

func routineEventTestActivation(scope Scope, id string, at time.Time) *RunbookActivation {
	hookID := HookID(scope, id)
	return &RunbookActivation{
		ID: id, Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent-1"}, ObjectiveID: "objective-1",
		AssignedAgentID: "agent-1", TriggerID: "event", Trigger: runbook.Trigger{Kind: runbook.TriggerEvent},
		Task: &ScheduledAgentTask{
			Goal: "Summarize the new order.", ConversationID: "conversation-1", SourceMessageID: "message-1", CreatedBy: "user-1",
			Event: &RoutineEventTrigger{
				Kind: RoutineEventWebhook, SourceRef: HookSourceRef(hookID), SourceLabel: "your shop", MaxPerHour: 2,
				Webhook: &RoutineWebhook{HookID: hookID},
			},
		},
		Status: RunbookActivationActive, Revision: 1, CreatedAt: at, UpdatedAt: at,
	}
}

func routineCalendarTestActivation(scope Scope, id string, at time.Time) *RunbookActivation {
	activation := routineEventTestActivation(scope, id, at)
	activation.Task.Event = &RoutineEventTrigger{
		Kind: RoutineEventCalendar, SourceRef: RoutineGoogleSourceRef("user-1", "sub-1"), SourceLabel: "your calendar", MaxPerHour: 20,
		Calendar: &RoutineCalendarTiming{On: "created"},
	}
	return activation
}

func routineTestEvent(scope Scope, activation *RunbookActivation, id string, at time.Time) RoutineEvent {
	return RoutineEvent{
		Scope: scope, ID: id, Kind: activation.Task.Event.Kind, SourceRef: activation.Task.Event.SourceRef,
		OccurredAt: at, Summary: "Order " + id, Fields: map[string][]string{"payload.kind": {"order"}},
		Payload: json.RawMessage(`{"order":"` + id + `"}`),
	}
}

type routineEventContract struct {
	t      *testing.T
	ctx    context.Context
	store  KernelStore
	probe  routineEventPayloadProbe
	scope  Scope
	now    time.Time
	serial int
}

func (c *routineEventContract) activation(id string, build func(Scope, string, time.Time) *RunbookActivation) *RunbookActivation {
	c.t.Helper()
	activation := build(c.scope, id, c.now.Add(-time.Hour))
	if err := c.store.CreateRunbookActivation(c.ctx, activation); err != nil {
		c.t.Fatal(err)
	}
	return activation
}

func (c *routineEventContract) setStatus(activation *RunbookActivation, status RunbookActivationStatus, at time.Time) *RunbookActivation {
	c.t.Helper()
	current, err := c.store.GetRunbookActivation(c.ctx, activation.Scope, activation.ID)
	if err != nil || current == nil {
		c.t.Fatalf("get activation: %#v, %v", current, err)
	}
	next := cloneRunbookActivation(current)
	next.Status, next.Revision, next.UpdatedAt = status, current.Revision+1, at
	if err := c.store.UpdateRunbookActivation(c.ctx, next, current.Revision); err != nil {
		c.t.Fatal(err)
	}
	return next
}

// insert stores an event received `ago` before now.
func (c *routineEventContract) insert(activation *RunbookActivation, id string, ago time.Duration, state RoutineEventState, reason string, test bool) bool {
	c.t.Helper()
	at := c.now.Add(-ago)
	event := routineTestEvent(c.scope, activation, id, at)
	if test {
		event.Test, event.TestBy = true, "user-1"
	}
	inserted, err := c.store.InsertRoutineEvent(c.ctx, &RoutineEventRecord{
		Scope: c.scope, ActivationID: activation.ID, Event: event, State: state, DroppedReason: reason, ReceivedAt: at,
	})
	if err != nil {
		c.t.Fatal(err)
	}
	return inserted
}

func (c *routineEventContract) pending(activation *RunbookActivation, count int, ago time.Duration, test bool) []string {
	c.t.Helper()
	ids := make([]string, 0, count)
	for i := 0; i < count; i++ {
		c.serial++
		id := fmt.Sprintf("event-%04d", c.serial)
		// Older first: event i is received before event i+1.
		if !c.insert(activation, id, ago-time.Duration(i)*time.Millisecond, RoutineEventPending, "", test) {
			c.t.Fatalf("event %s was a duplicate", id)
		}
		ids = append(ids, id)
	}
	return ids
}

func (c *routineEventContract) claim(activation *RunbookActivation) *RoutineEventBatch {
	c.t.Helper()
	return c.claimAt(activation, c.now)
}

func (c *routineEventContract) claimAt(activation *RunbookActivation, at time.Time) *RoutineEventBatch {
	c.t.Helper()
	batch, err := c.store.ClaimRoutineEventBatch(c.ctx, c.scope, activation.ID, RoutineEventMaxBatchEvents, at)
	if err != nil {
		c.t.Fatal(err)
	}
	return batch
}

func (c *routineEventContract) start(batch *RoutineEventBatch, runID string, at time.Time) {
	c.t.Helper()
	if err := c.store.MarkRoutineEventBatchStarted(c.ctx, c.scope, batch.ID, runID, at); err != nil {
		c.t.Fatal(err)
	}
}

func (c *routineEventContract) run() *AgentRun {
	c.t.Helper()
	run, err := NewPortfolioService(c.store).CreateAgentRun(c.ctx, CreateAgentRunRequest{
		Scope: c.scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent-1"}, AssignedAgentID: "agent-1", Goal: "handle the events", Source: RunSourceObjective,
	})
	if err != nil {
		c.t.Fatal(err)
	}
	return run
}

func (c *routineEventContract) cancelRun(run *AgentRun) {
	c.t.Helper()
	current, err := c.store.GetAgentRun(c.ctx, c.scope, run.ID)
	if err != nil {
		c.t.Fatal(err)
	}
	updated := cloneAgentRun(current)
	updated.Status, updated.Revision, updated.UpdatedAt = AgentRunStatusCanceled, current.Revision+1, c.now
	updated.CompletedAt = &c.now
	event := &ActivityEvent{
		ID: "cancel-" + run.ID, Scope: c.scope, EventType: "run.status_changed", Severity: ActivitySeverityInfo, AgentID: "agent-1", RunID: run.ID,
		Actor: ActivityActor{Type: "worker", ID: "test"}, Summary: "Canceled", Visibility: ActivityVisibilityScope, CreatedAt: c.now,
	}
	if _, err := c.store.UpdateAgentRunWithEvent(c.ctx, updated, current.Revision, event, nil); err != nil {
		c.t.Fatal(err)
	}
}

func (c *routineEventContract) pendingCount(activation *RunbookActivation) int {
	c.t.Helper()
	count, err := c.store.CountPendingRoutineEvents(c.ctx, c.scope, activation.ID)
	if err != nil {
		c.t.Fatal(err)
	}
	return count
}

func runRoutineEventStoreContract(t *testing.T, open func(t *testing.T) (KernelStore, routineEventPayloadProbe)) {
	cases := []struct {
		name string
		run  func(c *routineEventContract)
	}{
		{"dedupe on activation and event ID", func(c *routineEventContract) {
			first := c.activation("first", routineEventTestActivation)
			second := c.activation("second", routineEventTestActivation)
			if !c.insert(first, "same", 0, RoutineEventPending, "", false) || c.insert(first, "same", 0, RoutineEventPending, "", false) {
				c.t.Fatal("the second insert of one event must be a duplicate")
			}
			if !c.insert(second, "same", 0, RoutineEventPending, "", false) {
				c.t.Fatal("another routine gets its own copy of the event")
			}
			if c.insert(first, "same", 0, RoutineEventDropped, RoutineEventDroppedOverflow, false) {
				c.t.Fatal("a dropped copy of a stored event is a duplicate too")
			}
		}},
		{"dropped rows keep no payload", func(c *routineEventContract) {
			activation := c.activation("overflow", routineEventTestActivation)
			c.insert(activation, "kept", 0, RoutineEventPending, "", false)
			c.insert(activation, "over", 0, RoutineEventDropped, RoutineEventDroppedOverflow, false)
			if !c.probe(c.t, c.scope, activation.ID, "kept") || c.probe(c.t, c.scope, activation.ID, "over") {
				c.t.Fatal("pending rows keep their payload, dropped rows keep none")
			}
			if count := c.pendingCount(activation); count != 1 {
				c.t.Fatalf("pending = %d, want 1", count)
			}
			if _, err := c.store.InsertRoutineEvent(c.ctx, &RoutineEventRecord{Scope: c.scope, ActivationID: activation.ID,
				Event: routineTestEvent(c.scope, activation, "bad", c.now), State: RoutineEventDropped}); err == nil {
				c.t.Fatal("a dropped row needs a reason")
			}
			if _, err := c.store.InsertRoutineEvent(c.ctx, &RoutineEventRecord{Scope: c.scope, ActivationID: activation.ID,
				Event: routineTestEvent(c.scope, activation, "bad", c.now), State: RoutineEventStarted}); err == nil {
				c.t.Fatal("a row cannot be inserted as started")
			}
		}},
		{"claim forms at most 50 oldest first and returns the open batch again", func(c *routineEventContract) {
			activation := c.activation("burst", routineEventTestActivation)
			ids := c.pending(activation, 60, time.Minute, false)
			if count, err := c.store.CountPendingRoutineEventsInScope(c.ctx, c.scope); err != nil || count != 60 {
				c.t.Fatalf("pending in scope = %d, %v", count, err)
			}
			work, err := c.store.ListRoutineEventWork(c.ctx, c.scope, "", 10)
			if err != nil || len(work) != 1 || work[0] != activation.ID {
				c.t.Fatalf("work = %v, %v", work, err)
			}
			if after, err := c.store.ListRoutineEventWork(c.ctx, c.scope, activation.ID, 10); err != nil || len(after) != 0 {
				c.t.Fatalf("work after the only routine = %v, %v", after, err)
			}
			if scopes, err := c.store.ListRoutineEventWorkScopes(c.ctx); err != nil || len(scopes) != 1 || scopes[0] != c.scope {
				c.t.Fatalf("work scopes = %v, %v", scopes, err)
			}
			batch := c.claim(activation)
			if batch == nil || len(batch.EventIDs) != 50 || batch.Test || batch.StartedAt != nil {
				c.t.Fatalf("batch = %#v", batch)
			}
			for i, id := range batch.EventIDs {
				if id != ids[i] {
					c.t.Fatalf("batch event %d = %s, want %s", i, id, ids[i])
				}
			}
			again := c.claim(activation)
			if again == nil || again.ID != batch.ID || len(again.EventIDs) != 50 {
				c.t.Fatalf("second claim = %#v, want the open batch %s", again, batch.ID)
			}
			open, err := c.store.GetOpenRoutineEventBatch(c.ctx, c.scope, activation.ID)
			if err != nil || open == nil || open.ID != batch.ID {
				c.t.Fatalf("open batch = %#v, %v", open, err)
			}
			if count := c.pendingCount(activation); count != 60 {
				c.t.Fatalf("pending with an open batch = %d, want 60", count)
			}
			run := c.run()
			c.start(batch, run.ID, c.now)
			c.start(batch, run.ID, c.now) // a replay with the same Run is fine
			if err := c.store.MarkRoutineEventBatchStarted(c.ctx, c.scope, batch.ID, "another-run", c.now); !errors.Is(err, ErrRoutineEventBatchStarted) {
				c.t.Fatalf("start with another Run = %v", err)
			}
			if err := c.store.MarkRoutineEventBatchStarted(c.ctx, c.scope, "reb-missing", run.ID, c.now); !errors.Is(err, ErrRoutineEventBatchNotFound) {
				c.t.Fatalf("start of a missing batch = %v", err)
			}
			got, records, err := c.store.GetRoutineEventBatch(c.ctx, c.scope, batch.ID)
			if err != nil || got == nil || got.RunID != run.ID || got.StartedAt == nil || len(records) != 50 {
				c.t.Fatalf("started batch = %#v, %d records, %v", got, len(records), err)
			}
			if records[0].Event.ID != ids[0] || records[0].State != RoutineEventStarted || records[0].BatchID != batch.ID ||
				records[0].Event.Summary != "Order "+ids[0] || string(records[0].Event.Payload) == "" {
				c.t.Fatalf("first record = %#v", records[0])
			}
			if count := c.pendingCount(activation); count != 10 {
				c.t.Fatalf("pending after start = %d, want 10", count)
			}
			rest := c.claim(activation)
			if rest == nil || len(rest.EventIDs) != 10 || rest.EventIDs[0] != ids[50] {
				c.t.Fatalf("next batch = %#v", rest)
			}
			if missing, records, err := c.store.GetRoutineEventBatch(c.ctx, c.scope, "reb-missing"); err != nil || missing != nil || records != nil {
				c.t.Fatalf("missing batch = %#v, %v, %v", missing, records, err)
			}
		}},
		{"test and real events never share a batch", func(c *routineEventContract) {
			activation := c.activation("mixed", routineEventTestActivation)
			real := c.pending(activation, 2, 2*time.Minute, false)
			test := c.pending(activation, 1, time.Minute, true)
			batch := c.claim(activation)
			if batch == nil || !batch.Test || len(batch.EventIDs) != 1 || batch.EventIDs[0] != test[0] {
				c.t.Fatalf("first batch = %#v, want the test event alone", batch)
			}
			c.start(batch, c.run().ID, c.now)
			batch = c.claim(activation)
			if batch == nil || batch.Test || len(batch.EventIDs) != 2 || batch.EventIDs[0] != real[0] {
				c.t.Fatalf("second batch = %#v, want the two real events", batch)
			}
			c.start(batch, c.run().ID, c.now)
			if batch := c.claim(activation); batch != nil {
				c.t.Fatalf("nothing is pending, got %#v", batch)
			}
			if count, err := c.store.CountRoutineTestEventsSince(c.ctx, c.scope, activation.ID, c.now.Add(-time.Hour)); err != nil || count != 1 {
				c.t.Fatalf("test events in the hour = %d, %v", count, err)
			}
			if count, err := c.store.CountRoutineTestEventsSince(c.ctx, c.scope, activation.ID, c.now); err != nil || count != 0 {
				c.t.Fatalf("test events since now = %d, %v", count, err)
			}
		}},
		{"started batch counts since a time", func(c *routineEventContract) {
			activation := c.activation("counts", routineEventTestActivation)
			for _, ago := range []time.Duration{3 * time.Hour, 30 * time.Minute, 10 * time.Minute} {
				c.pending(activation, 1, ago, false)
				c.start(c.claim(activation), c.run().ID, c.now.Add(-ago))
			}
			c.pending(activation, 1, time.Minute, true)
			c.start(c.claim(activation), c.run().ID, c.now)
			c.pending(activation, 1, 0, false)
			c.claim(activation) // open, not started: never counted
			count, oldest, err := c.store.CountRoutineEventBatchesSince(c.ctx, c.scope, activation.ID, time.Time{}, false)
			if err != nil || count != 3 || oldest == nil || !oldest.Equal(c.now.Add(-3*time.Hour)) {
				c.t.Fatalf("all real = %d, %v, %v", count, oldest, err)
			}
			count, oldest, err = c.store.CountRoutineEventBatchesSince(c.ctx, c.scope, activation.ID, c.now.Add(-time.Hour), false)
			if err != nil || count != 2 || oldest == nil || !oldest.Equal(c.now.Add(-30*time.Minute)) {
				c.t.Fatalf("real in the hour = %d, %v, %v", count, oldest, err)
			}
			count, _, err = c.store.CountRoutineEventBatchesSince(c.ctx, c.scope, activation.ID, time.Time{}, true)
			if err != nil || count != 1 {
				c.t.Fatalf("test batches = %d, %v", count, err)
			}
			count, oldest, err = c.store.CountRoutineEventBatchesSince(c.ctx, c.scope, "nothing", time.Time{}, false)
			if err != nil || count != 0 || oldest != nil {
				c.t.Fatalf("unknown activation = %d, %v, %v", count, oldest, err)
			}
		}},
		{"overflow drops are counted in the next real batch", func(c *routineEventContract) {
			activation := c.activation("dropped-count", routineEventTestActivation)
			c.pending(activation, 1, 10*time.Minute, false)
			c.start(c.claimAt(activation, c.now.Add(-9*time.Minute)), c.run().ID, c.now.Add(-9*time.Minute))
			c.insert(activation, "over-1", 5*time.Minute, RoutineEventDropped, RoutineEventDroppedOverflow, false)
			c.insert(activation, "over-2", 4*time.Minute, RoutineEventDropped, RoutineEventDroppedOverflow, false)
			c.insert(activation, "paused-1", 3*time.Minute, RoutineEventDropped, RoutineEventDroppedPaused, false)
			c.pending(activation, 1, time.Minute, false)
			batch := c.claim(activation)
			if batch == nil || batch.Dropped != 2 {
				c.t.Fatalf("batch = %#v, want 2 overflow drops", batch)
			}
		}},
		{"drop on pause removes pending events and the open batch", func(c *routineEventContract) {
			activation := c.activation("paused", routineEventTestActivation)
			ids := c.pending(activation, 3, time.Minute, false)
			batch := c.claim(activation)
			c.pending(activation, 2, 0, false)
			dropped, err := c.store.DropRoutineEvents(c.ctx, c.scope, activation.ID, RoutineEventDroppedPaused)
			if err != nil || dropped != 5 {
				c.t.Fatalf("dropped = %d, %v", dropped, err)
			}
			if open, err := c.store.GetOpenRoutineEventBatch(c.ctx, c.scope, activation.ID); err != nil || open != nil {
				c.t.Fatalf("open batch after drop = %#v, %v", open, err)
			}
			if gone, _, err := c.store.GetRoutineEventBatch(c.ctx, c.scope, batch.ID); err != nil || gone != nil {
				c.t.Fatalf("batch row after drop = %#v, %v", gone, err)
			}
			if count := c.pendingCount(activation); count != 0 {
				c.t.Fatalf("pending after drop = %d", count)
			}
			if c.probe(c.t, c.scope, activation.ID, ids[0]) {
				c.t.Fatal("a dropped event keeps no payload")
			}
			if c.insert(activation, ids[0], 0, RoutineEventPending, "", false) {
				c.t.Fatal("a dropped event still dedupes")
			}
			if _, err := c.store.DropRoutineEvents(c.ctx, c.scope, activation.ID, "bored"); err == nil {
				c.t.Fatal("an unknown drop reason is refused")
			}
			// After a resume the next event forms a fresh batch.
			c.pending(activation, 1, 0, false)
			if next := c.claim(activation); next == nil || next.ID == batch.ID || len(next.EventIDs) != 1 {
				c.t.Fatalf("batch after resume = %#v", next)
			}
		}},
		{"activity numbers", func(c *routineEventContract) {
			limited := c.activation("limited", routineEventTestActivation) // maxPerHour 2
			quiet := c.activation("quiet", routineEventTestActivation)
			for _, ago := range []time.Duration{5 * time.Hour, 40 * time.Minute, 20 * time.Minute} {
				c.pending(limited, 1, ago, false)
				c.start(c.claim(limited), c.run().ID, c.now.Add(-ago))
			}
			c.pending(limited, 1, time.Minute, true)
			c.start(c.claim(limited), c.run().ID, c.now) // test batches never count
			c.pending(limited, 2, 0, false)
			c.insert(limited, "over", time.Hour, RoutineEventDropped, RoutineEventDroppedOverflow, false)
			c.insert(limited, "old-over", 30*time.Hour, RoutineEventDropped, RoutineEventDroppedOverflow, false)
			activity, err := c.store.RoutineEventActivity(c.ctx, c.scope, []string{limited.ID, quiet.ID, limited.ID, ""}, c.now)
			if err != nil || len(activity) != 2 {
				c.t.Fatalf("activity = %#v, %v", activity, err)
			}
			got := activity[limited.ID]
			wantNext := c.now.Add(-40 * time.Minute).Add(time.Hour)
			if got.ActivationID != limited.ID || got.LastFiredAt == nil || !got.LastFiredAt.Equal(c.now.Add(-20*time.Minute)) ||
				got.FiredLast24h != 3 || got.FiredLastHour != 2 || got.Pending != 2 || got.DroppedLast24h != 1 ||
				got.NextRunAt == nil || !got.NextRunAt.Equal(wantNext) {
				c.t.Fatalf("limited activity = %+v", got)
			}
			if got := activity[quiet.ID]; got != (RoutineEventActivity{ActivationID: quiet.ID}) {
				c.t.Fatalf("quiet activity = %+v", got)
			}
			// Under the hourly limit there is no next run time.
			activity, err = c.store.RoutineEventActivity(c.ctx, c.scope, []string{limited.ID}, c.now.Add(30*time.Minute))
			if err != nil || activity[limited.ID].FiredLastHour != 1 || activity[limited.ID].NextRunAt != nil {
				c.t.Fatalf("activity later = %+v, %v", activity[limited.ID], err)
			}
		}},
		{"active event Runs of the last day", func(c *routineEventContract) {
			activation := c.activation("runs", routineEventTestActivation)
			live, canceled, old := c.run(), c.run(), c.run()
			c.pending(activation, 1, time.Minute, false)
			c.start(c.claim(activation), live.ID, c.now)
			c.pending(activation, 1, time.Minute, false)
			c.start(c.claim(activation), canceled.ID, c.now)
			c.pending(activation, 1, time.Minute, false)
			c.start(c.claim(activation), old.ID, c.now.Add(-25*time.Hour))
			c.cancelRun(canceled)
			if count, err := c.store.CountActiveRoutineEventRuns(c.ctx, c.scope); err != nil || count != 1 {
				c.t.Fatalf("active event Runs = %d, %v", count, err)
			}
			if count, err := c.store.CountActiveRoutineEventRuns(c.ctx, Scope{Kind: "tenant", ID: "elsewhere"}); err != nil || count != 0 {
				c.t.Fatalf("active event Runs elsewhere = %d, %v", count, err)
			}
		}},
		{"event index follows create, pause, resume and retire", func(c *routineEventContract) {
			webhook := c.activation("hooked", routineEventTestActivation)
			calendar := c.activation("calendar", routineCalendarTestActivation)
			c.activation("calendar-2", routineCalendarTestActivation)
			list := func(activation *RunbookActivation) []*RunbookActivation {
				values, err := c.store.ListRoutineEventActivations(c.ctx, c.scope, activation.Task.Event.Kind, activation.Task.Event.SourceRef)
				if err != nil {
					c.t.Fatal(err)
				}
				return values
			}
			keys := func() []RoutineEventSourceKey {
				values, err := c.store.ListRoutineEventSourceKeysInScope(c.ctx, c.scope)
				if err != nil {
					c.t.Fatal(err)
				}
				return values
			}
			if got := list(calendar); len(got) != 2 || got[0].ID != "calendar" || got[1].ID != "calendar-2" {
				c.t.Fatalf("calendar routines = %d", len(got))
			}
			if got := list(webhook); len(got) != 1 || got[0].ID != webhook.ID || got[0].Task.Event.Webhook == nil {
				c.t.Fatalf("webhook routines = %#v", got)
			}
			if got := keys(); len(got) != 2 || got[0].Kind != RoutineEventCalendar || got[1].Kind != RoutineEventWebhook || got[1].Scope != c.scope {
				c.t.Fatalf("keys = %#v", got)
			}
			c.setStatus(webhook, RunbookActivationPaused, c.now)
			if got := list(webhook); len(got) != 0 {
				c.t.Fatalf("a paused routine is not listed: %d", len(got))
			}
			if got := keys(); len(got) != 1 || got[0].Kind != RoutineEventCalendar {
				c.t.Fatalf("keys while paused = %#v", got)
			}
			c.setStatus(webhook, RunbookActivationActive, c.now)
			if got := list(webhook); len(got) != 1 {
				c.t.Fatalf("a resumed routine is listed again: %d", len(got))
			}
			c.setStatus(webhook, RunbookActivationRetired, c.now)
			if got := list(webhook); len(got) != 0 {
				c.t.Fatalf("a retired routine is not listed: %d", len(got))
			}
			// A schedule Task never enters the index.
			schedule := routineEventTestActivation(c.scope, "schedule", c.now)
			schedule.Task.Event = nil
			schedule.Trigger = runbook.Trigger{Kind: runbook.TriggerSchedule, Schedule: &runbook.Schedule{Cron: "0 0 9 * * *", Timezone: "UTC"}}
			if err := c.store.CreateRunbookActivation(c.ctx, schedule); err != nil {
				c.t.Fatal(err)
			}
			if got := keys(); len(got) != 1 {
				c.t.Fatalf("keys with a schedule = %#v", got)
			}
		}},
		{"hooks", func(c *routineEventContract) {
			activation := c.activation("hook-owner", routineEventTestActivation)
			hookID := HookID(c.scope, activation.ID)
			created, err := c.store.CreateHook(c.ctx, &Hook{ID: hookID, Scope: c.scope, ActivationID: activation.ID, CreatedAt: c.now})
			if err != nil || created.Revision != 1 || created.PathTokenEnabled || !created.RotatedAt.Equal(c.now) {
				c.t.Fatalf("created = %#v, %v", created, err)
			}
			again, err := c.store.CreateHook(c.ctx, &Hook{ID: hookID, Scope: c.scope, ActivationID: activation.ID, CreatedAt: c.now.Add(time.Minute)})
			if err != nil || again.Revision != 1 || !again.CreatedAt.Equal(c.now) {
				c.t.Fatalf("create again = %#v, %v", again, err)
			}
			if _, err := c.store.CreateHook(c.ctx, &Hook{ID: hookID, Scope: c.scope, ActivationID: "another"}); !errors.Is(err, ErrHookConflict) {
				c.t.Fatalf("same ID for another routine = %v", err)
			}
			if _, err := c.store.CreateHook(c.ctx, &Hook{ID: HookID(c.scope, "other"), Scope: c.scope, ActivationID: activation.ID}); !errors.Is(err, ErrHookConflict) {
				c.t.Fatalf("second hook for one routine = %v", err)
			}
			if _, err := c.store.CreateHook(c.ctx, &Hook{ID: "not-a-hook", Scope: c.scope, ActivationID: activation.ID}); err == nil {
				c.t.Fatal("a malformed hook ID is refused")
			}
			got, err := c.store.GetHook(c.ctx, hookID)
			if err != nil || got == nil || got.Scope != c.scope || got.ActivationID != activation.ID {
				c.t.Fatalf("get = %#v, %v", got, err)
			}
			if missing, err := c.store.GetHook(c.ctx, HookID(c.scope, "nobody")); err != nil || missing != nil {
				c.t.Fatalf("missing hook = %#v, %v", missing, err)
			}
			enabled := true
			toggled, err := c.store.UpdateHook(c.ctx, c.scope, hookID, 1, false, &enabled, c.now.Add(time.Minute))
			if err != nil || toggled.Revision != 1 || !toggled.PathTokenEnabled || !toggled.RotatedAt.Equal(c.now) {
				c.t.Fatalf("path token on = %#v, %v", toggled, err)
			}
			rotatedAt := c.now.Add(2 * time.Minute)
			rotated, err := c.store.UpdateHook(c.ctx, c.scope, hookID, 1, true, nil, rotatedAt)
			if err != nil || rotated.Revision != 2 || !rotated.PathTokenEnabled || !rotated.RotatedAt.Equal(rotatedAt) {
				c.t.Fatalf("rotated = %#v, %v", rotated, err)
			}
			if _, err := c.store.UpdateHook(c.ctx, c.scope, hookID, 1, true, nil, c.now); !errors.Is(err, ErrHookRevision) {
				c.t.Fatalf("stale rotate = %v", err)
			}
			if _, err := c.store.UpdateHook(c.ctx, Scope{Kind: "tenant", ID: "elsewhere"}, hookID, 2, true, nil, c.now); !errors.Is(err, ErrHookNotFound) {
				c.t.Fatalf("rotate from another scope = %v", err)
			}
			// Prune: hooks of retired or missing routines older than the cutoff go.
			orphan := &Hook{ID: HookID(c.scope, "never-created"), Scope: c.scope, ActivationID: "never-created", CreatedAt: c.now.Add(-2 * time.Hour)}
			fresh := &Hook{ID: HookID(c.scope, "about-to-exist"), Scope: c.scope, ActivationID: "about-to-exist", CreatedAt: c.now}
			for _, hook := range []*Hook{orphan, fresh} {
				if _, err := c.store.CreateHook(c.ctx, hook); err != nil {
					c.t.Fatal(err)
				}
			}
			cutoff := c.now.Add(-time.Hour)
			if removed, err := c.store.PruneHooks(c.ctx, cutoff); err != nil || removed != 1 {
				c.t.Fatalf("prune = %d, %v", removed, err)
			}
			if got, _ := c.store.GetHook(c.ctx, orphan.ID); got != nil {
				c.t.Fatal("a hook without its routine is pruned")
			}
			if got, _ := c.store.GetHook(c.ctx, fresh.ID); got == nil {
				c.t.Fatal("a hook made just before its routine survives")
			}
			if got, _ := c.store.GetHook(c.ctx, hookID); got == nil {
				c.t.Fatal("the hook of a live routine survives")
			}
			c.setStatus(activation, RunbookActivationRetired, c.now)
			if removed, err := c.store.PruneHooks(c.ctx, c.now.Add(time.Second)); err != nil || removed != 2 {
				c.t.Fatalf("prune after retire = %d, %v", removed, err)
			}
			if got, _ := c.store.GetHook(c.ctx, hookID); got != nil {
				c.t.Fatal("the hook of a retired routine is pruned")
			}
		}},
		{"prune after 7 days", func(c *routineEventContract) {
			week := RoutineEventRetention
			live := c.activation("live", routineEventTestActivation)
			retired := routineEventTestActivation(c.scope, "retired", c.now.Add(-30*24*time.Hour))
			if err := c.store.CreateRunbookActivation(c.ctx, retired); err != nil {
				c.t.Fatal(err)
			}
			old := c.pending(live, 1, week+2*time.Hour, false)
			c.start(c.claim(live), c.run().ID, c.now.Add(-week-time.Hour))
			c.insert(live, "old-dropped", week+time.Hour, RoutineEventDropped, RoutineEventDroppedOverflow, false)
			c.insert(live, "new-dropped", time.Hour, RoutineEventDropped, RoutineEventDroppedOverflow, false)
			oldPending := c.pending(live, 1, week+3*time.Hour, false) // waiting events are never pruned
			// Batches of a routine retired 8 days ago go; so do those of a missing one.
			c.pending(retired, 1, week+3*time.Hour, false)
			retiredBatch := c.claimAt(retired, c.now.Add(-week-3*time.Hour))
			c.start(retiredBatch, c.run().ID, c.now.Add(-week-2*time.Hour))
			c.setStatus(retired, RunbookActivationRetired, c.now.Add(-week-time.Hour))
			ghost := routineEventTestActivation(c.scope, "ghost", c.now)
			c.insert(ghost, "ghost-event", week+3*time.Hour, RoutineEventPending, "", false)
			ghostBatch, err := c.store.ClaimRoutineEventBatch(c.ctx, c.scope, ghost.ID, 50, c.now.Add(-week-2*time.Hour))
			if err != nil || ghostBatch == nil {
				c.t.Fatalf("ghost batch = %#v, %v", ghostBatch, err)
			}
			liveBatches, _, _ := c.store.CountRoutineEventBatchesSince(c.ctx, c.scope, live.ID, time.Time{}, false)
			removed, err := c.store.PruneRoutineEvents(c.ctx, c.now.Add(-week), 1000)
			// Removed: the live routine's old started and dropped events, the
			// retired routine's started event and its batch, the ghost batch.
			if err != nil || removed != 5 {
				c.t.Fatalf("prune = %d, %v", removed, err)
			}
			if c.insert(live, old[0], 0, RoutineEventPending, "", false) != true {
				c.t.Fatal("a pruned event ID can arrive again")
			}
			if c.insert(live, "new-dropped", 0, RoutineEventPending, "", false) {
				c.t.Fatal("a recent dropped event still dedupes")
			}
			if c.insert(live, oldPending[0], 0, RoutineEventPending, "", false) {
				c.t.Fatal("a waiting event is never pruned")
			}
			if count, _, _ := c.store.CountRoutineEventBatchesSince(c.ctx, c.scope, live.ID, time.Time{}, false); count != liveBatches || count != 1 {
				c.t.Fatalf("batches of a live routine survive: %d", count)
			}
			if gone, _, _ := c.store.GetRoutineEventBatch(c.ctx, c.scope, retiredBatch.ID); gone != nil {
				c.t.Fatal("the batch of a routine retired a week ago is pruned")
			}
			if gone, _, _ := c.store.GetRoutineEventBatch(c.ctx, c.scope, ghostBatch.ID); gone != nil {
				c.t.Fatal("the batch of a missing routine is pruned")
			}
			if removed, err := c.store.PruneRoutineEvents(c.ctx, c.now.Add(-week), 1000); err != nil || removed != 0 {
				c.t.Fatalf("second prune = %d, %v", removed, err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, probe := open(t)
			tc.run(&routineEventContract{
				t: t, ctx: t.Context(), store: store, probe: probe,
				scope: Scope{Kind: "tenant", ID: "routines-" + uuid.NewString()[:8]},
				now:   time.Now().UTC().Truncate(time.Microsecond),
			})
		})
	}
	t.Run("subscriptions across scopes", func(t *testing.T) {
		store, _ := open(t)
		now := time.Now().UTC().Truncate(time.Microsecond)
		prefix := "subscriptions-" + uuid.NewString()[:8]
		first, second := Scope{Kind: "tenant", ID: prefix + "-a"}, Scope{Kind: "tenant", ID: prefix + "-b"}
		for _, activation := range []*RunbookActivation{
			routineEventTestActivation(first, "a-hook", now), routineCalendarTestActivation(first, "b-calendar", now),
			routineCalendarTestActivation(second, "a-calendar", now),
		} {
			if err := store.CreateRunbookActivation(t.Context(), activation); err != nil {
				t.Fatal(err)
			}
		}
		paused := routineCalendarTestActivation(second, "b-paused", now)
		paused.Status = RunbookActivationPaused
		if err := store.CreateRunbookActivation(t.Context(), paused); err != nil {
			t.Fatal(err)
		}
		mine := func(values []RoutineEventSubscription) []RoutineEventSubscription {
			out := make([]RoutineEventSubscription, 0)
			for _, value := range values {
				if value.Scope == first || value.Scope == second {
					out = append(out, value)
				}
			}
			return out
		}
		all := make([]RoutineEventSubscription, 0)
		after := ""
		for pages := 0; ; pages++ {
			page, next, err := store.ListRoutineEventSubscriptions(t.Context(), nil, after, 1)
			if err != nil || pages > 50 {
				t.Fatalf("page %d: %v", pages, err)
			}
			all = append(all, page...)
			if next == "" {
				break
			}
			after = next
		}
		all = mine(all)
		if len(all) != 3 || all[0].ActivationID != "a-hook" || all[1].ActivationID != "b-calendar" || all[2].Scope != second ||
			all[0].Kind != RoutineEventWebhook || all[0].CreatedBy != "user-1" || all[0].Owner.ID != "agent-1" ||
			all[1].Calendar == nil || all[1].Calendar.On != "created" {
			t.Fatalf("subscriptions = %#v", all)
		}
		calendars, _, err := store.ListRoutineEventSubscriptions(t.Context(), []RoutineEventKind{RoutineEventCalendar}, "", 100)
		if err != nil || len(mine(calendars)) != 2 {
			t.Fatalf("calendar subscriptions = %#v, %v", calendars, err)
		}
		if _, _, err := store.ListRoutineEventSubscriptions(t.Context(), []RoutineEventKind{"fax"}, "", 10); err == nil {
			t.Fatal("an unknown kind is refused")
		}
	})
}
