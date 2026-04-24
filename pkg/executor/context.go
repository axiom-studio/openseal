package executor

import (
	"sync"
	"time"
)

// ExecutionContext holds the runtime state during pipeline execution
type ExecutionContext struct {
	RunId            int
	InstanceId       int
	TriggerNodeId    string
	TriggerData      map[string]interface{}
	ResolvedBindings map[string]interface{}

	// Node outputs indexed by node ID
	NodeOutputs map[string]*NodeOutput
	outputMutex sync.RWMutex

	// Variables set by "set" nodes
	Variables map[string]interface{}
	varMutex  sync.RWMutex

	// Webhook response for sync mode
	WebhookResponse *WebhookResponse
	webhookMutex    sync.RWMutex

	// Execution timestamps
	StartedAt   time.Time
	CurrentNode string

	// Completion channel for sync webhook mode
	done     chan struct{}
	doneOnce sync.Once
}

// WebhookResponse holds the response for sync webhook mode
type WebhookResponse struct {
	StatusCode  int
	Headers     map[string]string
	Body        interface{}
	ContentType string
}

// NodeOutput represents the output of a node execution
type NodeOutput struct {
	NodeId      string
	Status      NodeStatus
	Output      interface{}
	Error       error
	StartedAt   time.Time
	CompletedAt time.Time
	Duration    time.Duration
}

// NodeStatus represents the execution status of a node
type NodeStatus string

const (
	NodeStatusPending   NodeStatus = "pending"
	NodeStatusRunning   NodeStatus = "running"
	NodeStatusCompleted NodeStatus = "completed"
	NodeStatusFailed    NodeStatus = "failed"
	NodeStatusSkipped   NodeStatus = "skipped"
)

// NewExecutionContext creates a new execution context
func NewExecutionContext(runId, instanceId int, triggerNodeId string, triggerData map[string]interface{}, bindings map[string]interface{}) *ExecutionContext {
	return &ExecutionContext{
		RunId:            runId,
		InstanceId:       instanceId,
		TriggerNodeId:    triggerNodeId,
		TriggerData:      triggerData,
		ResolvedBindings: bindings,
		NodeOutputs:      make(map[string]*NodeOutput),
		Variables:        make(map[string]interface{}),
		StartedAt:        time.Now(),
		done:             make(chan struct{}),
	}
}

// SetNodeOutput stores the output of a node execution
func (ctx *ExecutionContext) SetNodeOutput(nodeId string, output *NodeOutput) {
	ctx.outputMutex.Lock()
	defer ctx.outputMutex.Unlock()
	ctx.NodeOutputs[nodeId] = output
}

// GetNodeOutput retrieves the output of a node
func (ctx *ExecutionContext) GetNodeOutput(nodeId string) *NodeOutput {
	ctx.outputMutex.RLock()
	defer ctx.outputMutex.RUnlock()
	return ctx.NodeOutputs[nodeId]
}

// SetVariable sets a variable in the execution context
func (ctx *ExecutionContext) SetVariable(name string, value interface{}) {
	ctx.varMutex.Lock()
	defer ctx.varMutex.Unlock()
	ctx.Variables[name] = value
}

// GetVariable gets a variable from the execution context
func (ctx *ExecutionContext) GetVariable(name string) interface{} {
	ctx.varMutex.RLock()
	defer ctx.varMutex.RUnlock()
	return ctx.Variables[name]
}

// SetWebhookResponse sets the webhook response for sync mode
func (ctx *ExecutionContext) SetWebhookResponse(response *WebhookResponse) {
	ctx.webhookMutex.Lock()
	defer ctx.webhookMutex.Unlock()
	ctx.WebhookResponse = response
}

// GetWebhookResponse gets the webhook response
func (ctx *ExecutionContext) GetWebhookResponse() *WebhookResponse {
	ctx.webhookMutex.RLock()
	defer ctx.webhookMutex.RUnlock()
	return ctx.WebhookResponse
}

// Done signals that execution is complete
func (ctx *ExecutionContext) Done() {
	ctx.doneOnce.Do(func() {
		close(ctx.done)
	})
}

// Wait waits for execution to complete
func (ctx *ExecutionContext) Wait() <-chan struct{} {
	return ctx.done
}

// GetInputForNode constructs the input data for a node based on:
// 1. Trigger data (for trigger nodes)
// 2. Previous node outputs (following connections)
// 3. Resolved bindings
// 4. Variables
func (ctx *ExecutionContext) GetInputForNode(nodeId string, graph *ExecutionGraph) map[string]interface{} {
	input := make(map[string]interface{})

	// Add trigger data
	input["trigger"] = ctx.TriggerData

	// Add resolved bindings
	input["bindings"] = ctx.ResolvedBindings

	// Add variables
	ctx.varMutex.RLock()
	input["var"] = ctx.Variables
	ctx.varMutex.RUnlock()

	// Add previous node outputs
	ctx.outputMutex.RLock()
	previousOutputs := make(map[string]interface{})
	for id, output := range ctx.NodeOutputs {
		if output.Status == NodeStatusCompleted {
			previousOutputs[id] = output.Output
		}
	}
	input["nodes"] = previousOutputs
	ctx.outputMutex.RUnlock()

	// Find directly connected previous node output (input from parent)
	graphNode, err := graph.GetNode(nodeId)
	if err == nil && len(graphNode.IncomingEdges) > 0 {
		// Get the first parent's output as the primary input
		parentId := graphNode.IncomingEdges[0].Source
		if parentOutput := ctx.GetNodeOutput(parentId); parentOutput != nil {
			input["input"] = parentOutput.Output
		}
	}

	return input
}
