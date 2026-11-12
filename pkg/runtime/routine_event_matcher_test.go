package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/externalconversation"
	"github.com/axiom-studio/openseal/pkg/runtime/routine"
	"github.com/axiom-studio/openseal/pkg/runtime/routine/routinetest"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

func TestRoutineEventMatcherIndexesBySource(t *testing.T) {
	h := routinetest.NewEventHarness(t)
	first := h.Routine("first", nil)
	second := h.Routine("second", nil)
	if !h.Matcher.Listening(h.Scope, models.RoutineEventGmailMessage, first.Task.Event.SourceRef) {
		t.Fatal("index does not list the first routine's source")
	}
	if h.Matcher.Listening(h.Scope, models.RoutineEventGmailMessage, models.RoutineGoogleSourceRef("user-1", "sub-other")) {
		t.Fatal("index lists a source no routine uses")
	}
	if h.Matcher.Listening(models.Scope{Kind: "tenant", ID: "other"}, models.RoutineEventGmailMessage, first.Task.Event.SourceRef) {
		t.Fatal("index lists the source in another scope")
	}
	offer, err := h.Matcher.OfferRoutineEvent(h.Ctx, h.Event(first, "gmail:a"))
	if err != nil || offer != (externalconversation.RoutineEventOffer{Matched: 1, Inserted: 1}) {
		t.Fatalf("offer=%#v err=%v", offer, err)
	}
	if count, _ := h.Store.CountPendingRoutineEvents(h.Ctx, h.Scope, second.ID); count != 0 {
		t.Fatalf("event for the first source reached the second routine: %d", count)
	}
	// A duplicate is counted, not stored twice.
	offer, err = h.Matcher.OfferRoutineEvent(h.Ctx, h.Event(first, "gmail:a"))
	if err != nil || offer != (externalconversation.RoutineEventOffer{Matched: 1, Duplicates: 1}) {
		t.Fatalf("duplicate offer=%#v err=%v", offer, err)
	}
	// An event of another kind on the same SourceRef never matches.
	calendar := h.Event(first, "calendar:x:created")
	calendar.Kind, calendar.Fields = models.RoutineEventCalendar, map[string][]string{"on": {"created"}}
	if offer, err := h.Matcher.OfferRoutineEvent(h.Ctx, calendar); err != nil || offer.Matched != 0 {
		t.Fatalf("calendar offer=%#v err=%v", offer, err)
	}

	// Owner check: a Google routine whose creator is not the source's person
	// never matches, even with an equal SourceRef.
	foreign := h.Routine("foreign", func(trigger *models.RoutineEventTrigger) {
		trigger.SourceRef = models.RoutineGoogleSourceRef("user-2", "sub-foreign")
	})
	if !h.Matcher.Listening(h.Scope, models.RoutineEventGmailMessage, foreign.Task.Event.SourceRef) {
		t.Fatal("foreign routine is not indexed")
	}
	if offer, err := h.Matcher.OfferRoutineEvent(h.Ctx, h.Event(foreign, "gmail:b")); err != nil || offer.Matched != 0 {
		t.Fatalf("foreign offer=%#v err=%v", offer, err)
	}

	// Owner check: a webhook routine matches only when its hook row belongs
	// to it.
	webhook := h.Routine("webhook", func(trigger *models.RoutineEventTrigger) {
		hookID := models.HookID(h.Scope, routine.RunbookActivationIDForKey(h.Scope, "create-task:webhook"))
		*trigger = models.RoutineEventTrigger{Kind: models.RoutineEventWebhook, SourceRef: models.HookSourceRef(hookID), SourceLabel: "your shop", MaxPerHour: 20, Webhook: &models.RoutineWebhook{HookID: hookID}}
	})
	hookEvent := &models.RoutineEvent{Scope: h.Scope, ID: "hs:1", Kind: models.RoutineEventWebhook, SourceRef: webhook.Task.Event.SourceRef, OccurredAt: h.Now, Summary: "Order", Payload: json.RawMessage(`{"order":1}`)}
	if offer, err := h.Matcher.OfferRoutineEvent(h.Ctx, hookEvent); err != nil || offer.Matched != 0 {
		t.Fatalf("webhook without hook row offer=%#v err=%v", offer, err)
	}
	if _, err := h.Store.CreateHook(h.Ctx, &models.Hook{ID: webhook.Task.Event.Webhook.HookID, Scope: h.Scope, ActivationID: "someone-else"}); err != nil {
		t.Fatal(err)
	}
	if offer, err := h.Matcher.OfferRoutineEvent(h.Ctx, hookEvent); err != nil || offer.Matched != 0 {
		t.Fatalf("webhook with a foreign hook row offer=%#v err=%v", offer, err)
	}

	// Owner check: a GitHub routine matches only when its callback
	// registration is active, in the event's scope and made by the
	// routine's own deployment.
	github := func(name, deploymentID string, status models.CallbackRegistrationStatus) *models.RunbookActivation {
		h.CreateCallbackRegistration(name, deploymentID, status)
		return h.Routine(name, func(trigger *models.RoutineEventTrigger) {
			*trigger = models.RoutineEventTrigger{Kind: models.RoutineEventGitHub, SourceRef: models.RoutineCallbackSourceRef("callback-" + name), SourceLabel: "your repository", MaxPerHour: 20}
		})
	}
	githubEvent := func(activation *models.RunbookActivation, id string) *models.RoutineEvent {
		return &models.RoutineEvent{Scope: h.Scope, ID: id, Kind: models.RoutineEventGitHub, SourceRef: activation.Task.Event.SourceRef, OccurredAt: h.Now, Summary: "Pull request opened", Fields: map[string][]string{"event": {"pull_request"}}}
	}
	owned := github("gh-owned", h.Owner.ID, models.CallbackRegistrationActive)
	if offer, err := h.Matcher.OfferRoutineEvent(h.Ctx, githubEvent(owned, "github:1")); err != nil || offer != (externalconversation.RoutineEventOffer{Matched: 1, Inserted: 1}) {
		t.Fatalf("github owned offer=%#v err=%v", offer, err)
	}
	otherDeployment := github("gh-foreign", "agent-2", models.CallbackRegistrationActive)
	if offer, err := h.Matcher.OfferRoutineEvent(h.Ctx, githubEvent(otherDeployment, "github:2")); err != nil || offer.Matched != 0 {
		t.Fatalf("github foreign deployment offer=%#v err=%v", offer, err)
	}
	inactive := github("gh-paused", h.Owner.ID, models.CallbackRegistrationPaused)
	if offer, err := h.Matcher.OfferRoutineEvent(h.Ctx, githubEvent(inactive, "github:3")); err != nil || offer.Matched != 0 {
		t.Fatalf("github inactive registration offer=%#v err=%v", offer, err)
	}
}

