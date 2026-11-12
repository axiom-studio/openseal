package runtime

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation/conversationtest"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun/conversationruntest"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
	"github.com/axiom-studio/openseal/pkg/runtime/workqueue"
)

func continuationLifecycleFixture(t *testing.T, store *sqlite.Store, id string) (*models.AgentRun, *models.Conversation, *models.ChannelMessage) {
	t.Helper()
	service := runtimeconversation.NewConversationService(store)
	scope := models.Scope{Kind: "tenant", ID: id}
	conversationtest.SeedSpace(t, store, scope)
	conversation, _, err := service.CreateConversation(t.Context(), runtimeconversation.CreateConversationRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, Title: "Assistant", IdempotencyKey: id,
	})
	if err != nil {
		t.Fatal(err)
	}
	message := conversationruntest.PostMessage(t, service, conversation, models.ConversationParticipantUser, models.MessageIntentQuestion, "Review the release", id+"-message")
	result, _, err := conversationruntest.NewScheduler(t, store).ScheduleMessage(t.Context(), scope, conversation.ID, message.ID)
	if err != nil || result == nil || result.Run == nil {
		t.Fatalf("schedule conversation: %#v %v", result, err)
	}
	return result.Run, conversation, message
}

// backdateContinuationLifecycleRun makes the Run look created at createdAt.
// No store method changes a Run's creation time, so it rewrites the row on
// db, the test's own connection to the store's file.
func backdateContinuationLifecycleRun(t *testing.T, store *sqlite.Store, db *sql.DB, run *models.AgentRun, createdAt time.Time) *models.AgentRun {
	t.Helper()
	storetest.ExecSQL(t, db, `UPDATE agent_runs SET created_at = ? WHERE scope_kind = ? AND scope_id = ? AND id = ?`, createdAt, run.Scope.Kind, run.Scope.ID, run.ID)
	storetest.RewriteRowPayload(t, db, "agent_runs", run.Scope, run.ID, func(stored *models.AgentRun) { stored.CreatedAt = createdAt })
	return workerStoredRun(t, store, run.Scope, run.ID)
}

