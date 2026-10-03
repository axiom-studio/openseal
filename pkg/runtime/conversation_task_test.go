package runtime

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
)

func taskStartRequest(source *AgentRun, turn *AgentTurn) StartConversationTaskRequest {
	p := turn.RequestedTask
	return StartConversationTaskRequest{Scope: source.Scope, SourceRunID: source.ID, ExpectedSourceRevision: source.Revision, WorkerID: "worker", TurnID: turn.ID, AssignedAgentID: source.AssignedAgentID, TaskKey: p.TaskKey, Goal: p.Goal, Acknowledgment: p.Acknowledgment, Budget: cloneBudgetPolicy(p.Budget)}
}

func TestConversationTaskAdmissionConcurrentReplayAndProvenance(t *testing.T) {
	store := NewMemoryStore()
	source, turn, conversation, trigger := settledWorkerTaskFixture(t, store)
	service := NewConversationTaskService(store)
	req := taskStartRequest(source, turn)
	var wg sync.WaitGroup
	results := make(chan *ConversationTaskResult, 8)
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := service.Start(t.Context(), req)
			results <- result
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var accepted *ConversationTaskResult
	for result := range results {
		if accepted == nil {
			accepted = result
		}
		if result == nil || result.Task.ID != accepted.Task.ID || result.WorkRun.ID != accepted.WorkRun.ID {
			t.Fatal("replay changed accepted identity")
		}
	}
	if accepted.Task.AuthenticatedActor != trigger.Sender || accepted.Task.ConversationID != conversation.ID || accepted.Task.SourceTurnID != turn.ID || accepted.Task.SourceTurnNumber != turn.Sequence {
		t.Fatal("task did not retain canonical source provenance")
	}
	work := accepted.WorkRun
	if work.ParentRunID != "" || work.RootRunID != work.ID || work.Kind != RunKindAgentWork || work.ConcurrencyKey != "task:"+accepted.Task.ID || work.Context["voiceCallStarted"] != nil {
		t.Fatalf("work retained foreground ownership: %#v", work)
	}
	current, err := store.GetAgentRun(t.Context(), source.Scope, source.ID)
	if err != nil || current.Status != AgentRunStatusCompleted || current.Output["summary"] != turn.RequestedTask.Acknowledgment {
		t.Fatalf("source acknowledgment was not atomically completed: %#v %v", current, err)
	}
	tasks, err := service.List(t.Context(), ConversationTaskFilter{Scope: source.Scope, Owner: source.Owner, ConversationID: conversation.ID, AuthenticatedActor: trigger.Sender, Limit: 10})
	if err != nil || len(tasks) != 1 {
		t.Fatalf("replay created duplicate tasks: %#v %v", tasks, err)
	}
	req.Goal = "A different request with the same key"
	if _, err := service.Start(t.Context(), req); !errors.Is(err, ErrConversationTaskConflict) {
		t.Fatalf("request mutation was accepted: %v", err)
	}
	for _, mutate := range []func(*GetConversationTaskRequest){func(r *GetConversationTaskRequest) { r.Scope.ID = "foreign" }, func(r *GetConversationTaskRequest) { r.AuthenticatedActor.ID = "foreign" }, func(r *GetConversationTaskRequest) { r.ThreadRootID = "foreign" }} {
		r := GetConversationTaskRequest{Scope: source.Scope, Owner: source.Owner, ConversationID: conversation.ID, TaskID: accepted.Task.ID, AuthenticatedActor: trigger.Sender}
		mutate(&r)
		if _, err := service.Get(t.Context(), r); !errors.Is(err, ErrConversationTaskNotFound) {
			t.Fatalf("task leaked canonical scope: %v", err)
		}
	}
}

