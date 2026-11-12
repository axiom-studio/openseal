package runtime

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"
)

// Conversation task tests over the run loop: chat source Runs are driven by
// the worker pool's RunLoop with a scripted model-kind producer, the
// production step tool dispatcher and the control executor. The model's
// conversation_start_task call becomes a real step row, assistant entry and
// tool entry; only the model is scripted.

// workerTaskStep is one scripted reply of workerTaskProducer.
type workerTaskStep func(ctx context.Context, call int, in StepInput) (StepOutput, error)

// workerTaskProducer is a model-kind producer that replies from a script.
type workerTaskProducer struct {
	mu    sync.Mutex
	calls int
	reply workerTaskStep
}

func (*workerTaskProducer) Kind() StepProducerKind { return StepProducerModel }

func (p *workerTaskProducer) Produce(ctx context.Context, in StepInput) (StepOutput, error) {
	p.mu.Lock()
	p.calls++
	call := p.calls
	p.mu.Unlock()
	return p.reply(ctx, call, in)
}

func (p *workerTaskProducer) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// startTaskStep is a model step whose only tool call is
// conversation_start_task, with the call id the task fixtures key entries by.
func startTaskStep(goal, ack string) StepOutput {
	args, _ := json.Marshal(map[string]string{"goal": goal, "acknowledgment": ack})
	return StepOutput{ModelProvider: "fake", Model: "deterministic", Assistant: &AssistantEntry{
		Provider: ModelStepProviderAnthropic, Model: "claude-x", StopReason: ModelStopToolCalls,
		ToolCalls: []ModelToolCall{{ID: taskStartFixtureCallID, Name: string(ControlToolStartTask), Arguments: args}},
	}}
}

// answerStep is a model step that answers in plain text.
func answerStep(text string) StepOutput {
	return StepOutput{ModelProvider: "fake", Model: "deterministic", Assistant: &AssistantEntry{
		Provider: ModelStepProviderAnthropic, Model: "claude-x", StopReason: ModelStopEnd, Text: text,
	}}
}

// workerTaskResolver resolves chat source Runs the way the conversation
// binding does for the parts these tests need: the lane session (seeded with
// the trigger message), the control authority and the claim tool executor,
// whose control executor admits tasks through tasks.
func workerTaskResolver(t *testing.T, store *SQLiteStore, tasks ConversationTaskKernelStore, producer StepProducer) TurnRunnerResolverFunc {
	t.Helper()
	activity := NewRunActivityService(store, store)
	control := &ControlExecutor{
		Portfolio: store, Sessions: store, Activity: activity, Conversations: NewConversationService(store),
		Reporting: store, Tasks: NewConversationTaskService(tasks),
	}
	dispatcher, err := NewStepToolDispatcher(StepToolDispatcherConfig{Sessions: store, Turns: store, Actions: store, Control: control})
	if err != nil {
		t.Fatal(err)
	}
	return func(ctx context.Context, run *AgentRun) (*TurnRunnerBinding, error) {
		session, err := openWorkerTaskSession(ctx, store, run)
		if err != nil {
			return nil, err
		}
		authority, err := ResolveControlAuthority(ctx, ControlAuthorityInput{
			Run: run, DeploymentID: run.AssignedAgentID, ConversationTasks: store, Runs: store, Conversations: store,
		})
		if err != nil {
			return nil, err
		}
		return &TurnRunnerBinding{
			DeploymentID: run.AssignedAgentID, DefinitionID: run.AssignedAgentID, DefinitionVersion: "1",
			ModelProvider: "fake", Model: "deterministic", Producer: producer, Session: session, Authority: authority,
			Tools: dispatcher.ForClaim(LoopToolClaim{}),
		}, nil
	}
}

