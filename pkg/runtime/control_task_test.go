package runtime

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation/conversationtest"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun/conversationruntest"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
	"github.com/google/uuid"
)

type startTaskTestStore interface {
	models.ConversationTaskKernelStore
	models.AgentSessionStore
	models.RunCommandStore
	models.AgentRunScheduleStore
}

// forStartTaskStores runs test on SQLite.
func forStartTaskStores(t *testing.T, test func(*testing.T, startTaskTestStore)) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) {
		store, err := sqlite.NewSQLiteStore(filepath.Join(t.TempDir(), "start-task.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		test(t, store)
	})
}

const (
	startTaskTestWorker = "start-task-worker"
	startTaskTestGoal   = "Review the release notes and list every breaking change."
	startTaskTestAck    = "On it, I will go through the release notes in the background."
)

// startTaskFixture is a claimed foreground conversation Run whose finished
// and applied step 1 holds [user message, assistant, tool(pending)×N] in the
// Run's session, as the loop leaves it before the settle phase.
type startTaskFixture struct {
	t            *testing.T
	store        startTaskTestStore
	run          *models.AgentRun
	step         *models.AgentTurn
	session      *models.AgentSession
	user         *models.SessionEntry
	assistant    *models.SessionEntry
	entries      map[string]*models.SessionEntry // by call id
	conversation *models.Conversation
	message      *models.ChannelMessage
	exec         *ControlExecutor
	authority    agentrun.ControlAuthority
}

func startTaskCall(id, goal, ack string) models.ModelToolCall {
	raw, _ := json.Marshal(map[string]string{"goal": goal, "acknowledgment": ack})
	return models.ModelToolCall{ID: id, Name: string(models.ControlToolStartTask), Arguments: raw}
}