func TestRoutineEventMatcherHookOwnerMatches(t *testing.T) {
	h := routinetest.NewEventHarness(t)
	webhook := h.Routine("webhook", func(trigger *models.RoutineEventTrigger) {
		hookID := models.HookID(h.Scope, routine.RunbookActivationIDForKey(h.Scope, "create-task:webhook"))
		*trigger = models.RoutineEventTrigger{Kind: models.RoutineEventWebhook, SourceRef: models.HookSourceRef(hookID), SourceLabel: "your shop", MaxPerHour: 20, Webhook: &models.RoutineWebhook{HookID: hookID}}
	})
	if _, err := h.Store.CreateHook(h.Ctx, &models.Hook{ID: webhook.Task.Event.Webhook.HookID, Scope: h.Scope, ActivationID: webhook.ID}); err != nil {
		t.Fatal(err)
	}
	hookEvent := &models.RoutineEvent{Scope: h.Scope, ID: "hs:1", Kind: models.RoutineEventWebhook, SourceRef: webhook.Task.Event.SourceRef, OccurredAt: h.Now, Summary: "Order", Payload: json.RawMessage(`{"order":1}`)}
	if offer, err := h.Matcher.OfferRoutineEvent(h.Ctx, hookEvent); err != nil || offer != (externalconversation.RoutineEventOffer{Matched: 1, Inserted: 1}) {
		t.Fatalf("offer=%#v err=%v", offer, err)
	}
}

type recordingRoutineIndexPublisher struct{ scopes []models.Scope }

func (p *recordingRoutineIndexPublisher) PublishRoutineIndexChange(scope models.Scope) {
	p.scopes = append(p.scopes, scope)
}

