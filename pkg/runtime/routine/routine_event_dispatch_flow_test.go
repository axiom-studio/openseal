package routine_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/routine"
	"github.com/axiom-studio/openseal/pkg/runtime/routine/routinetest"
)

func TestRoutineEventDispatcherOneActiveRun(t *testing.T) {
	h := routinetest.NewEventHarness(t)
	activation := h.Routine("inbox", nil)
	first := h.Offer(activation, 3)
	if result := h.Pass(); result.Started != 1 {
		t.Fatalf("first pass = %#v", result)
	}
	runs := h.EventRuns(activation)
	if len(runs) != 1 {
		t.Fatalf("runs = %d", len(runs))
	}
	run := runs[0]
	if run.Source != models.RunSourceEvent || run.ActingUser != "user-1" || run.Entrypoint != "" || run.Plan != nil {
		t.Fatalf("run source=%q actingUser=%q entrypoint=%q plan=%#v", run.Source, run.ActingUser, run.Entrypoint, run.Plan)
	}
	for _, key := range []string{"runbookDefinitionId", "runbookDefinitionVersion", "scheduledFor", "payload"} {
		if _, ok := run.Context[key]; ok {
			t.Fatalf("event run context has %q: %#v", key, run.Context)
		}
	}
	if run.Context["runbookActivationId"] != activation.ID || run.Context[models.RoutineEventKindContextKey] != string(models.RoutineEventGmailMessage) ||
		run.Context[models.RoutineEventHeaderContextKey] != "3 new emails" || run.Context[models.ConversationRunConversationIDContextKey] != h.Conversation {
		t.Fatalf("event run context = %#v", run.Context)
	}
	if count := fmt.Sprint(run.Context[models.RoutineEventCountContextKey]); count != "3" {
		t.Fatalf("event count = %#v", run.Context[models.RoutineEventCountContextKey])
	}
	batch, _ := h.BatchOf(run)
	if strings.Join(batch.EventIDs, ",") != strings.Join(first, ",") || batch.RunID != run.ID || batch.StartedAt == nil {
		t.Fatalf("batch = %#v", batch)
	}

	second := h.Offer(activation, 2)
	if result := h.Pass(); result.Started != 0 || result.Waiting != 1 {
		t.Fatalf("pass with an active run = %#v", result)
	}
	if len(h.EventRuns(activation)) != 1 {
		t.Fatal("a second run started while the first was active")
	}
	h.FinishRuns()
	if result := h.Pass(); result.Started != 1 {
		t.Fatalf("pass after the run finished = %#v", result)
	}
	runs = h.EventRuns(activation)
	if len(runs) != 2 {
		t.Fatalf("runs = %d", len(runs))
	}
	batch, _ = h.BatchOf(runs[1])
	if strings.Join(batch.EventIDs, ",") != strings.Join(second, ",") {
		t.Fatalf("second batch = %#v, want %v", batch.EventIDs, second)
	}
}