// openWorkerTaskSession opens the chat Run's lane session and appends the
// trigger message once (the conversation catch-up's part).
func openWorkerTaskSession(ctx context.Context, store *SQLiteStore, run *AgentRun) (*AgentSession, error) {
	conversationID, _ := run.Context[conversationRunContextConversationID].(string)
	triggerID, _ := run.Context[conversationRunContextTriggerID].(string)
	thread, _ := run.Context["threadRootMessageId"].(string)
	now := time.Now().UTC()
	lease := AgentRunLeaseGuard{WorkerID: run.LeaseOwner, Now: now}
	opened, err := store.OpenConversationAgentSession(ctx, OpenConversationAgentSessionRequest{
		Scope: run.Scope, ConversationID: conversationID, ThreadRootID: thread, AgentID: run.AssignedAgentID, RunID: run.ID, Lease: lease, Now: now,
	})
	if err != nil {
		return nil, err
	}
	session := opened.Session
	userID := SessionEntryID(session.ID, "message:"+triggerID)
	if existing, err := optionalSessionEntry(store.GetSessionEntry(ctx, run.Scope, userID)); err != nil || existing != nil {
		return session, err
	}
	message, err := store.GetChannelMessage(ctx, run.Scope, conversationID, triggerID)
	if err != nil {
		return nil, err
	}
	user := &SessionEntry{ID: userID, RunID: run.ID, Type: SessionEntryUser, SourceKey: "message:" + triggerID,
		User: &UserEntry{Origin: UserEntryChannelMessage, Author: SessionAuthor{Type: "user", ID: message.Sender.ID},
			Content: []ContentPart{testTextPart(message.Content)}, MessageID: triggerID}}
	if _, err := store.AppendAgentSessionEntries(ctx, AppendAgentSessionEntriesRequest{
		Scope: run.Scope, SessionID: session.ID, RunID: run.ID, Lease: &lease, Entries: []*SessionEntry{user}, Now: now,
	}); err != nil {
		return nil, err
	}
	return store.GetAgentSession(ctx, run.Scope, session.ID)
}

func workerTaskPool(t *testing.T, store *SQLiteStore, scope Scope, producer StepProducer) *AgentRunWorkerPool {
	t.Helper()
	return workerTaskPoolWith(t, store, scope, workerTaskResolver(t, store, store, producer))
}