func TestConversationTaskAdmissionRejectsExpiredOrCancelledSource(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(map[bool]string{false: "lease expired", true: "source cancelled"}[cancel], func(t *testing.T) {
			store := NewMemoryStore()
			source, turn, conversation, _ := settledWorkerTaskFixture(t, store)
			if cancel {
				_, err := NewRunCommandService(store).CommandAgentRun(t.Context(), AgentRunCommandRequest{Scope: source.Scope, RunID: source.ID, ExpectedRevision: source.Revision, Kind: AgentRunCommandCancel, Actor: ActivityActor{Type: "user", ID: "sender-user"}, Summary: "Cancel pending foreground"})
				if err != nil {
					t.Fatal(err)
				}
			} else {
				store.mu.Lock()
				current := store.agentRuns[portfolioKey(source.Scope, source.ID)]
				at := time.Now().Add(-time.Second)
				current.LeaseExpiresAt = &at
				store.mu.Unlock()
			}
			if _, err := NewConversationTaskService(store).Start(t.Context(), taskStartRequest(source, turn)); err == nil {
				t.Fatal("non-current source admitted work")
			}
			tasks, err := store.ListConversationTasks(t.Context(), ConversationTaskFilter{Scope: source.Scope, Owner: source.Owner, ConversationID: conversation.ID, Limit: 10})
			if err != nil || len(tasks) != 0 {
				t.Fatal("rejected source left independent work")
			}
		})
	}
}

func TestConversationTaskTransfersOnlyRemainingSourceBudget(t *testing.T) {
	store := NewMemoryStore()
	source, turn, _, _ := settledWorkerTaskFixture(t, store)
	previous := source.Revision
	source.Budget = &BudgetPolicy{MaxAttempts: 10, MaxTurns: 10, MaxInputTokens: 60000, MaxOutputTokens: 40000, MaxTotalTokens: 100000, MaxCostMicros: 1000000, MaxDurationMS: 600000, MaxActions: 10}
	source.BudgetUsage = BudgetUsage{Attempts: 1, Turns: 1, InputTokens: 1000, OutputTokens: 100, CostMicros: 1000, DurationMS: 1000, Actions: 1}
	deadline := time.Now().Add(time.Hour)
	source.Deadline = &deadline
	source.Revision++
	source.UpdatedAt = time.Now()
	updateConversationTaskSQLRun(t, store, source, previous)
	result, err := NewConversationTaskService(store).Start(t.Context(), taskStartRequest(source, turn))
	if err != nil {
		t.Fatal(err)
	}
	b := result.WorkRun.Budget
	if b == nil || b.MaxAttempts != 9 || b.MaxTurns != 9 || b.MaxInputTokens != 59000 || b.MaxOutputTokens != 39900 || b.MaxTotalTokens != 98900 || b.MaxCostMicros != 999000 || b.MaxDurationMS != 599000 || b.MaxActions != 9 {
		t.Fatalf("bounded source capacity was lost: %#v", b)
	}
	allocation := result.SourceRun.BudgetAllocations[result.WorkRun.ID]
	if allocation != *b || result.WorkRun.ObjectiveID != "" || result.SourceRun.BudgetUsage != source.BudgetUsage {
		t.Fatal("task funding was not recorded exactly once on its completed source")
	}
	if result.WorkRun.Deadline == nil || !result.WorkRun.Deadline.Equal(deadline) {
		t.Fatal("independent work dropped its canonical deadline")
	}
	b.MaxTurns = 999
	canonical, err := store.GetAgentRun(t.Context(), source.Scope, result.WorkRun.ID)
	if err != nil || canonical.Budget.MaxTurns != 9 {
		t.Fatal("returned task budget aliases durable state")
	}
}

