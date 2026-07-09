package executor

import (
	"context"
	"fmt"
	"sync"
	"time"

	sdkResolver "github.com/axiom-studio/skills.sdk/resolver"
	"go.uber.org/zap"
)

// PipelineExecutor executes agent workflows
type PipelineExecutor struct {
	registry    *Registry
	logger      *zap.SugaredLogger
	emitBaseURL string // Base URL for streaming emit endpoint
}

// NewPipelineExecutor creates a new pipeline executor
func NewPipelineExecutor(registry *Registry, logger *zap.SugaredLogger) *PipelineExecutor {
	return &PipelineExecutor{
		registry: registry,
		logger:   logger,
	}
}

// SetEmitBaseURL sets the base URL for the streaming emit endpoint
// This should be called before Execute if streaming between nodes is desired
func (pe *PipelineExecutor) SetEmitBaseURL(url string) {
	pe.emitBaseURL = url
}

// ExecutionResult represents the result of a pipeline execution
type ExecutionResult struct {
	RunId       int
	Status      string
	NodeResults map[string]*NodeResult
	StartedAt   time.Time
	CompletedAt time.Time
	Error       error
}

// NodeResult represents the result of a single node execution
type NodeResult struct {
	NodeId         string
	NodeName       string
	NodeType       string
	Status         string
	Input          interface{}
	Output         interface{}
	Error          string
	StartedAt      time.Time
	CompletedAt    time.Time
	Duration       time.Duration
	ExecutionOrder int
}

// NodeUpdate contains information about a node's current state during execution
type NodeUpdate struct {
	NodeId      string
	NodeName    string
	NodeType    string
	Status      string // pending, running, streaming, completed, failed, skipped
	Input       interface{}
	Output      interface{} // Final output (when completed)
	Partial     interface{} // Incremental output chunk (when streaming)
	Progress    float64     // 0.0-1.0 progress indicator (when streaming)
	Error       string
	StartedAt   time.Time
	CompletedAt *time.Time
	Duration    time.Duration // Only set when completed/failed
}

// NodeUpdateFn is called when a node's status changes or emits streaming data
type NodeUpdateFn func(update *NodeUpdate)

// Execute runs a pipeline starting from the specified trigger node
func (pe *PipelineExecutor) Execute(
	ctx context.Context,
	runId int,
	nodes []*NodeDefinition,
	connections []*ConnectionDefinition,
	startNodeId string,
	triggerData map[string]interface{},
	bindings map[string]interface{},
) (*ExecutionResult, error) {
	return pe.ExecuteWithCallback(ctx, runId, nodes, connections, startNodeId, triggerData, bindings, nil, nil, nil)
}

// ExecuteWithCallback runs a pipeline and calls the callback for each node status change
func (pe *PipelineExecutor) ExecuteWithCallback(
	ctx context.Context,
	runId int,
	nodes []*NodeDefinition,
	connections []*ConnectionDefinition,
	startNodeId string,
	triggerData map[string]interface{},
	bindings map[string]interface{},
	onNodeUpdate NodeUpdateFn,
	previousNodeOutputs map[string]interface{},
	previousNodeMetadata map[string]map[string]interface{},
) (*ExecutionResult, error) {

	result := &ExecutionResult{
		RunId:       runId,
		Status:      "running",
		NodeResults: make(map[string]*NodeResult),
		StartedAt:   time.Now(),
	}

	// Build the execution graph (internal)
	graph, err := pe.buildGraph(nodes, connections)
	if err != nil {
		result.Status = "failed"
		result.Error = err
		result.CompletedAt = time.Now()
		return result, err
	}

	// Build the public execution graph for tool discovery
	execGraph, err := BuildGraph(nodes, connections)
	if err != nil {
		// Non-fatal: tool discovery won't work but execution can continue
		pe.logger.Warnw("failed to build public execution graph for tool discovery", "error", err)
	}

	// Create execution context
	var resultMu sync.Mutex
	execCtx := &executionContext{
		triggerData:  triggerData,
		bindings:     bindings,
		nodeOutputs:  make(map[string]interface{}),
		nodeMetadata: make(map[string]map[string]interface{}),
		variables:    make(map[string]interface{}),
		runMetadata: map[string]interface{}{
			"id":          runId,
			"triggeredBy": "test",
			"startedAt":   result.StartedAt.Format(time.RFC3339),
		},
		onNodeUpdate:    onNodeUpdate,
		emitBaseURL:     pe.emitBaseURL,
		execGraph:       execGraph,
		resultMu:        &resultMu,
		joinCompletions: make(map[string]int),
		joinExecuted:    make(map[string]bool),
	}

	// If this is a resume operation, pre-populate with previous state
	if len(previousNodeOutputs) > 0 {
		for nodeId, output := range previousNodeOutputs {
			execCtx.nodeOutputs[nodeId] = output
		}
		pe.logger.Infow("resuming with previous node outputs", "count", len(previousNodeOutputs))
	}
	if len(previousNodeMetadata) > 0 {
		for nodeId, metadata := range previousNodeMetadata {
			execCtx.nodeMetadata[nodeId] = metadata
		}
		pe.logger.Infow("resuming with previous node metadata", "count", len(previousNodeMetadata))
	}

	// Get the start node
	startNode, ok := graph.nodes[startNodeId]
	if !ok {
		err := fmt.Errorf("start node not found: %s", startNodeId)
		result.Status = "failed"
		result.Error = err
		result.CompletedAt = time.Now()
		return result, err
	}

	// Execute starting from the trigger node
	if err := pe.executeNode(ctx, startNode, graph, execCtx, result, ""); err != nil {
		result.Status = "failed"
		result.Error = err
		result.CompletedAt = time.Now()
		return result, err
	}

	result.Status = "completed"
	result.CompletedAt = time.Now()
	return result, nil
}

