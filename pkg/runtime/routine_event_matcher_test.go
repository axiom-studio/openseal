package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runbook"
)

// routineEventHarness is one in-memory store with a matcher, a dispatcher and a
// test clock, plus helpers to create event routines through the activation
// service (so index refresh is exercised on every create).
type routineEventHarness struct {
	t            *testing.T
	ctx          context.Context
	store        *SQLiteStore
	scope        Scope
	owner        ObjectiveOwner
	matcher      *RoutineEventMatcher
	dispatcher   *RoutineEventDispatcher
	now          time.Time
	conversation string
	message      string
	serial       int
}

func newRoutineEventHarness(t *testing.T) *routineEventHarness {
	t.Helper()
	h := &routineEventHarness{
		t: t, ctx: t.Context(), store: newTestKernelStore(t), scope: Scope{Kind: "tenant", ID: "event-routines"},
		owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent-1"}, now: time.Now().UTC(),
	}
	h.matcher = NewRoutineEventMatcher(h.store)
	t.Cleanup(h.matcher.Close)
	h.matcher.now = h.clock
	h.dispatcher = NewRoutineEventDispatcher(h.store, h.matcher)
	h.dispatcher.now = h.clock
	h.deployAgent()
	h.conversation, h.message = h.chat("Inbox")
	return h
}

// deployAgent deploys the routines' assigned Agent. The store checks the
// assigned Agent before each start and pauses a routine whose Agent is gone.
func (h *routineEventHarness) deployAgent() {
	h.t.Helper()
	agents := kernelagent.NewRegistryWithStore(h.store)
	definition := sqliteAgentDefinition("1.0.0")
	if _, err := agents.RegisterDefinition(h.ctx, definition); err != nil {
		h.t.Fatal(err)
	}
	if _, _, err := agents.CreateDeployment(h.ctx, &kernelagent.AgentDeployment{
		ID: h.owner.ID, Scope: capability.ScopeReference{Kind: h.scope.Kind, ID: h.scope.ID}, DefinitionID: definition.ID, ActiveVersion: definition.Version,
		RolloutStatus: kernelagent.RolloutActive, Environment: "default", Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 1},
	}, "user", "user-1", "test"); err != nil {
		h.t.Fatal(err)
	}
}

