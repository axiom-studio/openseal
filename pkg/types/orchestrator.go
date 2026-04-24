package types

import (
	"context"
	"time"
)

// NodeUpdateCallback is called when a node's status changes during test execution
type NodeUpdateCallback func(*TestNodeResult)

// AgentOrchestrator handles pipeline execution for agents
type AgentOrchestrator interface {
	// TriggerAgent starts a new agent run
	TriggerAgent(ctx context.Context, req *TriggerAgentRequest) (*AgentRunBean, error)

	// ExecutePipeline runs the full pipeline for a run
	ExecutePipeline(ctx context.Context, runId int) error

	// GetRun retrieves a run with its step runs
	GetRun(runId int) (*AgentRunBean, error)

	// GetRunsByInstance lists runs for an agent instance
	GetRunsByInstance(instanceId int, limit int) ([]*AgentRunListBean, error)

	// TestWorkflow executes a workflow without deployment for testing in the builder
	TestWorkflow(ctx context.Context, req *TestWorkflowRequest) (*TestWorkflowResponse, error)

	// TestWorkflowWithCallback executes a workflow and calls the callback for each node update
	TestWorkflowWithCallback(ctx context.Context, req *TestWorkflowRequest, onNodeUpdate NodeUpdateCallback) (*TestWorkflowResponse, error)

	// WaitForRunCompletion waits for a run to complete (for synchronous webhook execution)
	WaitForRunCompletion(ctx context.Context, runId int, timeout time.Duration) (*AgentRunBean, error)

	// NotifyRunCompletion notifies any waiters that a run has completed
	NotifyRunCompletion(runId int, status string)

	// GetFileStore returns the file store for serving files
	GetFileStore() FileStore
}

// ExecutionRequest is the message format for agent execution requests
type ExecutionRequest struct {
	RunId          int                    `json:"runId"`
	InstanceId     int                    `json:"instanceId"`
	TriggerData    map[string]interface{} `json:"triggerData"`
	TimeoutSeconds int                    `json:"timeoutSeconds,omitempty"`
	StartNodeId    string                 `json:"startNodeId,omitempty"` // Specific trigger node to start from
	WorkflowId     *int                   `json:"workflowId,omitempty"`  // Specific workflow to execute (null = default)
}

// StatusUpdate is the message format for status updates from runtime
type StatusUpdate struct {
	RunId     int       `json:"runId"`
	Status    string    `json:"status"`
	Error     string    `json:"error,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

// Heartbeat is the message format for heartbeats from runtime
type Heartbeat struct {
	ClusterId   int       `json:"clusterId"`
	WorkerCount int       `json:"workerCount"`
	Version     string    `json:"version"`
	Timestamp   time.Time `json:"timestamp"`
}

// TestWorkflowNATSRequest is the message format for test workflow requests via NATS
type TestWorkflowNATSRequest struct {
	CorrelationId  string               `json:"correlationId"`
	Request        *TestWorkflowRequest `json:"request"`
	TimeoutSeconds int                  `json:"timeoutSeconds,omitempty"`
}

// TestWorkflowNATSUpdate is the message format for node updates during test execution
type TestWorkflowNATSUpdate struct {
	CorrelationId string          `json:"correlationId"`
	NodeResult    *TestNodeResult `json:"nodeResult"`
	IsFinal       bool            `json:"isFinal"`
}

// TestWorkflowNATSResponse is the message format for test workflow completion
type TestWorkflowNATSResponse struct {
	CorrelationId string                `json:"correlationId"`
	Response      *TestWorkflowResponse `json:"response"`
	Error         string                `json:"error,omitempty"`
}

// FileStore is an interface for serving files
type FileStore interface {
	// ServeFile serves a file by path
	ServeFile(path string) ([]byte, error)
}
