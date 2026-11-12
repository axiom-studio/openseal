package openseal

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/runtime/runtimetest"
)

// routineEventFixture is an Engine scope with a chat, one request message in
// it and an Agent, ready for event routines.
type routineEventFixture struct {
	t            *testing.T
	ctx          context.Context
	engine       *Engine
	scope        Scope
	owner        ObjectiveOwner
	conversation string
	message      string
}

func newRoutineEventFixture(t *testing.T, ctx context.Context, engine *Engine, scope Scope, agentID string) *routineEventFixture {
	t.Helper()
	registerLoopAgent(t, ctx, engine, scope, agentID)
	f := &routineEventFixture{t: t, ctx: ctx, engine: engine, scope: scope, owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: agentID}}
	conversation, _, err := engine.CreateConversation(ctx, CreateConversationRequest{Scope: scope, Owner: f.owner, Title: "Inbox", IdempotencyKey: "chat:inbox"})
	if err != nil {
		t.Fatal(err)
	}
	posted, err := engine.PostChannelMessage(ctx, PostChannelMessageRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: models.ConversationParticipant{Type: ConversationParticipantUser, ID: "user-1"}, Intent: MessageIntentQuestion,
		Content: "Tell me when an invoice email comes in", Audience: models.ConversationAudience{Kind: ConversationAudienceChannel},
		IdempotencyKey: "request:inbox",
	})
	if err != nil {
		t.Fatal(err)
	}
	f.conversation, f.message = conversation.ID, posted.Message.ID
	return f
}