// chat creates a chat with one user request and returns their IDs.
func (h *routineEventHarness) chat(title string) (string, string) {
	h.t.Helper()
	conversations := NewConversationService(h.store)
	c, _, err := conversations.CreateConversation(h.ctx, CreateConversationRequest{Scope: h.scope, Owner: h.owner, Title: title, IdempotencyKey: "chat:" + title})
	if err != nil {
		h.t.Fatal(err)
	}
	posted, err := conversations.PostChannelMessage(h.ctx, PostChannelMessageRequest{
		Scope: h.scope, ConversationID: c.ID, ExpectedRevision: c.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "user-1"}, Intent: MessageIntentQuestion,
		Content: "Tell me when an email comes in", Audience: ConversationAudience{Kind: ConversationAudienceChannel}, IdempotencyKey: "request:" + title,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return c.ID, posted.Message.ID
}

func (h *routineEventHarness) clock() time.Time { return h.now }

func (h *routineEventHarness) advance(d time.Duration) { h.now = h.now.Add(d) }

// routine creates an active Gmail routine on its own Google source.
func (h *routineEventHarness) routine(name string, mutate func(*RoutineEventTrigger)) *RunbookActivation {
	h.t.Helper()
	objective, err := NewPortfolioService(h.store).CreateObjective(h.ctx, CreateObjectiveRequest{
		Scope: h.scope, Owner: h.owner, Title: name, Goal: "Summarize new email for " + name, Status: ObjectiveStatusActive,
		ExecutionPolicy: &ObjectiveExecutionPolicy{MaximumConcurrentRuns: 1},
	})
	if err != nil {
		h.t.Fatal(err)
	}
	trigger := &RoutineEventTrigger{
		Kind: RoutineEventGmailMessage, SourceRef: RoutineGoogleSourceRef("user-1", "sub-"+name), SourceLabel: "your Gmail", MaxPerHour: 20,
	}
	if mutate != nil {
		mutate(trigger)
	}
	activation, err := NewRunbookActivationService(h.store).Create(h.ctx, CreateRunbookActivationRequest{
		Scope: h.scope, Owner: h.owner, ObjectiveID: objective.ID, AssignedAgentID: h.owner.ID, TriggerID: "event", MaximumConcurrent: 1,
		Trigger: runbook.Trigger{Kind: runbook.TriggerEvent}, IdempotencyKey: "create-task:" + name,
		Task: &ScheduledAgentTask{
			Goal: "Summarize the new email.", ConversationID: h.conversation, SourceMessageID: h.message, CreatedBy: "user-1", Event: trigger,
		},
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return activation
}

func (h *routineEventHarness) event(activation *RunbookActivation, id string) *RoutineEvent {
	return &RoutineEvent{
		Scope: h.scope, ID: id, Kind: activation.Task.Event.Kind, SourceRef: activation.Task.Event.SourceRef, OccurredAt: h.now,
		Summary: "Email " + id, Fields: map[string][]string{"subject": {"Invoice " + id}},
		Payload: json.RawMessage(`{"messageId":"` + id + `"}`),
	}
}

// offer offers count new events to the routine and returns their IDs.
func (h *routineEventHarness) offer(activation *RunbookActivation, count int) []string {
	h.t.Helper()
	ids := make([]string, 0, count)
	for i := 0; i < count; i++ {
		h.serial++
		id := fmt.Sprintf("gmail:%06d", h.serial)
		offer, err := h.matcher.OfferRoutineEvent(h.ctx, h.event(activation, id))
		if err != nil {
			h.t.Fatal(err)
		}
		if offer.Matched != 1 || offer.Inserted+offer.Dropped != 1 {
			h.t.Fatalf("offer %s = %#v", id, offer)
		}
		ids = append(ids, id)
		h.advance(time.Millisecond)
	}
	return ids
}

func (h *routineEventHarness) pass() *RoutineEventDispatchResult {
	h.t.Helper()
	result, err := h.dispatcher.ReconcileAll(h.ctx, 200)
	if err != nil {
		h.t.Fatal(err)
	}
	return result
}

func (h *routineEventHarness) get(activation *RunbookActivation) *RunbookActivation {
	h.t.Helper()
	current, err := h.store.GetRunbookActivation(h.ctx, h.scope, activation.ID)
	if err != nil || current == nil {
		h.t.Fatalf("get activation: %#v %v", current, err)
	}
	return current
}

func (h *routineEventHarness) setStatus(activation *RunbookActivation, status RunbookActivationStatus) *RunbookActivation {
	h.t.Helper()
	current := h.get(activation)
	// The creator stays signed in to Google, so a resume passes its source check.
	signedIn := routineGoogleSourcesForTest{{Kind: current.Task.Event.Kind, Source: current.Task.Event.SourceRef, Available: true}}
	updated, err := NewRunbookActivationService(h.store).WithRoutineEventSources(signedIn).Update(h.ctx, h.scope, activation.ID, UpdateRunbookActivationRequest{ExpectedRevision: current.Revision, Status: status})
	if err != nil {
		h.t.Fatal(err)
	}
	return updated
}

// eventRuns lists the routine occurrence Runs of an activation, oldest first.
func (h *routineEventHarness) eventRuns(activation *RunbookActivation) []*AgentRun {
	h.t.Helper()
	runs, err := h.store.ListAgentRuns(h.ctx, AgentRunFilter{Scope: h.scope, ObjectiveID: activation.ObjectiveID, Limit: 500})
	if err != nil {
		h.t.Fatal(err)
	}
	values := make([]*AgentRun, 0, len(runs))
	for _, run := range runs {
		if _, ok := run.Context[RoutineEventBatchContextKey]; ok {
			values = append(values, run)
		}
	}
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j].CreatedAt.Before(values[j-1].CreatedAt); j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
	return values
}

func (h *routineEventHarness) activeEventRuns() int {
	h.t.Helper()
	runs, err := h.store.ListAgentRuns(h.ctx, AgentRunFilter{Scope: h.scope, Limit: 500})
	if err != nil {
		h.t.Fatal(err)
	}
	count := 0
	for _, run := range runs {
		if _, ok := run.Context[RoutineEventBatchContextKey]; ok && !isTerminalAgentRunStatus(run.Status) {
			count++
		}
	}
	return count
}

// finishRuns ends every Run that is not finished.
func (h *routineEventHarness) finishRuns() {
	h.t.Helper()
	runs, err := h.store.ListAgentRuns(h.ctx, AgentRunFilter{Scope: h.scope, Limit: 500})
	if err != nil {
		h.t.Fatal(err)
	}
	for _, run := range runs {
		if !isTerminalAgentRunStatus(run.Status) {
			h.finishRun(run)
		}
	}
}

func (h *routineEventHarness) finishRun(run *AgentRun) {
	h.t.Helper()
	current, err := h.store.GetAgentRun(h.ctx, h.scope, run.ID)
	if err != nil {
		h.t.Fatal(err)
	}
	updated := cloneAgentRun(current)
	completed := h.now
	updated.Status, updated.Revision, updated.UpdatedAt, updated.CompletedAt = AgentRunStatusCanceled, current.Revision+1, completed, &completed
	event := &ActivityEvent{
		ID: "finish-" + run.ID, Scope: h.scope, EventType: "run.status_changed", Severity: ActivitySeverityInfo, AgentID: run.AssignedAgentID, RunID: run.ID,
		Actor: ActivityActor{Type: "worker", ID: "test"}, Summary: "Finished", Visibility: ActivityVisibilityScope, CreatedAt: completed,
	}
	if _, err := h.store.UpdateAgentRunWithEvent(h.ctx, updated, current.Revision, event, nil); err != nil {
		h.t.Fatal(err)
	}
}

// batchOf returns the batch and records a Run handled.
func (h *routineEventHarness) batchOf(run *AgentRun) (*RoutineEventBatch, []*RoutineEventRecord) {
	h.t.Helper()
	batchID, _ := run.Context[RoutineEventBatchContextKey].(string)
	batch, records, err := h.store.GetRoutineEventBatch(h.ctx, h.scope, batchID)
	if err != nil || batch == nil {
		h.t.Fatalf("batch %q: %#v %v", batchID, batch, err)
	}
	return batch, records
}

func TestRoutineEventMatcherIndexesBySource(t *testing.T) {
	h := newRoutineEventHarness(t)
	first := h.routine("first", nil)
	second := h.routine("second", nil)
	if !h.matcher.Listening(h.scope, RoutineEventGmailMessage, first.Task.Event.SourceRef) {
		t.Fatal("index does not list the first routine's source")
	}
	if h.matcher.Listening(h.scope, RoutineEventGmailMessage, RoutineGoogleSourceRef("user-1", "sub-other")) {
		t.Fatal("index lists a source no routine uses")
	}
	if h.matcher.Listening(Scope{Kind: "tenant", ID: "other"}, RoutineEventGmailMessage, first.Task.Event.SourceRef) {
		t.Fatal("index lists the source in another scope")
	}
	offer, err := h.matcher.OfferRoutineEvent(h.ctx, h.event(first, "gmail:a"))
	if err != nil || offer != (RoutineEventOffer{Matched: 1, Inserted: 1}) {
		t.Fatalf("offer=%#v err=%v", offer, err)
	}
	if count, _ := h.store.CountPendingRoutineEvents(h.ctx, h.scope, second.ID); count != 0 {
		t.Fatalf("event for the first source reached the second routine: %d", count)
	}
	// A duplicate is counted, not stored twice.
	offer, err = h.matcher.OfferRoutineEvent(h.ctx, h.event(first, "gmail:a"))
	if err != nil || offer != (RoutineEventOffer{Matched: 1, Duplicates: 1}) {
		t.Fatalf("duplicate offer=%#v err=%v", offer, err)
	}
	// An event of another kind on the same SourceRef never matches.
	calendar := h.event(first, "calendar:x:created")
	calendar.Kind, calendar.Fields = RoutineEventCalendar, map[string][]string{"on": {"created"}}
	if offer, err := h.matcher.OfferRoutineEvent(h.ctx, calendar); err != nil || offer.Matched != 0 {
		t.Fatalf("calendar offer=%#v err=%v", offer, err)
	}

	// Owner check: a Google routine whose creator is not the source's person
	// never matches, even with an equal SourceRef.
	foreign := h.routine("foreign", func(trigger *RoutineEventTrigger) {
		trigger.SourceRef = RoutineGoogleSourceRef("user-2", "sub-foreign")
	})
	if !h.matcher.Listening(h.scope, RoutineEventGmailMessage, foreign.Task.Event.SourceRef) {
		t.Fatal("foreign routine is not indexed")
	}
	if offer, err := h.matcher.OfferRoutineEvent(h.ctx, h.event(foreign, "gmail:b")); err != nil || offer.Matched != 0 {
		t.Fatalf("foreign offer=%#v err=%v", offer, err)
	}

	// Owner check: a webhook routine matches only when its hook row belongs
	// to it.
	webhook := h.routine("webhook", func(trigger *RoutineEventTrigger) {
		hookID := HookID(h.scope, RunbookActivationIDForKey(h.scope, "create-task:webhook"))
		*trigger = RoutineEventTrigger{Kind: RoutineEventWebhook, SourceRef: HookSourceRef(hookID), SourceLabel: "your shop", MaxPerHour: 20, Webhook: &RoutineWebhook{HookID: hookID}}
	})
	hookEvent := &RoutineEvent{Scope: h.scope, ID: "hs:1", Kind: RoutineEventWebhook, SourceRef: webhook.Task.Event.SourceRef, OccurredAt: h.now, Summary: "Order", Payload: json.RawMessage(`{"order":1}`)}
	if offer, err := h.matcher.OfferRoutineEvent(h.ctx, hookEvent); err != nil || offer.Matched != 0 {
		t.Fatalf("webhook without hook row offer=%#v err=%v", offer, err)
	}
	if _, err := h.store.CreateHook(h.ctx, &Hook{ID: webhook.Task.Event.Webhook.HookID, Scope: h.scope, ActivationID: "someone-else"}); err != nil {
		t.Fatal(err)
	}
	if offer, err := h.matcher.OfferRoutineEvent(h.ctx, hookEvent); err != nil || offer.Matched != 0 {
		t.Fatalf("webhook with a foreign hook row offer=%#v err=%v", offer, err)
	}

	// Owner check: a GitHub routine matches only when its callback
	// registration is active, in the event's scope and made by the
	// routine's own deployment.
	github := func(name, deploymentID string, status CallbackRegistrationStatus) *RunbookActivation {
		h.createCallbackRegistration(name, deploymentID, status)
		return h.routine(name, func(trigger *RoutineEventTrigger) {
			*trigger = RoutineEventTrigger{Kind: RoutineEventGitHub, SourceRef: RoutineCallbackSourceRef("callback-" + name), SourceLabel: "your repository", MaxPerHour: 20}
		})
	}
	githubEvent := func(activation *RunbookActivation, id string) *RoutineEvent {
		return &RoutineEvent{Scope: h.scope, ID: id, Kind: RoutineEventGitHub, SourceRef: activation.Task.Event.SourceRef, OccurredAt: h.now, Summary: "Pull request opened", Fields: map[string][]string{"event": {"pull_request"}}}
	}
	owned := github("gh-owned", h.owner.ID, CallbackRegistrationActive)
	if offer, err := h.matcher.OfferRoutineEvent(h.ctx, githubEvent(owned, "github:1")); err != nil || offer != (RoutineEventOffer{Matched: 1, Inserted: 1}) {
		t.Fatalf("github owned offer=%#v err=%v", offer, err)
	}
	otherDeployment := github("gh-foreign", "agent-2", CallbackRegistrationActive)
	if offer, err := h.matcher.OfferRoutineEvent(h.ctx, githubEvent(otherDeployment, "github:2")); err != nil || offer.Matched != 0 {
		t.Fatalf("github foreign deployment offer=%#v err=%v", offer, err)
	}
	inactive := github("gh-paused", h.owner.ID, CallbackRegistrationPaused)
	if offer, err := h.matcher.OfferRoutineEvent(h.ctx, githubEvent(inactive, "github:3")); err != nil || offer.Matched != 0 {
		t.Fatalf("github inactive registration offer=%#v err=%v", offer, err)
	}
}

// createCallbackRegistration stores a GitHub callback registration with ID
// "callback-<name>" made by deploymentID.
func (h *routineEventHarness) createCallbackRegistration(name, deploymentID string, status CallbackRegistrationStatus) {
	h.t.Helper()
	if err := h.store.CreateCallbackRegistration(h.ctx, &CallbackRegistration{
		ID: "callback-" + name, IngressRoute: "route-" + name, Scope: h.scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: deploymentID}, DeploymentID: deploymentID,
		Name: "GitHub " + name, Provider: "github",
		Adapter:       CallbackAdapterReference{SkillID: "github", SkillVersion: "1.0.0", BindingID: "github", BindingRevision: 1, AdapterID: "webhooks"},
		Subscriptions: []CallbackSubscription{{EventType: "github.event", Consumer: "routines"}},
		Status:        status, Revision: 1, CreatedAt: h.now, UpdatedAt: h.now,
		Lifecycle: []CallbackRegistrationLifecycleEntry{{Revision: 1, Action: CallbackRegistrationActivated, Actor: ActivityActor{Type: "user", ID: "user-1"}, Reason: "connect", At: h.now}},
	}); err != nil {
		h.t.Fatal(err)
	}
}

func TestRoutineEventMatcherHookOwnerMatches(t *testing.T) {
	h := newRoutineEventHarness(t)
	webhook := h.routine("webhook", func(trigger *RoutineEventTrigger) {
		hookID := HookID(h.scope, RunbookActivationIDForKey(h.scope, "create-task:webhook"))
		*trigger = RoutineEventTrigger{Kind: RoutineEventWebhook, SourceRef: HookSourceRef(hookID), SourceLabel: "your shop", MaxPerHour: 20, Webhook: &RoutineWebhook{HookID: hookID}}
	})
	if _, err := h.store.CreateHook(h.ctx, &Hook{ID: webhook.Task.Event.Webhook.HookID, Scope: h.scope, ActivationID: webhook.ID}); err != nil {
		t.Fatal(err)
	}
	hookEvent := &RoutineEvent{Scope: h.scope, ID: "hs:1", Kind: RoutineEventWebhook, SourceRef: webhook.Task.Event.SourceRef, OccurredAt: h.now, Summary: "Order", Payload: json.RawMessage(`{"order":1}`)}
	if offer, err := h.matcher.OfferRoutineEvent(h.ctx, hookEvent); err != nil || offer != (RoutineEventOffer{Matched: 1, Inserted: 1}) {
		t.Fatalf("offer=%#v err=%v", offer, err)
	}
}

type recordingRoutineIndexPublisher struct{ scopes []Scope }

func (p *recordingRoutineIndexPublisher) PublishRoutineIndexChange(scope Scope) {
	p.scopes = append(p.scopes, scope)
}

func TestRoutineEventIndexRefreshOnActivationChange(t *testing.T) {
	h := newRoutineEventHarness(t)
	publisher := &recordingRoutineIndexPublisher{}
	h.matcher.SetRoutineIndexPublisher(publisher)
	activation := h.routine("inbox", nil)
	source := activation.Task.Event.SourceRef
	listening := func() bool { return h.matcher.Listening(h.scope, RoutineEventGmailMessage, source) }
	if !listening() {
		t.Fatal("created routine is not listening")
	}
	h.setStatus(activation, RunbookActivationPaused)
	if listening() {
		t.Fatal("paused routine is still listening")
	}
	h.setStatus(activation, RunbookActivationActive)
	if !listening() {
		t.Fatal("resumed routine is not listening")
	}
	h.setStatus(activation, RunbookActivationRetired)
	if listening() {
		t.Fatal("retired routine is still listening")
	}
	if len(publisher.scopes) != 4 {
		t.Fatalf("publisher got %d signals, want 4: %#v", len(publisher.scopes), publisher.scopes)
	}
	for _, scope := range publisher.scopes {
		if scope != h.scope {
			t.Fatalf("publisher got scope %#v", scope)
		}
	}

	// A peer's signal and the backstop rebuild read the store.
	peer := NewRoutineEventMatcher(h.store)
	defer peer.Close()
	other := h.routine("other", nil)
	if !peer.Listening(h.scope, RoutineEventGmailMessage, other.Task.Event.SourceRef) {
		t.Fatal("a second matcher on the same store did not follow the write")
	}
	cold := NewRoutineEventMatcher(&routineEventStoreOnly{RoutineEventStore: h.store, activations: h.store})
	defer cold.Close()
	if cold.Listening(h.scope, RoutineEventGmailMessage, other.Task.Event.SourceRef) {
		t.Fatal("a cold matcher listens before its first rebuild")
	}
	if err := cold.RebuildRoutineIndex(h.ctx); err != nil {
		t.Fatal(err)
	}
	if !cold.Listening(h.scope, RoutineEventGmailMessage, other.Task.Event.SourceRef) || cold.Listening(h.scope, RoutineEventGmailMessage, source) {
		t.Fatal("rebuild did not reflect the store")
	}
	h.setStatus(other, RunbookActivationPaused)
	if !cold.Listening(h.scope, RoutineEventGmailMessage, other.Task.Event.SourceRef) {
		t.Fatal("a matcher on another store handle followed the write without a signal")
	}
	if err := cold.RefreshRoutineIndex(h.ctx, h.scope); err != nil {
		t.Fatal(err)
	}
	if cold.Listening(h.scope, RoutineEventGmailMessage, other.Task.Event.SourceRef) {
		t.Fatal("refresh did not drop the paused routine")
	}
}

// routineEventStoreOnly is another handle on the same data, as a peer
// process would have.
type routineEventStoreOnly struct {
	RoutineEventStore
	activations *SQLiteStore
}

func (s *routineEventStoreOnly) GetRunbookActivation(ctx context.Context, scope Scope, id string) (*RunbookActivation, error) {
	return s.activations.GetRunbookActivation(ctx, scope, id)
}

func TestRoutineEventOverflowStoresNoPayload(t *testing.T) {
	h := newRoutineEventHarness(t)
	activation := h.routine("busy", nil)
	h.offer(activation, RoutineEventMaxPendingPerRoutine)
	h.serial++
	overflowID := fmt.Sprintf("gmail:%06d", h.serial)
	offer, err := h.matcher.OfferRoutineEvent(h.ctx, h.event(activation, overflowID))
	if err != nil || offer != (RoutineEventOffer{Matched: 1, Dropped: 1}) {
		t.Fatalf("overflow offer=%#v err=%v", offer, err)
	}
	var record *storedRoutineEvent
	for _, stored := range listStoredRoutineEvents(t, h.store, h.scope) {
		if stored.ActivationID == activation.ID && stored.EventID == overflowID {
			record = &stored
		}
	}
	if record == nil || record.State != RoutineEventDropped || record.DroppedReason != RoutineEventDroppedOverflow {
		t.Fatalf("overflow record = %#v", record)
	}
	if record.KeepsData {
		t.Fatalf("overflow record kept event data: %#v", record)
	}
	// A replay of the dropped event is still a duplicate.
	if offer, err := h.matcher.OfferRoutineEvent(h.ctx, h.event(activation, overflowID)); err != nil || offer.Duplicates != 1 {
		t.Fatalf("replayed overflow offer=%#v err=%v", offer, err)
	}
	h.pass()
	runs := h.eventRuns(activation)
	if len(runs) != 1 {
		t.Fatalf("runs = %d", len(runs))
	}
	batch, _ := h.batchOf(runs[0])
	if batch.Dropped != 1 || len(batch.EventIDs) != RoutineEventMaxBatchEvents {
		t.Fatalf("batch dropped=%d events=%d", batch.Dropped, len(batch.EventIDs))
	}
}

func TestRoutineEventStats(t *testing.T) {
	h := newRoutineEventHarness(t)
	activation := h.routine("inbox", nil)
	unbound := h.event(activation, "gmail:unbound")
	unbound.SourceRef = RoutineGoogleSourceRef("user-1", "sub-nobody")
	if _, err := h.matcher.OfferRoutineEvent(h.ctx, unbound); err != nil {
		t.Fatal(err)
	}
	h.offer(activation, 3)
	h.pass()
	if _, _, err := h.matcher.OfferRoutineTestEvent(h.ctx, h.scope, activation.ID, "user-1", nil); err != nil {
		t.Fatal(err)
	}
	h.offer(activation, 2)
	h.finishRuns()
	h.pass()
	stats := h.matcher.RoutineEventStats()
	if stats.In[RoutineEventGmailMessage] != 6 || stats.Matched[RoutineEventGmailMessage] != 5 || stats.Dropped[RoutineEventGmailMessage] != 0 {
		t.Fatalf("in=%d matched=%d dropped=%d", stats.In[RoutineEventGmailMessage], stats.Matched[RoutineEventGmailMessage], stats.Dropped[RoutineEventGmailMessage])
	}
	if stats.RunsStarted != 1 || stats.TestRunsStarted != 1 {
		t.Fatalf("runs=%d test runs=%d", stats.RunsStarted, stats.TestRunsStarted)
	}
	// After the test batch, two real events still wait.
	if stats.QueueDepth != 3 {
		t.Fatalf("queue depth = %d, want 3 (one test and two real events read in the pass)", stats.QueueDepth)
	}
	if len(stats.MatchLatencyBuckets) != len(RoutineEventLatencyBuckets) || len(stats.MatchLatencyCounts) != len(RoutineEventLatencyBuckets) {
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
	h := newRoutineEventHarness(t)
	activation := h.routine("once", func(trigger *RoutineEventTrigger) { trigger.MaximumOccurrences = 1 })
	if _, _, err := h.matcher.OfferRoutineTestEvent(h.ctx, h.scope, activation.ID, "user-2", nil); !errors.Is(err, ErrRoutineNotCreator) {
		t.Fatalf("non-creator test event err = %v", err)
	}
	eventID, offer, err := h.matcher.OfferRoutineTestEvent(h.ctx, h.scope, activation.ID, "user-1", nil)
	if err != nil || offer != (RoutineEventOffer{Matched: 1, Inserted: 1}) || !strings.HasPrefix(eventID, "test:") || !ValidRoutineEventID(eventID) {
		t.Fatalf("test offer=%#v id=%q err=%v", offer, eventID, err)
	}
	h.pass()
	runs := h.eventRuns(activation)
	if len(runs) != 1 || runs[0].Context[RoutineEventTestContextKey] != true {
		t.Fatalf("test run = %#v", runs)
	}
	if h.get(activation).Status != RunbookActivationActive {
		t.Fatal("a test run used up the routine's only occurrence")
	}
	h.finishRuns()
	h.offer(activation, 1)
	h.pass()
	runs = h.eventRuns(activation)
	if len(runs) != 2 || runs[1].Context[RoutineEventTestContextKey] != nil {
		t.Fatalf("runs = %d, last context %#v", len(runs), runs[len(runs)-1].Context)
	}
	if h.get(activation).Status != RunbookActivationRetired {
		t.Fatal("routine was not retired after its one real occurrence")
	}

	// Test events are capped per hour.
	h2 := newRoutineEventHarness(t)
	capped := h2.routine("capped", nil)
	for i := 0; i < RoutineTestEventsPerHour; i++ {
		if _, _, err := h2.matcher.OfferRoutineTestEvent(h2.ctx, h2.scope, capped.ID, "user-1", json.RawMessage(`{"n":1}`)); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := h2.matcher.OfferRoutineTestEvent(h2.ctx, h2.scope, capped.ID, "user-1", nil); !errors.Is(err, ErrRoutineTestEventLimit) {
		t.Fatalf("sixth test event err = %v", err)
	}
	h2.advance(time.Hour + time.Minute)
	if _, _, err := h2.matcher.OfferRoutineTestEvent(h2.ctx, h2.scope, capped.ID, "user-1", nil); err != nil {
		t.Fatalf("test event after an hour err = %v", err)
	}
}
