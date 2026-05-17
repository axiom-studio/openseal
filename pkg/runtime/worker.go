package runtime

import (
	"context"
	"time"

	"github.com/axiom-studio/openseal/pkg/executor"
	"go.uber.org/zap"
)

// WorkItem represents a unit of work for the worker pool.
type WorkItem struct {
	RunID       int
	Workflow    WorkflowEntry
	TriggerData map[string]interface{}
}

// WorkflowEntry holds the definition needed to execute a workflow.
type WorkflowEntry struct {
	Name        string
	Nodes       []*executor.NodeDefinition
	Connections []*executor.ConnectionDefinition
	StartNodeID string
}

// WorkerPool executes workflows using a pool of goroutines.
type WorkerPool struct {
	pe        *executor.PipelineExecutor
	store     ExecutionStore
	logger    *zap.SugaredLogger
	queue     chan WorkItem
	cancel    context.CancelFunc
	retry     *RetryPolicy
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
		pe:     pe,
		store:  store,
		logger: logger,
		queue:  make(chan WorkItem, 100),
		retry:  retry,
	}
}

// Start launches the worker goroutines.
func (wp *WorkerPool) Start(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	wp.cancel = cancel
	for i := 0; i < cap(wp.queue); i++ {
		go wp.worker(ctx)
	}
}

// Stop drains the queue and stops workers.
func (wp *WorkerPool) Stop() {
	if wp.cancel != nil {
		wp.cancel()
	}
	close(wp.queue)
}

// Enqueue adds a work item to the queue.
func (wp *WorkerPool) Enqueue(item WorkItem) {
	select {
	case wp.queue <- item:
	default:
		wp.logger.Warnw("worker queue full, dropping work item", "runId", item.RunID)
	}
}

func (wp *WorkerPool) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case item, ok := <-wp.queue:
			if !ok {
				return
			}
			wp.execute(ctx, item)
		}
	}
}

func (wp *WorkerPool) execute(ctx context.Context, item WorkItem) {
	runID := item.RunID
	wp.logger.Infow("executing workflow", "runId", runID, "workflow", item.Workflow.Name)

	wp.store.UpdateRunStatus(ctx, runID, "running", nil)

	execCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()

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
			wp.store.UpdateNodeResult(ctx, runID, update.NodeId, nr)
		}
	}

	result, err := wp.pe.ExecuteWithCallback(
		execCtx,
		runID,
		item.Workflow.Nodes,
		item.Workflow.Connections,
		item.Workflow.StartNodeID,
		item.TriggerData,
		nil,
		onNodeUpdate,
		nil,
		nil,
	)

	if err != nil {
		retryCount, _ := wp.store.IncrementRetryCount(ctx, runID)
		if wp.retry.ShouldRetry(retryCount, err) {
			delay := wp.retry.NextDelay(retryCount)
			wp.logger.Infow("scheduling retry", "runId", runID, "retryCount", retryCount, "delay", delay)
			wp.store.UpdateRunStatus(ctx, runID, "retrying", err)
			time.AfterFunc(delay, func() {
				wp.Enqueue(item)
			})
			return
		}
		wp.store.UpdateRunStatus(ctx, runID, "failed", err)
		wp.logger.Errorw("workflow execution failed", "runId", runID, "error", err)
		return
	}

	wp.store.UpdateRunStatus(ctx, runID, result.Status, result.Error)
	wp.logger.Infow("workflow completed", "runId", runID, "status", result.Status)
}

// SetStore allows swapping the execution store at runtime.
func (wp *WorkerPool) SetStore(store ExecutionStore) {
	wp.store = store
}