func TestRoutineEventDispatcherRateLimit(t *testing.T) {
	h := routinetest.NewEventHarness(t)
	activation := h.Routine("limited", func(trigger *models.RoutineEventTrigger) { trigger.MaxPerHour = 2 })
	seen := map[string]bool{}
	starts := []time.Time{}
	// 50 events arrive over 100 minutes; each pass finishes the runs first.
	for minute := 0; minute < 100; minute++ {
		if minute%2 == 0 {
			h.Offer(activation, 1)
		}
		h.FinishRuns()
		if result := h.Pass(); result.Started > 0 {
			starts = append(starts, h.Now)
		}
		h.Advance(time.Minute)
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
	h.FinishRuns()
	total := 0
	for _, run := range h.EventRuns(activation) {
		batch, _ := h.BatchOf(run)
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
	activity, err := h.Store.RoutineEventActivity(h.Ctx, h.Scope, []string{activation.ID}, h.Now)
	if err != nil {
		t.Fatal(err)
	}
	if total+activity[activation.ID].Pending != 50 {
		t.Fatalf("started %d + pending %d != 50", total, activity[activation.ID].Pending)
	}
}

func TestRoutineEventDispatcherBatchReplay(t *testing.T) {
	h := routinetest.NewEventHarness(t)
	activation := h.Routine("replay", nil)
	h.Offer(activation, 2)
	// The process claimed a batch and stopped before starting it.
	claimed, err := h.Store.ClaimRoutineEventBatch(h.Ctx, h.Scope, activation.ID, models.RoutineEventMaxBatchEvents, h.Now)
	if err != nil || claimed == nil {
		t.Fatalf("claim=%#v err=%v", claimed, err)
	}
	h.Offer(activation, 1)
	if result := h.Pass(); result.Started != 1 {
		t.Fatalf("pass = %#v", result)
	}
	runs := h.EventRuns(activation)
	if len(runs) != 1 || runs[0].ID != agentrun.RunIDForIdempotencyKey(h.Scope, routine.RoutineEventIdempotencyKey(claimed.ID)) {
		t.Fatalf("runs = %#v", runs)
	}
	batch, _ := h.BatchOf(runs[0])
	if batch.ID != claimed.ID || len(batch.EventIDs) != 2 {
		t.Fatalf("started batch = %#v, claimed %#v", batch, claimed)
	}

	// The process created the Run and stopped before marking the batch.
	h.FinishRuns()
	open, err := h.Store.ClaimRoutineEventBatch(h.Ctx, h.Scope, activation.ID, models.RoutineEventMaxBatchEvents, h.Now)
	if err != nil || open == nil {
		t.Fatalf("claim=%#v err=%v", open, err)
	}
	objective, err := h.Store.GetObjective(h.Ctx, h.Scope, activation.ObjectiveID)
	if err != nil {
		t.Fatal(err)
	}
	created, err := routine.StartActivationOccurrence(h.Ctx, h.Store, h.Store, h.Get(activation), objective, models.RunSourceEvent, routine.RoutineEventIdempotencyKey(open.ID),
		map[string]interface{}{models.RoutineEventBatchContextKey: open.ID}, models.ActivityActor{Type: "service", ID: "test"}, models.ActivityVisibilityScope, nil)
	if err != nil {
		t.Fatal(err)
	}
	result := h.Pass()
	if result.Started != 0 || result.Replayed != 1 {
		t.Fatalf("replay pass = %#v", result)
	}
	if runs := h.EventRuns(activation); len(runs) != 2 {
		t.Fatalf("runs after replay = %d", len(runs))
	}
	batch, _, err = h.Store.GetRoutineEventBatch(h.Ctx, h.Scope, open.ID)
	if err != nil || batch.RunID != created.Run.ID || batch.StartedAt == nil {
		t.Fatalf("replayed batch=%#v err=%v", batch, err)
	}
}

func TestRoutineEventMaximumOccurrencesRetires(t *testing.T) {
	h := routinetest.NewEventHarness(t)
	activation := h.Routine("twice", func(trigger *models.RoutineEventTrigger) { trigger.MaximumOccurrences = 2 })
	for i := 0; i < 2; i++ {
		h.Offer(activation, 2)
		h.Pass()
		h.FinishRuns()
	}
	if h.Get(activation).Status != models.RunbookActivationRetired {
		t.Fatal("routine is not retired after its last occurrence")
	}
	if h.Matcher.Listening(h.Scope, models.RoutineEventGmailMessage, activation.Task.Event.SourceRef) {
		t.Fatal("retired routine is still listening")
	}
	event := h.Event(activation, "gmail:late")
	if offer, err := h.Matcher.OfferRoutineEvent(h.Ctx, event); err != nil || offer.Matched != 0 {
		t.Fatalf("late offer=%#v err=%v", offer, err)
	}
	h.Pass()
	if runs := h.EventRuns(activation); len(runs) != 2 {
		t.Fatalf("runs = %d, want 2", len(runs))
	}

	// Events that waited when the last occurrence started are dropped.
	h2 := routinetest.NewEventHarness(t)
	once := h2.Routine("once", func(trigger *models.RoutineEventTrigger) { trigger.MaximumOccurrences = 1 })
	h2.Offer(once, 2)
	h2.Pass()
	// The routine no longer listens; a lagging peer may still have stored events.
	for i := 0; i < 2; i++ {
		if _, err := h2.Store.InsertRoutineEvent(h2.Ctx, &models.RoutineEventRecord{Scope: h2.Scope, ActivationID: once.ID, Event: *h2.Event(once, "gmail:lagging-"+string(rune('a'+i))), State: models.RoutineEventPending}); err != nil {
			t.Fatal(err)
		}
	}
	h2.Pass()
	if count, _ := h2.Store.CountPendingRoutineEvents(h2.Ctx, h2.Scope, once.ID); count != 0 {
		t.Fatalf("pending after exhaustion = %d", count)
	}
	activity, err := h2.Store.RoutineEventActivity(h2.Ctx, h2.Scope, []string{once.ID}, h2.Now)
	if err != nil || activity[once.ID].DroppedLast24h != 2 {
		t.Fatalf("activity=%#v err=%v", activity, err)
	}
}

func TestRoutineEventPausedDrops(t *testing.T) {
	h, db := routinetest.NewEventRowsHarness(t)
	activation := h.Routine("pause-me", nil)
	h.Offer(activation, 3)
	h.SetStatus(activation, models.RunbookActivationPaused)
	if offer, err := h.Matcher.OfferRoutineEvent(h.Ctx, h.Event(activation, "gmail:after-pause")); err != nil || offer.Matched != 0 {
		t.Fatalf("offer after pause=%#v err=%v", offer, err)
	}
	result := h.Pass()
	if result.Started != 0 || result.Dropped != 3 {
		t.Fatalf("pass = %#v", result)
	}
	if count, _ := h.Store.CountPendingRoutineEvents(h.Ctx, h.Scope, activation.ID); count != 0 {
		t.Fatalf("pending = %d", count)
	}
	for _, record := range routinetest.ListEventRows(t, db, h.Scope) {
		if record.ActivationID == activation.ID && (record.State != models.RoutineEventDropped || record.DroppedReason != models.RoutineEventDroppedPaused || record.KeepsData) {
			t.Fatalf("record %s = %#v", record.EventID, record)
		}
	}
}

func TestRoutineTestEventsNeverMixWithReal(t *testing.T) {
	h := routinetest.NewEventHarness(t)
	activation := h.Routine("mixed", nil)
	real := h.Offer(activation, 2)
	for i := 0; i < 2; i++ {
		if _, _, err := h.Matcher.OfferRoutineTestEvent(h.Ctx, h.Scope, activation.ID, "user-1", nil); err != nil {
			t.Fatal(err)
		}
	}
	h.Pass()
	h.FinishRuns()
	h.Pass()
	runs := h.EventRuns(activation)
	if len(runs) != 2 {
		t.Fatalf("runs = %d", len(runs))
	}
	testBatch, testRecords := h.BatchOf(runs[0])
	if !testBatch.Test || runs[0].Context[models.RoutineEventTestContextKey] != true || runs[0].Context[models.RoutineEventHeaderContextKey] != "2 test events" {
		t.Fatalf("first batch = %#v context %#v", testBatch, runs[0].Context)
	}
	for _, record := range testRecords {
		if !record.Event.Test {
			t.Fatalf("test batch holds a real event %s", record.Event.ID)
		}
	}
	realBatch, realRecords := h.BatchOf(runs[1])
	if realBatch.Test || runs[1].Context[models.RoutineEventTestContextKey] != nil || strings.Join(realBatch.EventIDs, ",") != strings.Join(real, ",") {
		t.Fatalf("second batch = %#v", realBatch)
	}
	for _, record := range realRecords {
		if record.Event.Test {
			t.Fatalf("real batch holds a test event %s", record.Event.ID)
		}
	}
	// A real batch at its hourly limit still lets test events through.
	h2 := routinetest.NewEventHarness(t)
	limited := h2.Routine("limited", func(trigger *models.RoutineEventTrigger) { trigger.MaxPerHour = 1 })
	h2.Offer(limited, 1)
	h2.Pass()
	h2.FinishRuns()
	h2.Offer(limited, 1)
	if _, _, err := h2.Matcher.OfferRoutineTestEvent(h2.Ctx, h2.Scope, limited.ID, "user-1", nil); err != nil {
		t.Fatal(err)
	}
	result := h2.Pass()
	if result.TestStarted != 1 || result.Started != 0 {
		t.Fatalf("pass at the hourly limit = %#v", result)
	}
}

func TestRoutineEventPauseWithOpenBatchThenResume(t *testing.T) {
	h := routinetest.NewEventHarness(t)
	activation := h.Routine("resume", nil)
	h.Offer(activation, 2)
	open, err := h.Store.ClaimRoutineEventBatch(h.Ctx, h.Scope, activation.ID, models.RoutineEventMaxBatchEvents, h.Now)
	if err != nil || open == nil {
		t.Fatalf("claim=%#v err=%v", open, err)
	}
	h.SetStatus(activation, models.RunbookActivationPaused)
	h.Pass()
	if current, err := h.Store.GetOpenRoutineEventBatch(h.Ctx, h.Scope, activation.ID); err != nil || current != nil {
		t.Fatalf("open batch after pause=%#v err=%v", current, err)
	}
	h.SetStatus(activation, models.RunbookActivationActive)
	fresh := h.Offer(activation, 1)
	h.Pass()
	runs := h.EventRuns(activation)
	if len(runs) != 1 {
		t.Fatalf("runs = %d", len(runs))
	}
	batch, _ := h.BatchOf(runs[0])
	if batch.ID == open.ID || strings.Join(batch.EventIDs, ",") != strings.Join(fresh, ",") {
		t.Fatalf("batch after resume = %#v", batch)
	}
	if runs[0].Context[models.RoutineEventHeaderContextKey] != "Email "+fresh[0] {
		t.Fatalf("single event header = %#v", runs[0].Context[models.RoutineEventHeaderContextKey])
	}
}

func TestRoutineEventOccurrenceCountSurvivesCrash(t *testing.T) {
	h := routinetest.NewEventHarness(t)
	activation := h.Routine("count", func(trigger *models.RoutineEventTrigger) { trigger.MaximumOccurrences = 2 })
	h.Offer(activation, 1)
	h.Pass()
	h.FinishRuns()
	// A new process: the count comes from the started batch rows.
	restarted := routine.NewRoutineEventDispatcher(h.Store, h.Matcher, models.WithClock(h.Clock))
	h.Dispatcher = restarted
	h.Offer(activation, 1)
	// It creates the Run and stops before marking the batch.
	open, err := h.Store.ClaimRoutineEventBatch(h.Ctx, h.Scope, activation.ID, models.RoutineEventMaxBatchEvents, h.Now)
	if err != nil || open == nil {
		t.Fatalf("claim=%#v err=%v", open, err)
	}
	objective, err := h.Store.GetObjective(h.Ctx, h.Scope, activation.ObjectiveID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := routine.StartActivationOccurrence(h.Ctx, h.Store, h.Store, h.Get(activation), objective, models.RunSourceEvent, routine.RoutineEventIdempotencyKey(open.ID),
		map[string]interface{}{models.RoutineEventBatchContextKey: open.ID}, models.ActivityActor{Type: "service", ID: "test"}, models.ActivityVisibilityScope, nil); err != nil {
		t.Fatal(err)
	}
	h.Pass()
	if h.Get(activation).Status != models.RunbookActivationRetired {
		t.Fatal("routine is not retired after the replayed second occurrence")
	}
	count, _, err := h.Store.CountRoutineEventBatchesSince(h.Ctx, h.Scope, activation.ID, time.Time{}, false)
	if err != nil || count != 2 {
		t.Fatalf("started batches=%d err=%v", count, err)
	}
	h.FinishRuns()
	h.Pass()
	if runs := h.EventRuns(activation); len(runs) != 2 {
		t.Fatalf("runs = %d, want 2", len(runs))
	}
}

// TestRoutineEventArchivedChatPausesOnlyThatRoutine archives the chat of the
// routine that sorts first. That routine is paused with its events closed
// out, and the routine sorted after it still starts in the same pass.
func TestRoutineEventArchivedChatPausesOnlyThatRoutine(t *testing.T) {
	h := routinetest.NewEventHarness(t)
	sharedChat, sharedMessage := h.Conversation, h.Message
	first := h.Routine("alpha", nil)
	otherChat, otherMessage := h.Chat("Other")
	h.Conversation, h.Message = otherChat, otherMessage
	second := h.Routine("beta", nil)
	h.Conversation, h.Message = sharedChat, sharedMessage
	// broken sorts before healthy in ListRoutineEventWork.
	broken, healthy, brokenChat := first, second, sharedChat
	if second.ID < first.ID {
		broken, healthy, brokenChat = second, first, otherChat
	}
	h.Offer(broken, 2)
	h.Offer(healthy, 1)
	conversations := conversation.NewConversationService(h.Store)
	chat, err := conversations.GetConversation(h.Ctx, h.Scope, brokenChat)
	if err != nil {
		t.Fatal(err)
	}
	archived := models.ConversationStatusArchived
	if _, err := conversations.UpdateConversation(h.Ctx, conversation.UpdateConversationRequest{
		Scope: h.Scope, ConversationID: chat.ID, ExpectedRevision: chat.Revision, Status: &archived,
	}); err != nil {
		t.Fatal(err)
	}
	result := h.Pass()
	if result.Started != 1 || result.Paused != 1 || result.Dropped != 2 || result.Failed != 0 {
		t.Fatalf("pass = %#v", result)
	}
	if len(h.EventRuns(healthy)) != 1 || len(h.EventRuns(broken)) != 0 {
		t.Fatalf("healthy runs = %d, broken runs = %d", len(h.EventRuns(healthy)), len(h.EventRuns(broken)))
	}
	if current := h.Get(broken); current.Status != models.RunbookActivationPaused || current.PausedReason != models.RoutinePausedUnverified {
		t.Fatalf("broken routine status=%q reason=%q", current.Status, current.PausedReason)
	}
	if count, _ := h.Store.CountPendingRoutineEvents(h.Ctx, h.Scope, broken.ID); count != 0 {
		t.Fatalf("broken routine still has %d waiting events", count)
	}
	if result := h.Pass(); result.Examined != 0 || result.Paused != 0 {
		t.Fatalf("next pass = %#v", result)
	}
}

// TestRoutineEventStartFailureStaysLocal makes one routine's start fail for
// a reason that is not a pause. The pass reports the error but still starts
// the routine sorted after it.
func TestRoutineEventStartFailureStaysLocal(t *testing.T) {
	h := routinetest.NewEventHarness(t)
	first := h.Routine("alpha", nil)
	second := h.Routine("beta", nil)
	broken, healthy := first, second
	if second.ID < first.ID {
		broken, healthy = second, first
	}
	h.Offer(broken, 1)
	h.Offer(healthy, 1)
	h.Dispatcher.SetAcceptedRunExecutionPreparer(failingRoutineExecutionPreparer{activationID: broken.ID})
	result, err := h.Dispatcher.ReconcileAll(h.Ctx, 200)
	if err == nil || !strings.Contains(err.Error(), broken.ID) {
		t.Fatalf("pass error = %v", err)
	}
	if result.Started != 1 || result.Failed != 1 || len(h.EventRuns(healthy)) != 1 {
		t.Fatalf("pass = %#v, healthy runs = %d", result, len(h.EventRuns(healthy)))
	}
	if current := h.Get(broken); current.Status != models.RunbookActivationActive {
		t.Fatalf("broken routine status = %q", current.Status)
	}
}

type failingRoutineExecutionPreparer struct{ activationID string }

func (p failingRoutineExecutionPreparer) PrepareAcceptedRunExecution(_ context.Context, request *models.CreateAgentRunRequest) error {
	if request.Context["runbookActivationId"] == p.activationID {
		return errors.New("execution preparation failed")
	}
	return nil
}

func TestRoutineEventCallableOnlyWhenExhausted(t *testing.T) {
	h := routinetest.NewEventHarness(t)
	once := h.Routine("once-callable", func(trigger *models.RoutineEventTrigger) { trigger.MaximumOccurrences = 1 })
	h.Offer(once, 1)
	h.Pass()
	exhausted := h.Get(once)
	if exhausted.Status != models.RunbookActivationRetired || !exhausted.Exhausted || !exhausted.Callable() {
		t.Fatalf("exhausted routine: status=%s exhausted=%v callable=%v", exhausted.Status, exhausted.Exhausted, exhausted.Callable())
	}
	// A routine the person cancelled is not callable, even with a ceiling.
	for _, mutate := range []func(*models.RoutineEventTrigger){nil, func(trigger *models.RoutineEventTrigger) { trigger.MaximumOccurrences = 5 }} {
		cancelled := h.SetStatus(h.Routine(fmt.Sprintf("cancelled-%v", mutate != nil), mutate), models.RunbookActivationRetired)
		if cancelled.Exhausted || cancelled.Callable() {
			t.Fatalf("cancelled routine is callable: %#v", cancelled)
		}
	}
	active := h.Routine("not-exhausted", nil)
	if _, err := routine.NewRunbookActivationService(h.Store).Update(h.Ctx, h.Scope, active.ID, routine.UpdateRunbookActivationRequest{
		ExpectedRevision: active.Revision, Status: models.RunbookActivationPaused, Exhausted: true,
	}); err == nil {
		t.Fatal("a paused routine was marked exhausted")
	}
}

func TestRoutineEventPausedRunBackpressures(t *testing.T) {
	h := routinetest.NewEventHarness(t)
	activation := h.Routine("paused-run", nil)
	h.Offer(activation, 1)
	if result := h.Pass(); result.Started != 1 {
		t.Fatalf("first pass = %#v", result)
	}
	h.PauseRun(h.EventRuns(activation)[0])
	h.Offer(activation, 1)
	if result := h.Pass(); result.Started != 0 || result.Waiting != 1 {
		t.Fatalf("pass with a paused run = %#v", result)
	}
	if runs := h.EventRuns(activation); len(runs) != 1 {
		t.Fatalf("runs = %d, want 1 while the first is paused", len(runs))
	}
}

func TestRoutineEventTenantCapRotatesRoutines(t *testing.T) {
	h := routinetest.NewEventHarness(t)
	h.Dispatcher.TenantRunCap = 2
	routines := make([]*models.RunbookActivation, 0, 4)
	for i := 0; i < 4; i++ {
		activation := h.Routine(fmt.Sprintf("rot%02d", i), nil)
		h.Offer(activation, 1)
		routines = append(routines, activation)
	}
	if result := h.Pass(); result.Started != 2 {
		t.Fatalf("first pass = %#v", result)
	}
	first := []*models.RunbookActivation{}
	for _, activation := range routines {
		if len(h.EventRuns(activation)) == 1 {
			first = append(first, activation)
		}
	}
	if len(first) != 2 {
		t.Fatalf("%d routines started in the first pass", len(first))
	}
	// The first two are busy again before the next pass: the slots freed go
	// to the two that waited, not back to the routines that sort first.
	h.FinishRuns()
	for _, activation := range first {
		h.Offer(activation, 1)
	}
	if result := h.Pass(); result.Started != 2 {
		t.Fatalf("second pass = %#v", result)
	}
	for _, activation := range routines {
		if runs := h.EventRuns(activation); len(runs) != 1 {
			t.Fatalf("routine %s has %d runs after two passes", activation.ID, len(runs))
		}
	}
	h.FinishRuns()
	if result := h.Pass(); result.Started != 2 {
		t.Fatalf("third pass = %#v", result)
	}
	for _, activation := range first {
		if len(h.EventRuns(activation)) != 2 {
			t.Fatal("the routines that waited a turn did not start next")
		}
	}
}

func TestRoutineEventDispatcherVisitsOnlyScopesWithWork(t *testing.T) {
	h := routinetest.NewEventHarness(t)
	h.Routine("idle", nil)
	if result := h.Pass(); result.Scopes != 0 || result.Examined != 0 {
		t.Fatalf("idle pass = %#v", result)
	}
}

func TestPauseRoutineLeavesCloseOutToDispatcher(t *testing.T) {
	h := routinetest.NewEventHarness(t)
	activation := h.Routine("paused-by-sync", nil)
	h.Offer(activation, 2)
	// The dispatcher has claimed the batch and is creating its Run.
	batch, err := h.Store.ClaimRoutineEventBatch(h.Ctx, h.Scope, activation.ID, models.RoutineEventMaxBatchEvents, h.Now)
	if err != nil || batch == nil {
		t.Fatalf("claim = %#v, %v", batch, err)
	}
	paused, err := h.Dispatcher.PauseRoutine(h.Ctx, h.Scope, activation.ID, models.RoutinePausedNeedsSignIn)
	if err != nil || !paused {
		t.Fatalf("pause = %v, %v", paused, err)
	}
	if stored, _, err := h.Store.GetRoutineEventBatch(h.Ctx, h.Scope, batch.ID); err != nil || stored == nil {
		t.Fatalf("pause deleted the batch being started: %#v, %v", stored, err)
	}
	if result := h.Pass(); result.Started != 0 || result.Dropped != 2 {
		t.Fatalf("pass after pause = %#v", result)
	}
	if count, _ := h.Store.CountPendingRoutineEvents(h.Ctx, h.Scope, activation.ID); count != 0 {
		t.Fatalf("pending after close-out = %d", count)
	}
}