// executionContext holds runtime state during execution
type executionContext struct {
	triggerData     map[string]interface{}
	bindings        map[string]interface{}
	nodeOutputs     map[string]interface{}
	nodeMetadata    map[string]map[string]interface{} // Node execution metadata (duration, status, etc.)
	variables       map[string]interface{}
	runMetadata     map[string]interface{}
	onNodeUpdate    NodeUpdateFn
	executionOrder  int
	emitBaseURL     string          // Base URL for the configured streaming emit endpoint.
	execGraph       *ExecutionGraph // Public execution graph for tool discovery
	mu              sync.Mutex      // Protects concurrent access to shared state
	resultMu        *sync.Mutex     // Protects concurrent access to result.NodeResults
	joinCompletions map[string]int  // Track how many branches have reached each join node
	joinExecuted    map[string]bool // Track which join nodes have executed
	joinMu          sync.Mutex      // Protects join tracking maps
}

// graphNode represents a node in the execution graph
type graphNode struct {
	definition *NodeDefinition
	outgoing   []*graphEdge
	incoming   []*graphEdge // Track incoming edges for parent node lookup
}

// graphEdge represents an edge in the execution graph
type graphEdge struct {
	source       *graphNode // Source node (for incoming edges)
	target       *graphNode // Target node (for outgoing edges)
	sourceHandle string
	label        string
}

// executionGraph represents the workflow graph
type executionGraph struct {
	nodes map[string]*graphNode
}

// buildGraph constructs an execution graph from nodes and connections
func (pe *PipelineExecutor) buildGraph(nodes []*NodeDefinition, connections []*ConnectionDefinition) (*executionGraph, error) {
	graph := &executionGraph{
		nodes: make(map[string]*graphNode),
	}

	// Add all nodes
	for _, n := range nodes {
		graph.nodes[n.Id] = &graphNode{
			definition: n,
			outgoing:   make([]*graphEdge, 0),
			incoming:   make([]*graphEdge, 0),
		}
	}

	// Add all connections
	for _, c := range connections {
		sourceNode, ok := graph.nodes[c.SourceNodeId]
		if !ok {
			continue
		}
		targetNode, ok := graph.nodes[c.TargetNodeId]
		if !ok {
			continue
		}

		edge := &graphEdge{
			source:       sourceNode,
			target:       targetNode,
			sourceHandle: c.SourceHandle,
			label:        c.Label,
		}
		sourceNode.outgoing = append(sourceNode.outgoing, edge)
		targetNode.incoming = append(targetNode.incoming, edge)
	}

	return graph, nil
}