func newStartTaskFixture(t *testing.T, store startTaskTestStore, calls ...models.ModelToolCall) *startTaskFixture {
	t.Helper()
	if len(calls) == 0 {
		calls = []models.ModelToolCall{startTaskCall("call-task", startTaskTestGoal, startTaskTestAck)}
	}
	ctx := t.Context()
	scope := models.Scope{Kind: "tenant", ID: "start-task-" + uuid.NewString()[:8]}
	conversations := conversation.NewConversationService(store)
	conversationtest.SeedSpace(t, store.(models.SpaceStore), scope)
	conversation, _, err := conversations.CreateConversation(ctx, conversation.CreateConversationRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, Title: "Assistant", IdempotencyKey: "start-task-chat",
	})
	if err != nil {
		t.Fatal(err)
	}
	message := conversationruntest.PostMessage(t, conversations, conversation, models.ConversationParticipantUser, models.MessageIntentQuestion,
		"Review the release in the background", "start-task-message")
	scheduler, err := conversationrun.NewConversationRunScheduler(store, store, conversationrun.ConversationRunSchedulerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	scheduled, _, err := scheduler.ScheduleMessage(ctx, scope, conversation.ID, message.ID)
	if err != nil || scheduled == nil || scheduled.Run == nil {
		t.Fatalf("schedule foreground: %#v %v", scheduled, err)
	}
	now := time.Now().UTC()
	run, err := store.ClaimNextAgentRun(ctx, models.AgentRunClaim{
		Scope: scope, Kind: models.RunKindConversation, WorkerID: startTaskTestWorker, Now: now, LeaseDuration: time.Hour, AgingInterval: time.Minute,
	})
	if err != nil || run == nil || run.ID != scheduled.Run.ID {
		t.Fatalf("claim foreground: %#v %v", run, err)
	}
	lease := models.AgentRunLeaseGuard{WorkerID: startTaskTestWorker, Now: now}
	thread, _ := run.Context["threadRootMessageId"].(string)
	if _, err := store.OpenConversationAgentSession(ctx, models.OpenConversationAgentSessionRequest{Scope: scope, ConversationID: conversation.ID,
		ThreadRootID: thread, AgentID: run.AssignedAgentID, RunID: run.ID, Lease: lease, Now: now}); err != nil {
		t.Fatalf("open lane session: %v", err)
	}
	turns := activity.NewAgentTurnService(store, store)
	step, err := turns.BeginTurn(ctx, activity.BeginAgentTurnRequest{Scope: scope, RunID: run.ID, SessionID: run.SessionID,
		Producer: models.StepProducerModel, WorkerID: startTaskTestWorker, LeaseDuration: time.Hour})
	if err != nil {
		t.Fatalf("begin step: %v", err)
	}
	sessionID := run.SessionID
	user := &models.SessionEntry{ID: models.SessionEntryID(sessionID, "message:"+message.ID), RunID: run.ID, Type: models.SessionEntryUser,
		SourceKey: "message:" + message.ID, User: &models.UserEntry{Origin: models.UserEntryChannelMessage, Author: models.SessionAuthor{Type: "user", ID: message.Sender.ID},
			Content: []models.ContentPart{storetest.TextPart(message.Content)}, MessageID: message.ID}}
	assistant := &models.SessionEntry{ID: models.SessionEntryID(sessionID, models.AssistantEntryKey(step.ID)), RunID: run.ID, StepID: step.ID, Step: step.Sequence,
		Type: models.SessionEntryAssistant, Assistant: &models.AssistantEntry{Provider: models.ModelStepProviderAnthropic, Model: "claude-x",
			StopReason: models.ModelStopToolCalls, ToolCalls: calls}}
	batch := []*models.SessionEntry{assistant}
	for _, call := range calls {
		batch = append(batch, &models.SessionEntry{ID: models.SessionEntryID(sessionID, models.ToolEntryKey(step.ID, call.ID)), RunID: run.ID, StepID: step.ID,
			Step: step.Sequence, Type: models.SessionEntryTool, State: models.ToolEntryPending, ToolCallID: call.ID,
			Tool: &models.ToolEntry{Name: call.Name, Arguments: call.Arguments, SideEffect: models.ToolSideEffectWrite}})
	}
	for _, entries := range [][]*models.SessionEntry{{user}, batch} {
		if _, err := store.AppendAgentSessionEntries(ctx, models.AppendAgentSessionEntriesRequest{Scope: scope, SessionID: sessionID,
			RunID: run.ID, Lease: &lease, Entries: entries, Now: now}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	step, err = turns.FinishTurn(ctx, scope, step.ID, activity.FinishAgentTurnRequest{ExpectedRevision: step.Revision,
		Status: models.AgentTurnStatusCompleted, WorkerID: startTaskTestWorker})
	if err != nil {
		t.Fatalf("finish step: %v", err)
	}
	current, err := store.GetAgentRun(ctx, scope, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	current.LastAppliedTurn = step.Sequence
	current.UpdatedAt = time.Now().UTC()
	current.Revision++
	storetest.UpdateRun(t, store, current, current.Revision-1)
	if run, err = store.GetAgentRun(ctx, scope, run.ID); err != nil {
		t.Fatal(err)
	}
	f := &startTaskFixture{t: t, store: store, run: run, step: step, conversation: conversation, message: message,
		entries: map[string]*models.SessionEntry{}}
	if f.session, err = store.GetAgentSession(ctx, scope, sessionID); err != nil {
		t.Fatal(err)
	}
	f.user, f.assistant = f.storedEntry(user.ID), f.storedEntry(assistant.ID)
	for _, entry := range batch[1:] {
		f.entries[entry.ToolCallID] = f.storedEntry(entry.ID)
	}
	f.authority = agentrun.ControlAuthority{AgentID: run.AssignedAgentID,
		TaskStart: &models.HostedConversationTaskContext{ConversationID: conversation.ID, ThreadRootID: thread, CanStart: true}}
	f.exec = &ControlExecutor{Portfolio: store, Sessions: store, Tasks: conversationrun.NewConversationTaskService(store)}
	return f
}

func (f *startTaskFixture) storedEntry(id string) *models.SessionEntry {
	f.t.Helper()
	entry, err := f.store.GetSessionEntry(f.t.Context(), f.run.Scope, id)
	if err != nil {
		f.t.Fatalf("get entry %s: %v", id, err)
	}
	return entry
}

func (f *startTaskFixture) context(callID string) agentrun.ControlContext {
	return NewControlContext(agentrun.StepToolsInput{
		Run: f.run, Step: f.step, Session: f.session, WorkerID: startTaskTestWorker, Assistant: f.assistant,
		Binding: &agentrun.TurnRunnerBinding{Authority: f.authority},
	}, f.entries[callID])
}

func (f *startTaskFixture) settle(callID string) ControlResult {
	f.t.Helper()
	result, err := f.exec.Settle(f.t.Context(), f.context(callID))
	if err != nil {
		f.t.Fatalf("settle: %v", err)
	}
	return result
}

// request is the admission the settle of callID makes.
func (f *startTaskFixture) request(callID string) conversationrun.StartConversationTaskRequest {
	entry := f.entries[callID]
	args := models.StartTaskEntryArgs(entry)
	if args == nil {
		f.t.Fatalf("entry %s has no start_task arguments", entry.ID)
	}
	return conversationrun.StartConversationTaskRequest{Scope: f.run.Scope, SourceRunID: f.run.ID, ExpectedSourceRevision: f.run.Revision,
		WorkerID: startTaskTestWorker, TurnID: f.step.ID, ToolEntryID: entry.ID, AssignedAgentID: f.run.AssignedAgentID,
		TaskKey: models.StartTaskKey(entry.ID), Goal: args.Goal, Acknowledgment: args.Acknowledgment, Budget: &models.BudgetPolicy{}}
}

func (f *startTaskFixture) tasks() []*models.ConversationTaskResult {
	f.t.Helper()
	tasks, err := f.store.ListConversationTasks(f.t.Context(), models.ConversationTaskFilter{Scope: f.run.Scope, Owner: f.run.Owner,
		ConversationID: f.conversation.ID, Limit: 10})
	if err != nil {
		f.t.Fatal(err)
	}
	return tasks
}

func (f *startTaskFixture) assertNotStarted(callID string) {
	f.t.Helper()
	if tasks := f.tasks(); len(tasks) != 0 {
		f.t.Fatalf("tasks = %#v", tasks)
	}
	source, err := f.store.GetAgentRun(f.t.Context(), f.run.Scope, f.run.ID)
	if err != nil || source.Status != models.AgentRunStatusRunning || len(source.Output) != 0 {
		f.t.Fatalf("source = %#v, %v", source, err)
	}
}

func TestStartTaskCompletesForegroundWithAcknowledgmentAtomically(t *testing.T) {
	forStartTaskStores(t, func(t *testing.T, store startTaskTestStore) {
		f := newStartTaskFixture(t, store)
		result := f.settle("call-task")
		if result.Kind != ControlEnded || result.Run == nil || result.Run.Status != models.AgentRunStatusCompleted {
			t.Fatalf("result = %#v", result)
		}
		source, err := store.GetAgentRun(t.Context(), f.run.Scope, f.run.ID)
		if err != nil {
			t.Fatal(err)
		}
		tasks := f.tasks()
		if len(tasks) != 1 {
			t.Fatalf("tasks = %#v", tasks)
		}
		task, work := tasks[0].Task, tasks[0].WorkRun
		if source.Status != models.AgentRunStatusCompleted || source.Output["summary"] != startTaskTestAck || source.LeaseOwner != "" ||
			source.Output["conversationTaskWorkRunId"] != work.ID || source.Revision != f.run.Revision+1 {
			t.Fatalf("source = %#v", source)
		}
		if work.Kind != models.RunKindAgentWork || work.Status != models.AgentRunStatusQueued || work.Goal != startTaskTestGoal ||
			work.ParentRunID != "" || work.RootRunID != work.ID || task.Acknowledgment != startTaskTestAck {
			t.Fatalf("work = %#v task = %#v", work, task)
		}
		entry := f.storedEntry(f.entries["call-task"].ID)
		value := controlResultValue(t, entry)
		if entry.State != models.ToolEntryCompleted || entry.Tool.Result.IsError || value["status"] != "started" ||
			value["taskId"] != task.ID || value["workRunId"] != work.ID || value["acknowledgment"] != startTaskTestAck {
			t.Fatalf("entry = %#v value = %v", entry, value)
		}
		if result.Entry == nil || result.Entry.State != models.ToolEntryCompleted {
			t.Fatalf("result entry = %#v", result.Entry)
		}
	})
}

func TestStartTaskProvenanceSourceTurnIsStepAndLastApplied(t *testing.T) {
	f := newStartTaskFixture(t, conversationtest.NewStore(t))
	f.settle("call-task")
	tasks := f.tasks()
	if len(tasks) != 1 {
		t.Fatalf("tasks = %#v", tasks)
	}
	task := tasks[0].Task
	entry := f.entries["call-task"]
	source, _ := f.store.GetAgentRun(t.Context(), f.run.Scope, f.run.ID)
	if task.SourceTurnID != f.step.ID || task.SourceTurnNumber != f.step.Sequence || source.LastAppliedTurn != task.SourceTurnNumber ||
		task.SourceToolEntryID != entry.ID || task.TaskKey != "task-"+entry.ID || task.SourceRunID != f.run.ID ||
		task.SourceMessageID != f.message.ID || task.AuthenticatedActor != f.message.Sender || task.Validate() != nil {
		t.Fatalf("task provenance = %#v", task)
	}
	events, err := f.store.ListActivity(t.Context(), models.ActivityFilter{Scope: f.run.Scope, RunID: f.run.ID, EventTypes: []string{"conversation.task_started"}})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.EventType == "conversation.task_started" {
			found = event.Payload["sourceToolEntryId"] == entry.ID && event.Payload["sourceTurnId"] == f.step.ID
		}
	}
	if !found {
		t.Fatalf("task_started event lacks the tool entry: %#v", events)
	}

	// A request that does not match the step's entry is refused.
	g := newStartTaskFixture(t, conversationtest.NewStore(t))
	for name, mutate := range map[string]func(*conversationrun.StartConversationTaskRequest){
		"tampered goal":     func(r *conversationrun.StartConversationTaskRequest) { r.Goal = "Something else entirely." },
		"tampered ack":      func(r *conversationrun.StartConversationTaskRequest) { r.Acknowledgment = "Done already." },
		"other task key":    func(r *conversationrun.StartConversationTaskRequest) { r.TaskKey = "task-other" },
		"unknown entry":     func(r *conversationrun.StartConversationTaskRequest) { r.ToolEntryID = "missing-entry" },
		"assistant entry":   func(r *conversationrun.StartConversationTaskRequest) { r.ToolEntryID = g.assistant.ID },
		"no tool entry":     func(r *conversationrun.StartConversationTaskRequest) { r.ToolEntryID = "" },
		"other step row id": func(r *conversationrun.StartConversationTaskRequest) { r.TurnID = "turn-other" },
	} {
		req := g.request("call-task")
		mutate(&req)
		if _, err := g.exec.Tasks.Start(t.Context(), req); err == nil {
			t.Errorf("%s: admitted", name)
		}
	}
	g.assertNotStarted("call-task")

	// The step must be the Run's last applied one.
	current, _ := g.store.GetAgentRun(t.Context(), g.run.Scope, g.run.ID)
	current.LastAppliedTurn++
	current.UpdatedAt = time.Now().UTC()
	current.Revision++
	storetest.UpdateRun(t, g.store, current, current.Revision-1)
	g.run = current
	if _, err := g.exec.Tasks.Start(t.Context(), g.request("call-task")); !errors.Is(err, models.ErrInvalidConversationTask) {
		t.Fatalf("start from a step that is not last applied = %v", err)
	}
}

func TestStartTaskRefusedWhenSteeringUnconsumed(t *testing.T) {
	steer := func(t *testing.T, f *startTaskFixture) *models.AgentRun {
		current, err := f.store.GetAgentRun(t.Context(), f.run.Scope, f.run.ID)
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		current.PendingInterventions = append(current.PendingInterventions, models.AgentRunIntervention{ID: "steer-1",
			Actor: models.ActivityActor{Type: "user", ID: "sender-user"}, Instruction: "Actually, only the security fixes.", CreatedAt: now})
		current.UpdatedAt = now
		current.Revision++
		storetest.UpdateRun(t, f.store, current, current.Revision-1)
		reloaded, _ := f.store.GetAgentRun(t.Context(), f.run.Scope, f.run.ID)
		return reloaded
	}
	check := func(t *testing.T, f *startTaskFixture, result ControlResult) {
		t.Helper()
		if result.Kind != ControlContinue {
			t.Fatalf("result = %#v", result)
		}
		entry := f.storedEntry(f.entries["call-task"].ID)
		value := controlResultValue(t, entry)
		if entry.State != models.ToolEntryError || value["error"] != "new_message" {
			t.Fatalf("entry = %#v value = %v", entry, value)
		}
		f.assertNotStarted("call-task")
	}
	t.Run("seen by the settle", func(t *testing.T) {
		f := newStartTaskFixture(t, conversationtest.NewStore(t))
		f.run = steer(t, f)
		check(t, f, f.settle("call-task"))
	})
	t.Run("arrived under the settle", func(t *testing.T) {
		// The settle's Run copy predates the message; the admission reads
		// the stored Run and refuses.
		f := newStartTaskFixture(t, conversationtest.NewStore(t))
		stale := f.run
		steered := steer(t, f)
		cc := f.context("call-task")
		cc.Run = models.CloneAgentRun(stale)
		cc.Run.Revision = steered.Revision // the settle saw the current revision, not the message
		result, err := f.exec.Settle(t.Context(), cc)
		if err != nil {
			t.Fatal(err)
		}
		check(t, f, result)
	})
}

func TestStartTaskReplayAfterCrashReturnsSameTask(t *testing.T) {
	forStartTaskStores(t, func(t *testing.T, store startTaskTestStore) {
		f := newStartTaskFixture(t, store)
		req := f.request("call-task")
		req.Budget = modelstep.ControlTaskBudget(nil)
		var wg sync.WaitGroup
		results := make(chan *models.ConversationTaskResult, 6)
		errs := make(chan error, 6)
		for range 6 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				result, err := f.exec.Tasks.Start(t.Context(), req)
				results <- result
				errs <- err
			}()
		}
		wg.Wait()
		close(results)
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("concurrent start: %v", err)
			}
		}
		var first *models.ConversationTaskResult
		fresh := 0
		for result := range results {
			if first == nil {
				first = result
			}
			if result.Task.ID != first.Task.ID || result.WorkRun.ID != first.WorkRun.ID {
				t.Fatal("replay changed the task identity")
			}
			if !result.Replayed {
				fresh++
			}
		}
		if fresh != 1 || len(f.tasks()) != 1 {
			t.Fatalf("fresh admissions = %d, tasks = %d", fresh, len(f.tasks()))
		}
		entry := f.storedEntry(f.entries["call-task"].ID)

		// The settle's response was lost: the same admission again returns
		// the committed task and writes nothing.
		again, err := f.exec.Tasks.Start(t.Context(), req)
		if err != nil || !again.Replayed || again.Task.ID != first.Task.ID || again.SourceRun.Status != models.AgentRunStatusCompleted {
			t.Fatalf("replay = %#v, %v", again, err)
		}
		if after := f.storedEntry(entry.ID); after.Revision != entry.Revision || after.State != models.ToolEntryCompleted {
			t.Fatalf("entry after replay = %#v", after)
		}
		// A changed request under the same key is a conflict.
		req.Goal = "A different request with the same key"
		if _, err := f.exec.Tasks.Start(t.Context(), req); !errors.Is(err, models.ErrConversationTaskConflict) {
			t.Fatalf("mutated replay = %v", err)
		}
	})
}

