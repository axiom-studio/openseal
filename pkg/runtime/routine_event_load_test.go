package runtime

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime/externalconversation"
)

// countingRoutineEventStore fails the test on any store call: an offer for
// a source no routine listens to must not reach the store.
type countingRoutineEventStore struct {
	RoutineEventStore
	calls int
}

func (s *countingRoutineEventStore) GetRunbookActivation(context.Context, Scope, string) (*RunbookActivation, error) {
	s.calls++
	return nil, nil
}

func (s *countingRoutineEventStore) ListRoutineEventActivations(context.Context, Scope, RoutineEventKind, string) ([]*RunbookActivation, error) {
	s.calls++
	return nil, nil
}

func (s *countingRoutineEventStore) InsertRoutineEvent(context.Context, *RoutineEventRecord) (bool, error) {
	s.calls++
	return false, nil
}

func (s *countingRoutineEventStore) CountPendingRoutineEvents(context.Context, Scope, string) (int, error) {
	s.calls++
	return 0, nil
}

func (s *countingRoutineEventStore) CountPendingRoutineEventsInScope(context.Context, Scope) (int, error) {
	s.calls++
	return 0, nil
}

func (s *countingRoutineEventStore) ListRoutineEventSourceKeysInScope(_ context.Context, scope Scope) ([]RoutineEventSourceKey, error) {
	return []RoutineEventSourceKey{{Scope: scope, Kind: RoutineEventSlackMessage, SourceRef: RoutineConversationSourceRef("deployment", "bound", "slack", "listening")}}, nil
}

func unboundRoutineEvent() *RoutineEvent {
	return &RoutineEvent{
		Scope: Scope{Kind: "tenant", ID: "busy"}, ID: "slack:message:T1:C1:1.0", Kind: RoutineEventSlackMessage,
		SourceRef: RoutineConversationSourceRef("deployment", "binding", "slack", "installation"), OccurredAt: time.Unix(1, 0).UTC(),
		Summary: "Message in #general", Fields: map[string][]string{"channel": {"C1"}, "text": {"hello"}},
	}
}

func newUnboundRoutineEventMatcher(tb testing.TB) (*RoutineEventMatcher, *countingRoutineEventStore) {
	tb.Helper()
	store := &countingRoutineEventStore{}
	matcher := NewRoutineEventMatcher(store)
	tb.Cleanup(matcher.Close)
	// The scope has a routine, but on another connection.
	if err := matcher.RefreshRoutineIndex(context.Background(), Scope{Kind: "tenant", ID: "busy"}); err != nil {
		tb.Fatal(err)
	}
	return matcher, store
}

func TestRoutineEventOfferUnboundAllocFree(t *testing.T) {
	matcher, store := newUnboundRoutineEventMatcher(t)
	event := unboundRoutineEvent()
	ctx := context.Background()
	allocs := testing.AllocsPerRun(1000, func() {
		if matcher.Listening(event.Scope, event.Kind, event.SourceRef) {
			t.Fatal("unbound source is listening")
		}
		offer, err := matcher.OfferRoutineEvent(ctx, event)
		if err != nil || offer != (externalconversation.RoutineEventOffer{}) {
			t.Fatalf("offer=%#v err=%v", offer, err)
		}
	})
	if allocs != 0 {
		t.Fatalf("unbound offer allocates %.1f times", allocs)
	}
	if store.calls != 0 {
		t.Fatalf("unbound offer made %d store calls", store.calls)
	}
	stats := matcher.RoutineEventStats()
	if stats.In[RoutineEventSlackMessage] == 0 || stats.Matched[RoutineEventSlackMessage] != 0 || stats.MatchLatencyCounts[len(stats.MatchLatencyCounts)-1] != 0 {
		t.Fatalf("stats after unbound offers: in=%d matched=%d latency=%v", stats.In[RoutineEventSlackMessage], stats.Matched[RoutineEventSlackMessage], stats.MatchLatencyCounts)
	}
}

func BenchmarkRoutineEventOfferUnbound(b *testing.B) {
	matcher, _ := newUnboundRoutineEventMatcher(b)
	event := unboundRoutineEvent()
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := matcher.OfferRoutineEvent(ctx, event); err != nil {
			b.Fatal(err)
		}
	}
}