// executeNode executes a single node and follows its outgoing connections
// sourceNodeName indicates which parent node triggered this execution (used for join stream mode)
func (pe *PipelineExecutor) executeNode(
	ctx context.Context,
	node *graphNode,
	graph *executionGraph,
	execCtx *executionContext,
	result *ExecutionResult,
	sourceNodeName string,
) error {
	// Check for context cancellation
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	// Handle explicit join nodes with internal synchronization
	if node.definition.Type == NodeTypeJoin && len(node.incoming) > 1 {
		// Get join mode from config (default: "all")
		mode := "all"
		if node.definition.Config != nil {
			if m, ok := node.definition.Config["mode"].(string); ok && m != "" {
				mode = m
			}
		}

		// Pass Through mode (stream): no synchronization, execute for each branch independently
		if mode == "stream" {
			pe.logger.Debugw("join node in pass-through mode, executing for each branch",
				"nodeId", node.definition.Id,
				"nodeName", node.definition.Name)
			// Continue to execute without synchronization
			// Each branch will execute join independently
		} else {
			// All other modes use synchronization
			execCtx.joinMu.Lock()

			// Check if already executed
			alreadyExecuted := execCtx.joinExecuted[node.definition.Id]
			if alreadyExecuted {
				// Another branch already executed this join node
				execCtx.joinMu.Unlock()
				pe.logger.Debugw("join node already executed, skipping",
					"nodeId", node.definition.Id,
					"nodeName", node.definition.Name,
					"mode", mode)
				return nil
			}

			// Increment completion counter
			execCtx.joinCompletions[node.definition.Id]++
			completedBranches := execCtx.joinCompletions[node.definition.Id]
			requiredBranches := len(node.incoming)

			pe.logger.Debugw("join node branch completed",
				"nodeId", node.definition.Id,
				"nodeName", node.definition.Name,
				"mode", mode,
				"completed", completedBranches,
				"required", requiredBranches)

			// Determine if we should execute based on mode
			shouldExecute := false
			switch mode {
			case "first":
				// First to Complete: execute on first branch to arrive
				shouldExecute = (completedBranches == 1)
			case "all":
				// Wait for All: execute when all branches have arrived
				shouldExecute = (completedBranches >= requiredBranches)
			default:
				// Unknown mode, default to "all"
				pe.logger.Warnw("unknown join mode, defaulting to 'all'",
					"nodeId", node.definition.Id,
					"mode", mode)
				shouldExecute = (completedBranches >= requiredBranches)
			}

			if !shouldExecute {
				// Not ready to execute yet
				execCtx.joinMu.Unlock()
				pe.logger.Debugw("join node not ready",
					"nodeId", node.definition.Id,
					"mode", mode,
					"remaining", requiredBranches-completedBranches)
				return nil
			}

			// Mark as executed and proceed
			execCtx.joinExecuted[node.definition.Id] = true
			execCtx.joinMu.Unlock()

			pe.logger.Debugw("join node executing",
				"nodeId", node.definition.Id,
				"nodeName", node.definition.Name,
				"mode", mode,
				"completedBranches", completedBranches)
		}
	}

	execCtx.mu.Lock()
	execCtx.executionOrder++
	executionOrder := execCtx.executionOrder
	execCtx.mu.Unlock()

	nodeResult := &NodeResult{
		NodeId:         node.definition.Id,
		NodeName:       node.definition.Name,
		NodeType:       node.definition.Type,
		Status:         "running",
		StartedAt:      time.Now(),
		ExecutionOrder: executionOrder,
	}

	execCtx.resultMu.Lock()
	result.NodeResults[node.definition.Id] = nodeResult
	execCtx.resultMu.Unlock()

	pe.logger.Infow("executing node",
		"nodeId", node.definition.Id,
		"nodeType", node.definition.Type,
		"nodeName", node.definition.Name)

	input := pe.buildNodeInput(node, execCtx, sourceNodeName)
	nodeResult.Input = input

	if execCtx.onNodeUpdate != nil {
		execCtx.onNodeUpdate(&NodeUpdate{
			NodeId:    node.definition.Id,
			NodeName:  node.definition.Name,
			NodeType:  node.definition.Type,
			Status:    "running",
			Input:     input,
			StartedAt: nodeResult.StartedAt,
		})
	}

	isProducer := pe.isStreamingProducer(node.definition.Type, node.definition.Config)
	var streamSetup bool

	if isProducer && execCtx.emitBaseURL != "" {
		downstreamNodes := pe.getNextNodes(node, "")

		if len(downstreamNodes) > 0 {
			stream := GetGlobalStreamRegistry().GetOrCreate(result.RunId, node.definition.Id, 1)

			execCtx.mu.Lock()
			varsSnapshot := make(map[string]interface{})
			for k, v := range execCtx.variables {
				varsSnapshot[k] = v
			}
			nodesSnapshot := make(map[string]interface{})
			for k, v := range execCtx.nodeOutputs {
				nodesSnapshot[k] = v
			}
			execCtx.mu.Unlock()

			processor := func(itemCtx context.Context, data interface{}) error {
				for _, downstream := range downstreamNodes {
					streamInput := map[string]interface{}{
						"trigger":  execCtx.triggerData,
						"bindings": execCtx.bindings,
						"var":      varsSnapshot,
						"nodes":    nodesSnapshot,
						"prev":     data,
					}

					if execCtx.onNodeUpdate != nil {
						execCtx.onNodeUpdate(&NodeUpdate{
							NodeId:   downstream.definition.Id,
							NodeName: downstream.definition.Name,
							NodeType: downstream.definition.Type,
							Status:   "running",
							Input:    streamInput,
						})
					}

					output, _, err := pe.runNode(itemCtx, downstream.definition, streamInput, execCtx)
					if err != nil {
						if execCtx.onNodeUpdate != nil {
							execCtx.onNodeUpdate(&NodeUpdate{
								NodeId:   downstream.definition.Id,
								NodeName: downstream.definition.Name,
								NodeType: downstream.definition.Type,
								Status:   "failed",
								Error:    err.Error(),
							})
						}
						return err
					}

					if execCtx.onNodeUpdate != nil {
						execCtx.onNodeUpdate(&NodeUpdate{
							NodeId:   downstream.definition.Id,
							NodeName: downstream.definition.Name,
							NodeType: downstream.definition.Type,
							Status:   "streaming",
							Partial:  output,
						})
					}
				}
				return nil
			}

			stream.SetProcessor(ctx, processor)
			streamSetup = true

			pe.logger.Debugw("streaming pipeline configured",
				"runId", result.RunId,
				"producerNodeId", node.definition.Id,
				"downstreamCount", len(downstreamNodes),
			)
		}
	}

	output, branchKey, err := pe.runNode(ctx, node.definition, input, execCtx)
	nodeResult.CompletedAt = time.Now()
	nodeResult.Duration = nodeResult.CompletedAt.Sub(nodeResult.StartedAt)

	if err != nil {
		nodeResult.Status = "failed"
		nodeResult.Error = err.Error()

		if execCtx.onNodeUpdate != nil {
			execCtx.onNodeUpdate(&NodeUpdate{
				NodeId:      node.definition.Id,
				NodeName:    node.definition.Name,
				NodeType:    node.definition.Type,
				Status:      "failed",
				Input:       input,
				Error:       err.Error(),
				StartedAt:   nodeResult.StartedAt,
				CompletedAt: &nodeResult.CompletedAt,
				Duration:    nodeResult.Duration,
			})
		}
		return err
	}

	nodeResult.Status = "completed"
	nodeResult.Output = output

	if execCtx.onNodeUpdate != nil {
		execCtx.onNodeUpdate(&NodeUpdate{
			NodeId:      node.definition.Id,
			NodeName:    node.definition.Name,
			NodeType:    node.definition.Type,
			Status:      "completed",
			Input:       input,
			Output:      output,
			StartedAt:   nodeResult.StartedAt,
			CompletedAt: &nodeResult.CompletedAt,
			Duration:    nodeResult.Duration,
		})
	}

	execCtx.mu.Lock()
	execCtx.nodeOutputs[node.definition.Name] = output
	execCtx.nodeMetadata[node.definition.Name] = map[string]interface{}{
		"duration":    nodeResult.Duration.Milliseconds(),
		"status":      nodeResult.Status,
		"startedAt":   nodeResult.StartedAt.Format(time.RFC3339),
		"completedAt": nodeResult.CompletedAt.Format(time.RFC3339),
	}
	execCtx.mu.Unlock()

	if streamSetup {
		stream := GetGlobalStreamRegistry().Get(result.RunId, node.definition.Id)
		itemsSent := 0
		if stream != nil {
			itemsSent = stream.ItemsSent()
			stream.Close()
			pe.logger.Debugw("closed streaming channel",
				"runId", result.RunId,
				"nodeId", node.definition.Id,
				"itemsSent", itemsSent,
			)
			GetGlobalStreamRegistry().Remove(result.RunId, node.definition.Id)
		}

		if itemsSent > 0 {
			downstreamNodes := pe.getNextNodes(node, branchKey)
			for _, downstream := range downstreamNodes {
				execCtx.resultMu.Lock()
				if _, exists := result.NodeResults[downstream.definition.Id]; !exists {
					result.NodeResults[downstream.definition.Id] = &NodeResult{
						NodeId:   downstream.definition.Id,
						NodeName: downstream.definition.Name,
						NodeType: downstream.definition.Type,
						Status:   "completed",
					}
				}
				execCtx.resultMu.Unlock()

				if execCtx.onNodeUpdate != nil {
					execCtx.onNodeUpdate(&NodeUpdate{
						NodeId:   downstream.definition.Id,
						NodeName: downstream.definition.Name,
						NodeType: downstream.definition.Type,
						Status:   "completed",
						Output:   map[string]interface{}{"streamed": true},
					})
				}
			}
			return nil
		}
	}

	actualNextNodes := pe.getNextNodes(node, branchKey)

	if node.definition.Type == NodeTypeSplit && len(actualNextNodes) > 1 {
		failFast := true
		if node.definition.Config != nil {
			if mode, ok := node.definition.Config["executionMode"].(string); ok {
				failFast = (mode == "failfast")
			} else if ff, ok := node.definition.Config["failFast"].(bool); ok {
				failFast = ff
			}
		}
		return pe.executeNodesInParallel(ctx, actualNextNodes, graph, execCtx, result, node.definition.Name, failFast)
	}

	if node.definition.Type == NodeTypeLoop && len(actualNextNodes) > 0 {
		return pe.executeLoop(ctx, node, actualNextNodes, graph, execCtx, result, output)
	}

	for _, nextNode := range actualNextNodes {
		if err := pe.executeNode(ctx, nextNode, graph, execCtx, result, node.definition.Name); err != nil {
			return err
		}
	}

	return nil
}

