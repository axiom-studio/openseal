package runtime_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation/conversationtest"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

// The task source fixture of runtime's conversation task tests, copied for
// the external active-runs test. It moves to a conversation run
// test-support package once the conversation run code leaves runtime.

// Copied from conversation_task_test.go
// settledTaskSourceFixture is settledWorkerTaskFixture's chat and source Run
// with the start_task step settled by hand (settleTaskSource) instead of by
// the worker: the Run is running, leased by "worker", its step completed and
// applied, and the tool entry pending.
func settledTaskSourceFixture(t *testing.T, store *sqlite.Store) (*models.AgentRun, *models.AgentTurn, *models.Conversation, *models.ChannelMessage) {
	t.Helper()
	scope := models.Scope{Kind: "tenant", ID: "worker-task"}
	conversations := runtimeconversation.NewConversationService(store)
	conversationtest.SeedSpace(t, store, scope)
	conversation, _, err := conversations.CreateConversation(t.Context(), runtimeconversation.CreateConversationRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, Title: "Assistant", IdempotencyKey: "worker-task-chat",
	})
	if err != nil {
		t.Fatal(err)
	}
	message := postConversationRunTestMessage(t, conversations, conversation, models.ConversationParticipantUser, models.MessageIntentQuestion,
		"Review the release in the background", "task-user-message")
	scheduled, _, err := mustConversationRunScheduler(t, store).ScheduleMessage(t.Context(), scope, conversation.ID, message.ID)
	if err != nil || scheduled == nil || scheduled.Run == nil {
		t.Fatalf("schedule foreground: %#v %v", scheduled, err)
	}
	claimed, err := store.ClaimNextAgentRun(t.Context(), models.AgentRunClaim{
		Scope: scope, Kind: models.RunKindConversation, WorkerID: "worker", Now: time.Now(), LeaseDuration: time.Minute, AgingInterval: time.Minute,
	})
	if err != nil || claimed == nil || claimed.ID != scheduled.Run.ID {
		t.Fatalf("claim foreground: %#v %v", claimed, err)
	}
	settled := settleTaskSource(t, store, scope, claimed.ID, "worker", taskProposalFixture())
	return settled.Run, settled.Turn, conversation, message
}

// Copied from conversation_task_test.go
// taskStartRequest is the admission of the task a loop-built step
// (settledWorkerTaskFixture) called conversation_start_task for: the request
// names that step's pending tool entry.
func taskStartRequest(t *testing.T, store interface{}, source *models.AgentRun, turn *models.AgentTurn) runtime.StartConversationTaskRequest {
	t.Helper()
	sessions, ok := store.(taskStartFixtureStore)
	if !ok {
		t.Fatalf("task start fixture needs a session store")
	}
	entryID := models.SessionEntryID(source.SessionID, models.ToolEntryKey(turn.ID, taskStartFixtureCallID))
	entry, err := models.OptionalSessionEntry(sessions.GetSessionEntry(t.Context(), source.Scope, entryID))
	if err != nil {
		t.Fatal(err)
	}
	args := models.StartTaskEntryArgs(entry)
	if source.SessionID == "" || args == nil {
		t.Fatalf("task start fixture needs a conversation_start_task tool entry")
	}
	current, err := sessions.GetAgentRun(t.Context(), source.Scope, source.ID)
	if err != nil || current == nil {
		t.Fatalf("get source: %#v %v", current, err)
	}
	return runtime.StartConversationTaskRequest{Scope: source.Scope, SourceRunID: source.ID, ExpectedSourceRevision: current.Revision, WorkerID: "worker",
		TurnID: turn.ID, ToolEntryID: entryID, AssignedAgentID: source.AssignedAgentID, TaskKey: models.StartTaskKey(entryID),
		Goal: args.Goal, Acknowledgment: args.Acknowledgment}
}

