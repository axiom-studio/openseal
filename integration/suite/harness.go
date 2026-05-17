// Package suite provides integration test utilities for verifying OpenSeal workflow
// execution compatibility with platform behavior.
package suite

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/axiom-studio/openseal/pkg/executor"
	"github.com/axiom-studio/openseal/pkg/types"
	"gopkg.in/yaml.v3"
)

// WorkflowYAML represents the top-level YAML structure exported by the platform.
// This mirrors AgentLibraryVersionExport but uses YAML struct tags for parsing.
type WorkflowYAML struct {
	APIVersion string                    `yaml:"apiVersion"`
	Kind       string                    `yaml:"kind"`
	Metadata   WorkflowYAMLMetadata      `yaml:"metadata"`
	Spec       WorkflowYAMLSpec          `yaml:"spec"`
}

// WorkflowYAMLMetadata contains library metadata
type WorkflowYAMLMetadata struct {
	Name        string   `yaml:"name"`
	Version     string   `yaml:"version"`
	Category    string   `yaml:"category"`
	Description string   `yaml:"description,omitempty"`
	Icon        string   `yaml:"icon,omitempty"`
	Author      string   `yaml:"author,omitempty"`
	Tags        []string `yaml:"tags,omitempty"`
}

// WorkflowYAMLSpec contains the workflow specification
type WorkflowYAMLSpec struct {
	Nodes       []WorkflowYAMLNode       `yaml:"nodes"`
	Connections []WorkflowYAMLConnection `yaml:"connections"`
}

// WorkflowYAMLNode represents a single node in the exported workflow YAML
type WorkflowYAMLNode struct {
	Id       string                 `yaml:"id"`
	Name     string                 `yaml:"name"`
	Type     string                 `yaml:"type"`
	Position WorkflowYAMLPosition   `yaml:"position"`
	Config   map[string]interface{} `yaml:"config,omitempty"`
}

// WorkflowYAMLPosition represents node position in the visual editor
type WorkflowYAMLPosition struct {
	X float64 `yaml:"x"`
	Y float64 `yaml:"y"`
}

// WorkflowYAMLConnection represents a connection between nodes
type WorkflowYAMLConnection struct {
	Id           string `yaml:"id"`
	SourceNodeId string `yaml:"sourceNodeId"`
	TargetNodeId string `yaml:"targetNodeId"`
	SourceHandle string `yaml:"sourceHandle,omitempty"`
	TargetHandle string `yaml:"targetHandle,omitempty"`
	Label        string `yaml:"label,omitempty"`
}

// ParsedWorkflow holds the fully parsed and converted workflow data
type ParsedWorkflow struct {
	Workflow    WorkflowYAML
	Nodes       []*types.AgentNodeDefinition
	Connections []*types.AgentConnection
	Graph       *executor.ExecutionGraph
}

// LoadWorkflowYAML reads and parses a workflow YAML file from the testdata directory.
func LoadWorkflowYAML(filename string) (*WorkflowYAML, error) {
	path := filepath.Join("..", "testdata", filename)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read workflow file %s: %w", path, err)
	}

	var workflow WorkflowYAML
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		return nil, fmt.Errorf("failed to parse workflow YAML: %w", err)
	}

	return &workflow, nil
}

// ConvertToTypes converts the parsed YAML workflow into openseal types.
// This verifies that YAML data can be faithfully represented in openseal's type system.
func ConvertToTypes(wf *WorkflowYAML) (*ParsedWorkflow, error) {
	if wf == nil {
		return nil, fmt.Errorf("workflow is nil")
	}

	nodes := make([]*types.AgentNodeDefinition, 0, len(wf.Spec.Nodes))
	for _, n := range wf.Spec.Nodes {
		node := &types.AgentNodeDefinition{
			Id:        n.Id,
			Name:      n.Name,
			Type:      n.Type,
			PositionX: n.Position.X,
			PositionY: n.Position.Y,
			Config:    n.Config,
		}
		nodes = append(nodes, node)
	}

	conns := make([]*types.AgentConnection, 0, len(wf.Spec.Connections))
	for _, c := range wf.Spec.Connections {
		conn := &types.AgentConnection{
			Id:           c.Id,
			SourceNodeId: c.SourceNodeId,
			TargetNodeId: c.TargetNodeId,
			SourceHandle:  c.SourceHandle,
			TargetHandle:  c.TargetHandle,
			Label:         c.Label,
		}
		conns = append(conns, conn)
	}

	// Convert to executor-aware NodeDefinition list for graph building
	graphNodes := make([]*executor.NodeDefinition, 0, len(nodes))
	for _, n := range nodes {
		graphNodes = append(graphNodes, &executor.NodeDefinition{
			Id:   n.Id,
			Name: n.Name,
			Type: n.Type,
			Config: func() map[string]interface{} {
				if n.Config != nil {
					return n.Config
				}
				return make(map[string]interface{})
			}(),
		})
	}

	// Convert connections for graph building
	graphConns := make([]*executor.ConnectionDefinition, 0, len(conns))
	for _, c := range conns {
		graphConns = append(graphConns, &executor.ConnectionDefinition{
			Id:           c.Id,
			SourceNodeId: c.SourceNodeId,
			TargetNodeId: c.TargetNodeId,
			SourceHandle: c.SourceHandle,
			TargetHandle: c.TargetHandle,
		})
	}

	graph, err := executor.BuildGraph(graphNodes, graphConns)
	if err != nil {
		return nil, fmt.Errorf("failed to build execution graph: %w", err)
	}

	return &ParsedWorkflow{
		Workflow:    *wf,
		Nodes:       nodes,
		Connections: conns,
		Graph:       graph,
	}, nil
}

// GetNodesByType returns all nodes of the specified type from the parsed workflow.
func GetNodesByType(pw *ParsedWorkflow, nodeType string) []*types.AgentNodeDefinition {
	var result []*types.AgentNodeDefinition
	for _, n := range pw.Nodes {
		if n.Type == nodeType {
			result = append(result, n)
		}
	}
	return result
}

// GetNodeByID returns a node by its ID, or nil if not found.
func GetNodeByID(pw *ParsedWorkflow, nodeID string) *types.AgentNodeDefinition {
	for _, n := range pw.Nodes {
		if n.Id == nodeID {
			return n
		}
	}
	return nil
}

// GetConnectionsFrom returns all connections originating from the given node.
func GetConnectionsFrom(pw *ParsedWorkflow, nodeID string) []*types.AgentConnection {
	var result []*types.AgentConnection
	for _, c := range pw.Connections {
		if c.SourceNodeId == nodeID {
			result = append(result, c)
		}
	}
	return result
}

// GetConnectionsTo returns all connections targeting the given node.
func GetConnectionsTo(pw *ParsedWorkflow, nodeID string) []*types.AgentConnection {
	var result []*types.AgentConnection
	for _, c := range pw.Connections {
		if c.TargetNodeId == nodeID {
			result = append(result, c)
		}
	}
	return result
}

// GetUniqueNodeTypes returns a set of all unique node types in the workflow.
func GetUniqueNodeTypes(pw *ParsedWorkflow) map[string]int {
	types := make(map[string]int)
	for _, n := range pw.Nodes {
		types[n.Type]++
	}
	return types
}

// GetUniqueSourceHandles returns all unique source handles in connections.
func GetUniqueSourceHandles(pw *ParsedWorkflow) map[string][]string {
	handles := make(map[string][]string)
	for _, c := range pw.Connections {
		if c.SourceHandle != "" {
			handles[c.SourceNodeId] = append(handles[c.SourceNodeId], c.SourceHandle)
		}
	}
	return handles
}