func TestConversationTaskContinuationReconcilesQueuedRunFromCreationTime(t *testing.T) {
	store, db := newWorkerSQLiteStore(t)
	run, conversation, _ := continuationLifecycleFixture(t, store, "queued-continuation")
	now := time.Now().UTC()
	run = backdateContinuationLifecycleRun(t, store, db, run, now.Add(-models.ConversationTaskForegroundTimeout))
	// A fresh activity timestamp is not a new foreground deadline.
	storetest.RewriteRowPayload(t, db, "agent_runs", run.Scope, run.ID, func(stored *models.AgentRun) { stored.UpdatedAt = now })
	reconciler, err := conversationrun.NewConversationTaskContinuationReconciler(store)
	if err != nil {
		t.Fatal(err)
	}
	results, err := reconciler.ReconcileScope(t.Context(), run.Scope, now)
	if err != nil || len(results) != 1 || results[0].WorkRun.ID != run.ID || results[0].WorkRun.Status != models.AgentRunStatusQueued {
		t.Fatalf("queued continuation: %#v %v", results, err)
	}
	if !models.ConversationTaskMatchesWorkRun(results[0].Task, results[0].WorkRun) {
		t.Fatal("promotion did not retain canonical same-Run identity")
	}
	foreground, err := store.ListConversationForegroundRuns(t.Context(), run.Scope, run.Owner, conversation.ID, 100, 0)
	if err != nil || len(foreground) != 0 {
		t.Fatalf("promoted Run remained in foreground restore: %#v %v", foreground, err)
	}
	restarted, _ := conversationrun.NewConversationTaskContinuationReconciler(store)
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
		store := conversationtest.NewStore(t)
		run, conversation, trigger := continuationLifecycleFixture(t, store, "slow-foreground")
		claimed, err := store.ClaimNextAgentRun(t.Context(), models.AgentRunClaim{Scope: run.Scope, Kind: models.RunKindConversation, WorkerID: "worker", Now: time.Now(), LeaseDuration: time.Minute, AgingInterval: time.Minute})
		if err != nil || claimed == nil {
			t.Fatalf("claim: %#v %v", claimed, err)
		}
		entered, release, done := make(chan context.Context, 1), make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		closeRelease := func() { releaseOnce.Do(func() { close(release) }) }
		var modelCalls atomic.Int32
		producer := &workerTaskProducer{reply: func(ctx context.Context, _ int, _ agentrun.StepInput) (agentrun.StepOutput, error) {
			modelCalls.Add(1)
			if !resolving {
				entered <- ctx
				<-release
			}
			return answerStep("Reviewed release"), ctx.Err()
		}}
		resolve := workerTaskResolver(t, store, store, producer)
		pool := workerTaskPoolWith(t, store, run.Scope, runtimeconversation.TurnRunnerResolverFunc(func(ctx context.Context, run *models.AgentRun) (*agentrun.TurnRunnerBinding, error) {
			if resolving {
				entered <- ctx
				<-release
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return resolve(ctx, run)
		}))
		go func() { defer close(done); pool.executeClaim(t.Context(), "worker", claimed) }()
		defer func() { closeRelease(); <-done }()
		invocationCtx := <-entered
		// Advance the real worker's timers without a wall-clock sleep. Both
		// runner resolution and an active model Turn cross the former deadline.
		time.Sleep(models.ConversationTaskForegroundTimeout + time.Second)
		synctest.Wait()
		current := assertConversationRunStillForeground(t, store, run, conversation, trigger, models.AgentRunStatusRunning)
		if current.LeaseOwner != "worker" || invocationCtx.Err() != nil {
			t.Fatalf("elapsed time interrupted the original invocation: %#v ctx=%v", current, invocationCtx.Err())
		}
		turns, err := store.ListAgentTurns(t.Context(), models.AgentTurnFilter{Scope: run.Scope, RunID: run.ID})
		if err != nil {
			t.Fatal(err)
		}
		if resolving && (len(turns) != 0 || modelCalls.Load() != 0) || !resolving && (len(turns) != 1 || turns[0].Status != models.AgentTurnStatusRunning || turns[0].LeaseOwner != "worker" || modelCalls.Load() != 1) {
			t.Fatalf("elapsed time restarted work: turns=%#v calls=%d", turns, modelCalls.Load())
		}
		closeRelease()
		<-done
		finished, err := store.GetAgentRun(t.Context(), run.Scope, run.ID)
		if err != nil || finished.Status != models.AgentRunStatusCompleted || finished.LastAppliedTurn != 1 || finished.Context[models.ConversationTaskContextKey] != nil || finished.Output["summary"] != "Reviewed release" || modelCalls.Load() != 1 {
			t.Fatalf("foreground invocation did not finish once: %#v calls=%d err=%v", finished, modelCalls.Load(), err)
		}
	})
}

func assertConversationRunStillForeground(t *testing.T, store *sqlite.Store, original *models.AgentRun, conversation *models.Conversation, trigger *models.ChannelMessage, status models.AgentRunStatus) *models.AgentRun {
	t.Helper()
	current, err := store.GetAgentRun(t.Context(), original.Scope, original.ID)
	if err != nil || current == nil || current.Status != status || current.Kind != models.RunKindConversation || current.ConcurrencyKey != original.ConcurrencyKey || current.Context[models.ConversationTaskContextKey] != nil {
		t.Fatalf("elapsed time changed the foreground execution lane: %#v %v", current, err)
	}
	if task, err := store.FindConversationTaskByWorkRunID(t.Context(), original.Scope, original.ID); err != nil || task != nil {
		t.Fatalf("elapsed time created a background task: %#v %v", task, err)
	}
	foreground, err := store.ListConversationForegroundRuns(t.Context(), original.Scope, original.Owner, conversation.ID, 100, 0)
	if err != nil || len(foreground) != 1 || foreground[0].ID != original.ID {
		t.Fatalf("aged run disappeared from foreground restore: %#v %v", foreground, err)
	}
	messages, err := store.ListChannelMessages(t.Context(), models.ChannelMessageFilter{Scope: original.Scope, ConversationID: conversation.ID, Limit: 10})
	if err != nil || len(messages) != 1 || messages[0].ID != trigger.ID {
		t.Fatalf("elapsed time published a background acknowledgment: %#v %v", messages, err)
	}
	events, err := store.ListActivity(t.Context(), models.ActivityFilter{Scope: original.Scope, RunID: original.ID, EventTypes: []string{"conversation.task_continued"}})
	if err != nil || len(events) != 0 {
		t.Fatalf("elapsed time recorded a task handoff: %#v %v", events, err)
	}
	return current
}