func (pe *PipelineExecutor) executeNodesInParallel(
	ctx context.Context,
	nodes []*graphNode,
	graph *executionGraph,
	execCtx *executionContext,
	result *ExecutionResult,
	sourceNodeName string,
	failFast bool,
) error {
	type nodeCompletion struct {
		nodeId string
		err    error
	}

	// Create cancellable context for parallel branches (only used in fail-fast mode)
	branchCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Track completion of all goroutines
	completionChan := make(chan nodeCompletion, len(nodes))

	// Start all parallel branches
	for _, node := range nodes {
		go func(n *graphNode) {
			err := pe.executeNode(branchCtx, n, graph, execCtx, result, sourceNodeName)
			completionChan <- nodeCompletion{nodeId: n.definition.Id, err: err}
		}(node)
	}

	// Wait for all branches to complete or fail
	completed := 0
	var firstError error
	failedNodeId := ""
	var allErrors []error
	failedNodeIds := []string{}

	for completed < len(nodes) {
		select {
		case <-ctx.Done():
			// Parent context cancelled - cancel all branches and wait for them to finish
			cancel()
			// Drain remaining completions
			for completed < len(nodes) {
				<-completionChan
				completed++
			}
			return ctx.Err()
		case completion := <-completionChan:
			completed++
			if completion.err != nil {
				if failFast {
					// Fail-fast mode: cancel other branches on first error
					if firstError == nil {
						firstError = completion.err
						failedNodeId = completion.nodeId
						cancel()
						pe.logger.Warnw("parallel branch failed (fail-fast mode), cancelling other branches",
							"failedNodeId", failedNodeId,
							"error", firstError,
							"remaining", len(nodes)-completed)
					}
				} else {
					// Continue mode: collect all errors, let other branches complete
					allErrors = append(allErrors, completion.err)
					failedNodeIds = append(failedNodeIds, completion.nodeId)
					if firstError == nil {
						firstError = completion.err
						failedNodeId = completion.nodeId
					}
					pe.logger.Warnw("parallel branch failed (continue mode), allowing other branches to complete",
						"failedNodeId", completion.nodeId,
						"error", completion.err,
						"remaining", len(nodes)-completed)
				}
			}
		}
	}

	// Handle errors based on mode
	if firstError != nil {
		if failFast {
			// Fail-fast mode: mark cancelled nodes as "cancelled"
			execCtx.resultMu.Lock()
			for _, node := range nodes {
				if nodeResult, exists := result.NodeResults[node.definition.Id]; exists {
					// If node is still in "running" state, it was cancelled
					if nodeResult.Status == "running" {
						nodeResult.Status = "cancelled"
						nodeResult.Error = "Cancelled due to failure in parallel branch"
						nodeResult.CompletedAt = time.Now()
						nodeResult.Duration = nodeResult.CompletedAt.Sub(nodeResult.StartedAt)

						pe.logger.Infow("marking cancelled parallel branch node",
							"nodeId", node.definition.Id,
							"nodeName", node.definition.Name)

						// Send node update for cancelled status
						if execCtx.onNodeUpdate != nil {
							execCtx.onNodeUpdate(&NodeUpdate{
								NodeId:   node.definition.Id,
								NodeName: node.definition.Name,
								NodeType: node.definition.Type,
								Status:   "cancelled",
								Error:    "Cancelled due to failure in parallel branch",
							})
						}
					}
				} else {
					// Node never started - create a cancelled result
					now := time.Now()
					result.NodeResults[node.definition.Id] = &NodeResult{
						NodeId:      node.definition.Id,
						NodeName:    node.definition.Name,
						NodeType:    node.definition.Type,
						Status:      "cancelled",
						Error:       "Cancelled before execution due to failure in parallel branch",
						StartedAt:   now,
						CompletedAt: now,
						Duration:    0,
					}

					pe.logger.Infow("marking never-started parallel branch node as cancelled",
						"nodeId", node.definition.Id,
						"nodeName", node.definition.Name)

					// Send node update for cancelled status
					if execCtx.onNodeUpdate != nil {
						execCtx.onNodeUpdate(&NodeUpdate{
							NodeId:   node.definition.Id,
							NodeName: node.definition.Name,
							NodeType: node.definition.Type,
							Status:   "cancelled",
							Error:    "Cancelled before execution due to failure in parallel branch",
						})
					}
				}
			}
			execCtx.resultMu.Unlock()

			return fmt.Errorf("error in parallel branch %s: %w", failedNodeId, firstError)
		} else {
			// Continue mode: all branches completed, some failed
			// Return first error but all branches have their own status
			pe.logger.Warnw("parallel execution completed with errors",
				"failedCount", len(allErrors),
				"failedNodeIds", failedNodeIds)
			return fmt.Errorf("error in parallel branch %s: %w", failedNodeId, firstError)
		}
	}

	return nil
}