func TestStartTaskNotAloneIsRefused(t *testing.T) {
	calls := []models.ModelToolCall{startTaskCall("call-task", startTaskTestGoal, startTaskTestAck),
		{ID: "call-react", Name: string(models.ControlToolReact), Arguments: json.RawMessage(`{"messageId":"m","emoji":"eyes"}`)}}
	refusals := CallRefusals(calls, agentrun.ControlAuthority{CanReact: true,
		TaskStart: &models.HostedConversationTaskContext{ConversationID: "c", CanStart: true}})
	if refusals["call-task"] == nil || refusals["call-react"] != nil {
		t.Fatalf("refusals = %#v", refusals)
	}
	// Settled anyway (a resumed entry), the handler applies the same rule.
	f := newStartTaskFixture(t, conversationtest.NewStore(t), calls...)
	result := f.settle("call-task")
	entry := f.storedEntry(f.entries["call-task"].ID)
	value := controlResultValue(t, entry)
	if result.Kind != ControlContinue || entry.State != models.ToolEntryError || value["error"] != controlTaskNotAloneCode ||
		value["message"] != "Call conversation_start_task alone; other work belongs inside the task." {
		t.Fatalf("result = %#v value = %v", result, value)
	}
	f.assertNotStarted("call-task")
}

func TestTaskWorkRunSessionForksBeforeStartDecision(t *testing.T) {
	forStartTaskStores(t, func(t *testing.T, store startTaskTestStore) {
		f := newStartTaskFixture(t, store)
		f.settle("call-task")
		tasks := f.tasks()
		if len(tasks) != 1 {
			t.Fatalf("tasks = %#v", tasks)
		}
		work := tasks[0].WorkRun
		if work.SessionID != models.RunAgentSessionID(work.Scope, work.ID) {
			t.Fatalf("work session id = %q", work.SessionID)
		}
		session, err := store.GetAgentSession(t.Context(), work.Scope, work.SessionID)
		if err != nil || session.OwnerRunID != work.ID || session.AgentID != f.run.AssignedAgentID ||
			session.ParentSessionID != f.session.ID || session.ForkSequence != f.assistant.Sequence-1 || session.ForkEntryID != f.user.ID ||
			session.ConversationID != f.conversation.ID {
			t.Fatalf("work session = %#v, %v", session, err)
		}
		path, err := store.LoadAgentSessionPath(t.Context(), work.Scope, session.ID)
		if err != nil || len(path.Entries) != 2 || path.Entries[0].ID != f.user.ID {
			t.Fatalf("work path = %#v, %v", path, err)
		}
		seed := path.Entries[1]
		if seed.Type != models.SessionEntryUser || seed.User == nil || seed.User.Origin != models.UserEntryGoal || seed.User.GoalKind != "task" ||
			!strings.Contains(seed.User.Content[0].Text, startTaskTestGoal) {
			t.Fatalf("seed = %#v", seed)
		}
		for _, entry := range path.Entries {
			if entry.Type == models.SessionEntryAssistant || entry.Type == models.SessionEntryTool {
				t.Fatalf("work path shows the start decision: %#v", entry)
			}
		}
	})
}

