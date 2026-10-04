package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func continuationLifecycleFixture(t *testing.T, store *MemoryStore, id string) (*AgentRun, *Conversation, *ChannelMessage) {
	t.Helper()
	service := NewConversationService(store)
	scope := Scope{Kind: "tenant", ID: id}
	conversation, _, err := service.CreateConversation(t.Context(), CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, Title: "Assistant", IdempotencyKey: id,
	})
	if err != nil {
		t.Fatal(err)
	}
	message := postConversationRunTestMessage(t, service, conversation, ConversationParticipantUser, MessageIntentQuestion, "Review the release", id+"-message")
	result, _, err := mustConversationRunScheduler(t, store).ScheduleMessage(t.Context(), scope, conversation.ID, message.ID)
	if err != nil || result == nil || result.Run == nil {
		t.Fatalf("schedule conversation: %#v %v", result, err)
	}
	return result.Run, conversation, message
}

func backdateContinuationLifecycleRun(t *testing.T, store *MemoryStore, run *AgentRun, createdAt time.Time) *AgentRun {
	t.Helper()
	store.mu.Lock()
	defer store.mu.Unlock()
	current := cloneAgentRun(store.agentRuns[portfolioKey(run.Scope, run.ID)])
	current.CreatedAt = createdAt
	store.saveMemoryAgentRunLocked(portfolioKey(run.Scope, run.ID), current)
	return cloneAgentRun(current)
}

func TestConversationTaskContinuationReconcilesQueuedRunFromCreationTime(t *testing.T) {
	store := NewMemoryStore()
	run, conversation, _ := continuationLifecycleFixture(t, store, "queued-continuation")
	now := time.Now().UTC()
	run = backdateContinuationLifecycleRun(t, store, run, now.Add(-ConversationTaskForegroundTimeout))
	// A fresh activity timestamp is not a new foreground deadline.
	store.mu.Lock()
	store.agentRuns[portfolioKey(run.Scope, run.ID)].UpdatedAt = now
	store.mu.Unlock()
	reconciler, err := NewConversationTaskContinuationReconciler(store)
	if err != nil {
		t.Fatal(err)
	}
	results, err := reconciler.ReconcileScope(t.Context(), run.Scope, now)
	if err != nil || len(results) != 1 || results[0].WorkRun.ID != run.ID || results[0].WorkRun.Status != AgentRunStatusQueued {
		t.Fatalf("queued continuation: %#v %v", results, err)
	}
	if !ConversationTaskMatchesWorkRun(results[0].Task, results[0].WorkRun) {
		t.Fatal("promotion did not retain canonical same-Run identity")
	}
	foreground, err := store.ListConversationForegroundRuns(t.Context(), run.Scope, run.Owner, conversation.ID, 100, 0)
	if err != nil || len(foreground) != 0 {
		t.Fatalf("promoted Run remained in foreground restore: %#v %v", foreground, err)
	}
	restarted, _ := NewConversationTaskContinuationReconciler(store)
	if results, err := restarted.ReconcileScope(t.Context(), run.Scope, now.Add(time.Second)); err != nil || len(results) != 0 {
		t.Fatalf("restart duplicated continuation: %#v %v", results, err)
	}
}

func TestAgentRunWorkerKeepsForegroundDuringSlowResolution(t *testing.T) {
	testAgentRunWorkerKeepsForegroundPastDeadline(t, true)
}