func TestRoutineEventIndexRefreshOnActivationChange(t *testing.T) {
	h := routinetest.NewEventHarness(t)
	publisher := &recordingRoutineIndexPublisher{}
	h.Matcher.SetRoutineIndexPublisher(publisher)
	activation := h.Routine("inbox", nil)
	source := activation.Task.Event.SourceRef
	listening := func() bool { return h.Matcher.Listening(h.Scope, models.RoutineEventGmailMessage, source) }
	if !listening() {
		t.Fatal("created routine is not listening")
	}
	h.SetStatus(activation, models.RunbookActivationPaused)
	if listening() {
		t.Fatal("paused routine is still listening")
	}
	h.SetStatus(activation, models.RunbookActivationActive)
	if !listening() {
		t.Fatal("resumed routine is not listening")
	}
	h.SetStatus(activation, models.RunbookActivationRetired)
	if listening() {
		t.Fatal("retired routine is still listening")
	}
	if len(publisher.scopes) != 4 {
		t.Fatalf("publisher got %d signals, want 4: %#v", len(publisher.scopes), publisher.scopes)
	}
	for _, scope := range publisher.scopes {
		if scope != h.Scope {
			t.Fatalf("publisher got scope %#v", scope)
		}
	}

	// A peer's signal and the backstop rebuild read the store.
	peer := routine.NewEventMatcher(h.Store)
	defer peer.Close()
	other := h.Routine("other", nil)
	if !peer.Listening(h.Scope, models.RoutineEventGmailMessage, other.Task.Event.SourceRef) {
		t.Fatal("a second matcher on the same store did not follow the write")
	}
	cold := routine.NewEventMatcher(&routineEventStoreOnly{RoutineEventStore: h.Store, activations: h.Store})
	defer cold.Close()
	if cold.Listening(h.Scope, models.RoutineEventGmailMessage, other.Task.Event.SourceRef) {
		t.Fatal("a cold matcher listens before its first rebuild")
	}
	if err := cold.RebuildRoutineIndex(h.Ctx); err != nil {
		t.Fatal(err)
	}
	if !cold.Listening(h.Scope, models.RoutineEventGmailMessage, other.Task.Event.SourceRef) || cold.Listening(h.Scope, models.RoutineEventGmailMessage, source) {
		t.Fatal("rebuild did not reflect the store")
	}
	h.SetStatus(other, models.RunbookActivationPaused)
	if !cold.Listening(h.Scope, models.RoutineEventGmailMessage, other.Task.Event.SourceRef) {
		t.Fatal("a matcher on another store handle followed the write without a signal")
	}
	if err := cold.RefreshRoutineIndex(h.Ctx, h.Scope); err != nil {
		t.Fatal(err)
	}
	if cold.Listening(h.Scope, models.RoutineEventGmailMessage, other.Task.Event.SourceRef) {
		t.Fatal("refresh did not drop the paused routine")
	}
}

// routineEventStoreOnly is another handle on the same data, as a peer
// process would have.
type routineEventStoreOnly struct {
	models.RoutineEventStore
	activations *sqlite.Store
}

func (s *routineEventStoreOnly) GetRunbookActivation(ctx context.Context, scope models.Scope, id string) (*models.RunbookActivation, error) {
	return s.activations.GetRunbookActivation(ctx, scope, id)
}

func TestRoutineEventOverflowStoresNoPayload(t *testing.T) {
	h, db := routinetest.NewEventRowsHarness(t)
	activation := h.Routine("busy", nil)
	h.Offer(activation, routine.EventMaxPendingPerRoutine)
	h.Serial++
	overflowID := fmt.Sprintf("gmail:%06d", h.Serial)
	offer, err := h.Matcher.OfferRoutineEvent(h.Ctx, h.Event(activation, overflowID))
	if err != nil || offer != (externalconversation.RoutineEventOffer{Matched: 1, Dropped: 1}) {
		t.Fatalf("overflow offer=%#v err=%v", offer, err)
	}
	var record *routinetest.StoredEvent
	for _, stored := range routinetest.ListEventRows(t, db, h.Scope) {
		if stored.ActivationID == activation.ID && stored.EventID == overflowID {
			record = &stored
		}
	}
	if record == nil || record.State != models.RoutineEventDropped || record.DroppedReason != models.RoutineEventDroppedOverflow {
		t.Fatalf("overflow record = %#v", record)
	}
	if record.KeepsData {
		t.Fatalf("overflow record kept event data: %#v", record)
	}
	// A replay of the dropped event is still a duplicate.
	if offer, err := h.Matcher.OfferRoutineEvent(h.Ctx, h.Event(activation, overflowID)); err != nil || offer.Duplicates != 1 {
		t.Fatalf("replayed overflow offer=%#v err=%v", offer, err)
	}
	h.Pass()
	runs := h.EventRuns(activation)
	if len(runs) != 1 {
		t.Fatalf("runs = %d", len(runs))
	}
	batch, _ := h.BatchOf(runs[0])
	if batch.Dropped != 1 || len(batch.EventIDs) != models.RoutineEventMaxBatchEvents {
		t.Fatalf("batch dropped=%d events=%d", batch.Dropped, len(batch.EventIDs))
	}
}