func TestOldTaskWithoutToolEntryStillValidates(t *testing.T) {
	store := conversationtest.NewStore(t)
	record := storetest.ConversationTaskRecord(t, store, "old")
	task := record.Task
	if task.SourceToolEntryID != "" || task.Validate() != nil {
		t.Fatalf("old task = %#v", task)
	}
	raw, err := json.Marshal(task)
	if err != nil || strings.Contains(string(raw), "sourceToolEntryId") {
		t.Fatalf("old task payload = %s, %v", raw, err)
	}
	var decoded models.ConversationTask
	if err := json.Unmarshal(raw, &decoded); err != nil || decoded.Validate() != nil || decoded.RequestDigest != task.RequestDigest {
		t.Fatalf("decoded old task = %#v, %v", decoded, err)
	}
	if err := models.ValidateConversationTaskCreateRecord(record); err != nil {
		t.Fatalf("old record = %v", err)
	}
	if !models.ConversationTaskMatchesWorkRun(task, record.WorkRun) {
		t.Fatal("old task no longer matches its work Run")
	}
	// Admission itself only starts tasks from a tool entry.
	if _, err := conversationrun.NewConversationTaskService(store).Start(t.Context(), conversationrun.StartConversationTaskRequest{Scope: task.Scope, SourceRunID: task.SourceRunID,
		ExpectedSourceRevision: 1, WorkerID: record.WorkerID, TurnID: task.SourceTurnID, AssignedAgentID: task.TargetAgentID,
		TaskKey: task.TaskKey, Goal: task.Goal, Acknowledgment: task.Acknowledgment}); !errors.Is(err, models.ErrInvalidConversationTask) {
		t.Fatalf("start without a tool entry = %v", err)
	}
}

