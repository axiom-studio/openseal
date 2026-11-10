package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestRoutineEventDispatcherOneActiveRun(t *testing.T) {
	h := newRoutineEventHarness(t)
	activation := h.routine("inbox", nil)
	first := h.offer(activation, 3)
	if result := h.pass(); result.Started != 1 {
		t.Fatalf("first pass = %#v", result)
	}
	runs := h.eventRuns(activation)
	if len(runs) != 1 {
		t.Fatalf("runs = %d", len(runs))
	}
	run := runs[0]
	if run.Source != RunSourceEvent || run.ActingUser != "user-1" || run.Entrypoint != "" || run.Plan != nil {
		t.Fatalf("run source=%q actingUser=%q entrypoint=%q plan=%#v", run.Source, run.ActingUser, run.Entrypoint, run.Plan)
	}
	for _, key := range []string{"runbookDefinitionId", "runbookDefinitionVersion", "scheduledFor", "payload"} {
		if _, ok := run.Context[key]; ok {
			t.Fatalf("event run context has %q: %#v", key, run.Context)
		}
	}
	if run.Context["runbookActivationId"] != activation.ID || run.Context[RoutineEventKindContextKey] != string(RoutineEventGmailMessage) ||
		run.Context[RoutineEventHeaderContextKey] != "3 new emails" || run.Context[conversationRunContextConversationID] != h.conversation {
		t.Fatalf("event run context = %#v", run.Context)
	}
	if count := fmt.Sprint(run.Context[RoutineEventCountContextKey]); count != "3" {
		t.Fatalf("event count = %#v", run.Context[RoutineEventCountContextKey])
	}
	batch, _ := h.batchOf(run)
	if strings.Join(batch.EventIDs, ",") != strings.Join(first, ",") || batch.RunID != run.ID || batch.StartedAt == nil {
		t.Fatalf("batch = %#v", batch)
	}

	second := h.offer(activation, 2)
	if result := h.pass(); result.Started != 0 || result.Waiting != 1 {
		t.Fatalf("pass with an active run = %#v", result)
	}
	if len(h.eventRuns(activation)) != 1 {
		t.Fatal("a second run started while the first was active")
	}
	h.finishRuns()
	if result := h.pass(); result.Started != 1 {
		t.Fatalf("pass after the run finished = %#v", result)
	}
	runs = h.eventRuns(activation)
	if len(runs) != 2 {
		t.Fatalf("runs = %d", len(runs))
	}
	batch, _ = h.batchOf(runs[1])
	if strings.Join(batch.EventIDs, ",") != strings.Join(second, ",") {
		t.Fatalf("second batch = %#v, want %v", batch.EventIDs, second)
	}
}

func TestRoutineEventDispatcherRateLimit(t *testing.T) {
	h := newRoutineEventHarness(t)
	activation := h.routine("limited", func(trigger *RoutineEventTrigger) { trigger.MaxPerHour = 2 })
	seen := map[string]bool{}
	starts := []time.Time{}
	// 50 events arrive over 100 minutes; each pass finishes the runs first.
	for minute := 0; minute < 100; minute++ {
		if minute%2 == 0 {
			h.offer(activation, 1)
		}
		h.finishRuns()
		if result := h.pass(); result.Started > 0 {
			starts = append(starts, h.now)
		}
		h.advance(time.Minute)
	}
	for i := range starts {
		inHour := 0
		for _, other := range starts {
			if !other.Before(starts[i]) && other.Before(starts[i].Add(time.Hour)) {
				inHour++
			}
		}
		if inHour > 2 {
			t.Fatalf("%d batches started within an hour of %s", inHour, starts[i])
		}
	}
	h.finishRuns()
	total := 0
	for _, run := range h.eventRuns(activation) {
		batch, _ := h.batchOf(run)
		for _, id := range batch.EventIDs {
			if seen[id] {
				t.Fatalf("event %s is in two batches", id)
			}
			seen[id] = true
			total++
		}
	}
	if len(starts) == 0 || len(starts) > 4 {
		t.Fatalf("batches started = %d over 100 minutes with maxPerHour 2", len(starts))
	}
	activity, err := h.store.RoutineEventActivity(h.ctx, h.scope, []string{activation.ID}, h.now)
	if err != nil {
		t.Fatal(err)
	}
	if total+activity[activation.ID].Pending != 50 {
		t.Fatalf("started %d + pending %d != 50", total, activity[activation.ID].Pending)
	}
}