func (pe *PipelineExecutor) executeLoop(
	ctx context.Context,
	loopNode *graphNode,
	nextNodes []*graphNode,
	graph *executionGraph,
	execCtx *executionContext,
	result *ExecutionResult,
	loopOutput interface{},
) error {
	outputMap, ok := loopOutput.(map[string]interface{})
	if !ok {
		return fmt.Errorf("loop node output must be a map")
	}

	isLoop, _ := outputMap["loop"].(bool)
	if !isLoop {
		return nil
	}

	itemsRaw, ok := outputMap["items"]
	if !ok {
		return fmt.Errorf("loop node output missing 'items'")
	}

	items, ok := itemsRaw.([]interface{})
	if !ok {
		return fmt.Errorf("loop node 'items' must be an array, got %T", itemsRaw)
	}

	itemVar := "item"
	if v, ok := outputMap["itemVar"].(string); ok && v != "" {
		itemVar = v
	}

	indexVar := "index"
	if v, ok := outputMap["indexVar"].(string); ok && v != "" {
		indexVar = v
	}

	joinNode := pe.findLoopJoinNode(loopNode, nextNodes, graph)

	var loopResults []interface{}

	pe.logger.Infow("starting loop execution",
		"loopNode", loopNode.definition.Name,
		"itemCount", len(items),
		"itemVar", itemVar,
		"indexVar", indexVar,
		"hasJoin", joinNode != nil)

	for i, item := range items {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		execCtx.mu.Lock()
		execCtx.variables[itemVar] = item
		execCtx.variables[indexVar] = i
		execCtx.mu.Unlock()

		for _, nextNode := range nextNodes {
			pe.logger.Debugw("executing loop iteration",
				"loopNode", loopNode.definition.Name,
				"iteration", i,
				"itemVar", itemVar,
				"bodyNode", nextNode.definition.Name)

			if joinNode != nil {
				if err := pe.executeLoopBodyChain(ctx, nextNode, joinNode, loopNode, graph, execCtx, result, loopNode.definition.Name, i, &loopResults, item); err != nil {
					return err
				}
			} else {
				if err := pe.executeNode(ctx, nextNode, graph, execCtx, result, loopNode.definition.Name); err != nil {
					return fmt.Errorf("loop iteration %d failed: %w", i, err)
				}
				execCtx.mu.Lock()
				if out, exists := execCtx.nodeOutputs[nextNode.definition.Name]; exists {
					loopResults = append(loopResults, out)
				}
				execCtx.mu.Unlock()
			}
		}
	}

	execCtx.mu.Lock()
	delete(execCtx.variables, itemVar)
	delete(execCtx.variables, indexVar)
	execCtx.nodeOutputs[loopNode.definition.Name] = map[string]interface{}{
		"loop":    true,
		"count":   len(items),
		"results": loopResults,
	}
	execCtx.mu.Unlock()

	loopNodeResult := result.NodeResults[loopNode.definition.Id]
	if loopNodeResult != nil {
		loopNodeResult.Output = map[string]interface{}{
			"loop":    true,
			"count":   len(items),
			"results": loopResults,
		}
	}

	if joinNode != nil {
		execCtx.mu.Lock()
		execCtx.nodeOutputs[joinNode.definition.Name] = map[string]interface{}{
			"count":   len(loopResults),
			"results": loopResults,
		}
		if len(loopResults) > 0 {
			merged := make(map[string]interface{})
			for i, r := range loopResults {
				if rm, ok := r.(map[string]interface{}); ok {
					for k, v := range rm {
						merged[fmt.Sprintf("%s_%d", k, i)] = v
					}
				}
			}
			execCtx.nodeOutputs[joinNode.definition.Name].(map[string]interface{})["merged"] = merged
		}
		execCtx.mu.Unlock()

		pe.logger.Infow("loop completed, join node has collected results",
			"joinNode", joinNode.definition.Name,
			"resultCount", len(loopResults))

		afterJoinNodes := pe.getNextNodes(joinNode, "")
		for _, afterJoin := range afterJoinNodes {
			pe.logger.Infow("continuing after loop join",
				"node", afterJoin.definition.Name)
			if err := pe.executeNode(ctx, afterJoin, graph, execCtx, result, joinNode.definition.Name); err != nil {
				return err
			}
		}
	}

	return nil
}

func (pe *PipelineExecutor) executeLoopBodyChain(
	ctx context.Context,
	startNode *graphNode,
	joinNode *graphNode,
	loopNode *graphNode,
	graph *executionGraph,
	execCtx *executionContext,
	result *ExecutionResult,
	sourceNodeName string,
	iteration int,
	loopResults *[]interface{},
	currentItem interface{},
) error {
	input := pe.buildLoopIterationInputFromContext(execCtx, startNode, currentItem)

	execCtx.resultMu.Lock()
	nodeResult := &NodeResult{
		NodeId:         startNode.definition.Id,
		NodeName:       startNode.definition.Name,
		NodeType:       startNode.definition.Type,
		Status:         "running",
		StartedAt:      time.Now(),
		ExecutionOrder: execCtx.executionOrder + 1,
		Input:          input,
	}
	iterationNodeId := fmt.Sprintf("%s_iter%d", startNode.definition.Id, iteration)
	result.NodeResults[iterationNodeId] = nodeResult
	execCtx.resultMu.Unlock()

	if execCtx.onNodeUpdate != nil {
		execCtx.onNodeUpdate(&NodeUpdate{
			NodeId:    startNode.definition.Id,
			NodeName:  startNode.definition.Name,
			NodeType:  startNode.definition.Type,
			Status:    "running",
			Input:     input,
			StartedAt: nodeResult.StartedAt,
		})
	}

	output, branchKey, err := pe.runNode(ctx, startNode.definition, input, execCtx)

	nodeResult.CompletedAt = time.Now()
	nodeResult.Duration = nodeResult.CompletedAt.Sub(nodeResult.StartedAt)

	if err != nil {
		nodeResult.Status = "failed"
		nodeResult.Error = err.Error()
		if execCtx.onNodeUpdate != nil {
			execCtx.onNodeUpdate(&NodeUpdate{
				NodeId:   startNode.definition.Id,
				NodeName: startNode.definition.Name,
				NodeType: startNode.definition.Type,
				Status:   "failed",
				Error:    err.Error(),
			})
		}
		return fmt.Errorf("loop iteration %d failed in node %s: %w", iteration, startNode.definition.Name, err)
	}

	nodeResult.Status = "completed"
	nodeResult.Output = output

	if execCtx.onNodeUpdate != nil {
		execCtx.onNodeUpdate(&NodeUpdate{
			NodeId:      startNode.definition.Id,
			NodeName:    startNode.definition.Name,
			NodeType:    startNode.definition.Type,
			Status:      "completed",
			Output:      output,
			CompletedAt: &nodeResult.CompletedAt,
			Duration:    nodeResult.Duration,
		})
	}

	execCtx.mu.Lock()
	execCtx.nodeOutputs[startNode.definition.Name] = output
	execCtx.mu.Unlock()

	bodyNextNodes := pe.getNextNodes(startNode, branchKey)
	for _, bodyNext := range bodyNextNodes {
		if bodyNext.definition.Id == joinNode.definition.Id {
			*loopResults = append(*loopResults, output)
			pe.logger.Infow("collected loop iteration result",
				"iteration", iteration,
				"bodyNode", startNode.definition.Name,
				"totalCollected", len(*loopResults))
		} else if bodyNext.definition.Id == loopNode.definition.Id {
			pe.logger.Debugw("skipping loop node in body chain to prevent re-execution",
				"loopNode", loopNode.definition.Name,
				"iteration", iteration)
		} else {
			if err := pe.executeLoopBodyChain(ctx, bodyNext, joinNode, loopNode, graph, execCtx, result, startNode.definition.Name, iteration, loopResults, currentItem); err != nil {
				return err
			}
		}
	}

	return nil
}

