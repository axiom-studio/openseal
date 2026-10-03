package runtime

import (
	"context"
	"errors"
	"testing"
	"time"
)

func settledWorkerTaskFixture(t *testing.T, store *MemoryStore) (*AgentRun, *AgentTurn, *Conversation, *ChannelMessage) {
	t.Helper()
	scope := Scope{Kind: "tenant", ID: "worker-task"}
	conversations := NewConversationService(store)
	conversation, _, err := conversations.CreateConversation(t.Context(), CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, Title: "Assistant", IdempotencyKey: "worker-task-chat",
	})
	if err != nil {
		t.Fatal(err)
	}
	message := postConversationRunTestMessage(t, conversations, conversation, ConversationParticipantUser, MessageIntentQuestion,
		"Review the release in the background", "task-user-message")
	scheduled, _, err := mustConversationRunScheduler(t, store).ScheduleMessage(t.Context(), scope, conversation.ID, message.ID)
	if err != nil || scheduled == nil || scheduled.Run == nil {
		t.Fatalf("schedule foreground: %#v %v", scheduled, err)
	}
	claimed, err := store.ClaimNextAgentRun(t.Context(), AgentRunClaim{
		Scope: scope, Kind: RunKindConversation, WorkerID: "worker", Now: time.Now(), LeaseDuration: time.Minute, AgingInterval: time.Minute,
	})
	if err != nil || claimed == nil || claimed.ID != scheduled.Run.ID {
		t.Fatalf("claim foreground: %#v %v", claimed, err)
	}
	result, err := NewTurnCoordinator(store, store, store).Advance(t.Context(), AdvanceAgentRunRequest{
		Scope: scope, RunID: claimed.ID, WorkerID: "worker", DefinitionID: "agent", DefinitionVersion: "1",
	}, TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
		return &TurnOutcome{NextRunStatus: AgentRunStatusRunning, ProposedTask: taskProposalFixture()}, nil
	}))
	if err != nil || result == nil || result.Run == nil || result.Turn == nil || result.Turn.RequestedTask == nil {
		t.Fatalf("settle foreground task proposal: %#v %v", result, err)
	}
	if result.Run.Status != AgentRunStatusRunning || len(result.Run.Output) != 0 {
		t.Fatal("settled foreground acknowledged a task before admission")
	}
	return result.Run, result.Turn, conversation, message
}

func workerTaskPool(t *testing.T, store KernelStore, scope Scope, runner TurnRunner) *AgentRunWorkerPool {
	t.Helper()
	pool, err := NewAgentRunWorkerPool(store, TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*TurnRunnerBinding, error) {
		return &TurnRunnerBinding{DeploymentID: "agent", DefinitionID: "agent", DefinitionVersion: "1", Runner: runner}, nil
	}), nil, AgentRunWorkerConfig{Scope: scope, MaxTurnsPerClaim: 1})
	if err != nil {
		t.Fatal(err)
	}
	return pool
}

func TestAgentRunWorkerRecoversAppliedTaskBeforeAnotherModelTurn(t *testing.T) {
	store := NewMemoryStore()
	source, turn, conversation, _ := settledWorkerTaskFixture(t, store)
	modelCalls := 0
	pool := workerTaskPool(t, store, source.Scope, TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
		modelCalls++
		return nil, errors.New("recovery must reuse the settled proposal")
	}))
	// Simulate a process ending after the Turn and LastAppliedTurn committed,
	// before materializeTurnTask created any independent work.
	pool.executeClaim(t.Context(), "worker", source)
	completed, err := store.GetAgentRun(t.Context(), source.Scope, source.ID)
	if err != nil || completed.Status != AgentRunStatusCompleted || completed.Output["summary"] != turn.RequestedTask.Acknowledgment || modelCalls != 0 {
		t.Fatalf("recovered completion: %#v calls=%d err=%v", completed, modelCalls, err)
	}
	tasks, err := store.ListConversationTasks(t.Context(), ConversationTaskFilter{
		Scope: source.Scope, Owner: source.Owner, ConversationID: conversation.ID, Limit: 20,
	})
	if err != nil || len(tasks) != 1 || tasks[0].WorkRun.ParentRunID != "" || tasks[0].WorkRun.RootRunID != tasks[0].WorkRun.ID || tasks[0].WorkRun.Status != AgentRunStatusQueued {
		t.Fatalf("independent task was not admitted once: %#v %v", tasks, err)
	}
	turns, err := store.ListAgentTurns(t.Context(), AgentTurnFilter{Scope: source.Scope, RunID: source.ID})
	if err != nil || len(turns) != 1 {
		t.Fatalf("recovery created another turn: %#v %v", turns, err)
	}
	replayed, err := pool.materializeTurnTask(t.Context(), "worker", completed, turn, &TurnRunnerBinding{DeploymentID: "agent"})
	if err != nil || replayed.ID != completed.ID || replayed.Status != AgentRunStatusCompleted {
		t.Fatalf("admission replay: %#v %v", replayed, err)
	}
	tasks, err = store.ListConversationTasks(t.Context(), ConversationTaskFilter{Scope: source.Scope, Owner: source.Owner, ConversationID: conversation.ID, Limit: 20})
	if err != nil || len(tasks) != 1 {
		t.Fatal("admission replay duplicated independent work")
	}
}