func TestRoutineEventDispatcherBatchReplay(t *testing.T) {
	h := newRoutineEventHarness(t)
	activation := h.routine("replay", nil)
	h.offer(activation, 2)
	// The process claimed a batch and stopped before starting it.
	claimed, err := h.store.ClaimRoutineEventBatch(h.ctx, h.scope, activation.ID, RoutineEventMaxBatchEvents, h.now)
	if err != nil || claimed == nil {
		t.Fatalf("claim=%#v err=%v", claimed, err)
	}
	h.offer(activation, 1)
	if result := h.pass(); result.Started != 1 {
		t.Fatalf("pass = %#v", result)
	}
	runs := h.eventRuns(activation)
	if len(runs) != 1 || runs[0].ID != runIDForIdempotencyKey(h.scope, routineEventIdempotencyKey(claimed.ID)) {
		t.Fatalf("runs = %#v", runs)
	}
	batch, _ := h.batchOf(runs[0])
	if batch.ID != claimed.ID || len(batch.EventIDs) != 2 {
		t.Fatalf("started batch = %#v, claimed %#v", batch, claimed)
	}

	// The process created the Run and stopped before marking the batch.
	h.finishRuns()
	open, err := h.store.ClaimRoutineEventBatch(h.ctx, h.scope, activation.ID, RoutineEventMaxBatchEvents, h.now)
	if err != nil || open == nil {
		t.Fatalf("claim=%#v err=%v", open, err)
	}
	objective, err := h.store.GetObjective(h.ctx, h.scope, activation.ObjectiveID)
	if err != nil {
		t.Fatal(err)
	}
	created, err := startActivationOccurrence(h.ctx, h.store, h.store, h.get(activation), objective, RunSourceEvent, routineEventIdempotencyKey(open.ID),
		map[string]interface{}{RoutineEventBatchContextKey: open.ID}, ActivityActor{Type: "service", ID: "test"}, ActivityVisibilityScope, nil)
	if err != nil {
		t.Fatal(err)
	}
	result := h.pass()
	if result.Started != 0 || result.Replayed != 1 {
		t.Fatalf("replay pass = %#v", result)
	}
	if runs := h.eventRuns(activation); len(runs) != 2 {
		t.Fatalf("runs after replay = %d", len(runs))
	}
	batch, _, err = h.store.GetRoutineEventBatch(h.ctx, h.scope, open.ID)
	if err != nil || batch.RunID != created.Run.ID || batch.StartedAt == nil {
		t.Fatalf("replayed batch=%#v err=%v", batch, err)
	}
}

func TestRoutineEventMaximumOccurrencesRetires(t *testing.T) {
	h := newRoutineEventHarness(t)
	activation := h.routine("twice", func(trigger *RoutineEventTrigger) { trigger.MaximumOccurrences = 2 })
	for i := 0; i < 2; i++ {
		h.offer(activation, 2)
		h.pass()
		h.finishRuns()
	}
	if h.get(activation).Status != RunbookActivationRetired {
		t.Fatal("routine is not retired after its last occurrence")
	}
	if h.matcher.Listening(h.scope, RoutineEventGmailMessage, activation.Task.Event.SourceRef) {
		t.Fatal("retired routine is still listening")
	}
	event := h.event(activation, "gmail:late")
	if offer, err := h.matcher.OfferRoutineEvent(h.ctx, event); err != nil || offer.Matched != 0 {
		t.Fatalf("late offer=%#v err=%v", offer, err)
	}
	h.pass()
	if runs := h.eventRuns(activation); len(runs) != 2 {
		t.Fatalf("runs = %d, want 2", len(runs))
	}

	// Events that waited when the last occurrence started are dropped.
	h2 := newRoutineEventHarness(t)
	once := h2.routine("once", func(trigger *RoutineEventTrigger) { trigger.MaximumOccurrences = 1 })
	h2.offer(once, 2)
	h2.pass()
	// The routine no longer listens; a lagging peer may still have stored events.
	for i := 0; i < 2; i++ {
		if _, err := h2.store.InsertRoutineEvent(h2.ctx, &RoutineEventRecord{Scope: h2.scope, ActivationID: once.ID, Event: *h2.event(once, "gmail:lagging-"+string(rune('a'+i))), State: RoutineEventPending}); err != nil {
			t.Fatal(err)
		}
	}
	h2.pass()
	if count, _ := h2.store.CountPendingRoutineEvents(h2.ctx, h2.scope, once.ID); count != 0 {
		t.Fatalf("pending after exhaustion = %d", count)
	}
	activity, err := h2.store.RoutineEventActivity(h2.ctx, h2.scope, []string{once.ID}, h2.now)
	if err != nil || activity[once.ID].DroppedLast24h != 2 {
		t.Fatalf("activity=%#v err=%v", activity, err)
	}
}

