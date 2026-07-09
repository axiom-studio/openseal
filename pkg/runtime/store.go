package runtime

import (
	"context"
	"time"

	"github.com/axiom-studio/openseal/pkg/executor"
)

const (
	RunStatusPending   = "pending"
	RunStatusRunning   = "running"
	RunStatusRetrying  = "retrying"
	RunStatusCompleted = "completed"
	RunStatusFailed    = "failed"
)

// WorkflowEntry is the durable workflow snapshot needed to execute a run.
// Runs persist this value so workers can reconstruct work after a restart.
type WorkflowEntry struct {
	Name        string                           `json:"name"`
	Nodes       []*executor.NodeDefinition       `json:"nodes"`
	Connections []*executor.ConnectionDefinition `json:"connections"`
	StartNodeID string                           `json:"startNodeId"`
}

// RunRecord stores the state of a workflow execution.
type RunRecord struct {
	RunID          int                             `json:"runId"`
	WorkflowName   string                          `json:"workflowName"`
	Workflow       WorkflowEntry                   `json:"workflow"`
	TriggerData    map[string]interface{}          `json:"triggerData,omitempty"`
	Status         string                          `json:"status"`
	NodeResults    map[string]*executor.NodeResult `json:"nodeResults,omitempty"`
	CreatedAt      time.Time                       `json:"createdAt"`
	StartedAt      *time.Time                      `json:"startedAt,omitempty"`
	UpdatedAt      time.Time                       `json:"updatedAt"`
	AvailableAt    time.Time                       `json:"availableAt"`
	LeaseOwner     string                          `json:"leaseOwner,omitempty"`
	LeaseExpiresAt *time.Time                      `json:"leaseExpiresAt,omitempty"`
	CompletedAt    *time.Time                      `json:"completedAt,omitempty"`
	Error          string                          `json:"error,omitempty"`
	RetryCount     int                             `json:"retryCount"`
}

// ExecutionStore persists workflow execution state.
// Implementations must be safe for concurrent use.
type ExecutionStore interface {
	// CreateRun initializes a durable run snapshot and returns its ID.
	CreateRun(ctx context.Context, workflow WorkflowEntry, triggerData map[string]interface{}) (int, error)

	// GetRun retrieves a run by ID.
	GetRun(ctx context.Context, runID int) (*RunRecord, error)

	// ClaimNextRunnable atomically claims one pending, due-retry, or expired-lease
	// run. A nil record means no work is currently eligible.
	ClaimNextRunnable(ctx context.Context, workerID string, leaseDuration time.Duration) (*RunRecord, error)

	// RenewLease extends a lease only when workerID still owns it.
	RenewLease(ctx context.Context, runID int, workerID string, leaseDuration time.Duration) error

	// UpdateNodeResult persists a node result only for the current lease owner.
	UpdateNodeResult(ctx context.Context, runID int, workerID, nodeID string, result *executor.NodeResult) error

	// CompleteRun applies a terminal state only for the current lease owner.
	CompleteRun(ctx context.Context, runID int, workerID, status string, err error) error

	// ScheduleRetry persists retry timing and releases the current lease.
	ScheduleRetry(ctx context.Context, runID int, workerID string, availableAt time.Time, err error) error

	// ListRuns returns runs, newest first.
	ListRuns(ctx context.Context, limit int) ([]*RunRecord, error)
}
