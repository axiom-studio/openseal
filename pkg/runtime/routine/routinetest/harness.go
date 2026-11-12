package routinetest

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation/conversationtest"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/routine"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

func NewEventHarness(t *testing.T) *EventHarness {
	t.Helper()
	return NewEventHarnessOn(t, conversationtest.NewStore(t))
}

// NewEventRowsHarness is NewEventHarness on a store file, with
// the test's own connection to it for reading the routine_events rows the
// store keeps to itself (ListEventRows).
func NewEventRowsHarness(t *testing.T) (*EventHarness, *sql.DB) {
	t.Helper()
	store, db := newFileStoreWithDB(t)
	return NewEventHarnessOn(t, store), db
}

// ListEventRows reads the routine_events rows of scope from a SQLite
// database.
func ListEventRows(t testing.TB, db *sql.DB, scope models.Scope) []StoredEvent {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), `SELECT activation_id, event_id, state, COALESCE(dropped_reason, ''), payload IS NOT NULL
		FROM routine_events WHERE scope_kind = ? AND scope_id = ? ORDER BY activation_id, event_id`, scope.Kind, scope.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var events []StoredEvent
	for rows.Next() {
		var event StoredEvent
		if err := rows.Scan(&event.ActivationID, &event.EventID, &event.State, &event.DroppedReason, &event.KeepsData); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return events
}

// EventHarness is one store (in memory unless the test reads raw
// rows) with a matcher, a dispatcher and a test clock, plus helpers to
// create event routines through the activation service (so index refresh is
// exercised on every create).
type EventHarness struct {
	T            *testing.T
	Ctx          context.Context
	Store        *sqlite.Store
	Scope        models.Scope
	Owner        models.ObjectiveOwner
	Matcher      *routine.RoutineEventMatcher
	Dispatcher   *routine.RoutineEventDispatcher
	Now          time.Time
	Conversation string
	Message      string
	Serial       int
}

// deployOwner installs the routines' Agent: a routine starts only while its
// assigned Agent is deployed and active.
func (h *EventHarness) deployOwner() {
	h.T.Helper()
	registry := kernelagent.NewRegistryWithStore(h.Store)
	definition := &kernelagent.AgentDefinition{
		ID: "inbox-agent", Version: "1.0.0", DisplayName: "Inbox", Purpose: "Summarize email", SystemPrompt: "Summarize email.",
		Authority: kernelagent.AuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1},
	}
	if _, err := registry.RegisterDefinition(h.Ctx, definition); err != nil {
		h.T.Fatal(err)
	}
	if _, _, err := registry.CreateDeployment(h.Ctx, &kernelagent.AgentDeployment{
		ID: h.Owner.ID, Scope: capability.ScopeReference{Kind: h.Scope.Kind, ID: h.Scope.ID}, DefinitionID: definition.ID, ActiveVersion: definition.Version,
		RolloutStatus: kernelagent.RolloutActive, Environment: "default", Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 1},
	}, "user", "admin", "routine event test"); err != nil {
		h.T.Fatal(err)
	}
}

// Chat creates a chat with one user request and returns their IDs.
func (h *EventHarness) Chat(title string) (string, string) {
	h.T.Helper()
	conversations := conversation.NewConversationService(h.Store)
	c, _, err := conversations.CreateConversation(h.Ctx, conversation.CreateConversationRequest{Scope: h.Scope, Owner: h.Owner, Title: title, IdempotencyKey: "chat:" + title})
	if err != nil {
		h.T.Fatal(err)
	}
	posted, err := conversations.PostChannelMessage(h.Ctx, conversation.PostChannelMessageRequest{
		Scope: h.Scope, ConversationID: c.ID, ExpectedRevision: c.Revision,
		Sender: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "user-1"}, Intent: models.MessageIntentQuestion,
		Content: "Tell me when an email comes in", Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel}, IdempotencyKey: "request:" + title,
	})
	if err != nil {
		h.T.Fatal(err)
	}
	return c.ID, posted.Message.ID
}