func (pe *PipelineExecutor) buildLoopIterationInputFromContext(execCtx *executionContext, node *graphNode, currentItem interface{}) map[string]interface{} {
	execCtx.mu.Lock()
	nodeOutputsSnapshot := make(map[string]interface{})
	for k, v := range execCtx.nodeOutputs {
		nodeOutputsSnapshot[k] = v
	}

	nodeMetadataSnapshot := make(map[string]map[string]interface{})
	for k, v := range execCtx.nodeMetadata {
		nodeMetadataSnapshot[k] = v
	}

	variablesSnapshot := make(map[string]interface{})
	for k, v := range execCtx.variables {
		variablesSnapshot[k] = v
	}
	execCtx.mu.Unlock()

	enrichedNodeOutputs := make(map[string]interface{})
	for nodeName, output := range nodeOutputsSnapshot {
		if metadata, ok := nodeMetadataSnapshot[nodeName]; ok {
			enrichedNodeOutputs[nodeName] = pe.enrichPrevWithMetadata(output, metadata)
		} else {
			enrichedNodeOutputs[nodeName] = output
		}
	}

	runMetadata := make(map[string]interface{})
	for k, v := range execCtx.runMetadata {
		runMetadata[k] = v
	}
	runMetadata["nodes"] = nodeMetadataSnapshot

	var elapsedMs int64 = 0
	for _, metadata := range nodeMetadataSnapshot {
		if duration, ok := metadata["duration"].(int64); ok {
			elapsedMs += duration
		}
	}

	selfContext := map[string]interface{}{
		"name": node.definition.Name,
	}

	input := map[string]interface{}{
		"trigger":    execCtx.triggerData,
		"bindings":   execCtx.bindings,
		"var":        variablesSnapshot,
		"nodes":      enrichedNodeOutputs,
		"run":        runMetadata,
		"self":       selfContext,
		"elapsed_ms": elapsedMs,
	}

	if execCtx.bindings != nil {
		if vaultData, ok := execCtx.bindings["vault"]; ok {
			input["vault"] = vaultData
		}
	}

	if len(node.incoming) > 0 {
		parentOutputs := make(map[string]interface{})
		var singleParentName string
		for _, edge := range node.incoming {
			parentName := edge.source.definition.Name
			if parentOutput, ok := enrichedNodeOutputs[parentName]; ok {
				parentOutputs[parentName] = parentOutput
				singleParentName = parentName
			}
		}

		if len(parentOutputs) == 1 {
			input["prev"] = parentOutputs[singleParentName]
		} else if len(parentOutputs) > 1 {
			input["prev"] = parentOutputs
		} else {
			input["prev"] = currentItem
		}
	} else {
		input["prev"] = currentItem
	}

	return input
}

func (pe *PipelineExecutor) findLoopJoinNode(loopNode *graphNode, bodyNodes []*graphNode, graph *executionGraph) *graphNode {
	for _, bodyNode := range bodyNodes {
		visited := make(map[string]bool)
		if join := pe.findJoinInChain(bodyNode, loopNode.definition.Id, visited); join != nil {
			return join
		}
	}
	return nil
}

func (pe *PipelineExecutor) findJoinInChain(node *graphNode, loopNodeId string, visited map[string]bool) *graphNode {
	if visited[node.definition.Id] {
		return nil
	}
	visited[node.definition.Id] = true

	if node.definition.Type == NodeTypeJoin {
		return node
	}

	for _, edge := range node.outgoing {
		if join := pe.findJoinInChain(edge.target, loopNodeId, visited); join != nil {
			return join
		}
	}

	return nil
}