// Copied from conversation_task_test.go
// settleTaskSource settles one step on the claimed source Run runID (held by
// workerID) the way the loop leaves a conversation_start_task step before its
// settle: the step row is completed and applied, and the step's [assistant,
// conversation_start_task] entries are in the Run's lane session after the
// triggering message.
func settleTaskSource(t *testing.T, store interface{}, scope models.Scope, runID, workerID string, proposal taskSourceProposal) settledTaskSource {
	t.Helper()
	ctx := t.Context()
	sessions, ok := store.(taskStartFixtureStore)
	if !ok {
		t.Fatalf("task start fixture needs a session store")
	}
	run, step := settleFixtureStep(t, store, scope, runID, workerID, activity.RunTransitionRequest{Status: models.AgentRunStatusRunning, Summary: "Step applied"})
	var err error
	entryID, revision := ensureStartTaskToolEntry(t, sessions, run, step, proposal.Goal, proposal.Acknowledgment)
	if run, err = sessions.GetAgentRun(ctx, scope, runID); err != nil || run == nil {
		t.Fatalf("reload source: %#v %v", run, err)
	}
	return settledTaskSource{Run: run, Turn: step, Request: runtime.StartConversationTaskRequest{Scope: scope, SourceRunID: runID, ExpectedSourceRevision: revision,
		WorkerID: workerID, TurnID: step.ID, ToolEntryID: entryID, AssignedAgentID: run.AssignedAgentID, TaskKey: models.StartTaskKey(entryID),
		Goal: proposal.Goal, Acknowledgment: proposal.Acknowledgment, Budget: models.CloneBudgetPolicy(proposal.Budget)}}
}

// Copied from conversation_task_test.go
func taskProposalFixture() taskSourceProposal {
	return taskSourceProposal{Goal: "Review the release and report the outcome", Acknowledgment: "I’ve started the release review."}
}

// Copied from conversation_task_test.go
type taskStartFixtureStore interface {
	models.AgentSessionStore
	conversationTaskSQLTestStore
}

// Copied from conversation_task_test.go
const taskStartFixtureCallID = "call-start-task"

// Copied from conversation_task_test.go
// taskSourceProposal is the conversation_start_task call a settled fixture
// step makes.
type taskSourceProposal struct {
	Goal, Acknowledgment string
	Budget               *models.BudgetPolicy
}

// Copied from conversation_task_test.go
// settledTaskSource is a claimed conversation Run with one completed,
// applied step whose conversation_start_task call Request admits.
type settledTaskSource struct {
	Run     *models.AgentRun
	Turn    *models.AgentTurn
	Request runtime.StartConversationTaskRequest
}

// Copied from conversation_task_test.go
// settleFixtureStep runs one completed step on the claimed Run runID (held
// by workerID) and applies it with transition (its revision, step, lease
// and applied-turn fields are filled in).
func settleFixtureStep(t *testing.T, store interface{}, scope models.Scope, runID, workerID string, transition activity.RunTransitionRequest) (*models.AgentRun, *models.AgentTurn) {
	t.Helper()
	ctx := t.Context()
	type fixtureStepStore interface {
		models.PortfolioStore
		models.RunActivityStore
		models.AgentTurnStore
	}
	steps, ok := store.(fixtureStepStore)
	if !ok {
		t.Fatalf("step fixture needs portfolio, activity and turn stores")
	}
	turns := activity.NewAgentTurnService(steps, steps)
	activities := activity.NewRunActivityService(steps, steps)
	run, err := steps.GetAgentRun(ctx, scope, runID)
	if err != nil || run == nil {
		t.Fatalf("get run: %#v %v", run, err)
	}
	if run.Status == models.AgentRunStatusPlanning {
		if run, _, err = activities.TransitionRun(ctx, scope, runID, activity.RunTransitionRequest{ExpectedRevision: run.Revision, Status: models.AgentRunStatusRunning,
			Summary: "Run started", LeaseOwner: workerID}); err != nil {
			t.Fatalf("start run: %v", err)
		}
	}
	step, err := turns.BeginTurn(ctx, activity.BeginAgentTurnRequest{Scope: scope, RunID: runID, SessionID: run.SessionID, Producer: models.StepProducerModel,
		WorkerID: workerID, LeaseDuration: time.Minute})
	if err != nil {
		t.Fatalf("begin step: %v", err)
	}
	if step, err = turns.FinishTurn(ctx, scope, step.ID, activity.FinishAgentTurnRequest{ExpectedRevision: step.Revision, Status: models.AgentTurnStatusCompleted, WorkerID: workerID}); err != nil {
		t.Fatalf("finish step: %v", err)
	}
	transition.ExpectedRevision, transition.TurnID, transition.AppliedTurn, transition.LeaseOwner = run.Revision, step.ID, step.Sequence, workerID
	if run, _, err = activities.TransitionRun(ctx, scope, runID, transition); err != nil {
		t.Fatalf("apply step: %v", err)
	}
	return run, step
}

