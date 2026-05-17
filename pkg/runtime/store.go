package runtime

import (
	"context"
	"time"

	"github.com/axiom-studio/openseal/pkg/executor"
)

// RunRecord stores the state of a workflow execution.
type RunRecord struct {
	RunID        int                      `json:"runId"`
	WorkflowName string                   `json:"workflowName"`
	Status       string                   `json:"status"` // pending, running, completed, failed
	NodeResults  map[string]*executor.NodeResult `json:"nodeResults,omitempty"`
	StartedAt    time.Time                `json:"startedAt"`
	CompletedAt  *time.Time               `json:"completedAt,omitempty"`
	Error        string                   `json:"error,omitempty"`
	RetryCount   int                      `json:"retryCount"`
}

// ExecutionStore persists workflow execution state.
// Implementations must be safe for concurrent use.
type ExecutionStore interface {
	// CreateRun initializes a new run record and returns its ID.
	CreateRun(ctx context.Context, workflowName string) (int, error)

	// GetRun retrieves a run by ID.
	GetRun(ctx context.Context, runID int) (*RunRecord, error)

	// UpdateNodeResult persists the result of a single node execution.
	UpdateNodeResult(ctx context.Context, runID int, nodeID string, result *executor.NodeResult) error

	// UpdateRunStatus updates the overall run status.
	UpdateRunStatus(ctx context.Context, runID int, status string, err error) error

	// IncrementRetryCount bumps the retry counter.
	IncrementRetryCount(ctx context.Context, runID int) (int, error)

	// ListRuns returns runs, newest first.
	ListRuns(ctx context.Context, limit int) ([]*RunRecord, error)
}