func (pe *PipelineExecutor) buildLoopIterationInput(
	loopNode *graphNode,
	execCtx *executionContext,
	item interface{},
	index int,
	itemVar, indexVar string,
) map[string]interface{} {
	execCtx.mu.Lock()
	nodeOutputsSnapshot := make(map[string]interface{})
	for k, v := range execCtx.nodeOutputs {
		nodeOutputsSnapshot[k] = v
	}

	nodeMetadataSnapshot := make(map[string]map[string]interface{})
	for k, v := range execCtx.nodeMetadata {
		nodeMetadataSnapshot[k] = v
	}

	variablesSnapshot := make(map[string]interface{})
	for k, v := range execCtx.variables {
		variablesSnapshot[k] = v
	}
	execCtx.mu.Unlock()

	enrichedNodeOutputs := make(map[string]interface{})
	for nodeName, output := range nodeOutputsSnapshot {
		if metadata, ok := nodeMetadataSnapshot[nodeName]; ok {
			enrichedNodeOutputs[nodeName] = pe.enrichPrevWithMetadata(output, metadata)
		} else {
			enrichedNodeOutputs[nodeName] = output
		}
	}

	runMetadata := make(map[string]interface{})
	for k, v := range execCtx.runMetadata {
		runMetadata[k] = v
	}
	runMetadata["nodes"] = nodeMetadataSnapshot

	var elapsedMs int64 = 0
	for _, metadata := range nodeMetadataSnapshot {
		if duration, ok := metadata["duration"].(int64); ok {
			elapsedMs += duration
		}
	}

	selfContext := map[string]interface{}{
		"name": loopNode.definition.Name,
	}

	input := map[string]interface{}{
		"trigger":    execCtx.triggerData,
		"bindings":   execCtx.bindings,
		"var":        variablesSnapshot,
		"nodes":      enrichedNodeOutputs,
		"run":        runMetadata,
		"self":       selfContext,
		"elapsed_ms": elapsedMs,
	}

	if execCtx.bindings != nil {
		if vaultData, ok := execCtx.bindings["vault"]; ok {
			input["vault"] = vaultData
		}
	}

	input["prev"] = item

	return input
}

// enrichPrevWithMetadata merges the previous node's output with its metadata
// If output is a map, add metadata fields directly. Otherwise, wrap it.
func (pe *PipelineExecutor) enrichPrevWithMetadata(output interface{}, metadata map[string]interface{}) interface{} {
	if metadata == nil {
		return output
	}

	// If output is already a map, merge metadata into it
	if outputMap, ok := output.(map[string]interface{}); ok {
		enriched := make(map[string]interface{})
		// Copy all output fields
		for k, v := range outputMap {
			enriched[k] = v
		}
		// Add metadata fields (duration, status, etc.)
		if duration, ok := metadata["duration"].(int64); ok {
			enriched["duration"] = duration
		}
		if status, ok := metadata["status"].(string); ok {
			enriched["status"] = status
		}
		if startedAt, ok := metadata["startedAt"].(string); ok {
			enriched["startedAt"] = startedAt
		}
		if completedAt, ok := metadata["completedAt"].(string); ok {
			enriched["completedAt"] = completedAt
		}
		return enriched
	}

	// If output is not a map, wrap it with metadata
	enriched := make(map[string]interface{})
	enriched["output"] = output
	if duration, ok := metadata["duration"].(int64); ok {
		enriched["duration"] = duration
	}
	if status, ok := metadata["status"].(string); ok {
		enriched["status"] = status
	}
	if startedAt, ok := metadata["startedAt"].(string); ok {
		enriched["startedAt"] = startedAt
	}
	if completedAt, ok := metadata["completedAt"].(string); ok {
		enriched["completedAt"] = completedAt
	}
	return enriched
}

func (pe *PipelineExecutor) buildNodeInput(node *graphNode, execCtx *executionContext, sourceNodeName string) map[string]interface{} {
	execCtx.mu.Lock()
	nodeOutputsSnapshot := make(map[string]interface{})
	for k, v := range execCtx.nodeOutputs {
		nodeOutputsSnapshot[k] = v
	}

	nodeMetadataSnapshot := make(map[string]map[string]interface{})
	for k, v := range execCtx.nodeMetadata {
		nodeMetadataSnapshot[k] = v
	}

	variablesSnapshot := make(map[string]interface{})
	for k, v := range execCtx.variables {
		variablesSnapshot[k] = v
	}
	execCtx.mu.Unlock()

	// Enrich node outputs with metadata (duration, status, etc.)
	enrichedNodeOutputs := make(map[string]interface{})
	for nodeName, output := range nodeOutputsSnapshot {
		if metadata, ok := nodeMetadataSnapshot[nodeName]; ok {
			enrichedNodeOutputs[nodeName] = pe.enrichPrevWithMetadata(output, metadata)
		} else {
			enrichedNodeOutputs[nodeName] = output
		}
	}

	// Build run metadata with node execution info
	runMetadata := make(map[string]interface{})
	for k, v := range execCtx.runMetadata {
		runMetadata[k] = v
	}
	runMetadata["nodes"] = nodeMetadataSnapshot

	// Calculate elapsed time from all completed nodes
	var elapsedMs int64 = 0
	for _, metadata := range nodeMetadataSnapshot {
		if duration, ok := metadata["duration"].(int64); ok {
			elapsedMs += duration
		}
	}

	// Build self context with current node's name
	selfContext := map[string]interface{}{
		"name": node.definition.Name,
	}

	input := map[string]interface{}{
		"trigger":    execCtx.triggerData,
		"bindings":   execCtx.bindings,
		"var":        variablesSnapshot,
		"nodes":      enrichedNodeOutputs,
		"run":        runMetadata,
		"self":       selfContext,
		"elapsed_ms": elapsedMs,
	}

	// Add vault credentials from bindings if present
	if execCtx.bindings != nil {
		if vaultData, ok := execCtx.bindings["vault"]; ok {
			input["vault"] = vaultData
		}
	}

	if len(node.incoming) > 0 {
		parentOutputs := make(map[string]interface{})
		var singleParentName string
		for _, edge := range node.incoming {
			parentName := edge.source.definition.Name
			if parentOutput, ok := enrichedNodeOutputs[parentName]; ok {
				parentOutputs[parentName] = parentOutput
				singleParentName = parentName
			}
		}

		// For join nodes in stream mode, only use the specific source node's output
		if node.definition.Type == NodeTypeJoin && sourceNodeName != "" {
			mode := "all"
			if node.definition.Config != nil {
				if m, ok := node.definition.Config["mode"].(string); ok && m != "" {
					mode = m
				}
			}

			if mode == "stream" && parentOutputs[sourceNodeName] != nil {
				// Already enriched with metadata
				input["prev"] = parentOutputs[sourceNodeName]
				return input
			}
		}

		if len(parentOutputs) == 1 {
			// Single parent - already enriched with metadata
			input["prev"] = parentOutputs[singleParentName]
		} else if len(parentOutputs) > 1 {
			// Multiple parents - already enriched with metadata
			input["prev"] = parentOutputs
		}
	}

	return input
}

