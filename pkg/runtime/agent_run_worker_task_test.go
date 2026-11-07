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
	}), nil, AgentRunWorkerConfig{Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	return pool
}

type rejectedWorkerTaskStore struct{ *MemoryStore }

func (s *rejectedWorkerTaskStore) CreateConversationTask(context.Context, ConversationTaskCreateRecord) (*ConversationTaskResult, error) {
	return nil, errors.New("task transaction rejected")
}

func TestActiveConversationRunsIncludeOnlyExactIndependentTaskThreadAndActor(t *testing.T) {
	store := NewMemoryStore()
	source, turn, conversation, trigger := settledWorkerTaskFixture(t, store)
	if _, err := NewConversationTaskService(store).Start(t.Context(), taskStartRequest(t, store, source, turn)); err != nil {
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