type promotionTurnPublisher struct {
	store *sqlite.Store
	calls int
}

func (p *promotionTurnPublisher) PublishTurnOutput(ctx context.Context, run *models.AgentRun, _ *models.AgentTurn) (map[string]interface{}, error) {
	p.calls++
	result, err := p.store.PromoteConversationTask(ctx, models.ConversationTaskPromotionRequest{Scope: run.Scope, RunID: run.ID, Now: time.Now()})
	if err == nil && result == nil {
		err = errors.New("promotion did not occur")
	}
	return map[string]interface{}{"summary": "Action prepared"}, err
}

func TestAgentRunWorkerContinuationAbsorbsNativeTaskProposal(t *testing.T) {
	store, db := newWorkerSQLiteStore(t)
	run, conversation, _ := continuationLifecycleFixture(t, store, "native-continuation")
	run = backdateContinuationLifecycleRun(t, store, db, run, time.Now().Add(-models.ConversationTaskForegroundTimeout-time.Second))
	claimed, err := store.ClaimNextAgentRun(t.Context(), models.AgentRunClaim{Scope: run.Scope, Kind: models.RunKindConversation, WorkerID: "worker", Now: time.Now(), LeaseDuration: time.Minute, AgingInterval: time.Minute})
	if err != nil || claimed == nil {
		t.Fatalf("claim: %#v %v", claimed, err)
	}
	modelCalls := 0
	proposal := workerTaskProposal()
	pool := workerTaskPool(t, store, run.Scope, &workerTaskProducer{reply: func(ctx context.Context, call int, in agentrun.StepInput) (agentrun.StepOutput, error) {
		modelCalls = call
		if call == 1 {
			if _, err := store.PromoteConversationTask(ctx, models.ConversationTaskPromotionRequest{Scope: run.Scope, RunID: run.ID, Now: time.Now()}); err != nil {
				return agentrun.StepOutput{}, err
			}
			return startTaskStep(proposal.goal, proposal.ack), nil
		}
		if in.Run.ID != run.ID || in.Step.Sequence != 2 {
			return agentrun.StepOutput{}, errors.New("continuation lost original Run or step lineage")
		}
		return answerStep("Release review complete"), nil
	}})
	pool.executeClaim(t.Context(), "worker", claimed)
	finished, err := store.GetAgentRun(t.Context(), run.Scope, run.ID)
	if err != nil || finished.Status != models.AgentRunStatusCompleted || modelCalls != 2 || finished.LastAppliedTurn != 2 {
		t.Fatalf("native proposal abandoned same-Run continuation: %#v calls=%d err=%v", finished, modelCalls, err)
	}
	work, err := store.ListAgentRuns(t.Context(), models.AgentRunFilter{Scope: run.Scope, Kind: models.RunKindAgentWork})
	if err != nil || len(work) != 0 {
		t.Fatalf("redundant proposal created another Run: %#v %v", work, err)
	}
	tasks, err := store.ListConversationTasks(t.Context(), models.ConversationTaskFilter{Scope: run.Scope, Owner: run.Owner, ConversationID: conversation.ID, Limit: 10})
	if err != nil || len(tasks) != 1 || tasks[0].Task.Mode != models.ConversationTaskModeContinuation {
		t.Fatalf("redundant proposal duplicated Task: %#v %v", tasks, err)
	}
}