func TestRoutineEventPausedDrops(t *testing.T) {
	h := newRoutineEventHarness(t)
	activation := h.routine("pause-me", nil)
	h.offer(activation, 3)
	h.setStatus(activation, RunbookActivationPaused)
	if offer, err := h.matcher.OfferRoutineEvent(h.ctx, h.event(activation, "gmail:after-pause")); err != nil || offer.Matched != 0 {
		t.Fatalf("offer after pause=%#v err=%v", offer, err)
	}
	result := h.pass()
	if result.Started != 0 || result.Dropped != 3 {
		t.Fatalf("pass = %#v", result)
	}
	if count, _ := h.store.CountPendingRoutineEvents(h.ctx, h.scope, activation.ID); count != 0 {
		t.Fatalf("pending = %d", count)
	}
	h.store.mu.RLock()
	for key, record := range h.store.routineEvents {
		if key.activationID == activation.ID && (record.State != RoutineEventDropped || record.DroppedReason != RoutineEventDroppedPaused || len(record.Event.Payload) != 0) {
			h.store.mu.RUnlock()
			t.Fatalf("record %s = %#v", key.eventID, record)
		}
	}
	h.store.mu.RUnlock()
}

func TestRoutineTestEventsNeverMixWithReal(t *testing.T) {
	h := newRoutineEventHarness(t)
	activation := h.routine("mixed", nil)
	real := h.offer(activation, 2)
	for i := 0; i < 2; i++ {
		if _, _, err := h.matcher.OfferRoutineTestEvent(h.ctx, h.scope, activation.ID, "user-1", nil); err != nil {
			t.Fatal(err)
		}
	}
	h.pass()
	h.finishRuns()
	h.pass()
	runs := h.eventRuns(activation)
	if len(runs) != 2 {
		t.Fatalf("runs = %d", len(runs))
	}
	testBatch, testRecords := h.batchOf(runs[0])
	if !testBatch.Test || runs[0].Context[RoutineEventTestContextKey] != true || runs[0].Context[RoutineEventHeaderContextKey] != "2 test events" {
		t.Fatalf("first batch = %#v context %#v", testBatch, runs[0].Context)
	}
	for _, record := range testRecords {
		if !record.Event.Test {
			t.Fatalf("test batch holds a real event %s", record.Event.ID)
		}
	}
	realBatch, realRecords := h.batchOf(runs[1])
	if realBatch.Test || runs[1].Context[RoutineEventTestContextKey] != nil || strings.Join(realBatch.EventIDs, ",") != strings.Join(real, ",") {
		t.Fatalf("second batch = %#v", realBatch)
	}
	for _, record := range realRecords {
		if record.Event.Test {
			t.Fatalf("real batch holds a test event %s", record.Event.ID)
		}
	}
	// A real batch at its hourly limit still lets test events through.
	h2 := newRoutineEventHarness(t)
	limited := h2.routine("limited", func(trigger *RoutineEventTrigger) { trigger.MaxPerHour = 1 })
	h2.offer(limited, 1)
	h2.pass()
	h2.finishRuns()
	h2.offer(limited, 1)
	if _, _, err := h2.matcher.OfferRoutineTestEvent(h2.ctx, h2.scope, limited.ID, "user-1", nil); err != nil {
		t.Fatal(err)
	}
	result := h2.pass()
	if result.TestStarted != 1 || result.Started != 0 {
		t.Fatalf("pass at the hourly limit = %#v", result)
	}
}