func TestRoutineEventTenantRunCap(t *testing.T) {
	h := newRoutineEventHarness(t)
	routines := make([]*RunbookActivation, 0, 12)
	for i := 0; i < 12; i++ {
		activation := h.routine(fmt.Sprintf("r%02d", i), nil)
		h.offer(activation, 1)
		routines = append(routines, activation)
	}
	result := h.pass()
	if result.Started != RoutineEventTenantRunCap || result.TenantCapped != 1 || h.activeEventRuns() != RoutineEventTenantRunCap {
		t.Fatalf("first pass = %#v, active %d", result, h.activeEventRuns())
	}
	if result := h.pass(); result.Started != 0 || h.activeEventRuns() != RoutineEventTenantRunCap {
		t.Fatalf("second pass = %#v, active %d", result, h.activeEventRuns())
	}
	// Finish three Runs: the two waiting routines start, and no more.
	finished := 0
	for _, activation := range routines {
		for _, run := range h.eventRuns(activation) {
			if finished < 3 && !isTerminalAgentRunStatus(run.Status) {
				h.finishRun(run)
				finished++
			}
		}
	}
	if result := h.pass(); result.Started != 2 || h.activeEventRuns() != 9 {
		t.Fatalf("pass after three finished = %#v, active %d", result, h.activeEventRuns())
	}
	for _, activation := range routines {
		if runs := h.eventRuns(activation); len(runs) != 1 {
			t.Fatalf("routine %s has %d runs", activation.ID, len(runs))
		}
	}
	// A smaller cap set on the dispatcher applies.
	h.dispatcher.TenantRunCap = 8
	h.finishRun(h.eventRuns(routines[3])[0])
	h.offer(routines[3], 1)
	if result := h.pass(); result.Started != 0 || result.TenantCapped != 1 || h.activeEventRuns() != 8 {
		t.Fatalf("pass under cap 8 = %#v, active %d", result, h.activeEventRuns())
	}
}

// TestRoutineEventBurstRespectsCaps sends 1 000 events to 20 routines of one
// tenant (520 to one of them, so it overflows) while Runs finish over time.
func TestRoutineEventBurstRespectsCaps(t *testing.T) {
	h, db := newRoutineEventRowsHarness(t)
	const maxPerHour = 3
	routines := make([]*RunbookActivation, 0, 20)
	for i := 0; i < 20; i++ {
		routines = append(routines, h.routine(fmt.Sprintf("burst%02d", i), func(trigger *RoutineEventTrigger) { trigger.MaxPerHour = maxPerHour }))
	}
	for i := 0; i < 520; i++ {
		h.offer(routines[0], 1)
	}
	for i := 0; i < 480; i++ {
		h.offer(routines[1+i%19], 1)
	}
	starts := map[string][]time.Time{}
	runStartedAt := map[string]time.Time{}
	for minute := 0; minute < 180; minute++ {
		h.pass()
		if active := h.activeEventRuns(); active > RoutineEventTenantRunCap {
			t.Fatalf("minute %d: %d event Runs are not finished", minute, active)
		}
		for _, activation := range routines {
			for _, run := range h.eventRuns(activation) {
				if _, ok := runStartedAt[run.ID]; !ok {
					runStartedAt[run.ID] = h.now
					starts[activation.ID] = append(starts[activation.ID], h.now)
				}
				// Each Run takes three minutes.
				if !isTerminalAgentRunStatus(run.Status) && h.now.Sub(runStartedAt[run.ID]) >= 3*time.Minute {
					h.finishRun(run)
				}
			}
		}
		h.advance(time.Minute)
	}
	for id, values := range starts {
		for i := range values {
			inHour := 0
			for _, other := range values {
				if !other.Before(values[i]) && other.Before(values[i].Add(time.Hour)) {
					inHour++
				}
			}
			if inHour > maxPerHour {
				t.Fatalf("routine %s started %d batches within an hour", id, inHour)
			}
		}
	}
	seen := map[string]bool{}
	for _, activation := range routines {
		for _, run := range h.eventRuns(activation) {
			batch, _ := h.batchOf(run)
			if len(batch.EventIDs) > RoutineEventMaxBatchEvents {
				t.Fatalf("batch %s holds %d events", batch.ID, len(batch.EventIDs))
			}
			for _, id := range batch.EventIDs {
				key := activation.ID + "/" + id
				if seen[key] {
					t.Fatalf("event %s is in two batches", key)
				}
				seen[key] = true
			}
		}
	}
	overflow := 0
	for _, record := range listRoutineEventRows(t, db, h.scope) {
		if record.State == RoutineEventDropped && record.DroppedReason == RoutineEventDroppedOverflow {
			overflow++
			if record.KeepsData {
				t.Fatalf("overflow row keeps event data: %#v", record)
			}
		}
	}
	if overflow != 20 {
		t.Fatalf("overflow rows = %d, want 20", overflow)
	}
	if len(seen) == 0 {
		t.Fatal("no batch started")
	}
}