func (h *EventHarness) Clock() time.Time { return h.Now }

func (h *EventHarness) Advance(d time.Duration) { h.Now = h.Now.Add(d) }

// Routine creates an active Gmail routine on its own Google source.
func (h *EventHarness) Routine(name string, mutate func(*models.RoutineEventTrigger)) *models.RunbookActivation {
	h.T.Helper()
	objective, err := portfolio.NewPortfolioService(h.Store).CreateObjective(h.Ctx, portfolio.CreateObjectiveRequest{
		Scope: h.Scope, Owner: h.Owner, Title: name, Goal: "Summarize new email for " + name, Status: models.ObjectiveStatusActive,
		ExecutionPolicy: &models.ObjectiveExecutionPolicy{MaximumConcurrentRuns: 1},
	})
	if err != nil {
		h.T.Fatal(err)
	}
	trigger := &models.RoutineEventTrigger{
		Kind: models.RoutineEventGmailMessage, SourceRef: models.RoutineGoogleSourceRef("user-1", "sub-"+name), SourceLabel: "your Gmail", MaxPerHour: 20,
	}
	if mutate != nil {
		mutate(trigger)
	}
	activation, err := routine.NewRunbookActivationService(h.Store, models.WithClock(h.Clock)).Create(h.Ctx, routine.CreateRunbookActivationRequest{
		Scope: h.Scope, Owner: h.Owner, ObjectiveID: objective.ID, AssignedAgentID: h.Owner.ID, TriggerID: "event", MaximumConcurrent: 1,
		Trigger: runbook.Trigger{Kind: runbook.TriggerEvent}, IdempotencyKey: "create-task:" + name,
		Task: &models.ScheduledAgentTask{
			Goal: "Summarize the new email.", ConversationID: h.Conversation, SourceMessageID: h.Message, CreatedBy: "user-1", Event: trigger,
		},
	})
	if err != nil {
		h.T.Fatal(err)
	}
	return activation
}

func (h *EventHarness) Event(activation *models.RunbookActivation, id string) *models.RoutineEvent {
	return &models.RoutineEvent{
		Scope: h.Scope, ID: id, Kind: activation.Task.Event.Kind, SourceRef: activation.Task.Event.SourceRef, OccurredAt: h.Now,
		Summary: "Email " + id, Fields: map[string][]string{"subject": {"Invoice " + id}},
		Payload: json.RawMessage(`{"messageId":"` + id + `"}`),
	}
}

// Offer offers count new events to the routine and returns their IDs.
func (h *EventHarness) Offer(activation *models.RunbookActivation, count int) []string {
	h.T.Helper()
	ids := make([]string, 0, count)
	for i := 0; i < count; i++ {
		h.Serial++
		id := fmt.Sprintf("gmail:%06d", h.Serial)
		offer, err := h.Matcher.OfferRoutineEvent(h.Ctx, h.Event(activation, id))
		if err != nil {
			h.T.Fatal(err)
		}
		if offer.Matched != 1 || offer.Inserted+offer.Dropped != 1 {
			h.T.Fatalf("offer %s = %#v", id, offer)
		}
		ids = append(ids, id)
		h.Advance(time.Millisecond)
	}
	return ids
}

func (h *EventHarness) Pass() *routine.RoutineEventDispatchResult {
	h.T.Helper()
	result, err := h.Dispatcher.ReconcileAll(h.Ctx, 200)
	if err != nil {
		h.T.Fatal(err)
	}
	return result
}

func (h *EventHarness) Get(activation *models.RunbookActivation) *models.RunbookActivation {
	h.T.Helper()
	current, err := h.Store.GetRunbookActivation(h.Ctx, h.Scope, activation.ID)
	if err != nil || current == nil {
		h.T.Fatalf("get activation: %#v %v", current, err)
	}
	return current
}