func TestRoutineEventStats(t *testing.T) {
	h := routinetest.NewEventHarness(t)
	activation := h.Routine("inbox", nil)
	unbound := h.Event(activation, "gmail:unbound")
	unbound.SourceRef = models.RoutineGoogleSourceRef("user-1", "sub-nobody")
	if _, err := h.Matcher.OfferRoutineEvent(h.Ctx, unbound); err != nil {
		t.Fatal(err)
	}
	h.Offer(activation, 3)
	h.Pass()
	if _, _, err := h.Matcher.OfferRoutineTestEvent(h.Ctx, h.Scope, activation.ID, "user-1", nil); err != nil {
		t.Fatal(err)
	}
	h.Offer(activation, 2)
	h.FinishRuns()
	h.Pass()
	stats := h.Matcher.RoutineEventStats()
	if stats.In[models.RoutineEventGmailMessage] != 6 || stats.Matched[models.RoutineEventGmailMessage] != 5 || stats.Dropped[models.RoutineEventGmailMessage] != 0 {
		t.Fatalf("in=%d matched=%d dropped=%d", stats.In[models.RoutineEventGmailMessage], stats.Matched[models.RoutineEventGmailMessage], stats.Dropped[models.RoutineEventGmailMessage])
	}
	if stats.RunsStarted != 1 || stats.TestRunsStarted != 1 {
		t.Fatalf("runs=%d test runs=%d", stats.RunsStarted, stats.TestRunsStarted)
	}
	// After the test batch, two real events still wait.
	if stats.QueueDepth != 3 {
		t.Fatalf("queue depth = %d, want 3 (one test and two real events read in the pass)", stats.QueueDepth)
	}
	if len(stats.MatchLatencyBuckets) != len(routine.EventLatencyBuckets) || len(stats.MatchLatencyCounts) != len(routine.EventLatencyBuckets) {
		t.Fatalf("latency buckets %#v counts %#v", stats.MatchLatencyBuckets, stats.MatchLatencyCounts)
	}
	if last := stats.MatchLatencyCounts[len(stats.MatchLatencyCounts)-1]; last != 5 {
		t.Fatalf("latency observations = %d, want 5 index hits", last)
	}
	for i := 1; i < len(stats.MatchLatencyCounts); i++ {
		if stats.MatchLatencyCounts[i] < stats.MatchLatencyCounts[i-1] {
			t.Fatalf("latency counts are not cumulative: %#v", stats.MatchLatencyCounts)
		}
	}
	if stats.MatchLatencySum <= 0 {
		t.Fatalf("latency sum = %v", stats.MatchLatencySum)
	}
}

func TestRoutineTestEventDoesNotConsumeOccurrence(t *testing.T) {
	h := routinetest.NewEventHarness(t)
	activation := h.Routine("once", func(trigger *models.RoutineEventTrigger) { trigger.MaximumOccurrences = 1 })
	if _, _, err := h.Matcher.OfferRoutineTestEvent(h.Ctx, h.Scope, activation.ID, "user-2", nil); !errors.Is(err, routine.ErrRoutineNotCreator) {
		t.Fatalf("non-creator test event err = %v", err)
	}
	eventID, offer, err := h.Matcher.OfferRoutineTestEvent(h.Ctx, h.Scope, activation.ID, "user-1", nil)
	if err != nil || offer != (externalconversation.RoutineEventOffer{Matched: 1, Inserted: 1}) || !strings.HasPrefix(eventID, "test:") || !models.ValidRoutineEventID(eventID) {
		t.Fatalf("test offer=%#v id=%q err=%v", offer, eventID, err)
	}
	h.Pass()
	runs := h.EventRuns(activation)
	if len(runs) != 1 || runs[0].Context[models.RoutineEventTestContextKey] != true {
		t.Fatalf("test run = %#v", runs)
	}
	if h.Get(activation).Status != models.RunbookActivationActive {
		t.Fatal("a test run used up the routine's only occurrence")
	}
	h.FinishRuns()
	h.Offer(activation, 1)
	h.Pass()
	runs = h.EventRuns(activation)
	if len(runs) != 2 || runs[1].Context[models.RoutineEventTestContextKey] != nil {
		t.Fatalf("runs = %d, last context %#v", len(runs), runs[len(runs)-1].Context)
	}
	if h.Get(activation).Status != models.RunbookActivationRetired {
		t.Fatal("routine was not retired after its one real occurrence")
	}

	// Test events are capped per hour.
	h2 := routinetest.NewEventHarness(t)
	capped := h2.Routine("capped", nil)
	for i := 0; i < routine.TestEventsPerHour; i++ {
		if _, _, err := h2.Matcher.OfferRoutineTestEvent(h2.Ctx, h2.Scope, capped.ID, "user-1", json.RawMessage(`{"n":1}`)); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := h2.Matcher.OfferRoutineTestEvent(h2.Ctx, h2.Scope, capped.ID, "user-1", nil); !errors.Is(err, routine.ErrRoutineTestEventLimit) {
		t.Fatalf("sixth test event err = %v", err)
	}
	h2.Advance(time.Hour + time.Minute)
	if _, _, err := h2.Matcher.OfferRoutineTestEvent(h2.Ctx, h2.Scope, capped.ID, "user-1", nil); err != nil {
		t.Fatalf("test event after an hour err = %v", err)
	}
}