// gmailRoutine creates an active Gmail routine through the Engine API.
func (f *routineEventFixture) gmailRoutine(name string) *RunbookActivation {
	f.t.Helper()
	objective, err := f.engine.CreateObjective(f.ctx, CreateObjectiveRequest{
		Scope: f.scope, Owner: f.owner, Title: name, Goal: "Summarize invoice email for " + name, Status: models.ObjectiveStatusActive,
		ExecutionPolicy: &ObjectiveExecutionPolicy{MaximumConcurrentRuns: 1},
	})
	if err != nil {
		f.t.Fatal(err)
	}
	activation, err := f.engine.CreateRunbookActivation(f.ctx, CreateRunbookActivationRequest{
		Scope: f.scope, Owner: f.owner, ObjectiveID: objective.ID, AssignedAgentID: f.owner.ID, TriggerID: "event", MaximumConcurrent: 1,
		Trigger: runbook.Trigger{Kind: runbook.TriggerEvent}, IdempotencyKey: "create-task:" + name,
		Task: &ScheduledAgentTask{
			Goal: "Summarize each new invoice email in one line.", ConversationID: f.conversation, SourceMessageID: f.message, CreatedBy: "user-1",
			Event: &RoutineEventTrigger{Kind: RoutineEventGmailMessage, SourceRef: RoutineGoogleSourceRef("user-1", "sub-"+name), SourceLabel: "your Gmail", MaxPerHour: 20},
		},
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return activation
}

func (f *routineEventFixture) listening(activation *RunbookActivation) bool {
	return f.engine.RoutineEventListening(f.scope, activation.Task.Event.Kind, activation.Task.Event.SourceRef)
}

func (f *routineEventFixture) setStatus(activation *RunbookActivation, status models.RunbookActivationStatus) *RunbookActivation {
	f.t.Helper()
	current, err := f.engine.GetRunbookActivation(f.ctx, f.scope, activation.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	updated, err := f.engine.UpdateRunbookActivation(f.ctx, f.scope, activation.ID, UpdateRunbookActivationRequest{ExpectedRevision: current.Revision, Status: status})
	if err != nil {
		f.t.Fatal(err)
	}
	return updated
}

func TestEngineRoutineEventEndToEnd(t *testing.T) {
	store := runtimetest.NewKernelStore(t)
	scope := Scope{Kind: "tenant", ID: "routines-e2e"}
	var engine *Engine
	host := &loopStepHost{decide: func(context.Context, ModelStepRequest) (loopReply, error) {
		return loopReply{Text: "Invoice #4411 from Ana Ruiz is due Friday."}, nil
	}}
	var err error
	engine, err = New(WithStore(store), WithAgentRunWorkers(AgentRunWorkerConfig{
		Scope: scope, AssignedAgentID: "inbox-agent", PollInterval: 5 * time.Millisecond,
		LeaseDuration: time.Second, TurnLeaseDuration: time.Second,
	}, loopResolver(&engine, host)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	f := newRoutineEventFixture(t, ctx, engine, scope, "inbox-agent")
	activation := f.gmailRoutine("invoices")
	if !f.listening(activation) {
		t.Fatal("new routine is not listening")
	}

	offer, err := engine.OfferRoutineEvent(ctx, &RoutineEvent{
		Scope: scope, ID: "gmail:msg-1", Kind: RoutineEventGmailMessage, SourceRef: activation.Task.Event.SourceRef,
		OccurredAt: time.Now().UTC(), Summary: "New email from Ana Ruiz: Invoice #4411",
		Fields:  map[string][]string{"from": {"ana@acme.com"}, "subject": {"Invoice #4411"}},
		Payload: json.RawMessage(`{"messageId":"msg-1","snippet":"Due Friday"}`),
	})
	if err != nil || offer != (RoutineEventOffer{Matched: 1, Inserted: 1}) {
		t.Fatalf("offer = %#v err=%v", offer, err)
	}
	result, err := engine.ReconcileAllRoutineEvents(ctx, 100)
	if err != nil || result.Started != 1 {
		t.Fatalf("dispatch = %#v err=%v", result, err)
	}
	runs, err := engine.ListAgentRuns(ctx, models.AgentRunFilter{Scope: scope, ObjectiveID: activation.ObjectiveID})
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs = %#v err=%v", runs, err)
	}
	run := runs[0]
	batchID, _ := run.Context[RoutineEventBatchContextKey].(string)
	if run.Source != RunSourceEvent || run.ActingUser != "user-1" || batchID == "" ||
		run.Context[RoutineEventHeaderContextKey] != "New email from Ana Ruiz: Invoice #4411" {
		t.Fatalf("occurrence run = %#v", run)
	}

	if err := engine.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer engine.Stop()
	done := waitForRunStatus(t, ctx, engine, scope, run.ID, AgentRunStatusCompleted)
	if done.SessionID == "" {
		t.Fatalf("completed run has no session: %#v", done)
	}
	requests := host.requestsFor(run.ID)
	if len(requests) == 0 {
		t.Fatal("the occurrence never reached the model")
	}
	text := loopRequestText(requests[0])
	for _, want := range []string{
		"Summarize each new invoice email in one line.",
		"A routine started this run because of 1 event.",
		`"gmail:msg-1"`, `"New email from Ana Ruiz: Invoice #4411"`, `"snippet":"Due Friday"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("first step does not contain %q:\n%s", want, text)
		}
	}

	view, err := engine.GetRoutineEventBatchView(ctx, scope, activation.ID, batchID)
	if err != nil || view.RunID != run.ID || len(view.Events) != 1 || view.Events[0].Summary != "New email from Ana Ruiz: Invoice #4411" {
		t.Fatalf("batch view = %#v err=%v", view, err)
	}
	if _, err := engine.GetRoutineEventBatchView(ctx, scope, "another-routine", batchID); err != ErrRoutineEventBatchNotFound {
		t.Fatalf("batch view of another routine err = %v", err)
	}
	stats := engine.RoutineEventStats()
	if stats.In[RoutineEventGmailMessage] != 1 || stats.Matched[RoutineEventGmailMessage] != 1 || stats.RunsStarted != 1 {
		t.Fatalf("stats = %#v", stats)
	}
	activity, err := engine.RoutineEventActivity(ctx, scope, []string{activation.ID})
	if err != nil || activity[activation.ID].FiredLast24h != 1 {
		t.Fatalf("activity = %#v err=%v", activity, err)
	}
}

// routineGoogleSignIns is a host Google catalog that lists these sources.
type routineGoogleSignIns []RoutineEventSourceOption

func (c routineGoogleSignIns) ListRoutineEventSources(context.Context, Scope, ObjectiveOwner, string) ([]RoutineEventSourceOption, error) {
	return append([]RoutineEventSourceOption(nil), c...), nil
}

func TestEngineRoutineCreateListensAtOnce(t *testing.T) {
	engine, err := New(WithStore(runtimetest.NewKernelStore(t)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	f := newRoutineEventFixture(t, t.Context(), engine, Scope{Kind: "tenant", ID: "routines-listen"}, "inbox-agent")
	activation := f.gmailRoutine("listen")
	if !f.listening(activation) {
		t.Fatal("created routine is not listening")
	}
	pausedRoutine := f.setStatus(activation, RunbookActivationPaused)
	if f.listening(activation) {
		t.Fatal("paused routine is still listening")
	}
	// Without the creator's Google sign-in the resume is refused.
	_, err = engine.UpdateRunbookActivation(f.ctx, f.scope, activation.ID, UpdateRunbookActivationRequest{ExpectedRevision: pausedRoutine.Revision, Status: RunbookActivationActive})
	var refused *RoutineSourceUnavailableError
	if !errors.As(err, &refused) || refused.Reason != RoutinePausedNeedsSignIn {
		t.Fatalf("resume without a sign-in: %v", err)
	}
	engine.SetRoutineEventSourceCatalog(routineGoogleSignIns{{Kind: RoutineEventGmailMessage, Source: activation.Task.Event.SourceRef, Available: true}})
	f.setStatus(activation, RunbookActivationActive)
	if !f.listening(activation) {
		t.Fatal("resumed routine is not listening")
	}
	f.setStatus(activation, RunbookActivationRetired)
	if f.listening(activation) {
		t.Fatal("retired routine is still listening")
	}

	// A system pause records why and drops nothing that is not waiting.
	again := f.gmailRoutine("signin")
	paused, err := engine.PauseRoutinesForSource(t.Context(), f.scope, RoutineEventGmailMessage, again.Task.Event.SourceRef, RoutinePausedNeedsSignIn)
	if err != nil || paused != 1 || f.listening(again) {
		t.Fatalf("pause for source = %d err=%v listening=%v", paused, err, f.listening(again))
	}
	current, err := engine.GetRunbookActivation(t.Context(), f.scope, again.ID)
	if err != nil || current.Status != RunbookActivationPaused || current.PausedReason != RoutinePausedNeedsSignIn {
		t.Fatalf("paused routine = %#v err=%v", current, err)
	}
	if err := engine.PauseRoutine(t.Context(), f.scope, again.ID, RoutinePausedNeedsSignIn); err != nil {
		t.Fatalf("pausing again for the same reason: %v", err)
	}
}

type recordingIndexPublisher struct{ scopes []Scope }

func (p *recordingIndexPublisher) PublishRoutineIndexChange(scope Scope) {
	p.scopes = append(p.scopes, scope)
}

func TestEngineRoutineIndexPublisher(t *testing.T) {
	store := runtimetest.NewKernelStore(t)
	engine, err := New(WithStore(store))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	publisher := &recordingIndexPublisher{}
	engine.SetRoutineIndexPublisher(publisher)
	ctx := t.Context()
	f := newRoutineEventFixture(t, ctx, engine, Scope{Kind: "tenant", ID: "routines-index"}, "inbox-agent")
	created := f.gmailRoutine("published")
	if len(publisher.scopes) != 1 || publisher.scopes[0] != f.scope || !f.listening(created) {
		t.Fatalf("publisher scopes = %#v listening=%v", publisher.scopes, f.listening(created))
	}

	// Another process writes a routine straight to the shared store: this
	// Engine hears it only when the peer's signal makes it refresh.
	peer := *created
	peer.ID, peer.Revision, peer.IdempotencyKeyHash, peer.CreationFingerprint = "peer-routine", 1, "", ""
	task := *created.Task
	trigger := *created.Task.Event
	trigger.SourceRef = RoutineGoogleSourceRef("user-1", "sub-peer")
	task.Event, peer.Task = &trigger, &task
	if err := store.CreateRunbookActivation(ctx, &peer); err != nil {
		t.Fatal(err)
	}
	if f.listening(&peer) {
		t.Fatal("a write this process did not see is already in the index")
	}
	if err := engine.RefreshRoutineIndex(ctx, f.scope); err != nil {
		t.Fatal(err)
	}
	if !f.listening(&peer) || len(publisher.scopes) != 1 {
		t.Fatalf("after refresh listening=%v publishes=%d", f.listening(&peer), len(publisher.scopes))
	}

	// A full rebuild (on signal reconnect) sees every scope.
	other := peer
	other.ID = "peer-routine-2"
	otherTrigger := trigger
	otherTrigger.SourceRef = RoutineGoogleSourceRef("user-1", "sub-peer-2")
	otherTask := task
	otherTask.Event, other.Task = &otherTrigger, &otherTask
	if err := store.CreateRunbookActivation(ctx, &other); err != nil {
		t.Fatal(err)
	}
	if err := engine.RebuildRoutineIndex(ctx); err != nil {
		t.Fatal(err)
	}
	if !f.listening(&other) || !f.listening(created) {
		t.Fatal("rebuild lost a routine")
	}
}
