package runtime

import "context"

// Scheduler coordinates workflow execution by enqueuing work into a WorkerPool.
type Scheduler struct {
	pool    *WorkerPool
	store   ExecutionStore
	nextRunID int
}

// NewScheduler creates a scheduler backed by a worker pool.
func NewScheduler(pool *WorkerPool, store ExecutionStore) *Scheduler {
	return &Scheduler{
		pool:  pool,
		store: store,
	}
}

// Schedule creates a run record and enqueues the workflow for execution.
func (s *Scheduler) Schedule(ctx context.Context, workflow WorkflowEntry, triggerData map[string]interface{}) (int, error) {
	runID, err := s.store.CreateRun(ctx, workflow.Name)
	if err != nil {
		return 0, err
	}

	s.pool.Enqueue(WorkItem{
		RunID:       runID,
		Workflow:    workflow,
		TriggerData: triggerData,
	})
	return runID, nil
}