func TestAgentRunWorkerKeepsForegroundDuringSlowTurn(t *testing.T) {
	testAgentRunWorkerKeepsForegroundPastDeadline(t, false)
}

func TestAgentRunWorkerKeepsQueuedForegroundPastDeadlineWithSaturatedLimiter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := conversationtest.NewStore(t)
		run, conversation, trigger := continuationLifecycleFixture(t, store, "limited-foreground")
		var modelCalls atomic.Int32
		pool := workerTaskPool(t, store, run.Scope, &workerTaskProducer{reply: func(context.Context, int, agentrun.StepInput) (agentrun.StepOutput, error) {
			modelCalls.Add(1)
			return agentrun.StepOutput{}, errors.New("worker should remain queued behind limiter")
		}})
		limiter, err := workqueue.NewWorkerLimiter(1)
		if err != nil {
			t.Fatal(err)
		}
		release, err := limiter.Acquire(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		pool.SetWorkerLimiter(limiter)
		pool.Start(t.Context())
		defer pool.Stop()
		time.Sleep(models.ConversationTaskForegroundTimeout + 2*time.Second)
		synctest.Wait()
		assertConversationRunStillForeground(t, store, run, conversation, trigger, models.AgentRunStatusQueued)
		if modelCalls.Load() != 0 {
			t.Fatalf("background maintenance invoked the model %d times behind the limiter", modelCalls.Load())
		}
	})
}

func TestConversationRunReconcilerKeepsAgedRunsForeground(t *testing.T) {
	for _, status := range []models.AgentRunStatus{models.AgentRunStatusQueued, models.AgentRunStatusRunning} {
		t.Run(string(status), func(t *testing.T) {
			store, db := newWorkerSQLiteStore(t)
			run, conversation, trigger := continuationLifecycleFixture(t, store, "reconcile-foreground")
			run = backdateContinuationLifecycleRun(t, store, db, run, time.Now().Add(-time.Minute))
			if status == models.AgentRunStatusRunning {
				claimed, err := store.ClaimNextAgentRun(t.Context(), models.AgentRunClaim{Scope: run.Scope, Kind: models.RunKindConversation, WorkerID: "worker", Now: time.Now(), LeaseDuration: time.Minute, AgingInterval: time.Minute})
				if err != nil || claimed == nil {
					t.Fatalf("claim: %#v %v", claimed, err)
				}
			}
			reconciler, err := conversationrun.NewConversationRunReconciler(conversationruntest.NewScheduler(t, store), workqueue.WorkerScopeSourceFunc(func(context.Context) ([]models.Scope, error) {
				return []models.Scope{run.Scope}, nil
			}), nil, conversationrun.ConversationRunReconcilerConfig{})
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

type boundedContinuationStore struct {
	runs     []*models.AgentRun
	filters  []models.ConversationTaskDueFilter
	promoted []string
}

func (s *boundedContinuationStore) ListDueConversationTaskRuns(_ context.Context, filter models.ConversationTaskDueFilter) ([]*models.AgentRun, error) {
	s.filters = append(s.filters, filter)
	result := make([]*models.AgentRun, 0, filter.Limit)
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

func (s *boundedContinuationStore) PromoteConversationTask(_ context.Context, req models.ConversationTaskPromotionRequest) (*models.ConversationTaskResult, error) {
	s.promoted = append(s.promoted, req.RunID)
	return nil, nil
}

func TestConversationTaskContinuationReconcilerBoundsAndAdvancesPastIneligibleRuns(t *testing.T) {
	now := time.Now().UTC()
	scope := models.Scope{Kind: "tenant", ID: "busy-continuations"}
	store := &boundedContinuationStore{}
	for i := 0; i < 205; i++ {
		store.runs = append(store.runs, &models.AgentRun{ID: fmt.Sprintf("run-%03d", i), CreatedAt: now.Add(-time.Minute)})
	}
	reconciler, _ := conversationrun.NewConversationTaskContinuationReconciler(store)
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