func testAgentRunWorkerKeepsForegroundPastDeadline(t *testing.T, resolving bool) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		store := NewMemoryStore()
		run, conversation, trigger := continuationLifecycleFixture(t, store, "slow-foreground")
		claimed, err := store.ClaimNextAgentRun(t.Context(), AgentRunClaim{Scope: run.Scope, Kind: RunKindConversation, WorkerID: "worker", Now: time.Now(), LeaseDuration: time.Minute, AgingInterval: time.Minute})
		if err != nil || claimed == nil {
			t.Fatalf("claim: %#v %v", claimed, err)
		}
		entered, release, done := make(chan context.Context, 1), make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		closeRelease := func() { releaseOnce.Do(func() { close(release) }) }
		var modelCalls atomic.Int32
		runner := TurnRunnerFunc(func(ctx context.Context, _ TurnExecutionContext) (*TurnOutcome, error) {
			modelCalls.Add(1)
			if !resolving {
				entered <- ctx
				<-release
			}
			return &TurnOutcome{NextRunStatus: AgentRunStatusCompleted, RunOutput: map[string]interface{}{"summary": "Reviewed release"}}, ctx.Err()
		})
		pool, err := NewAgentRunWorkerPool(store, TurnRunnerResolverFunc(func(ctx context.Context, _ *AgentRun) (*TurnRunnerBinding, error) {
			if resolving {
				entered <- ctx
				<-release
			}
			return &TurnRunnerBinding{DeploymentID: "agent", Runner: runner}, ctx.Err()
		}), nil, AgentRunWorkerConfig{Scope: run.Scope, Kind: RunKindConversation, LeaseDuration: time.Minute, TurnLeaseDuration: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		go func() { defer close(done); pool.executeClaim(t.Context(), "worker", claimed) }()
		defer func() { closeRelease(); <-done }()
		invocationCtx := <-entered
		// Advance the real worker's timers without a wall-clock sleep. Both
		// runner resolution and an active model Turn cross the former deadline.
		time.Sleep(ConversationTaskForegroundTimeout + time.Second)
		synctest.Wait()
		current := assertConversationRunStillForeground(t, store, run, conversation, trigger, AgentRunStatusRunning)
		if current.LeaseOwner != "worker" || invocationCtx.Err() != nil {
			t.Fatalf("elapsed time interrupted the original invocation: %#v ctx=%v", current, invocationCtx.Err())
		}
		turns, err := store.ListAgentTurns(t.Context(), AgentTurnFilter{Scope: run.Scope, RunID: run.ID})
		if err != nil {
			t.Fatal(err)
		}
		if resolving && (len(turns) != 0 || modelCalls.Load() != 0) || !resolving && (len(turns) != 1 || turns[0].Status != AgentTurnStatusRunning || turns[0].LeaseOwner != "worker" || modelCalls.Load() != 1) {
			t.Fatalf("elapsed time restarted work: turns=%#v calls=%d", turns, modelCalls.Load())
		}
		closeRelease()
		<-done
		finished, err := store.GetAgentRun(t.Context(), run.Scope, run.ID)
		if err != nil || finished.Status != AgentRunStatusCompleted || finished.LastAppliedTurn != 1 || finished.Context[ConversationTaskContextKey] != nil || finished.Output["summary"] != "Reviewed release" || modelCalls.Load() != 1 {
			t.Fatalf("foreground invocation did not finish once: %#v calls=%d err=%v", finished, modelCalls.Load(), err)
		}
	})
}

func assertConversationRunStillForeground(t *testing.T, store *MemoryStore, original *AgentRun, conversation *Conversation, trigger *ChannelMessage, status AgentRunStatus) *AgentRun {
	t.Helper()
	current, err := store.GetAgentRun(t.Context(), original.Scope, original.ID)
	if err != nil || current == nil || current.Status != status || current.Kind != RunKindConversation || current.ConcurrencyKey != original.ConcurrencyKey || current.Context[ConversationTaskContextKey] != nil {
		t.Fatalf("elapsed time changed the foreground execution lane: %#v %v", current, err)
	}
	if task, err := store.FindConversationTaskByWorkRunID(t.Context(), original.Scope, original.ID); err != nil || task != nil {
		t.Fatalf("elapsed time created a background task: %#v %v", task, err)
	}
	foreground, err := store.ListConversationForegroundRuns(t.Context(), original.Scope, original.Owner, conversation.ID, 100, 0)
	if err != nil || len(foreground) != 1 || foreground[0].ID != original.ID {
		t.Fatalf("aged run disappeared from foreground restore: %#v %v", foreground, err)
	}
	messages, err := store.ListChannelMessages(t.Context(), ChannelMessageFilter{Scope: original.Scope, ConversationID: conversation.ID, Limit: 10})
	if err != nil || len(messages) != 1 || messages[0].ID != trigger.ID {
		t.Fatalf("elapsed time published a background acknowledgment: %#v %v", messages, err)
	}
	events, err := store.ListActivity(t.Context(), ActivityFilter{Scope: original.Scope, RunID: original.ID, EventTypes: []string{"conversation.task_continued"}})
	if err != nil || len(events) != 0 {
		t.Fatalf("elapsed time recorded a task handoff: %#v %v", events, err)
	}
	return current
}

type promotionTurnPublisher struct {
	store *MemoryStore
	calls int
}

func (p *promotionTurnPublisher) PublishTurnOutput(ctx context.Context, run *AgentRun, _ *AgentTurn) (map[string]interface{}, error) {
	p.calls++
	result, err := p.store.PromoteConversationTask(ctx, ConversationTaskPromotionRequest{Scope: run.Scope, RunID: run.ID, Now: time.Now()})
	if err == nil && result == nil {
		err = errors.New("promotion did not occur")
	}
	return map[string]interface{}{"summary": "Action prepared"}, err
}

