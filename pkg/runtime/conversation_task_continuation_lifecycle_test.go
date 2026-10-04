package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
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

func TestAgentRunWorkerContinuationDeadlineIncludesRunnerResolution(t *testing.T) {
	store := NewMemoryStore()
	run, conversation, _ := continuationLifecycleFixture(t, store, "resolution-continuation")
	run = backdateContinuationLifecycleRun(t, store, run, time.Now().Add(-ConversationTaskForegroundTimeout+40*time.Millisecond))
	claimed, err := store.ClaimNextAgentRun(t.Context(), AgentRunClaim{Scope: run.Scope, Kind: RunKindConversation, WorkerID: "worker", Now: time.Now(), LeaseDuration: time.Minute, AgingInterval: time.Minute})
	if err != nil || claimed == nil {
		t.Fatalf("claim: %#v %v", claimed, err)
	}
	entered, release, done := make(chan context.Context, 1), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	closeRelease := func() { releaseOnce.Do(func() { close(release) }) }
	defer closeRelease()
	var modelCalls atomic.Int32
	pool, err := NewAgentRunWorkerPool(store, TurnRunnerResolverFunc(func(ctx context.Context, _ *AgentRun) (*TurnRunnerBinding, error) {
		entered <- ctx
		<-release
		return &TurnRunnerBinding{DeploymentID: "agent", Runner: TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
			modelCalls.Add(1)
			return &TurnOutcome{NextRunStatus: AgentRunStatusCompleted, RunOutput: map[string]interface{}{"summary": "Reviewed release"}}, nil
		})}, nil
	}), nil, AgentRunWorkerConfig{Scope: run.Scope, Kind: RunKindConversation})
	if err != nil {
		t.Fatal(err)
	}
	go func() { defer close(done); pool.executeClaim(t.Context(), "worker", claimed) }()
	invocationCtx := <-entered
	var task *ConversationTask
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		task, err = store.FindConversationTaskByWorkRunID(t.Context(), run.Scope, run.ID)
		if err != nil || task != nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err != nil || task == nil {
		closeRelease()
		<-done
		t.Fatalf("resolution exceeded deadline without promotion: %#v %v", task, err)
	}
	current, err := store.GetAgentRun(t.Context(), run.Scope, run.ID)
	if err != nil || current.LeaseOwner != "worker" || current.Status != AgentRunStatusRunning || invocationCtx.Err() != nil {
		closeRelease()
		<-done
		t.Fatalf("promotion interrupted original invocation: %#v %v ctx=%v", current, err, invocationCtx.Err())
	}
	service := NewConversationService(store)
	conversation, err = service.GetConversation(t.Context(), run.Scope, conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	next := postConversationRunTestMessage(t, service, conversation, ConversationParticipantUser, MessageIntentQuestion, "What else is new?", "resolution-next-message")
	if _, _, err := mustConversationRunScheduler(t, store).ScheduleMessage(t.Context(), run.Scope, conversation.ID, next.ID); err != nil {
		t.Fatal(err)
	}
	current, err = store.GetAgentRun(t.Context(), run.Scope, run.ID)
	if err != nil || current.Status != AgentRunStatusRunning || invocationCtx.Err() != nil {
		t.Fatalf("new message superseded continuation: %#v %v", current, err)
	}
	closeRelease()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("same invocation did not finish")
	}
	finished, err := store.GetAgentRun(t.Context(), run.Scope, run.ID)
	if err != nil || finished.Status != AgentRunStatusCompleted || modelCalls.Load() != 1 || !ConversationTaskMatchesWorkRun(task, finished) {
		t.Fatalf("same-Run completion: %#v calls=%d err=%v", finished, modelCalls.Load(), err)
	}
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

func TestAgentRunWorkerContinuationKeepsInflightTurnAlive(t *testing.T) {
	store := NewMemoryStore()
	run, _, _ := continuationLifecycleFixture(t, store, "inflight-continuation")
	run = backdateContinuationLifecycleRun(t, store, run, time.Now().Add(-ConversationTaskForegroundTimeout+100*time.Millisecond))
	claimed, err := store.ClaimNextAgentRun(t.Context(), AgentRunClaim{Scope: run.Scope, Kind: RunKindConversation, WorkerID: "worker", Now: time.Now(), LeaseDuration: time.Minute, AgingInterval: time.Minute})
	if err != nil || claimed == nil {
		t.Fatalf("claim: %#v %v", claimed, err)
	}
	entered, release, done := make(chan context.Context, 1), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	closeRelease := func() { releaseOnce.Do(func() { close(release) }) }
	defer closeRelease()
	var calls atomic.Int32
	pool := workerTaskPool(t, store, run.Scope, TurnRunnerFunc(func(ctx context.Context, input TurnExecutionContext) (*TurnOutcome, error) {
		calls.Add(1)
		entered <- ctx
		<-release
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return &TurnOutcome{NextRunStatus: AgentRunStatusCompleted, RunOutput: map[string]interface{}{"summary": "Completed original invocation"}}, nil
	}))
	go func() { defer close(done); pool.executeClaim(t.Context(), "worker", claimed) }()
	invocationCtx := <-entered
	deadline := time.Now().Add(2 * time.Second)
	var task *ConversationTask
	for time.Now().Before(deadline) {
		task, err = store.FindConversationTaskByWorkRunID(t.Context(), run.Scope, run.ID)
		if err != nil || task != nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err != nil || task == nil {
		closeRelease()
		<-done
		t.Fatalf("inflight Turn was not promoted: %#v %v", task, err)
	}
	turns, err := store.ListAgentTurns(t.Context(), AgentTurnFilter{Scope: run.Scope, RunID: run.ID})
	if err != nil || len(turns) != 1 || turns[0].Status != AgentTurnStatusRunning || turns[0].LeaseOwner != "worker" || invocationCtx.Err() != nil || calls.Load() != 1 {
		t.Fatalf("promotion interrupted current Turn: %#v calls=%d ctx=%v err=%v", turns, calls.Load(), invocationCtx.Err(), err)
	}
	closeRelease()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("inflight Turn did not finish")
	}
	finished, err := store.GetAgentRun(t.Context(), run.Scope, run.ID)
	if err != nil || finished.Status != AgentRunStatusCompleted || finished.LastAppliedTurn != 1 || calls.Load() != 1 || !ConversationTaskMatchesWorkRun(task, finished) {
		t.Fatalf("inflight continuation restarted execution: %#v calls=%d err=%v", finished, calls.Load(), err)
	}
}

func TestAgentRunWorkerContinuationScanSurvivesSaturatedLimiter(t *testing.T) {
	store := NewMemoryStore()
	run, _, _ := continuationLifecycleFixture(t, store, "limited-continuation")
	run = backdateContinuationLifecycleRun(t, store, run, time.Now().Add(-ConversationTaskForegroundTimeout-time.Second))
	pool := workerTaskPool(t, store, run.Scope, TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
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
	deadline := time.Now().Add(2 * time.Second)
	var task *ConversationTask
	for time.Now().Before(deadline) {
		task, err = store.FindConversationTaskByWorkRunID(t.Context(), run.Scope, run.ID)
		if err != nil || task != nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err != nil || task == nil {
		t.Fatalf("limiter prevented durable deadline reconciliation: %#v %v", task, err)
	}
	current, err := store.GetAgentRun(t.Context(), run.Scope, run.ID)
	if err != nil || current.Status != AgentRunStatusQueued || current.ID != task.WorkRunID {
		t.Fatalf("deadline scan dispatched blocked work: %#v %v", current, err)
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