// runNode executes a node and returns its output and branch key (for conditionals)
func (pe *PipelineExecutor) runNode(
	ctx context.Context,
	def *NodeDefinition,
	input map[string]interface{},
	execCtx *executionContext,
) (interface{}, string, error) {
	pe.logger.Infow("runNode called",
		"nodeId", def.Id,
		"nodeType", def.Type,
		"nodeName", def.Name)

	// Set current node info in run metadata for streaming emit
	runMetadataWithNode := make(map[string]interface{})
	for k, v := range execCtx.runMetadata {
		runMetadataWithNode[k] = v
	}
	runMetadataWithNode["currentNodeId"] = def.Id
	if execCtx.emitBaseURL != "" {
		runMetadataWithNode["emitUrl"] = execCtx.emitBaseURL
	}

	// Create a resolver for template expressions using the shared SDK resolver
	resolver := NewResolver(sdkResolver.Config{
		Trigger:   execCtx.triggerData,
		Bindings:  execCtx.bindings,
		Prev:      getMap(input, "prev"),
		Nodes:     execCtx.nodeOutputs,
		Variables: execCtx.variables,
		Run:       runMetadataWithNode,
		Self:      getMap(input, "self"),
		ElapsedMs: getElapsedMs(input),
	})
	resolver.SetGraph(execCtx.execGraph)

	// Convert NodeDefinition to StepDefinition for the executor
	step := &StepDefinition{
		Id:     def.Id,
		Name:   def.Name,
		Type:   def.Type,
		Config: def.Config,
	}

	// Handle trigger nodes specially - output the user data portion
	switch def.Type {
	case NodeTypeWebhook, NodeTypeCron, NodeTypeManual, NodeTypeK8sEvent, NodeTypeK8sWatch:
		// Output the data portion for downstream nodes, not the full trigger envelope
		// This allows {{nodes.TriggerName.field}} instead of {{nodes.TriggerName.data.field}}
		if data, ok := execCtx.triggerData["data"]; ok {
			return data, "", nil
		}
		return execCtx.triggerData, "", nil
	}

	// Get the executor for this node type
	pe.logger.Infow("getting executor for node type",
		"nodeType", def.Type,
		"nodeName", def.Name)
	executor, err := pe.registry.Get(def.Type)
	if err != nil {
		pe.logger.Errorw("executor not found",
			"nodeType", def.Type,
			"error", err)
		return nil, "", err
	}
	pe.logger.Infow("executor found, about to execute",
		"nodeType", def.Type,
		"nodeName", def.Name,
		"executorType", fmt.Sprintf("%T", executor))

	// Check if executor supports streaming and we have a callback
	var stepResult *StepResult
	isStreamingExecutor := false
	if se, ok := executor.(StreamingExecutor); ok {
		isStreamingExecutor = se.SupportsStreaming(def.Config)
	}
	pe.logger.Infow("checking streaming support",
		"nodeType", def.Type,
		"isStreamingExecutor", isStreamingExecutor,
		"hasCallback", execCtx.onNodeUpdate != nil)

	if streamingExec, ok := executor.(StreamingExecutor); ok && execCtx.onNodeUpdate != nil && streamingExec.SupportsStreaming(def.Config) {
		pe.logger.Infow("using streaming execution path",
			"nodeType", def.Type,
			"nodeName", def.Name)
		// Create stream callback that forwards to node update callback
		streamCallback := func(update *StreamUpdate) {
			execCtx.onNodeUpdate(&NodeUpdate{
				NodeId:   def.Id,
				NodeName: def.Name,
				NodeType: def.Type,
				Status:   "streaming",
				Input:    input,
				Partial:  update.Partial,
				Progress: update.Progress,
			})
		}

		// Execute with streaming
		pe.logger.Infow("calling ExecuteStreaming",
			"nodeType", def.Type,
			"nodeName", def.Name)
		stepResult, err = streamingExec.ExecuteStreaming(ctx, step, resolver, streamCallback)
		pe.logger.Infow("ExecuteStreaming returned",
			"nodeType", def.Type,
			"nodeName", def.Name,
			"hasError", err != nil)
	} else {
		// Execute without streaming
		pe.logger.Infow("calling executor.Execute (non-streaming)",
			"nodeType", def.Type,
			"nodeName", def.Name)
		stepResult, err = executor.Execute(ctx, step, resolver)
		pe.logger.Infow("executor.Execute returned",
			"nodeType", def.Type,
			"nodeName", def.Name,
			"hasError", err != nil)
	}

	if err != nil {
		pe.logger.Errorw("executor returned error",
			"nodeType", def.Type,
			"nodeName", def.Name,
			"error", err)
		return nil, "", err
	}

	// Handle variables set by the node
	if def.Type == NodeTypeSet {
		if varName, ok := def.Config["variable"].(string); ok {
			execCtx.variables[varName] = stepResult.Output
		}
	}

	return stepResult.Output, stepResult.NextStep, nil
}

// isStreamingProducer checks if a node can produce streaming output
func (pe *PipelineExecutor) isStreamingProducer(nodeType string, config map[string]interface{}) bool {
	executor, err := pe.registry.Get(nodeType)
	if err != nil {
		return false
	}
	if producer, ok := executor.(StreamProducer); ok {
		return producer.SupportsStreamingOutput(config)
	}
	return false
}

// getNextNodes returns the next nodes to execute based on branch key
func (pe *PipelineExecutor) getNextNodes(node *graphNode, branchKey string) []*graphNode {
	nextNodes := make([]*graphNode, 0)

	for _, edge := range node.outgoing {
		// For non-conditional nodes, follow all edges
		if branchKey == "" {
			nextNodes = append(nextNodes, edge.target)
			continue
		}

		// For conditional nodes, only follow matching branch
		if edge.sourceHandle == branchKey || edge.label == branchKey {
			nextNodes = append(nextNodes, edge.target)
		}
	}

	return nextNodes
}