func TestTurnCoordinatorRetriesCommittedTurnAfterContinuationPromotion(t *testing.T) {
	store := NewMemoryStore()
	run, _, _ := continuationLifecycleFixture(t, store, "turn-cas-continuation")
	run = backdateContinuationLifecycleRun(t, store, run, time.Now().Add(-ConversationTaskForegroundTimeout-time.Second))
	claimed, err := store.ClaimNextAgentRun(t.Context(), AgentRunClaim{Scope: run.Scope, Kind: RunKindConversation, WorkerID: "worker", Now: time.Now(), LeaseDuration: time.Minute, AgingInterval: time.Minute})
	if err != nil || claimed == nil {
		t.Fatalf("claim: %#v %v", claimed, err)
	}
	modelCalls := 0
	publisher := &promotionTurnPublisher{store: store}
	result, err := NewTurnCoordinator(store, store, store).Advance(t.Context(), AdvanceAgentRunRequest{Scope: run.Scope, RunID: run.ID, WorkerID: "worker", OutputPublisher: publisher}, TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
		modelCalls++
		return &TurnOutcome{NextRunStatus: AgentRunStatusRunning, ProposedActions: []TurnAction{{Type: "skill_action", Capability: "release.review", Summary: "Review release"}}, Usage: TurnUsage{InputTokens: 10}}, nil
	}))
	if err != nil || result == nil || result.Run == nil || result.Run.LastAppliedTurn != 1 || len(result.Turn.RequestedActions) != 1 || modelCalls != 1 || publisher.calls != 1 {
		t.Fatalf("finished Turn retry: %#v model=%d publisher=%d err=%v", result, modelCalls, publisher.calls, err)
	}
	task, err := store.FindConversationTaskByWorkRunID(t.Context(), run.Scope, run.ID)
	if err != nil || !ConversationTaskMatchesWorkRun(task, result.Run) {
		t.Fatalf("Turn application lost continuation: %#v %v", task, err)
	}
	turns, err := store.ListAgentTurns(t.Context(), AgentTurnFilter{Scope: run.Scope, RunID: run.ID})
	if err != nil || len(turns) != 1 {
		t.Fatalf("promotion reran the Turn: %#v %v", turns, err)
	}
}

func TestAgentRunWorkerContinuationAbsorbsNativeTaskProposal(t *testing.T) {
	store := NewMemoryStore()
	run, conversation, _ := continuationLifecycleFixture(t, store, "native-continuation")
	run = backdateContinuationLifecycleRun(t, store, run, time.Now().Add(-ConversationTaskForegroundTimeout-time.Second))
	claimed, err := store.ClaimNextAgentRun(t.Context(), AgentRunClaim{Scope: run.Scope, Kind: RunKindConversation, WorkerID: "worker", Now: time.Now(), LeaseDuration: time.Minute, AgingInterval: time.Minute})
	if err != nil || claimed == nil {
		t.Fatalf("claim: %#v %v", claimed, err)
	}
	modelCalls := 0
	pool := workerTaskPool(t, store, run.Scope, TurnRunnerFunc(func(ctx context.Context, input TurnExecutionContext) (*TurnOutcome, error) {
		modelCalls++
		if modelCalls == 1 {
			if _, err := store.PromoteConversationTask(ctx, ConversationTaskPromotionRequest{Scope: run.Scope, RunID: run.ID, Now: time.Now()}); err != nil {
				return nil, err
			}
			return &TurnOutcome{NextRunStatus: AgentRunStatusRunning, ProposedTask: taskProposalFixture()}, nil
		}
		if input.Run.ID != run.ID || input.Turn.Sequence != 2 {
			return nil, errors.New("continuation lost original Run or Turn lineage")
		}
		return &TurnOutcome{NextRunStatus: AgentRunStatusCompleted, RunOutput: map[string]interface{}{"summary": "Release review complete"}}, nil
	}))
	pool.config.MaxTurnsPerClaim = 2
	pool.executeClaim(t.Context(), "worker", claimed)
	finished, err := store.GetAgentRun(t.Context(), run.Scope, run.ID)
	if err != nil || finished.Status != AgentRunStatusCompleted || modelCalls != 2 || finished.LastAppliedTurn != 2 {
		t.Fatalf("native proposal abandoned same-Run continuation: %#v calls=%d err=%v", finished, modelCalls, err)
	}
	work, err := store.ListAgentRuns(t.Context(), AgentRunFilter{Scope: run.Scope, Kind: RunKindAgentWork})
	if err != nil || len(work) != 0 {
		t.Fatalf("redundant proposal created another Run: %#v %v", work, err)
	}
	tasks, err := store.ListConversationTasks(t.Context(), ConversationTaskFilter{Scope: run.Scope, Owner: run.Owner, ConversationID: conversation.ID, Limit: 10})
	if err != nil || len(tasks) != 1 || tasks[0].Task.Mode != ConversationTaskModeContinuation {
		t.Fatalf("redundant proposal duplicated Task: %#v %v", tasks, err)
	}
}