func TestRoutineEventPauseWithOpenBatchThenResume(t *testing.T) {
	h := newRoutineEventHarness(t)
	activation := h.routine("resume", nil)
	h.offer(activation, 2)
	open, err := h.store.ClaimRoutineEventBatch(h.ctx, h.scope, activation.ID, RoutineEventMaxBatchEvents, h.now)
	if err != nil || open == nil {
		t.Fatalf("claim=%#v err=%v", open, err)
	}
	h.setStatus(activation, RunbookActivationPaused)
	h.pass()
	if current, err := h.store.GetOpenRoutineEventBatch(h.ctx, h.scope, activation.ID); err != nil || current != nil {
		t.Fatalf("open batch after pause=%#v err=%v", current, err)
	}
	h.setStatus(activation, RunbookActivationActive)
	fresh := h.offer(activation, 1)
	h.pass()
	runs := h.eventRuns(activation)
	if len(runs) != 1 {
		t.Fatalf("runs = %d", len(runs))
	}
	batch, _ := h.batchOf(runs[0])
	if batch.ID == open.ID || strings.Join(batch.EventIDs, ",") != strings.Join(fresh, ",") {
		t.Fatalf("batch after resume = %#v", batch)
	}
	if runs[0].Context[RoutineEventHeaderContextKey] != "Email "+fresh[0] {
		t.Fatalf("single event header = %#v", runs[0].Context[RoutineEventHeaderContextKey])
	}
}

