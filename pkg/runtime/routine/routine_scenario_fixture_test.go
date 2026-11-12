package routine_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"testing"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/externalconversation"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/routine"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// copied from routine_event_matcher_test.go
func newRoutineEventHarness(t *testing.T) *routineEventHarness {
	t.Helper()
	return newRoutineEventHarnessOn(t, newTestKernelStore(t))
}

// copied from routine_event_matcher_test.go
// newRoutineEventRowsHarness is newRoutineEventHarness on a store file, with
// the test's own connection to it for reading the routine_events rows the
// store keeps to itself (listRoutineEventRows).
func newRoutineEventRowsHarness(t *testing.T) (*routineEventHarness, *sql.DB) {
	t.Helper()
	store, db := newTestSQLiteStore(t)
	return newRoutineEventHarnessOn(t, store), db
}

// copied from raw_sql_test.go
// listRoutineEventRows reads the routine_events rows of scope from a SQLite
// database.
func listRoutineEventRows(t testing.TB, db *sql.DB, scope models.Scope) []storedRoutineEvent {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), `SELECT activation_id, event_id, state, COALESCE(dropped_reason, ''), payload IS NOT NULL
		FROM routine_events WHERE scope_kind = ? AND scope_id = ? ORDER BY activation_id, event_id`, scope.Kind, scope.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var events []storedRoutineEvent
	for rows.Next() {
		var event storedRoutineEvent
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

// copied from runbook_action_test.go
// routineTestStore is a SQLite kernel store for routine action tests.
func routineTestStore(t *testing.T, kind string) models.KernelStore {
	t.Helper()
	store, err := sqlite.NewSQLiteStore(filepath.Join(t.TempDir(), "routines.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// copied from scheduled_task_test.go
func scheduledTaskFixture(t *testing.T, store models.KernelStore, runbookDefinitions ...*skill.Definition) (*models.AgentRun, *skill.Catalog, *skill.BoundAction, map[string]interface{}) {
	t.Helper()
	ctx := t.Context()
	scope := models.Scope{Kind: "tenant", ID: "task-test"}
	owner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "researcher"}
	reader := store.(models.ConversationStore)
	service := conversation.NewConversationService(reader)
	seedTestSpace(t, store.(models.SpaceStore), scope)
	c, _, err := service.CreateConversation(ctx, conversation.CreateConversationRequest{Scope: scope, Owner: owner, Title: "Recurring research", IdempotencyKey: "chat"})
	if err != nil {
		t.Fatal(err)
	}
	posted, err := service.PostChannelMessage(ctx, conversation.PostChannelMessageRequest{Scope: scope, ConversationID: c.ID, ExpectedRevision: c.Revision, Sender: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "user"}, Intent: models.MessageIntentQuestion, Content: "Check the public forecast every thirty minutes", Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel}, IdempotencyKey: "user-request"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := portfolio.NewPortfolioService(store).CreateAgentRun(ctx, models.CreateAgentRunRequest{Scope: scope, Kind: models.RunKindConversation, Owner: owner, AssignedAgentID: owner.ID, ActingUser: "user", Goal: "Handle request", Source: models.RunSourceChat, Context: map[string]interface{}{models.ConversationRunConversationIDContextKey: c.ID, models.ConversationRunTriggerIDContextKey: posted.Message.ID}})
	if err != nil {
		t.Fatal(err)
	}
	registryStore, ok := store.(kernelagent.Store)
	if !ok {
		registryStore = kernelagent.NewMemoryStore()
	}
	registry := kernelagent.NewRegistryWithStore(registryStore)
	definition := sqliteAgentDefinition("1.0.0")
	if _, err := registry.RegisterDefinition(ctx, definition); err != nil {
		t.Fatal(err)
	}
	if _, _, err := registry.CreateDeployment(ctx, &kernelagent.AgentDeployment{ID: owner.ID, Scope: capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DefinitionID: definition.ID, ActiveVersion: definition.Version, RolloutStatus: kernelagent.RolloutActive, Environment: "default", Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 1}}, "user", "user", "test"); err != nil {
		t.Fatal(err)
	}
	catalog := skill.NewCatalog()
	def := routine.RunbookManagementSkill()
	if len(runbookDefinitions) == 1 {
		def = runbookDefinitions[0]
	}
	if err := catalog.Register(ctx, def); err != nil {
		t.Fatal(err)
	}
	binding := &skill.Binding{ID: "routines", Revision: 1, Scope: capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: owner.ID, SkillID: def.ID, SkillVersion: def.Version, AllowedActions: []string{action.RunbookActionCreateTask, action.RunbookActionList, action.RunbookActionSetStatus, routine.RunbookActionStart, routine.RunbookActionReplaceSchedule, routine.RunbookActionEventSources, routine.RunbookActionSendTestEvent}, MaximumRisk: skill.RiskLevelWrite}
	if err := catalog.Bind(ctx, binding); err != nil {
		t.Fatal(err)
	}
	bound, err := catalog.Resolve(ctx, binding.Scope, owner.ID, def.ID, def.Version, action.RunbookActionCreateTask)
	if err != nil {
		t.Fatal(err)
	}
	args := map[string]interface{}{"title": "Forecast update", "goal": "Use the browser to read the current public forecast and summarize it with its source.",
		"trigger": map[string]interface{}{"schedule": map[string]interface{}{"cron": "0 */30 * * * *", "timezone": "Asia/Kolkata", "maximumOccurrences": float64(2)}},
		"brief":   map[string]interface{}{"summary": "Reads the public forecast every thirty minutes and posts a summary here.", "grants": []interface{}{}}}
	return run, catalog, bound, args
}

// copied from routine_event_sources_test.go
func newEventRoutineFixture(t *testing.T, hooksAvailable bool) *eventRoutineFixture {
	t.Helper()
	ctx := t.Context()
	store := newTestKernelStore(t)
	run, catalog, bound, args := scheduledTaskFixture(t, store)
	now := time.Now().UTC()
	owner := run.Owner
	gateway := &models.ExternalConversationGatewayRegistration{
		ID: "gw-slack", IngressRoute: "route-gw-slack", Name: "Acme", Status: models.ExternalConversationGatewayActive, Revision: 1, CreatedAt: now, UpdatedAt: now,
		Gateway:   models.ExternalConversationIngressGateway{Scope: run.Scope, Owner: owner, DeploymentID: owner.ID, Adapter: routineSlackAdapterForTest, Provider: "slack", InstallationID: "T1"},
		Lifecycle: []models.ExternalConversationGatewayLifecycleEntry{{Revision: 1, Action: models.ExternalConversationGatewayCreated, Actor: models.ActivityActor{Type: "user", ID: "user"}, Reason: "test", At: now}},
	}
	if err := store.CreateExternalConversationGateway(ctx, gateway); err != nil {
		t.Fatal(err)
	}
	endpoint := func(id, name, address string, mode capability.ConversationEndpointMode) *models.ExternalConversationEndpoint {
		return &models.ExternalConversationEndpoint{
			ID: id, IngressRoute: "route-" + id, Scope: run.Scope, Owner: owner, DeploymentID: owner.ID, Name: name,
			Adapter: routineSlackAdapterForTest, Provider: "slack", Mode: mode, InstallationID: "T1", Address: address,
			Handler: models.ExternalConversationHandler{Kind: models.ExternalConversationHandlerAgent, ID: owner.ID},
			Policy:  models.ExternalConversationPolicy{MessageSelection: models.ExternalConversationSelectAllMessages, ReplyMode: models.ExternalConversationReplyProviderDefault},
			Status:  models.ExternalConversationEndpointActive, Revision: 1, CreatedAt: now, UpdatedAt: now,
		}
	}
	for _, value := range []*models.ExternalConversationEndpoint{
		endpoint("ep-support", "#support", "C1", capability.ConversationEndpointChannel),
		endpoint("ep-dm", "Direct messages", "D1", capability.ConversationEndpointDirect),
	} {
		if err := store.CreateExternalConversationEndpoint(ctx, value); err != nil {
			t.Fatal(err)
		}
	}
	registration := func(id, name, deploymentID string, subscriptions ...models.CallbackSubscription) *models.CallbackRegistration {
		registrationOwner := owner
		registrationOwner.ID = deploymentID
		return &models.CallbackRegistration{
			ID: id, IngressRoute: "route-" + id, Scope: run.Scope, Owner: registrationOwner, DeploymentID: deploymentID, Name: name, Provider: "github",
			Adapter:       models.CallbackAdapterReference{SkillID: "github", SkillVersion: "1.0.0", BindingID: "github-binding", BindingRevision: 1, AdapterID: "callbacks"},
			Subscriptions: subscriptions, Status: models.CallbackRegistrationActive, Revision: 1, CreatedAt: now, UpdatedAt: now,
			Lifecycle: []models.CallbackRegistrationLifecycleEntry{{Revision: 1, Action: models.CallbackRegistrationCreated, Actor: models.ActivityActor{Type: "user", ID: "user"}, Reason: "test", At: now}},
		}
	}
	for _, value := range []*models.CallbackRegistration{
		registration("reg-api", "acme/api", owner.ID, models.CallbackSubscription{EventType: "github.issues.opened", Consumer: "approvals"}, models.CallbackSubscription{EventType: "github.issues.opened", Consumer: "routines"}, models.CallbackSubscription{EventType: "github.pull_request.opened", Consumer: "routines"}),
		registration("reg-approvals", "acme/web", owner.ID, models.CallbackSubscription{EventType: "github.issues.opened", Consumer: "approvals"}),
		registration("reg-other", "other/repo", "someone-else", models.CallbackSubscription{EventType: "github.issues.opened", Consumer: "routines"}),
	} {
		if err := store.CreateCallbackRegistration(ctx, value); err != nil {
			t.Fatal(err)
		}
	}
	offerer := &routineTestOfferer{}
	google := routineGoogleSourcesForTest{
		{Kind: models.RoutineEventGmailMessage, Source: models.RoutineGoogleSourceRef("user", "g-user"), Label: "your Gmail (user@acme.com)", Available: true, SkillID: "google"},
		{Kind: models.RoutineEventGmailMessage, Source: models.RoutineGoogleSourceRef("other", "g-other"), Label: "Gmail (other@acme.com)", Available: true, SkillID: "google"},
		{Kind: models.RoutineEventCalendar, Source: models.RoutineGoogleSourceRef("user", "g-user"), Label: "your Google Calendar (user@acme.com)", Available: false, Reason: routine.RoutineEventSourceNeedsSignIn, SkillID: "google"},
	}
	validator, err := routine.NewRunbookActionValidator(store)
	if err != nil {
		t.Fatal(err)
	}
	validator.SetRoutineEvents(offerer, google, hooksAvailable)
	dispatcher, err := routine.NewRunbookActionDispatcher(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	dispatcher.SetRoutineEvents(offerer, google, hooksAvailable)
	return &eventRoutineFixture{
		store: store, run: run, catalog: catalog, bound: bound, args: args, offerer: offerer, google: google,
		validator: validator, dispatcher: dispatcher,
		slackRef:  models.RoutineConversationSourceRef(owner.ID, routineSlackAdapterForTest.BindingID, routineSlackAdapterForTest.AdapterID, "T1"),
		githubRef: models.RoutineCallbackSourceRef("reg-api"),
	}
}

// copied from routine_event_sources_test.go
// routineGoogleSourcesForTest returns its options whoever asks, so the
// kernel's own person check is what is tested.
type routineGoogleSourcesForTest []routine.RoutineEventSourceOption

// copied from routine_event_sources_test.go
func (c routineGoogleSourcesForTest) ListRoutineEventSources(context.Context, models.Scope, models.ObjectiveOwner, string) ([]routine.RoutineEventSourceOption, error) {
	return append([]routine.RoutineEventSourceOption(nil), c...), nil
}

// copied from routine_event_matcher_test.go
// routineEventHarness is one store (in memory unless the test reads raw
// rows) with a matcher, a dispatcher and a test clock, plus helpers to
// create event routines through the activation service (so index refresh is
// exercised on every create).
type routineEventHarness struct {
	t            *testing.T
	ctx          context.Context
	store        *sqlite.Store
	scope        models.Scope
	owner        models.ObjectiveOwner
	matcher      *routine.RoutineEventMatcher
	dispatcher   *routine.RoutineEventDispatcher
	now          time.Time
	conversation string
	message      string
	serial       int
}

// copied from routine_event_matcher_test.go
// deployOwner installs the routines' Agent: a routine starts only while its
// assigned Agent is deployed and active.
func (h *routineEventHarness) deployOwner() {
	h.t.Helper()
	registry := kernelagent.NewRegistryWithStore(h.store)
	definition := &kernelagent.AgentDefinition{
		ID: "inbox-agent", Version: "1.0.0", DisplayName: "Inbox", Purpose: "Summarize email", SystemPrompt: "Summarize email.",
		Authority: kernelagent.AuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1},
	}
	if _, err := registry.RegisterDefinition(h.ctx, definition); err != nil {
		h.t.Fatal(err)
	}
	if _, _, err := registry.CreateDeployment(h.ctx, &kernelagent.AgentDeployment{
		ID: h.owner.ID, Scope: capability.ScopeReference{Kind: h.scope.Kind, ID: h.scope.ID}, DefinitionID: definition.ID, ActiveVersion: definition.Version,
		RolloutStatus: kernelagent.RolloutActive, Environment: "default", Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 1},
	}, "user", "admin", "routine event test"); err != nil {
		h.t.Fatal(err)
	}
}

// copied from routine_event_matcher_test.go
// chat creates a chat with one user request and returns their IDs.
func (h *routineEventHarness) chat(title string) (string, string) {
	h.t.Helper()
	conversations := conversation.NewConversationService(h.store)
	c, _, err := conversations.CreateConversation(h.ctx, conversation.CreateConversationRequest{Scope: h.scope, Owner: h.owner, Title: title, IdempotencyKey: "chat:" + title})
	if err != nil {
		h.t.Fatal(err)
	}
	posted, err := conversations.PostChannelMessage(h.ctx, conversation.PostChannelMessageRequest{
		Scope: h.scope, ConversationID: c.ID, ExpectedRevision: c.Revision,
		Sender: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "user-1"}, Intent: models.MessageIntentQuestion,
		Content: "Tell me when an email comes in", Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel}, IdempotencyKey: "request:" + title,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return c.ID, posted.Message.ID
}

// copied from routine_event_matcher_test.go
func (h *routineEventHarness) clock() time.Time { return h.now }

// copied from routine_event_matcher_test.go
func (h *routineEventHarness) advance(d time.Duration) { h.now = h.now.Add(d) }

// copied from routine_event_matcher_test.go
// routine creates an active Gmail routine on its own Google source.
func (h *routineEventHarness) routine(name string, mutate func(*models.RoutineEventTrigger)) *models.RunbookActivation {
	h.t.Helper()
	objective, err := portfolio.NewPortfolioService(h.store).CreateObjective(h.ctx, portfolio.CreateObjectiveRequest{
		Scope: h.scope, Owner: h.owner, Title: name, Goal: "Summarize new email for " + name, Status: models.ObjectiveStatusActive,
		ExecutionPolicy: &models.ObjectiveExecutionPolicy{MaximumConcurrentRuns: 1},
	})
	if err != nil {
		h.t.Fatal(err)
	}
	trigger := &models.RoutineEventTrigger{
		Kind: models.RoutineEventGmailMessage, SourceRef: models.RoutineGoogleSourceRef("user-1", "sub-"+name), SourceLabel: "your Gmail", MaxPerHour: 20,
	}
	if mutate != nil {
		mutate(trigger)
	}
	activation, err := routine.NewRunbookActivationService(h.store, models.WithClock(h.clock)).Create(h.ctx, routine.CreateRunbookActivationRequest{
		Scope: h.scope, Owner: h.owner, ObjectiveID: objective.ID, AssignedAgentID: h.owner.ID, TriggerID: "event", MaximumConcurrent: 1,
		Trigger: runbook.Trigger{Kind: runbook.TriggerEvent}, IdempotencyKey: "create-task:" + name,
		Task: &models.ScheduledAgentTask{
			Goal: "Summarize the new email.", ConversationID: h.conversation, SourceMessageID: h.message, CreatedBy: "user-1", Event: trigger,
		},
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return activation
}

// copied from routine_event_matcher_test.go
func (h *routineEventHarness) event(activation *models.RunbookActivation, id string) *models.RoutineEvent {
	return &models.RoutineEvent{
		Scope: h.scope, ID: id, Kind: activation.Task.Event.Kind, SourceRef: activation.Task.Event.SourceRef, OccurredAt: h.now,
		Summary: "Email " + id, Fields: map[string][]string{"subject": {"Invoice " + id}},
		Payload: json.RawMessage(`{"messageId":"` + id + `"}`),
	}
}

// copied from routine_event_matcher_test.go
// offer offers count new events to the routine and returns their IDs.
func (h *routineEventHarness) offer(activation *models.RunbookActivation, count int) []string {
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

// copied from routine_event_matcher_test.go
func (h *routineEventHarness) pass() *routine.RoutineEventDispatchResult {
	h.t.Helper()
	result, err := h.dispatcher.ReconcileAll(h.ctx, 200)
	if err != nil {
		h.t.Fatal(err)
	}
	return result
}

// copied from routine_event_matcher_test.go
func (h *routineEventHarness) get(activation *models.RunbookActivation) *models.RunbookActivation {
	h.t.Helper()
	current, err := h.store.GetRunbookActivation(h.ctx, h.scope, activation.ID)
	if err != nil || current == nil {
		h.t.Fatalf("get activation: %#v %v", current, err)
	}
	return current
}

// copied from routine_event_matcher_test.go
func (h *routineEventHarness) setStatus(activation *models.RunbookActivation, status models.RunbookActivationStatus) *models.RunbookActivation {
	h.t.Helper()
	current := h.get(activation)
	// The creator stays signed in to Google, so a resume passes its source check.
	signedIn := routineGoogleSourcesForTest{{Kind: current.Task.Event.Kind, Source: current.Task.Event.SourceRef, Available: true}}
	updated, err := routine.NewRunbookActivationService(h.store, models.WithClock(h.clock)).WithRoutineEventSources(signedIn).Update(h.ctx, h.scope, activation.ID, routine.UpdateRunbookActivationRequest{ExpectedRevision: current.Revision, Status: status})
	if err != nil {
		h.t.Fatal(err)
	}
	return updated
}

// copied from routine_event_matcher_test.go
// eventRuns lists the routine occurrence Runs of an activation, oldest first.
func (h *routineEventHarness) eventRuns(activation *models.RunbookActivation) []*models.AgentRun {
	h.t.Helper()
	runs, err := h.store.ListAgentRuns(h.ctx, models.AgentRunFilter{Scope: h.scope, ObjectiveID: activation.ObjectiveID, Limit: 500})
	if err != nil {
		h.t.Fatal(err)
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

// copied from routine_event_matcher_test.go
func (h *routineEventHarness) activeEventRuns() int {
	h.t.Helper()
	runs, err := h.store.ListAgentRuns(h.ctx, models.AgentRunFilter{Scope: h.scope, Limit: 500})
	if err != nil {
		h.t.Fatal(err)
	}
	count := 0
	for _, run := range runs {
		if _, ok := run.Context[models.RoutineEventBatchContextKey]; ok && !models.IsTerminalAgentRunStatus(run.Status) {
			count++
		}
	}
	return count
}

// copied from routine_event_matcher_test.go
// finishRuns ends every Run that is not finished.
func (h *routineEventHarness) finishRuns() {
	h.t.Helper()
	runs, err := h.store.ListAgentRuns(h.ctx, models.AgentRunFilter{Scope: h.scope, Limit: 500})
	if err != nil {
		h.t.Fatal(err)
	}
	for _, run := range runs {
		if !models.IsTerminalAgentRunStatus(run.Status) {
			h.finishRun(run)
		}
	}
}

// copied from routine_event_matcher_test.go
func (h *routineEventHarness) finishRun(run *models.AgentRun) {
	h.t.Helper()
	current, err := h.store.GetAgentRun(h.ctx, h.scope, run.ID)
	if err != nil {
		h.t.Fatal(err)
	}
	updated := models.CloneAgentRun(current)
	completed := h.now
	updated.Status, updated.Revision, updated.UpdatedAt, updated.CompletedAt = models.AgentRunStatusCanceled, current.Revision+1, completed, &completed
	event := &models.ActivityEvent{
		ID: "finish-" + run.ID, Scope: h.scope, EventType: "run.status_changed", Severity: models.ActivitySeverityInfo, AgentID: run.AssignedAgentID, RunID: run.ID,
		Actor: models.ActivityActor{Type: "worker", ID: "test"}, Summary: "Finished", Visibility: models.ActivityVisibilityScope, CreatedAt: completed,
	}
	if _, err := h.store.UpdateAgentRunWithEvent(h.ctx, updated, current.Revision, event, nil); err != nil {
		h.t.Fatal(err)
	}
}

// copied from routine_event_matcher_test.go
// batchOf returns the batch and records a Run handled.
func (h *routineEventHarness) batchOf(run *models.AgentRun) (*models.RoutineEventBatch, []*models.RoutineEventRecord) {
	h.t.Helper()
	batchID, _ := run.Context[models.RoutineEventBatchContextKey].(string)
	batch, records, err := h.store.GetRoutineEventBatch(h.ctx, h.scope, batchID)
	if err != nil || batch == nil {
		h.t.Fatalf("batch %q: %#v %v", batchID, batch, err)
	}
	return batch, records
}

// copied from routine_event_matcher_test.go
// createCallbackRegistration stores a GitHub callback registration with ID
// "callback-<name>" made by deploymentID.
func (h *routineEventHarness) createCallbackRegistration(name, deploymentID string, status models.CallbackRegistrationStatus) {
	h.t.Helper()
	if err := h.store.CreateCallbackRegistration(h.ctx, &models.CallbackRegistration{
		ID: "callback-" + name, IngressRoute: "route-" + name, Scope: h.scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: deploymentID}, DeploymentID: deploymentID,
		Name: "GitHub " + name, Provider: "github",
		Adapter:       models.CallbackAdapterReference{SkillID: "github", SkillVersion: "1.0.0", BindingID: "github", BindingRevision: 1, AdapterID: "webhooks"},
		Subscriptions: []models.CallbackSubscription{{EventType: "github.event", Consumer: "routines"}},
		Status:        status, Revision: 1, CreatedAt: h.now, UpdatedAt: h.now,
		Lifecycle: []models.CallbackRegistrationLifecycleEntry{{Revision: 1, Action: models.CallbackRegistrationActivated, Actor: models.ActivityActor{Type: "user", ID: "user-1"}, Reason: "connect", At: h.now}},
	}); err != nil {
		h.t.Fatal(err)
	}
}

// copied from routine_event_matcher_test.go
// pauseRun sets a Run to paused, as budget exhaustion or recovery does.
func (h *routineEventHarness) pauseRun(run *models.AgentRun) {
	h.t.Helper()
	current, err := h.store.GetAgentRun(h.ctx, h.scope, run.ID)
	if err != nil {
		h.t.Fatal(err)
	}
	updated := models.CloneAgentRun(current)
	updated.Status, updated.Revision, updated.UpdatedAt = models.AgentRunStatusPaused, current.Revision+1, h.now
	event := &models.ActivityEvent{
		ID: "pause-" + run.ID, Scope: h.scope, EventType: "run.status_changed", Severity: models.ActivitySeverityInfo, AgentID: run.AssignedAgentID, RunID: run.ID,
		Actor: models.ActivityActor{Type: "worker", ID: "test"}, Summary: "Paused", Visibility: models.ActivityVisibilityScope, CreatedAt: h.now,
	}
	if _, err := h.store.UpdateAgentRunWithEvent(h.ctx, updated, current.Revision, event, nil); err != nil {
		h.t.Fatal(err)
	}
}

// copied from routine_event_matcher_test.go
func newRoutineEventHarnessOn(t *testing.T, store *sqlite.Store) *routineEventHarness {
	t.Helper()
	h := &routineEventHarness{
		t: t, ctx: t.Context(), store: store, scope: models.Scope{Kind: "tenant", ID: "event-routines"},
		owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent-1"}, now: time.Now().UTC(),
	}
	h.matcher = routine.NewRoutineEventMatcher(h.store, models.WithClock(h.clock))
	t.Cleanup(h.matcher.Close)
	h.dispatcher = routine.NewRoutineEventDispatcher(h.store, h.matcher, models.WithClock(h.clock))
	seedTestSpace(t, h.store, h.scope)
	h.deployOwner()
	h.conversation, h.message = h.chat("Inbox")
	return h
}

// copied from kernel_store_test.go
// newTestKernelStore returns an empty in-memory kernel store that is closed
// when the test ends. runtimetest.NewKernelStore, skillbindingtest.NewStore,
// actiontest.NewStore, conversationtest.NewStore and the openers in the
// store/sqlite, callback, skillbinding, action and conversation tests are
// its copies for other packages (importing runtimetest here would be a
// cycle); change them together.
func newTestKernelStore(t testing.TB) *sqlite.Store {
	t.Helper()
	store, err := sqlite.NewInMemorySQLiteStore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// copied from kernel_store_test.go
// newTestSQLiteStore opens a SQLite store on a fresh file and the test's own
// connection to that file, both closed when the test ends. Use it instead of
// newTestKernelStore when the test reads or writes rows no store method
// offers.
func newTestSQLiteStore(t testing.TB) (*sqlite.Store, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kernel.db")
	store, err := sqlite.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store, openTestSQLiteDB(t, path)
}

// copied from raw_sql_test.go
// storedRoutineEvent is one row of routine_events. KeepsData reports whether
// the row still holds the event's payload, summary and fields.
type storedRoutineEvent struct {
	ActivationID, EventID string
	State                 models.RoutineEventState
	DroppedReason         string
	KeepsData             bool
}

// copied from space_seed_test.go
// seedTestSpace gives the empty owner a default space in the scope, so a
// conversation created with no space and no person behind it has one to go to.
func seedTestSpace(t testing.TB, store models.SpaceStore, scope models.Scope) *models.Space {
	t.Helper()
	return seedTestSpaceFor(t, store, scope, "")
}

// copied from agent_registry_sqlite_test.go
func sqliteAgentDefinition(version string) *kernelagent.AgentDefinition {
	return &kernelagent.AgentDefinition{
		ID: "operator", Version: version, DisplayName: "Operator", Purpose: "Operate", SystemPrompt: "Operate safely.",
		Authority: kernelagent.AuthorityPolicy{MaximumRisk: capability.RiskLevelExternal, MaxConcurrentRuns: 2},
	}
}

// copied from routine_event_sources_test.go
type eventRoutineFixture struct {
	store      *sqlite.Store
	run        *models.AgentRun
	catalog    *skill.Catalog
	bound      *skill.BoundAction
	args       map[string]interface{}
	offerer    *routineTestOfferer
	google     routineGoogleSourcesForTest
	validator  *routine.RunbookActionValidator
	dispatcher *routine.RunbookActionDispatcher
	slackRef   string
	githubRef  string
}

// copied from routine_event_sources_test.go
func (f *eventRoutineFixture) boundAction(t *testing.T, action string) *skill.BoundAction {
	t.Helper()
	bound, err := f.catalog.Resolve(t.Context(), f.bound.Binding.Scope, f.run.AssignedAgentID, f.bound.Definition.ID, f.bound.Definition.Version, action)
	if err != nil {
		t.Fatal(err)
	}
	return bound
}

// copied from routine_event_sources_test.go
// withEvent returns create_task arguments whose trigger is this event.
func (f *eventRoutineFixture) withEvent(event map[string]interface{}) map[string]interface{} {
	args := maps.Clone(f.args)
	args["trigger"] = map[string]interface{}{"event": event}
	return args
}

// copied from routine_event_sources_test.go
func (f *eventRoutineFixture) validate(t *testing.T, run *models.AgentRun, action string, args map[string]interface{}) (map[string]interface{}, error) {
	t.Helper()
	bound := f.boundAction(t, action)
	if err := f.catalog.ValidateInput(t.Context(), bound, args); err != nil {
		t.Fatalf("arguments break the %s contract: %v", action, err)
	}
	return f.validator.ValidateActionProposal(t.Context(), agentrun.ActionProposalValidationInput{Run: run, Bound: bound, Arguments: args})
}

// copied from routine_event_sources_test.go
func (f *eventRoutineFixture) dispatch(t *testing.T, run *models.AgentRun, action string, args map[string]interface{}, callID string) (map[string]interface{}, error) {
	t.Helper()
	bound := f.boundAction(t, action)
	result, err := f.dispatcher.DispatchAction(t.Context(), agentrun.ActionDispatchInput{Run: run, Bound: bound, Call: &models.ActionCall{ID: callID, Scope: run.Scope, RunID: run.ID, DeploymentID: run.AssignedAgentID, IdempotencyKey: callID}, Arguments: args})
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if err := f.catalog.ValidateOutput(t.Context(), bound, decoded); err != nil {
		t.Fatalf("%s result contract: %v", action, err)
	}
	return result, nil
}

// copied from routine_event_sources_test.go
func (f *eventRoutineFixture) slackEvent(channels ...string) map[string]interface{} {
	condition := map[string]interface{}{"field": "channel", "op": "equals", "value": channels[0]}
	if len(channels) > 1 {
		values := make([]interface{}, 0, len(channels))
		for _, channel := range channels {
			values = append(values, channel)
		}
		condition = map[string]interface{}{"field": "channel", "op": "one_of", "values": values}
	}
	return map[string]interface{}{"kind": "slack.message", "source": f.slackRef, "filter": []interface{}{condition}}
}

// copied from routine_event_sources_test.go
// setRoutineStatus runs set_status the way a chat does.
func (f *eventRoutineFixture) setRoutineStatus(t *testing.T, activationID string, status models.RunbookActivationStatus, callID string) (*models.RunbookActivation, error) {
	t.Helper()
	current, err := f.store.GetRunbookActivation(t.Context(), f.run.Scope, activationID)
	if err != nil {
		t.Fatal(err)
	}
	result, err := f.dispatch(t, f.run, action.RunbookActionSetStatus, map[string]interface{}{
		"activationId": activationID, "expectedRevision": float64(current.Revision), "status": string(status), "reason": "test",
	}, callID)
	if err != nil {
		return nil, err
	}
	return result["activation"].(*models.RunbookActivation), nil
}

// copied from routine_event_sources_test.go
func (f *eventRoutineFixture) setEndpointStatus(t *testing.T, id string, status models.ExternalConversationEndpointStatus) {
	t.Helper()
	endpoint, err := f.store.GetExternalConversationEndpoint(t.Context(), f.run.Scope, id)
	if err != nil {
		t.Fatal(err)
	}
	next := *endpoint
	next.Status, next.Revision = status, endpoint.Revision+1
	if err := f.store.UpdateExternalConversationEndpoint(t.Context(), &next, endpoint.Revision); err != nil {
		t.Fatal(err)
	}
}

// copied from routine_event_sources_test.go
var routineSlackAdapterForTest = models.ExternalConversationAdapterReference{SkillID: "slack", SkillVersion: "1.0.0", BindingID: "slack-binding", BindingRevision: 1, AdapterID: "events"}

// copied from routine_event_sources_test.go
// routineTestOfferer records test events instead of matching them.
type routineTestOfferer struct {
	tests []routineTestOffer
}

// copied from routine_event_sources_test.go
func (o *routineTestOfferer) Listening(models.Scope, models.RoutineEventKind, string) bool {
	return false
}

// copied from routine_event_sources_test.go
func (o *routineTestOfferer) OfferRoutineEvent(context.Context, *models.RoutineEvent) (externalconversation.RoutineEventOffer, error) {
	return externalconversation.RoutineEventOffer{}, nil
}

// copied from routine_event_sources_test.go
func (o *routineTestOfferer) OfferRoutineTestEvent(_ context.Context, scope models.Scope, activationID, testBy string, payload json.RawMessage) (string, externalconversation.RoutineEventOffer, error) {
	o.tests = append(o.tests, routineTestOffer{scope: scope, activationID: activationID, testBy: testBy, payload: payload})
	return "test:fixed", externalconversation.RoutineEventOffer{Matched: 1, Inserted: 1}, nil
}

// copied from kernel_store_test.go
// openTestSQLiteDB opens the test's own connection to the SQLite file at
// path, closed when the test ends.
func openTestSQLiteDB(t testing.TB, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", path+"?_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// copied from space_seed_test.go
// seedTestSpaceFor creates a space owned by ownerID. The owner's first space
// in the scope is their default space. An owner who already has a default
// space keeps it and gets no second one, so a fixture that is built more than
// once on a store can seed every time.
func seedTestSpaceFor(t testing.TB, store models.SpaceStore, scope models.Scope, ownerID string) *models.Space {
	t.Helper()
	if existing, err := store.DefaultSpace(context.Background(), scope, ownerID); err == nil {
		return existing
	} else if !errors.Is(err, models.ErrNoDefaultSpace) {
		t.Fatalf("seed space: %v", err)
	}
	space, err := portfolio.NewSpaceService(store).CreateSpace(context.Background(), portfolio.CreateSpaceRequest{
		Scope: scope, Name: "Home", Look: models.DefaultSpaceLook, OwnerID: ownerID,
	})
	if err != nil {
		t.Fatalf("seed space: %v", err)
	}
	return space
}

// copied from routine_event_sources_test.go
type routineTestOffer struct {
	scope        models.Scope
	activationID string
	testBy       string
	payload      json.RawMessage
}