func TestAgentRunWorkerKeepsForegroundDuringSlowTurn(t *testing.T) {
	testAgentRunWorkerKeepsForegroundPastDeadline(t, false)
}

func TestAgentRunWorkerKeepsQueuedForegroundPastDeadlineWithSaturatedLimiter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := NewMemoryStore()
		run, conversation, trigger := continuationLifecycleFixture(t, store, "limited-foreground")
		var modelCalls atomic.Int32
		pool := workerTaskPool(t, store, run.Scope, TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
			modelCalls.Add(1)
			return nil, errors.New("worker should remain queued behind limiter")
		}))
		limiter, err := NewWorkerLimiter(1)
		if err != nil {
			t.Fatal(err)
		}
		release, err := limiter.acquire(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		pool.SetWorkerLimiter(limiter)
		pool.Start(t.Context())
		defer pool.Stop()
		time.Sleep(ConversationTaskForegroundTimeout + 2*time.Second)
		synctest.Wait()
		assertConversationRunStillForeground(t, store, run, conversation, trigger, AgentRunStatusQueued)
		if modelCalls.Load() != 0 {
			t.Fatalf("background maintenance invoked the model %d times behind the limiter", modelCalls.Load())
		}
	})
}

func TestConversationRunReconcilerKeepsAgedRunsForeground(t *testing.T) {
	for _, status := range []AgentRunStatus{AgentRunStatusQueued, AgentRunStatusRunning} {
		t.Run(string(status), func(t *testing.T) {
			store := NewMemoryStore()
			run, conversation, trigger := continuationLifecycleFixture(t, store, "reconcile-foreground")
			run = backdateContinuationLifecycleRun(t, store, run, time.Now().Add(-time.Minute))
			if status == AgentRunStatusRunning {
				claimed, err := store.ClaimNextAgentRun(t.Context(), AgentRunClaim{Scope: run.Scope, Kind: RunKindConversation, WorkerID: "worker", Now: time.Now(), LeaseDuration: time.Minute, AgingInterval: time.Minute})
				if err != nil || claimed == nil {
					t.Fatalf("claim: %#v %v", claimed, err)
				}
			}
			reconciler, err := NewConversationRunReconciler(mustConversationRunScheduler(t, store), WorkerScopeSourceFunc(func(context.Context) ([]Scope, error) {
				return []Scope{run.Scope}, nil
			}), nil, ConversationRunReconcilerConfig{})
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := reconciler.Reconcile(t.Context()); err != nil {
					t.Fatal(err)
				}
				assertConversationRunStillForeground(t, store, run, conversation, trigger, status)
			}
		})
	}
}