// Copied from conversation_task_test.go
func ensureStartTaskToolEntry(t *testing.T, store taskStartFixtureStore, source *models.AgentRun, turn *models.AgentTurn, goal, ack string) (string, int64) {
	t.Helper()
	ctx := t.Context()
	entryID := models.SessionEntryID(source.SessionID, models.ToolEntryKey(turn.ID, taskStartFixtureCallID))
	current, err := store.GetAgentRun(ctx, source.Scope, source.ID)
	if err != nil || current == nil {
		t.Fatalf("get source: %#v %v", current, err)
	}
	now := time.Now().UTC()
	if current.Status != models.AgentRunStatusRunning || current.LeaseExpiresAt == nil || !current.LeaseExpiresAt.After(now) {
		return entryID, source.Revision
	}
	lease := models.AgentRunLeaseGuard{WorkerID: current.LeaseOwner, Now: now}
	conversationID, _ := current.Context[models.ConversationRunConversationIDContextKey].(string)
	triggerID, _ := current.Context[models.ConversationRunTriggerIDContextKey].(string)
	thread, _ := current.Context["threadRootMessageId"].(string)
	if current.SessionID == "" {
		// A hand-built source Run: bind it to its lane session first.
		current.SessionID = models.ConversationSessionID(current.Scope, models.ConversationRunLane(conversationID, thread, current.AssignedAgentID))
		current.UpdatedAt = now
		current.Revision++
		updateConversationTaskSQLRun(t, store, current, current.Revision-1)
		entryID = models.SessionEntryID(current.SessionID, models.ToolEntryKey(turn.ID, taskStartFixtureCallID))
	}
	if _, err := store.OpenConversationAgentSession(ctx, models.OpenConversationAgentSessionRequest{Scope: current.Scope, ConversationID: conversationID,
		ThreadRootID: thread, AgentID: current.AssignedAgentID, RunID: current.ID, Lease: lease, Now: now}); err != nil {
		t.Fatalf("open lane session: %v", err)
	}
	args, _ := json.Marshal(map[string]string{"goal": goal, "acknowledgment": ack})
	user := &models.SessionEntry{ID: models.SessionEntryID(current.SessionID, "message:"+triggerID), RunID: current.ID, Type: models.SessionEntryUser,
		SourceKey: "message:" + triggerID, User: &models.UserEntry{Origin: models.UserEntryChannelMessage, Author: models.SessionAuthor{Type: "user", ID: "requester"},
			Content: []models.ContentPart{storetest.TextPart("Please do this in the background.")}, MessageID: triggerID}}
	assistant := &models.SessionEntry{ID: models.SessionEntryID(current.SessionID, models.AssistantEntryKey(turn.ID)), RunID: current.ID, StepID: turn.ID, Step: turn.Sequence,
		Type: models.SessionEntryAssistant, Assistant: &models.AssistantEntry{Provider: models.ModelStepProviderAnthropic, Model: "claude-x", StopReason: models.ModelStopToolCalls,
			ToolCalls: []models.ModelToolCall{{ID: taskStartFixtureCallID, Name: string(models.ControlToolStartTask), Arguments: args}}}}
	tool := &models.SessionEntry{ID: entryID, RunID: current.ID, StepID: turn.ID, Step: turn.Sequence, Type: models.SessionEntryTool, State: models.ToolEntryPending,
		ToolCallID: taskStartFixtureCallID, Tool: &models.ToolEntry{Name: string(models.ControlToolStartTask), Arguments: args, SideEffect: models.ToolSideEffectWrite}}
	if existing, err := store.GetSessionEntry(ctx, current.Scope, entryID); err == nil && existing != nil {
		return entryID, current.Revision
	}
	for _, batch := range [][]*models.SessionEntry{{user}, {assistant, tool}} {
		if _, err := store.AppendAgentSessionEntries(ctx, models.AppendAgentSessionEntriesRequest{Scope: current.Scope, SessionID: current.SessionID,
			RunID: current.ID, Lease: &lease, Entries: batch, Now: now}); err != nil {
			t.Fatalf("append start_task entries: %v", err)
		}
	}
	reloaded, err := store.GetAgentRun(ctx, current.Scope, current.ID)
	if err != nil {
		t.Fatal(err)
	}
	return entryID, reloaded.Revision
}

// Copied from conversation_task_sql_test.go
type conversationTaskSQLTestStore = storetest.ConversationTaskStore

// Copied from conversation_task_sql_test.go
// updateConversationTaskSQLRun is storetest.UpdateRun for the runtime tests
// that call it by this name.
func updateConversationTaskSQLRun(t *testing.T, store conversationTaskSQLTestStore, run *models.AgentRun, expectedRevision int64) {
	t.Helper()
	storetest.UpdateRun(t, store, run, expectedRevision)
}
