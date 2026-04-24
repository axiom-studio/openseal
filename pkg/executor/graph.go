package executor

import (
	"errors"
	"fmt"
	"strings"
)

// GraphProvider provides access to the execution graph for executors that need it
// Executors like AI can use this to discover connected tool nodes
type GraphProvider interface {
	GetGraph() *ExecutionGraph
}

// ConnectionDefinition defines a connection between nodes
type ConnectionDefinition struct {
	Id           string `json:"id"`
	SourceNodeId string `json:"sourceNodeId"`
	TargetNodeId string `json:"targetNodeId"`
	SourceHandle string `json:"sourceHandle,omitempty"`
	TargetHandle string `json:"targetHandle,omitempty"`
	Label        string `json:"label,omitempty"`
}

// ExecutionGraph represents the workflow as a graph for execution
type ExecutionGraph struct {
	Nodes       map[string]*GraphNode
	Connections map[string][]*GraphConnection
	StartNodes  []string // Trigger nodes that can start execution
}

// GraphNode wraps a node with execution metadata
type GraphNode struct {
	Node          *NodeDefinition
	IncomingEdges []*GraphConnection
	OutgoingEdges []*GraphConnection
}

// GraphConnection represents a connection between nodes
type GraphConnection struct {
	Source       string
	Target       string
	SourceHandle string // For conditional nodes (if/switch): "then", "else", case values
	TargetHandle string // For tool connections: "tools-target"
	Label        string
}

// BuildGraph constructs an execution graph from nodes and connections
func BuildGraph(nodes []*NodeDefinition, connections []*ConnectionDefinition) (*ExecutionGraph, error) {
	if len(nodes) == 0 {
		return nil, errors.New("no nodes provided")
	}

	graph := &ExecutionGraph{
		Nodes:       make(map[string]*GraphNode),
		Connections: make(map[string][]*GraphConnection),
		StartNodes:  make([]string, 0),
	}

	// Add all nodes
	for _, node := range nodes {
		graphNode := &GraphNode{
			Node:          node,
			IncomingEdges: make([]*GraphConnection, 0),
			OutgoingEdges: make([]*GraphConnection, 0),
		}
		graph.Nodes[node.Id] = graphNode

		// Identify trigger nodes as potential start points
		if isTriggerType(node.Type) {
			graph.StartNodes = append(graph.StartNodes, node.Id)
		}
	}

	// Add all connections
	for _, conn := range connections {
		gc := &GraphConnection{
			Source:       conn.SourceNodeId,
			Target:       conn.TargetNodeId,
			SourceHandle: conn.SourceHandle,
			TargetHandle: conn.TargetHandle,
			Label:        conn.Label,
		}

		// Add to source node's outgoing edges
		if sourceNode, ok := graph.Nodes[conn.SourceNodeId]; ok {
			sourceNode.OutgoingEdges = append(sourceNode.OutgoingEdges, gc)
		}

		// Add to target node's incoming edges
		if targetNode, ok := graph.Nodes[conn.TargetNodeId]; ok {
			targetNode.IncomingEdges = append(targetNode.IncomingEdges, gc)
		}

		// Store in connections map
		graph.Connections[conn.SourceNodeId] = append(graph.Connections[conn.SourceNodeId], gc)
	}

	return graph, nil
}

// GetNode returns a node by ID
func (g *ExecutionGraph) GetNode(nodeId string) (*GraphNode, error) {
	if node, ok := g.Nodes[nodeId]; ok {
		return node, nil
	}
	return nil, fmt.Errorf("node not found: %s", nodeId)
}

// GetNextNodes returns the next nodes to execute after the given node
// For conditional nodes, it uses the branchKey to determine which path to follow
func (g *ExecutionGraph) GetNextNodes(nodeId string, branchKey string) []*GraphNode {
	connections, ok := g.Connections[nodeId]
	if !ok {
		return nil
	}

	result := make([]*GraphNode, 0)
	for _, conn := range connections {
		// For conditional nodes, only follow the matching branch
		if branchKey != "" && conn.SourceHandle != "" && conn.SourceHandle != branchKey {
			continue
		}

		if node, ok := g.Nodes[conn.Target]; ok {
			result = append(result, node)
		}
	}

	return result
}

// GetTriggerNode returns the trigger node for a specific node ID
func (g *ExecutionGraph) GetTriggerNode(nodeId string) (*GraphNode, error) {
	for _, startNodeId := range g.StartNodes {
		if startNodeId == nodeId {
			return g.GetNode(nodeId)
		}
	}
	return nil, fmt.Errorf("trigger node not found: %s", nodeId)
}

// GetConnectedTools returns tool node definitions connected to a node's tools handle.
// Tool nodes are connected to AI nodes via the "tools-target" target handle.
func (g *ExecutionGraph) GetConnectedTools(nodeId string) []*NodeDefinition {
	node, ok := g.Nodes[nodeId]
	if !ok {
		return nil
	}

	var tools []*NodeDefinition
	for _, edge := range node.IncomingEdges {
		// Check if edge connects to this node's tools handle
		if edge.TargetHandle == "tools-target" {
			sourceNode, ok := g.Nodes[edge.Source]
			if ok && sourceNode.Node != nil && strings.HasPrefix(sourceNode.Node.Type, "tool_") {
				tools = append(tools, sourceNode.Node)
			}
		}
	}
	return tools
}

// isTriggerType checks if a node type is a trigger
func isTriggerType(nodeType string) bool {
	switch nodeType {
	case NodeTypeWebhook, NodeTypeCron, NodeTypeManual:
		return true
	default:
		return false
	}
}

// TopologicalSort returns nodes in execution order
// This is useful for validating the graph has no cycles
func (g *ExecutionGraph) TopologicalSort() ([]*GraphNode, error) {
	visited := make(map[string]bool)
	inStack := make(map[string]bool)
	result := make([]*GraphNode, 0)

	var visit func(nodeId string) error
	visit = func(nodeId string) error {
		if inStack[nodeId] {
			return fmt.Errorf("cycle detected at node: %s", nodeId)
		}
		if visited[nodeId] {
			return nil
		}

		inStack[nodeId] = true
		for _, conn := range g.Connections[nodeId] {
			if err := visit(conn.Target); err != nil {
				return err
			}
		}
		inStack[nodeId] = false
		visited[nodeId] = true

		node, _ := g.GetNode(nodeId)
		result = append([]*GraphNode{node}, result...)
		return nil
	}

	for _, startNode := range g.StartNodes {
		if err := visit(startNode); err != nil {
			return nil, err
		}
	}

	return result, nil
}