type rejectedWorkerTaskStore struct{ *MemoryStore }

func (s *rejectedWorkerTaskStore) CreateConversationTask(context.Context, ConversationTaskCreateRecord) (*ConversationTaskResult, error) {
	return nil, errors.New("task transaction rejected")
}

func TestAgentRunWorkerRejectedTaskDoesNotAcknowledgeOrCreateWork(t *testing.T) {
	store := &rejectedWorkerTaskStore{MemoryStore: NewMemoryStore()}
	source, turn, conversation, _ := settledWorkerTaskFixture(t, store.MemoryStore)
	pool := workerTaskPool(t, store, source.Scope, TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) { return nil, nil }))
	if _, err := pool.materializeTurnTask(t.Context(), "worker", source, turn, &TurnRunnerBinding{DeploymentID: "agent"}); err == nil {
		t.Fatal("rejected task transaction was acknowledged")
	}
	current, err := store.GetAgentRun(t.Context(), source.Scope, source.ID)
	if err != nil || current.Status != AgentRunStatusRunning || len(current.Output) != 0 || current.Revision != source.Revision {
		t.Fatalf("rejection changed foreground output: %#v %v", current, err)
	}
	tasks, err := store.ListConversationTasks(t.Context(), ConversationTaskFilter{Scope: source.Scope, Owner: source.Owner, ConversationID: conversation.ID, Limit: 20})
	if err != nil || len(tasks) != 0 {
		t.Fatal("rejected transaction left a durable task")
	}
	runs, err := store.ListAgentRuns(t.Context(), AgentRunFilter{Scope: source.Scope, Kind: RunKindAgentWork})
	if err != nil || len(runs) != 0 {
		t.Fatal("rejected transaction left a work run")
	}
}

func TestAgentRunWorkerDoesNotRecoverTaskSupersededByNewGuidance(t *testing.T) {
	store := NewMemoryStore()
	source, _, _, _ := settledWorkerTaskFixture(t, store)
	pool := workerTaskPool(t, store, source.Scope, TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) { return nil, nil }))
	source.PendingInterventions = []AgentRunIntervention{{ID: "new-guidance"}}
	if turn, err := pool.pendingAppliedTaskTurn(t.Context(), source); err != nil || turn != nil {
		t.Fatalf("superseded proposal was replayed: %#v %v", turn, err)
	}
}

func TestActiveConversationRunsIncludeOnlyExactIndependentTaskThreadAndActor(t *testing.T) {
	store := NewMemoryStore()
	source, turn, conversation, trigger := settledWorkerTaskFixture(t, store)
	pool := workerTaskPool(t, store, source.Scope, TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) { return nil, nil }))
	if _, err := pool.materializeTurnTask(t.Context(), "worker", source, turn, &TurnRunnerBinding{DeploymentID: "agent"}); err != nil {
		t.Fatal(err)
	}
	runner := &ConversationRunTurnRunner{portfolio: store}
	active, err := runner.activeConversationRuns(t.Context(), conversation, trigger)
	if err != nil || len(active) != 1 || active[0].Revision < 1 || !slicesContainsRunCommand(active[0].AvailableControls, RunActionCancel) {
		t.Fatalf("task controls absent from authoritative thread snapshot: %#v %v", active, err)
	}
	// Provider channels scope controls to their exact thread; ordinary app
	// conversations retain task visibility for later messages in the same chat.
	conversation.Origin = &ConversationReference{Kind: ConversationReferenceExternalSource, ID: "external-channel"}
	for _, change := range []func(*ChannelMessage){
		func(message *ChannelMessage) { message.Sender.ID = "other-user" },
		func(message *ChannelMessage) { message.ThreadRootID = "other-thread" },
	} {
		other := cloneChannelMessage(trigger)
		change(other)
		active, err := runner.activeConversationRuns(t.Context(), conversation, other)
		if err != nil || len(active) != 0 {
			t.Fatalf("task leaked across actor or thread: %#v %v", active, err)
		}
	}
}