func TestAgentRunWorkerStartsIndependentTaskWhenModelChoosesBackground(t *testing.T) {
	for _, age := range []time.Duration{0, time.Minute} {
		t.Run(age.String(), func(t *testing.T) {
			store := NewMemoryStore()
			run, conversation, trigger := continuationLifecycleFixture(t, store, "model-background")
			if age > 0 {
				run = backdateContinuationLifecycleRun(t, store, run, time.Now().Add(-age))
			}
			claimed, err := store.ClaimNextAgentRun(t.Context(), AgentRunClaim{Scope: run.Scope, Kind: RunKindConversation, WorkerID: "worker", Now: time.Now(), LeaseDuration: time.Minute, AgingInterval: time.Minute})
			if err != nil || claimed == nil {
				t.Fatalf("claim: %#v %v", claimed, err)
			}
			modelCalls := 0
			proposal := taskProposalFixture()
			pool := workerTaskPool(t, store, run.Scope, TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
				modelCalls++
				return &TurnOutcome{NextRunStatus: AgentRunStatusRunning, ProposedTask: proposal}, nil
			}))
			pool.executeClaim(t.Context(), "worker", claimed)
			completed, err := store.GetAgentRun(t.Context(), run.Scope, run.ID)
			if err != nil || completed.Status != AgentRunStatusCompleted || completed.Output["summary"] != proposal.Acknowledgment || modelCalls != 1 {
				t.Fatalf("model-selected task did not complete admission: %#v calls=%d err=%v", completed, modelCalls, err)
			}
			turns, err := store.ListAgentTurns(t.Context(), AgentTurnFilter{Scope: run.Scope, RunID: run.ID})
			if err != nil || len(turns) != 1 || turns[0].RequestedTask == nil {
				t.Fatalf("model task decision was not durable: %#v %v", turns, err)
			}
			if _, err := pool.materializeTurnTask(t.Context(), "worker", completed, turns[0], &TurnRunnerBinding{DeploymentID: "agent"}); err != nil {
				t.Fatalf("admission replay: %v", err)
			}
			tasks, err := store.ListConversationTasks(t.Context(), ConversationTaskFilter{Scope: run.Scope, Owner: run.Owner, ConversationID: conversation.ID, Limit: 10})
			if err != nil || len(tasks) != 1 {
				t.Fatalf("model admission or replay duplicated background work: %#v %v", tasks, err)
			}
			task, work := tasks[0].Task, tasks[0].WorkRun
			if task.Mode != ConversationTaskModeIndependent || task.SourceRunID != run.ID || task.WorkRunID == run.ID || task.AuthenticatedActor != trigger.Sender || work.ID != task.WorkRunID || work.Kind != RunKindAgentWork || work.ParentRunID != "" || work.RootRunID != work.ID || work.Status != AgentRunStatusQueued {
				t.Fatalf("duration changed the model's independent task into a continuation: task=%#v work=%#v", task, work)
			}
			messages, err := store.ListChannelMessages(t.Context(), ChannelMessageFilter{Scope: run.Scope, ConversationID: conversation.ID, Limit: 10})
			if err != nil || len(messages) != 2 || messages[0].ID != trigger.ID || messages[1].Content != proposal.Acknowledgment {
				t.Fatalf("background acknowledgment did not commit exactly once: %#v %v", messages, err)
			}
		})
	}
}

type boundedContinuationStore struct {
	runs     []*AgentRun
	filters  []ConversationTaskDueFilter
	promoted []string
}

func (s *boundedContinuationStore) ListDueConversationTaskRuns(_ context.Context, filter ConversationTaskDueFilter) ([]*AgentRun, error) {
	s.filters = append(s.filters, filter)
	result := make([]*AgentRun, 0, filter.Limit)
	for _, run := range s.runs {
		if !run.CreatedAt.After(filter.BeforeCreatedAt) && (filter.AfterCreatedAt == nil || run.CreatedAt.After(*filter.AfterCreatedAt) || run.CreatedAt.Equal(*filter.AfterCreatedAt) && run.ID > filter.AfterID) {
			result = append(result, run)
			if len(result) == filter.Limit {
				break
			}
		}
	}
	return result, nil
}

func (s *boundedContinuationStore) PromoteConversationTask(_ context.Context, req ConversationTaskPromotionRequest) (*ConversationTaskResult, error) {
	s.promoted = append(s.promoted, req.RunID)
	return nil, nil
}

func TestConversationTaskContinuationReconcilerBoundsAndAdvancesPastIneligibleRuns(t *testing.T) {
	now := time.Now().UTC()
	scope := Scope{Kind: "tenant", ID: "busy-continuations"}
	store := &boundedContinuationStore{}
	for i := 0; i < 205; i++ {
		store.runs = append(store.runs, &AgentRun{ID: fmt.Sprintf("run-%03d", i), CreatedAt: now.Add(-time.Minute)})
	}
	reconciler, _ := NewConversationTaskContinuationReconciler(store)
	for range 3 {
		if _, err := reconciler.ReconcileScope(t.Context(), scope, now); err != nil {
			t.Fatal(err)
		}
	}
	if len(store.filters) != 3 || len(store.promoted) != 205 || store.filters[0].AfterCreatedAt != nil || store.filters[1].AfterID != "run-099" || store.filters[2].AfterID != "run-199" {
		t.Fatalf("unbounded or stalled scan: filters=%#v promotions=%d", store.filters, len(store.promoted))
	}
	for _, filter := range store.filters {
		if filter.Limit != 100 {
			t.Fatalf("page size = %d", filter.Limit)
		}
	}
}
