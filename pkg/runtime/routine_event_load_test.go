package runtime

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/externalconversation"
	"github.com/axiom-studio/openseal/pkg/runtime/routine"
	"github.com/axiom-studio/openseal/pkg/runtime/routine/routinetest"
)

// countingRoutineEventStore fails the test on any store call: an offer for
// a source no routine listens to must not reach the store.
type countingRoutineEventStore struct {
	models.RoutineEventStore
	calls int
}

func (s *countingRoutineEventStore) GetRunbookActivation(context.Context, models.Scope, string) (*models.RunbookActivation, error) {
	s.calls++
	return nil, nil
}

func (s *countingRoutineEventStore) ListRoutineEventActivations(context.Context, models.Scope, models.RoutineEventKind, string) ([]*models.RunbookActivation, error) {
	s.calls++
	return nil, nil
}

func (s *countingRoutineEventStore) InsertRoutineEvent(context.Context, *models.RoutineEventRecord) (bool, error) {
	s.calls++
	return false, nil
}

func (s *countingRoutineEventStore) CountPendingRoutineEvents(context.Context, models.Scope, string) (int, error) {
	s.calls++
	return 0, nil
}

func (s *countingRoutineEventStore) CountPendingRoutineEventsInScope(context.Context, models.Scope) (int, error) {
	s.calls++
	return 0, nil
}

func (s *countingRoutineEventStore) ListRoutineEventSourceKeysInScope(_ context.Context, scope models.Scope) ([]models.RoutineEventSourceKey, error) {
	return []models.RoutineEventSourceKey{{Scope: scope, Kind: models.RoutineEventSlackMessage, SourceRef: models.RoutineConversationSourceRef("deployment", "bound", "slack", "listening")}}, nil
}

func unboundRoutineEvent() *models.RoutineEvent {
	return &models.RoutineEvent{
		Scope: models.Scope{Kind: "tenant", ID: "busy"}, ID: "slack:message:T1:C1:1.0", Kind: models.RoutineEventSlackMessage,
		SourceRef: models.RoutineConversationSourceRef("deployment", "binding", "slack", "installation"), OccurredAt: time.Unix(1, 0).UTC(),
		Summary: "Message in #general", Fields: map[string][]string{"channel": {"C1"}, "text": {"hello"}},
	}
}

func newUnboundRoutineEventMatcher(tb testing.TB) (*routine.RoutineEventMatcher, *countingRoutineEventStore) {
	tb.Helper()
	store := &countingRoutineEventStore{}
	matcher := routine.NewRoutineEventMatcher(store)
	tb.Cleanup(matcher.Close)
	// The scope has a routine, but on another connection.
	if err := matcher.RefreshRoutineIndex(context.Background(), models.Scope{Kind: "tenant", ID: "busy"}); err != nil {
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
	if stats.In[models.RoutineEventSlackMessage] == 0 || stats.Matched[models.RoutineEventSlackMessage] != 0 || stats.MatchLatencyCounts[len(stats.MatchLatencyCounts)-1] != 0 {
		t.Fatalf("stats after unbound offers: in=%d matched=%d latency=%v", stats.In[models.RoutineEventSlackMessage], stats.Matched[models.RoutineEventSlackMessage], stats.MatchLatencyCounts)
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
	h := routinetest.NewEventHarness(t)
	routines := make([]*models.RunbookActivation, 0, 12)
	for i := 0; i < 12; i++ {
		activation := h.Routine(fmt.Sprintf("r%02d", i), nil)
		h.Offer(activation, 1)
		routines = append(routines, activation)
	}
	result := h.Pass()
	if result.Started != routine.RoutineEventTenantRunCap || result.TenantCapped != 1 || h.ActiveEventRuns() != routine.RoutineEventTenantRunCap {
		t.Fatalf("first pass = %#v, active %d", result, h.ActiveEventRuns())
	}
	if result := h.Pass(); result.Started != 0 || h.ActiveEventRuns() != routine.RoutineEventTenantRunCap {
		t.Fatalf("second pass = %#v, active %d", result, h.ActiveEventRuns())
	}
	// Finish three Runs: the two waiting routines start, and no more.
	finished := 0
	for _, activation := range routines {
		for _, run := range h.EventRuns(activation) {
			if finished < 3 && !models.IsTerminalAgentRunStatus(run.Status) {
				h.FinishRun(run)
				finished++
			}
		}
	}
	if result := h.Pass(); result.Started != 2 || h.ActiveEventRuns() != 9 {
		t.Fatalf("pass after three finished = %#v, active %d", result, h.ActiveEventRuns())
	}
	for _, activation := range routines {
		if runs := h.EventRuns(activation); len(runs) != 1 {
			t.Fatalf("routine %s has %d runs", activation.ID, len(runs))
		}
	}
	// A smaller cap set on the dispatcher applies.
	h.Dispatcher.TenantRunCap = 8
	h.FinishRun(h.EventRuns(routines[3])[0])
	h.Offer(routines[3], 1)
	if result := h.Pass(); result.Started != 0 || result.TenantCapped != 1 || h.ActiveEventRuns() != 8 {
		t.Fatalf("pass under cap 8 = %#v, active %d", result, h.ActiveEventRuns())
	}
}

// TestRoutineEventBurstRespectsCaps sends 1 000 events to 20 routines of one
// tenant (520 to one of them, so it overflows) while Runs finish over time.
func TestRoutineEventBurstRespectsCaps(t *testing.T) {
	h, db := routinetest.NewEventRowsHarness(t)
	const maxPerHour = 3
	routines := make([]*models.RunbookActivation, 0, 20)
	for i := 0; i < 20; i++ {
		routines = append(routines, h.Routine(fmt.Sprintf("burst%02d", i), func(trigger *models.RoutineEventTrigger) { trigger.MaxPerHour = maxPerHour }))
	}
	for i := 0; i < 520; i++ {
		h.Offer(routines[0], 1)
	}
	for i := 0; i < 480; i++ {
		h.Offer(routines[1+i%19], 1)
	}
	starts := map[string][]time.Time{}
	runStartedAt := map[string]time.Time{}
	for minute := 0; minute < 180; minute++ {
		h.Pass()
		if active := h.ActiveEventRuns(); active > routine.RoutineEventTenantRunCap {
			t.Fatalf("minute %d: %d event Runs are not finished", minute, active)
		}
		for _, activation := range routines {
			for _, run := range h.EventRuns(activation) {
				if _, ok := runStartedAt[run.ID]; !ok {
					runStartedAt[run.ID] = h.Now
					starts[activation.ID] = append(starts[activation.ID], h.Now)
				}
				// Each Run takes three minutes.
				if !models.IsTerminalAgentRunStatus(run.Status) && h.Now.Sub(runStartedAt[run.ID]) >= 3*time.Minute {
					h.FinishRun(run)
				}
			}
		}
		h.Advance(time.Minute)
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
		for _, run := range h.EventRuns(activation) {
			batch, _ := h.BatchOf(run)
			if len(batch.EventIDs) > models.RoutineEventMaxBatchEvents {
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
	for _, record := range routinetest.ListEventRows(t, db, h.Scope) {
		if record.State == models.RoutineEventDropped && record.DroppedReason == models.RoutineEventDroppedOverflow {
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