func TestRoutineEventOccurrenceCountSurvivesCrash(t *testing.T) {
	h := newRoutineEventHarness(t)
	activation := h.routine("count", func(trigger *RoutineEventTrigger) { trigger.MaximumOccurrences = 2 })
	h.offer(activation, 1)
	h.pass()
	h.finishRuns()
	// A new process: the count comes from the started batch rows.
	restarted := NewRoutineEventDispatcher(h.store, h.matcher)
	restarted.now = h.clock
	h.dispatcher = restarted
	h.offer(activation, 1)
	// It creates the Run and stops before marking the batch.
	open, err := h.store.ClaimRoutineEventBatch(h.ctx, h.scope, activation.ID, RoutineEventMaxBatchEvents, h.now)
	if err != nil || open == nil {
		t.Fatalf("claim=%#v err=%v", open, err)
	}
	objective, err := h.store.GetObjective(h.ctx, h.scope, activation.ObjectiveID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := startActivationOccurrence(h.ctx, h.store, h.store, h.get(activation), objective, RunSourceEvent, routineEventIdempotencyKey(open.ID),
		map[string]interface{}{RoutineEventBatchContextKey: open.ID}, ActivityActor{Type: "service", ID: "test"}, ActivityVisibilityScope, nil); err != nil {
		t.Fatal(err)
	}
	h.pass()
	if h.get(activation).Status != RunbookActivationRetired {
		t.Fatal("routine is not retired after the replayed second occurrence")
	}
	count, _, err := h.store.CountRoutineEventBatchesSince(h.ctx, h.scope, activation.ID, time.Time{}, false)
	if err != nil || count != 2 {
		t.Fatalf("started batches=%d err=%v", count, err)
	}
	h.finishRuns()
	h.pass()
	if runs := h.eventRuns(activation); len(runs) != 2 {
		t.Fatalf("runs = %d, want 2", len(runs))
	}
}

func TestRoutineEventBatchHeader(t *testing.T) {
	records := func(summaries ...string) ([]string, []*RoutineEventRecord) {
		ids := make([]string, 0, len(summaries))
		values := make([]*RoutineEventRecord, 0, len(summaries))
		for i, summary := range summaries {
			id := string(rune('a' + i))
			ids = append(ids, id)
			values = append(values, &RoutineEventRecord{Event: RoutineEvent{ID: id, Summary: summary}})
		}
		return ids, values
	}
	ids, values := records("New email from Ana Ruiz: Invoice #4411")
	if got := routineEventBatchHeader(RoutineEventGmailMessage, &RoutineEventBatch{EventIDs: ids}, values); got != "New email from Ana Ruiz: Invoice #4411" {
		t.Fatalf("one event header = %q", got)
	}
	ids, values = records("a", "b", "c", "d")
	for kind, want := range map[RoutineEventKind]string{
		RoutineEventGmailMessage: "4 new emails", RoutineEventSlackMessage: "4 new messages", RoutineEventTelegramMessage: "4 new messages",
		RoutineEventGitHub: "4 new events", RoutineEventCalendar: "4 new events", RoutineEventWebhook: "4 new events",
	} {
		if got := routineEventBatchHeader(kind, &RoutineEventBatch{EventIDs: ids}, values); got != want {
			t.Fatalf("%s header = %q, want %q", kind, got, want)
		}
	}
	ids, values = records("Test event")
	if got := routineEventBatchHeader(RoutineEventWebhook, &RoutineEventBatch{EventIDs: ids, Test: true}, values); got != "Test event" {
		t.Fatalf("one test header = %q", got)
	}
	ids, values = records("Test event", "Test event")
	if got := routineEventBatchHeader(RoutineEventWebhook, &RoutineEventBatch{EventIDs: ids, Test: true}, values); got != "2 test events" {
		t.Fatalf("two test header = %q", got)
	}
	ids, values = records(strings.Repeat("é", 150))
	got := routineEventBatchHeader(RoutineEventWebhook, &RoutineEventBatch{EventIDs: ids}, values)
	if len(got) > RoutineEventHeaderMaxBytes || !strings.HasPrefix(strings.Repeat("é", 150), got) || len(got) != 200 {
		t.Fatalf("long header has %d bytes: %q", len(got), got)
	}
}

// TestRoutineEventArchivedChatPausesOnlyThatRoutine archives the chat of the
// routine that sorts first. That routine is paused with its events closed
// out, and the routine sorted after it still starts in the same pass.
func TestRoutineEventArchivedChatPausesOnlyThatRoutine(t *testing.T) {
	h := newRoutineEventHarness(t)
	sharedChat, sharedMessage := h.conversation, h.message
	first := h.routine("alpha", nil)
	otherChat, otherMessage := h.chat("Other")
	h.conversation, h.message = otherChat, otherMessage
	second := h.routine("beta", nil)
	h.conversation, h.message = sharedChat, sharedMessage
	// broken sorts before healthy in ListRoutineEventWork.
	broken, healthy, brokenChat := first, second, sharedChat
	if second.ID < first.ID {
		broken, healthy, brokenChat = second, first, otherChat
	}
	h.offer(broken, 2)
	h.offer(healthy, 1)
	conversations := NewConversationService(h.store)
	chat, err := conversations.GetConversation(h.ctx, h.scope, brokenChat)
	if err != nil {
		t.Fatal(err)
	}
	archived := ConversationStatusArchived
	if _, err := conversations.UpdateConversation(h.ctx, UpdateConversationRequest{
		Scope: h.scope, ConversationID: chat.ID, ExpectedRevision: chat.Revision, Status: &archived,
	}); err != nil {
		t.Fatal(err)
	}
	result := h.pass()
	if result.Started != 1 || result.Paused != 1 || result.Dropped != 2 || result.Failed != 0 {
		t.Fatalf("pass = %#v", result)
	}
	if len(h.eventRuns(healthy)) != 1 || len(h.eventRuns(broken)) != 0 {
		t.Fatalf("healthy runs = %d, broken runs = %d", len(h.eventRuns(healthy)), len(h.eventRuns(broken)))
	}
	if current := h.get(broken); current.Status != RunbookActivationPaused || current.PausedReason != RoutinePausedUnverified {
		t.Fatalf("broken routine status=%q reason=%q", current.Status, current.PausedReason)
	}
	if count, _ := h.store.CountPendingRoutineEvents(h.ctx, h.scope, broken.ID); count != 0 {
		t.Fatalf("broken routine still has %d waiting events", count)
	}
	if result := h.pass(); result.Examined != 0 || result.Paused != 0 {
		t.Fatalf("next pass = %#v", result)
	}
}

// TestRoutineEventStartFailureStaysLocal makes one routine's start fail for
// a reason that is not a pause. The pass reports the error but still starts
// the routine sorted after it.
func TestRoutineEventStartFailureStaysLocal(t *testing.T) {
	h := newRoutineEventHarness(t)
	first := h.routine("alpha", nil)
	second := h.routine("beta", nil)
	broken, healthy := first, second
	if second.ID < first.ID {
		broken, healthy = second, first
	}
	h.offer(broken, 1)
	h.offer(healthy, 1)
	h.dispatcher.SetAcceptedRunExecutionPreparer(failingRoutineExecutionPreparer{activationID: broken.ID})
	result, err := h.dispatcher.ReconcileAll(h.ctx, 200)
	if err == nil || !strings.Contains(err.Error(), broken.ID) {
		t.Fatalf("pass error = %v", err)
	}
	if result.Started != 1 || result.Failed != 1 || len(h.eventRuns(healthy)) != 1 {
		t.Fatalf("pass = %#v, healthy runs = %d", result, len(h.eventRuns(healthy)))
	}
	if current := h.get(broken); current.Status != RunbookActivationActive {
		t.Fatalf("broken routine status = %q", current.Status)
	}
}

type failingRoutineExecutionPreparer struct{ activationID string }

func (p failingRoutineExecutionPreparer) PrepareAcceptedRunExecution(_ context.Context, request *CreateAgentRunRequest) error {
	if request.Context["runbookActivationId"] == p.activationID {
		return errors.New("execution preparation failed")
	}
	return nil
}