func TestStartTaskSpecAndArguments(t *testing.T) {
	authority := agentrun.ControlAuthority{TaskStart: &models.HostedConversationTaskContext{ConversationID: "c", CanStart: true}}
	var spec *ControlToolSpec
	for _, s := range builtinControlSpecs(authority) {
		if s.Name == models.ControlToolStartTask {
			s := s
			spec = &s
		}
	}
	if spec == nil || spec.Phase != ControlPhaseSettle {
		t.Fatalf("spec = %#v", spec)
	}
	properties := spec.InputSchema["properties"].(map[string]interface{})
	if spec.InputSchema["additionalProperties"] != false || properties["goal"] == nil || properties["acknowledgment"] == nil || len(properties) != 2 {
		t.Fatalf("schema = %#v", spec.InputSchema)
	}
	voice := authority
	voice.VoiceCallStarted = true
	for _, s := range builtinControlSpecs(voice) {
		if s.Name == models.ControlToolStartTask {
			t.Fatal("start_task offered in a voice call")
		}
	}
	for name, raw := range map[string]string{
		"unknown field": `{"goal":"g","acknowledgment":"a","budget":{}}`,
		"empty goal":    `{"goal":"  ","acknowledgment":"a"}`,
		"long ack":      `{"goal":"g","acknowledgment":"` + strings.Repeat("a", 601) + `"}`,
		"two values":    `{"goal":"g","acknowledgment":"a"}{}`,
	} {
		if _, err := (startTaskHandler{}).Decode(json.RawMessage(raw)); err == nil {
			t.Errorf("%s: decoded", name)
		}
	}
	value, err := (startTaskHandler{}).Decode(json.RawMessage(`{"goal":" g ","acknowledgment":" a "}`))
	if err != nil || value.(*models.StartTaskArgs).Goal != "g" || value.(*models.StartTaskArgs).Acknowledgment != "a" {
		t.Fatalf("decode = %#v, %v", value, err)
	}
}