func (h *EventHarness) SetStatus(activation *models.RunbookActivation, status models.RunbookActivationStatus) *models.RunbookActivation {
	h.T.Helper()
	current := h.Get(activation)
	// The creator stays signed in to Google, so a resume passes its source check.
	signedIn := GoogleSources{{Kind: current.Task.Event.Kind, Source: current.Task.Event.SourceRef, Available: true}}
	updated, err := routine.NewRunbookActivationService(h.Store, models.WithClock(h.Clock)).WithRoutineEventSources(signedIn).Update(h.Ctx, h.Scope, activation.ID, routine.UpdateRunbookActivationRequest{ExpectedRevision: current.Revision, Status: status})
	if err != nil {
		h.T.Fatal(err)
	}
	return updated
}

// EventRuns lists the routine occurrence Runs of an activation, oldest first.
func (h *EventHarness) EventRuns(activation *models.RunbookActivation) []*models.AgentRun {
	h.T.Helper()
	runs, err := h.Store.ListAgentRuns(h.Ctx, models.AgentRunFilter{Scope: h.Scope, ObjectiveID: activation.ObjectiveID, Limit: 500})
	if err != nil {
		h.T.Fatal(err)
	}
	values := make([]*models.AgentRun, 0, len(runs))
	for _, run := range runs {
		if _, ok := run.Context[models.RoutineEventBatchContextKey]; ok {
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

func (h *EventHarness) ActiveEventRuns() int {
	h.T.Helper()
	runs, err := h.Store.ListAgentRuns(h.Ctx, models.AgentRunFilter{Scope: h.Scope, Limit: 500})
	if err != nil {
		h.T.Fatal(err)
	}
	count := 0
	for _, run := range runs {
		if _, ok := run.Context[models.RoutineEventBatchContextKey]; ok && !models.IsTerminalAgentRunStatus(run.Status) {
			count++
		}
	}
	return count
}

// FinishRuns ends every Run that is not finished.
func (h *EventHarness) FinishRuns() {
	h.T.Helper()
	runs, err := h.Store.ListAgentRuns(h.Ctx, models.AgentRunFilter{Scope: h.Scope, Limit: 500})
	if err != nil {
		h.T.Fatal(err)
	}
	for _, run := range runs {
		if !models.IsTerminalAgentRunStatus(run.Status) {
			h.FinishRun(run)
		}
	}
}

func (h *EventHarness) FinishRun(run *models.AgentRun) {
	h.T.Helper()
	current, err := h.Store.GetAgentRun(h.Ctx, h.Scope, run.ID)
	if err != nil {
		h.T.Fatal(err)
	}
	updated := models.CloneAgentRun(current)
	completed := h.Now
	updated.Status, updated.Revision, updated.UpdatedAt, updated.CompletedAt = models.AgentRunStatusCanceled, current.Revision+1, completed, &completed
	event := &models.ActivityEvent{
		ID: "finish-" + run.ID, Scope: h.Scope, EventType: "run.status_changed", Severity: models.ActivitySeverityInfo, AgentID: run.AssignedAgentID, RunID: run.ID,
		Actor: models.ActivityActor{Type: "worker", ID: "test"}, Summary: "Finished", Visibility: models.ActivityVisibilityScope, CreatedAt: completed,
	}
	if _, err := h.Store.UpdateAgentRunWithEvent(h.Ctx, updated, current.Revision, event, nil); err != nil {
		h.T.Fatal(err)
	}
}

// BatchOf returns the batch and records a Run handled.
func (h *EventHarness) BatchOf(run *models.AgentRun) (*models.RoutineEventBatch, []*models.RoutineEventRecord) {
	h.T.Helper()
	batchID, _ := run.Context[models.RoutineEventBatchContextKey].(string)
	batch, records, err := h.Store.GetRoutineEventBatch(h.Ctx, h.Scope, batchID)
	if err != nil || batch == nil {
		h.T.Fatalf("batch %q: %#v %v", batchID, batch, err)
	}
	return batch, records
}

// CreateCallbackRegistration stores a GitHub callback registration with ID
// "callback-<name>" made by deploymentID.
func (h *EventHarness) CreateCallbackRegistration(name, deploymentID string, status models.CallbackRegistrationStatus) {
	h.T.Helper()
	if err := h.Store.CreateCallbackRegistration(h.Ctx, &models.CallbackRegistration{
		ID: "callback-" + name, IngressRoute: "route-" + name, Scope: h.Scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: deploymentID}, DeploymentID: deploymentID,
		Name: "GitHub " + name, Provider: "github",
		Adapter:       models.CallbackAdapterReference{SkillID: "github", SkillVersion: "1.0.0", BindingID: "github", BindingRevision: 1, AdapterID: "webhooks"},
		Subscriptions: []models.CallbackSubscription{{EventType: "github.event", Consumer: "routines"}},
		Status:        status, Revision: 1, CreatedAt: h.Now, UpdatedAt: h.Now,
		Lifecycle: []models.CallbackRegistrationLifecycleEntry{{Revision: 1, Action: models.CallbackRegistrationActivated, Actor: models.ActivityActor{Type: "user", ID: "user-1"}, Reason: "connect", At: h.Now}},
	}); err != nil {
		h.T.Fatal(err)
	}
}

// PauseRun sets a Run to paused, as budget exhaustion or recovery does.
func (h *EventHarness) PauseRun(run *models.AgentRun) {
	h.T.Helper()
	current, err := h.Store.GetAgentRun(h.Ctx, h.Scope, run.ID)
	if err != nil {
		h.T.Fatal(err)
	}
	updated := models.CloneAgentRun(current)
	updated.Status, updated.Revision, updated.UpdatedAt = models.AgentRunStatusPaused, current.Revision+1, h.Now
	event := &models.ActivityEvent{
		ID: "pause-" + run.ID, Scope: h.Scope, EventType: "run.status_changed", Severity: models.ActivitySeverityInfo, AgentID: run.AssignedAgentID, RunID: run.ID,
		Actor: models.ActivityActor{Type: "worker", ID: "test"}, Summary: "Paused", Visibility: models.ActivityVisibilityScope, CreatedAt: h.Now,
	}
	if _, err := h.Store.UpdateAgentRunWithEvent(h.Ctx, updated, current.Revision, event, nil); err != nil {
		h.T.Fatal(err)
	}
}

func NewEventHarnessOn(t *testing.T, store *sqlite.Store) *EventHarness {
	t.Helper()
	h := &EventHarness{
		T: t, Ctx: t.Context(), Store: store, Scope: models.Scope{Kind: "tenant", ID: "event-routines"},
		Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent-1"}, Now: time.Now().UTC(),
	}
	h.Matcher = routine.NewRoutineEventMatcher(h.Store, models.WithClock(h.Clock))
	t.Cleanup(h.Matcher.Close)
	h.Dispatcher = routine.NewRoutineEventDispatcher(h.Store, h.Matcher, models.WithClock(h.Clock))
	conversationtest.SeedSpace(t, h.Store, h.Scope)
	h.deployOwner()
	h.Conversation, h.Message = h.Chat("Inbox")
	return h
}

// newFileStoreWithDB opens a SQLite store on a fresh file and the test's own
// connection to that file, both closed when the test ends.
func newFileStoreWithDB(t testing.TB) (*sqlite.Store, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kernel.db")
	store, err := sqlite.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store, conversationtest.OpenSQLiteDB(t, path)
}

// StoredEvent is one row of routine_events. KeepsData reports whether
// the row still holds the event's payload, summary and fields.
type StoredEvent struct {
	ActivationID, EventID string
	State                 models.RoutineEventState
	DroppedReason         string
	KeepsData             bool
}