func workerTaskPoolWith(t *testing.T, store *SQLiteStore, scope Scope, resolver TurnRunnerResolver) *AgentRunWorkerPool {
	t.Helper()
	pool, err := NewAgentRunWorkerPool(store, resolver, nil, AgentRunWorkerConfig{
		Scope: scope, Kind: RunKindConversation, LeaseDuration: time.Minute, TurnLeaseDuration: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	return pool
}

// admissionLostTaskStore loses the source lease at the task admission
// commit: the worker died after its step was applied, before the settle.
type admissionLostTaskStore struct{ *SQLiteStore }

func (s *admissionLostTaskStore) CreateConversationTask(context.Context, ConversationTaskCreateRecord) (*ConversationTaskResult, error) {
	return nil, ErrLeaseLost
}

// settledWorkerTaskFixture drives a chat Run through the worker's run loop to
// the point where its conversation_start_task call is recorded and its step
// applied, and stops before admission: the Run is running, leased by
// "worker", its last step completed and applied, and the tool entry
// pending. Callers admit the task themselves (taskStartRequest).
func settledWorkerTaskFixture(t *testing.T, store *SQLiteStore) (*AgentRun, *AgentTurn, *Conversation, *ChannelMessage) {
	t.Helper()
	scope := Scope{Kind: "tenant", ID: "worker-task"}
	conversations := NewConversationService(store)
	seedTestSpace(t, store, scope)
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
	proposal := workerTaskProposal()
	producer := &workerTaskProducer{reply: func(context.Context, int, StepInput) (StepOutput, error) {
		return startTaskStep(proposal.goal, proposal.ack), nil
	}}
	pool := workerTaskPoolWith(t, store, scope, workerTaskResolver(t, store, &admissionLostTaskStore{SQLiteStore: store}, producer))
	pool.executeClaim(t.Context(), "worker", claimed)

	source, err := store.GetAgentRun(t.Context(), scope, claimed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if source.Status != AgentRunStatusRunning || source.LeaseOwner != "worker" || len(source.Output) != 0 || source.LastAppliedTurn != 1 || producer.count() != 1 {
		t.Fatalf("source Run after the start_task step: %#v (model calls %d)", source, producer.count())
	}
	turns, err := store.ListAgentTurns(t.Context(), AgentTurnFilter{Scope: scope, RunID: source.ID})
	if err != nil || len(turns) != 1 || turns[0].Status != AgentTurnStatusCompleted || turns[0].Sequence != source.LastAppliedTurn {
		t.Fatalf("start_task step row: %#v %v", turns, err)
	}
	entry, err := store.GetSessionEntry(t.Context(), scope, SessionEntryID(source.SessionID, ToolEntryKey(turns[0].ID, taskStartFixtureCallID)))
	if err != nil || entry == nil || entry.State != ToolEntryPending || entry.StepID != turns[0].ID {
		t.Fatalf("start_task tool entry: %#v %v", entry, err)
	}
	return source, turns[0], conversation, message
}

type workerTaskProposalText struct{ goal, ack string }

func workerTaskProposal() workerTaskProposalText {
	return workerTaskProposalText{goal: "Review the release and report the outcome", ack: "I’ve started the release review."}
}

// A worker that dies between applying the start_task step and admitting the
// task leaves the call pending. The next claim settles it from the log
// without calling the model again: the task is admitted once, the source Run
// completes with the acknowledgment, and the tool entry completes.
func TestLoopResumesPendingTaskStartWithoutAnotherStep(t *testing.T) {
	store := newTestKernelStore(t)
	source, step, conversation, _ := settledWorkerTaskFixture(t, store)
	expireRunLease(t, store, source)

	producer := &workerTaskProducer{reply: func(context.Context, int, StepInput) (StepOutput, error) {
		return answerStep("unexpected second step"), nil
	}}
	pool := workerTaskPool(t, store, source.Scope, producer)
	claimed, err := pool.scheduler.ClaimNext(t.Context(), AgentRunClaimRequest{
		Scope: source.Scope, Kind: RunKindConversation, WorkerID: "worker-2", LeaseDuration: time.Minute,
	})
	if err != nil || claimed == nil || claimed.ID != source.ID {
		t.Fatalf("reclaim source: %#v %v", claimed, err)
	}
	pool.executeClaim(t.Context(), "worker-2", claimed)

	finished, err := store.GetAgentRun(t.Context(), source.Scope, source.ID)
	if err != nil || finished.Status != AgentRunStatusCompleted || finished.LastAppliedTurn != 1 || producer.count() != 0 {
		t.Fatalf("resumed source: %#v model=%d %v", finished, producer.count(), err)
	}
	// The next claim's step settles the open call in its resume phase; it
	// records no model output of its own.
	assistants, err := store.ListAgentSessionEntries(t.Context(), SessionEntryFilter{
		Scope: source.Scope, RunID: source.ID, Types: []SessionEntryType{SessionEntryAssistant}, Limit: 10,
	})
	if err != nil || len(assistants) != 1 || assistants[0].StepID != step.ID {
		t.Fatalf("resume called the model again: %#v %v", assistants, err)
	}
	entry, err := store.GetSessionEntry(t.Context(), source.Scope, SessionEntryID(source.SessionID, ToolEntryKey(step.ID, taskStartFixtureCallID)))
	if err != nil || entry == nil || entry.State != ToolEntryCompleted {
		t.Fatalf("start_task entry after resume: %#v %v", entry, err)
	}
	tasks, err := store.ListConversationTasks(t.Context(), ConversationTaskFilter{Scope: source.Scope, Owner: source.Owner, ConversationID: conversation.ID, Limit: 10})
	if err != nil || len(tasks) != 1 || tasks[0].Task.SourceToolEntryID != entry.ID || tasks[0].Task.SourceRunID != source.ID {
		t.Fatalf("admitted tasks: %#v %v", tasks, err)
	}
	work, err := store.ListAgentRuns(t.Context(), AgentRunFilter{Scope: source.Scope, Kind: RunKindAgentWork})
	if err != nil || len(work) != 1 {
		t.Fatalf("task work Runs: %#v %v", work, err)
	}
}

// expireRunLease moves the Run's lease into the past so another worker can
// claim it (the first worker is gone).
func expireRunLease(t *testing.T, store *SQLiteStore, run *AgentRun) {
	t.Helper()
	current := getStoredRun(t, store, run.Scope, run.ID)
	past := time.Now().UTC().Add(-time.Second)
	current.LeaseExpiresAt = &past
	putStoredRun(t, store, current)
}

func TestActiveConversationRunsIncludeOnlyExactIndependentTaskThreadAndActor(t *testing.T) {
	store := newTestKernelStore(t)
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