func TestConversationTaskReportingPreservesOriginalReadersAndThread(t *testing.T) {
	for _, threaded := range []bool{false, true} {
		t.Run(map[bool]string{false: "directed question", true: "private thread"}[threaded], func(t *testing.T) {
			store := NewMemoryStore()
			service := NewConversationService(store)
			scope := Scope{Kind: "tenant", ID: "private-task"}
			conversation, _, err := service.CreateConversation(t.Context(), CreateConversationRequest{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, Title: "Private question", IdempotencyKey: "private-task"})
			if err != nil {
				t.Fatal(err)
			}
			actor := ConversationParticipant{Type: ConversationParticipantUser, ID: "requester"}
			target := ConversationParticipant{Type: ConversationParticipantAgent, ID: "agent"}
			root := ""
			if threaded {
				parent, err := service.PostChannelMessage(t.Context(), PostChannelMessageRequest{Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision, Sender: actor, Intent: MessageIntentQuestion, Content: "Private call thread", Audience: ConversationAudience{Kind: ConversationAudienceParticipants, Participants: []ConversationParticipant{target}}, IdempotencyKey: "private-root"})
				if err != nil {
					t.Fatal(err)
				}
				conversation = parent.Conversation
				root = parent.Message.ID
			}
			audience := ConversationAudience{Kind: ConversationAudienceParticipants, Participants: []ConversationParticipant{target}}
			if threaded {
				audience = ConversationAudience{Kind: ConversationAudienceChannel}
			}
			posted, err := service.PostChannelMessage(t.Context(), PostChannelMessageRequest{Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision, Sender: actor, Intent: MessageIntentQuestion, Content: "Review the release", Audience: audience, ReplyToMessageID: root, RequiresResponse: true, IdempotencyKey: "work"})
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = mustConversationRunScheduler(t, store).ScheduleMessage(t.Context(), scope, conversation.ID, posted.Message.ID)
			if err != nil {
				t.Fatal(err)
			}
			source, err := store.ClaimNextAgentRun(t.Context(), AgentRunClaim{Scope: scope, Kind: RunKindConversation, WorkerID: "worker", Now: time.Now(), LeaseDuration: time.Minute, AgingInterval: time.Minute})
			if err != nil || source == nil {
				t.Fatalf("claim: %#v %v", source, err)
			}
			settled, err := NewTurnCoordinator(store, store, store).Advance(t.Context(), AdvanceAgentRunRequest{Scope: scope, RunID: source.ID, WorkerID: "worker", DefinitionID: "agent", DefinitionVersion: "1"}, TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
				return &TurnOutcome{NextRunStatus: AgentRunStatusRunning, ProposedTask: taskProposalFixture()}, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			accepted, err := NewConversationTaskService(store).Start(t.Context(), taskStartRequest(settled.Run, settled.Turn))
			if err != nil {
				t.Fatal(err)
			}
			work := terminalReportingContractComplete(t, store, accepted.WorkRun, time.Now())
			// Deliver a final claim first, as a concurrent outbox worker or a fast
			// completed task can do. It must durably post the acknowledgment first.
			if err := projectTerminalRunReporting(t.Context(), store, work); err != nil {
				t.Fatal(err)
			}
			if err := projectTerminalRunReporting(t.Context(), store, accepted.SourceRun); err != nil {
				t.Fatal(err)
			}
			messages, err := store.ListChannelMessages(t.Context(), ChannelMessageFilter{Scope: scope, ConversationID: conversation.ID, Limit: 100})
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			last := int64(0)
			for _, message := range messages {
				if message.Intent != MessageIntentAnswer {
					continue
				}
				count++
				if message.Sequence <= last {
					t.Fatal("report ordering regressed")
				}
				last = message.Sequence
				if message.ReplyToMessageID != posted.Message.ID || message.BroadcastToChannel == threaded {
					t.Fatalf("task widened explicit thread delivery: %#v", message)
				}
				for _, reader := range []ConversationParticipant{actor, target} {
					if _, err := service.GetVisibleChannelMessage(t.Context(), scope, conversation.ID, message.ID, ConversationViewer{Participant: reader}); err != nil {
						t.Fatalf("original reader lost reply access: %v", err)
					}
				}
				if _, err := service.GetVisibleChannelMessage(t.Context(), scope, conversation.ID, message.ID, ConversationViewer{Participant: ConversationParticipant{Type: ConversationParticipantUser, ID: "stranger"}}); !errors.Is(err, ErrChannelMessageNotFound) {
					t.Fatalf("private source widened report visibility: %v", err)
				}
			}
			if count != 2 {
				t.Fatalf("expected one acknowledgment and one final: %d", count)
			}
		})
	}
}

func TestConversationTaskLifecycleKeepsStatusForegroundAndCancelGoverned(t *testing.T) {
	store := NewMemoryStore()
	source, turn, conversation, trigger := settledWorkerTaskFixture(t, store)
	first, err := NewConversationTaskService(store).Start(t.Context(), taskStartRequest(source, turn))
	if err != nil {
		t.Fatal(err)
	}
	held, err := store.ClaimNextAgentRun(t.Context(), AgentRunClaim{Scope: source.Scope, Kind: RunKindAgentWork, WorkerID: "background", Now: time.Now(), LeaseDuration: time.Minute, AgingInterval: time.Minute, MaxActiveForAgent: 4})
	if err != nil || held == nil || held.ID != first.WorkRun.ID {
		t.Fatalf("hold background: %#v %v", held, err)
	}
	conversations := NewConversationService(store)
	scheduler := mustConversationRunScheduler(t, store)
	post := func(key, content string) *AgentRun {
		t.Helper()
		current, e := conversations.GetConversation(t.Context(), source.Scope, conversation.ID)
		if e != nil {
			t.Fatal(e)
		}
		message := postConversationRunTestMessage(t, conversations, current, ConversationParticipantUser, MessageIntentQuestion, content, key)
		scheduled, _, e := scheduler.ScheduleMessage(t.Context(), source.Scope, conversation.ID, message.ID)
		if e != nil {
			t.Fatal(e)
		}
		return scheduled.Run
	}
	status := post("status", "How is the release review going?")
	claimed, err := store.ClaimNextAgentRun(t.Context(), AgentRunClaim{Scope: source.Scope, Kind: RunKindConversation, WorkerID: "worker", Now: time.Now(), LeaseDuration: time.Minute, AgingInterval: time.Minute, MaxActiveForAgent: 4})
	if err != nil || claimed == nil || claimed.ID != status.ID {
		t.Fatalf("status could not enter foreground: %#v %v", claimed, err)
	}
	fake := TurnRunnerFunc(func(ctx context.Context, input TurnExecutionContext) (*TurnOutcome, error) {
		if input.ForegroundConversation == nil || input.ForegroundConversation.ID != status.ID {
			t.Fatal("status lost its canonical foreground")
		}
		if !strings.Contains(input.Run.Goal, held.ID) {
			t.Fatal("foreground status did not receive bounded independent task state")
		}
		return &TurnOutcome{NextRunStatus: AgentRunStatusCompleted, RunOutput: map[string]interface{}{"summary": "Your release review is still running."}}, nil
	})
	runner, err := NewConversationRunTurnRunner(store, conversationRunTestCoordinator(t, conversations), ConversationRunTurnRunnerConfig{AgentTurns: TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*TurnRunnerBinding, error) {
		return &TurnRunnerBinding{DeploymentID: "agent", Runner: fake}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	result, err := NewTurnCoordinator(store, store, store).Advance(t.Context(), AdvanceAgentRunRequest{Scope: source.Scope, RunID: status.ID, WorkerID: "worker", DefinitionID: "agent", DefinitionVersion: "1"}, runner)
	if err != nil || result.Run.Status != AgentRunStatusCompleted {
		t.Fatalf("normal foreground status failed: %#v %v", result, err)
	}
	still, err := store.GetAgentRun(t.Context(), held.Scope, held.ID)
	if err != nil || still.Status != AgentRunStatusRunning || still.LeaseOwner != "background" {
		t.Fatalf("status interrupted background: %#v %v", still, err)
	}
	secondSource := post("another-task", "Independently review the documentation too.")
	claimed, err = store.ClaimNextAgentRun(t.Context(), AgentRunClaim{Scope: source.Scope, Kind: RunKindConversation, WorkerID: "worker", Now: time.Now(), LeaseDuration: time.Minute, AgingInterval: time.Minute, MaxActiveForAgent: 4})
	if err != nil || claimed == nil || claimed.ID != secondSource.ID {
		t.Fatalf("second task source claim: %#v %v", claimed, err)
	}
	proposal := taskProposalFixture()
	proposal.TaskKey = "documentation"
	proposal.Goal = "Review documentation independently"
	proposal.Acknowledgment = "I will review the documentation in the background."
	settled, err := NewTurnCoordinator(store, store, store).Advance(t.Context(), AdvanceAgentRunRequest{Scope: source.Scope, RunID: claimed.ID, WorkerID: "worker", DefinitionID: "agent", DefinitionVersion: "1"}, TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
		return &TurnOutcome{NextRunStatus: AgentRunStatusRunning, ProposedTask: proposal}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewConversationTaskService(store).Start(t.Context(), taskStartRequest(settled.Run, settled.Turn))
	if err != nil {
		t.Fatal(err)
	}
	if second.WorkRun.ID == held.ID || second.WorkRun.ConcurrencyKey == held.ConcurrencyKey {
		t.Fatal("independent requests shared an execution lane")
	}
	cancelSource := post("cancel-task", "Stop only the documentation review.")
	definition := RunManagementSkill()
	bound := &skill.BoundAction{Definition: definition, Binding: &skill.Binding{DeploymentID: "agent"}, Action: definition.Actions[RunActionCancel]}
	validator, _ := NewRunActionValidator(store)
	args, _, err := validator.ResolveActionProposalArguments(t.Context(), ActionProposalValidationInput{Run: cancelSource, Bound: bound, Arguments: map[string]interface{}{"runId": second.WorkRun.ID, "reason": "User requested cancellation"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := validator.ValidateActionProposal(t.Context(), ActionProposalValidationInput{Run: cancelSource, Bound: bound, Arguments: args}); err != nil {
		t.Fatal(err)
	}
	dispatcher, _ := NewRunActionDispatcher(store, nil)
	if _, err := dispatcher.DispatchAction(t.Context(), ActionDispatchInput{Run: cancelSource, Bound: bound, Call: &ActionCall{Scope: source.Scope, Arguments: args}}); err != nil {
		t.Fatal(err)
	}
	cancelled, _ := store.GetAgentRun(t.Context(), source.Scope, second.WorkRun.ID)
	still, _ = store.GetAgentRun(t.Context(), source.Scope, held.ID)
	if cancelled.Status != AgentRunStatusCanceled || still.Status != AgentRunStatusRunning {
		t.Fatal("governed cancellation affected unrelated work")
	}
	terminal, _, err := NewRunActivityService(store, store).TransitionRun(t.Context(), held.Scope, held.ID, RunTransitionRequest{ExpectedRevision: still.Revision, Status: AgentRunStatusCompleted, Output: map[string]interface{}{"summary": "The release review found two fixes."}})
	if err != nil {
		t.Fatal(err)
	}
	if err := projectTerminalRunReporting(t.Context(), store, terminal); err != nil {
		t.Fatal(err)
	}
	if err := projectTerminalRunReporting(t.Context(), store, terminal); err != nil {
		t.Fatal(err)
	}
	messages, err := store.ListChannelMessages(t.Context(), ChannelMessageFilter{Scope: source.Scope, ConversationID: conversation.ID, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	ackSequence, resultSequence := int64(0), int64(0)
	for _, message := range messages {
		if message.Content == first.Task.Acknowledgment {
			if ackSequence != 0 {
				t.Fatal("duplicate acknowledgment")
			}
			ackSequence = message.Sequence
		}
		if message.Content == "The release review found two fixes." {
			if resultSequence != 0 {
				t.Fatal("duplicate final result")
			}
			resultSequence = message.Sequence
			if message.ReplyToMessageID != trigger.ID || message.ThreadRootID != trigger.ID || len(message.References) != 2 || message.References[1].Kind != ConversationReferenceTask || message.References[1].ID != first.Task.ID {
				t.Fatalf("final lost canonical task/thread identity: %#v", message)
			}
		}
	}
	if ackSequence == 0 || resultSequence <= ackSequence {
		t.Fatalf("final preceded durable acknowledgment: ack=%d final=%d", ackSequence, resultSequence)
	}
	snapshot, err := NewConversationTaskService(store).Get(t.Context(), GetConversationTaskRequest{Scope: source.Scope, Owner: source.Owner, ConversationID: conversation.ID, AuthenticatedActor: trigger.Sender, TaskID: first.Task.ID})
	if err != nil || snapshot.TerminalReportMessageID == "" {
		t.Fatalf("terminal delivery receipt missing: %#v %v", snapshot, err)
	}
	worker := terminalReportingContractWorker(t, store, time.Now())
	if _, err := worker.ProcessBatch(t.Context()); err != nil {
		t.Fatal(err)
	}
	messagesAfter, _ := store.ListChannelMessages(t.Context(), ChannelMessageFilter{Scope: source.Scope, ConversationID: conversation.ID, Limit: 100})
	count := 0
	for _, message := range messagesAfter {
		if message.Content == "The release review found two fixes." {
			count++
		}
	}
	if count != 1 {
		t.Fatal("outbox replay duplicated independently completed work")
	}
}
