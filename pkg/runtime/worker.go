package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/axiom-studio/openseal/pkg/executor"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

var ErrLeaseLost = errors.New("run lease lost")

// WorkerPool executes persisted runs. The wake channel is only a latency hint;
// the ExecutionStore is the authoritative queue.
type WorkerPool struct {
	pe            *executor.PipelineExecutor
	store         ExecutionStore
	logger        *zap.SugaredLogger
	wake          chan struct{}
	cancel        context.CancelFunc
	retry         *RetryPolicy
	concurrency   int
	leaseDuration time.Duration
	pollInterval  time.Duration
	poolID        string
	wg            sync.WaitGroup
	startOnce     sync.Once
	stopOnce      sync.Once
}

// NewWorkerPool creates a worker pool with the given concurrency.
func NewWorkerPool(
	pe *executor.PipelineExecutor,
	store ExecutionStore,
	logger *zap.SugaredLogger,
	concurrency int,
	retry *RetryPolicy,
) *WorkerPool {
	if concurrency <= 0 {
		concurrency = 4
	}
	if retry == nil {
		retry = DefaultRetryPolicy()
	}
	return &WorkerPool{
		pe:            pe,
		store:         store,
		logger:        logger,
		wake:          make(chan struct{}, 1),
		retry:         retry,
		concurrency:   concurrency,
		leaseDuration: 30 * time.Second,
		pollInterval:  500 * time.Millisecond,
		poolID:        uuid.NewString(),
	}
}

// Start launches the worker goroutines.
func (wp *WorkerPool) Start(ctx context.Context) {
	wp.startOnce.Do(func() {
		workerCtx, cancel := context.WithCancel(ctx)
		wp.cancel = cancel
		for i := 0; i < wp.concurrency; i++ {
			wp.wg.Add(1)
			go wp.worker(workerCtx, fmt.Sprintf("%s-%d", wp.poolID, i))
		}
		wp.Wake()
	})
}

// Stop cancels workers and waits for them to release process resources. Any
// unfinished run becomes reclaimable when its persisted lease expires.
func (wp *WorkerPool) Stop() {
	wp.stopOnce.Do(func() {
		if wp.cancel != nil {
			wp.cancel()
		}
		wp.wg.Wait()
	})
}

// Wake hints that persisted work may be available. Hints may be coalesced;
// workers also poll the durable store, so no run can be dropped.
func (wp *WorkerPool) Wake() {
	select {
	case wp.wake <- struct{}{}:
	default:
	}
}

func (wp *WorkerPool) worker(ctx context.Context, workerID string) {
	defer wp.wg.Done()
	ticker := time.NewTicker(wp.pollInterval)
	defer ticker.Stop()
	for {
		run, err := wp.store.ClaimNextRunnable(ctx, workerID, wp.leaseDuration)
		if err != nil && ctx.Err() == nil {
			wp.logger.Errorw("failed to claim runnable work", "workerId", workerID, "error", err)
		}
		if run != nil {
			wp.execute(ctx, workerID, run)
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-wp.wake:
		case <-ticker.C:
		}
	}
}

func (wp *WorkerPool) execute(ctx context.Context, workerID string, run *RunRecord) {
	runID := run.RunID
	wp.logger.Infow("executing workflow", "runId", runID, "workflow", run.WorkflowName, "workerId", workerID)

	execCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	leaseDone := make(chan struct{})
	go wp.renewLease(execCtx, cancel, leaseDone, runID, workerID)
	defer close(leaseDone)

	// Build a node-update callback that persists to the store
	var nodeResultsCache = make(map[string]*executor.NodeResult)
	onNodeUpdate := func(update *executor.NodeUpdate) {
		if update.Status == "completed" || update.Status == "failed" {
			nr := &executor.NodeResult{
				NodeId:         update.NodeId,
				NodeName:       update.NodeName,
				NodeType:       update.NodeType,
				Status:         update.Status,
				Input:          update.Input,
				Output:         update.Output,
				Error:          update.Error,
				StartedAt:      update.StartedAt,
				CompletedAt:    *update.CompletedAt,
				Duration:       update.Duration,
				ExecutionOrder: len(nodeResultsCache),
			}
			nodeResultsCache[update.NodeId] = nr
			if err := wp.store.UpdateNodeResult(execCtx, runID, workerID, update.NodeId, nr); err != nil {
				wp.logger.Warnw("failed to persist node result", "runId", runID, "nodeId", update.NodeId, "error", err)
				if errors.Is(err, ErrLeaseLost) {
					cancel()
				}
			}
		}
	}

	result, err := wp.pe.ExecuteWithCallback(
		execCtx,
		runID,
		run.Workflow.Nodes,
		run.Workflow.Connections,
		run.Workflow.StartNodeID,
		run.TriggerData,
		nil,
		onNodeUpdate,
		nil,
		nil,
	)

	if err != nil {
		retryCount := run.RetryCount + 1
		if wp.retry.ShouldRetry(retryCount, err) {
			delay := wp.retry.NextDelay(retryCount)
			wp.logger.Infow("scheduling retry", "runId", runID, "retryCount", retryCount, "delay", delay)
			if retryErr := wp.store.ScheduleRetry(ctx, runID, workerID, time.Now().Add(delay), err); retryErr != nil {
				wp.logger.Errorw("failed to persist retry", "runId", runID, "error", retryErr)
			}
			wp.Wake()
			return
		}
		if completeErr := wp.store.CompleteRun(ctx, runID, workerID, RunStatusFailed, err); completeErr != nil {
			wp.logger.Errorw("failed to persist terminal run failure", "runId", runID, "error", completeErr)
		}
		wp.logger.Errorw("workflow execution failed", "runId", runID, "error", err)
		return
	}

	if err := wp.store.CompleteRun(ctx, runID, workerID, result.Status, result.Error); err != nil {
		wp.logger.Errorw("failed to complete run", "runId", runID, "error", err)
		return
	}
	wp.logger.Infow("workflow completed", "runId", runID, "status", result.Status)
}

func (wp *WorkerPool) renewLease(ctx context.Context, cancel context.CancelFunc, done <-chan struct{}, runID int, workerID string) {
	interval := wp.leaseDuration / 3
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-ticker.C:
			if err := wp.store.RenewLease(ctx, runID, workerID, wp.leaseDuration); err != nil {
				wp.logger.Warnw("run lease renewal failed", "runId", runID, "workerId", workerID, "error", err)
				cancel()
				return
			}
		}
	}
}

// SetStore allows swapping the execution store at runtime.
func (wp *WorkerPool) SetStore(store ExecutionStore) {
	wp.store = store
}
