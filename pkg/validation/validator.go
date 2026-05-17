package validation

import (
	"fmt"
	"strings"

	"github.com/axiom-studio/openseal/internal/workflow"
	"github.com/axiom-studio/openseal/pkg/executor"
)

// Issue represents a single validation problem.
type Issue struct {
	Level   string `json:"level"`   // error, warning
	Message string `json:"message"`
	NodeID  string `json:"nodeId,omitempty"`
	EdgeID  string `json:"edgeId,omitempty"`
}

// Result holds all validation issues for a workflow.
type Result struct {
	Valid   bool    `json:"valid"`
	Issues  []Issue `json:"issues"`
	Summary string  `json:"summary"`
}

// Validator validates OpenSeal workflows.
type Validator struct {
	nodeMeta map[string]*executor.NodeMetadata
	registry *executor.Registry
}

// NewValidator creates a validator with loaded metadata.
func NewValidator(reg *executor.Registry) *Validator {
	meta, _ := executor.LoadNodeMetadata()
	return &Validator{
		nodeMeta: meta,
		registry: reg,
	}
}

// Validate checks a workflow for structural and schema issues.
func (v *Validator) Validate(wf *workflow.Workflow) *Result {
	var issues []Issue

	issues = append(issues, v.validateNodeIDs(wf)...)
	issues = append(issues, v.validateNodeTypes(wf)...)
	issues = append(issues, v.validateEdges(wf)...)
	issues = append(issues, v.validateNodeConfig(wf)...)
	issues = append(issues, v.validateGraph(wf)...)

	errCount := 0
	for _, i := range issues {
		if i.Level == "error" {
			errCount++
		}
	}

	valid := errCount == 0
	summary := fmt.Sprintf("%d error(s), %d warning(s)", errCount, len(issues)-errCount)
	if valid {
		summary = "Valid. " + summary
	}

	return &Result{
		Valid:   valid,
		Issues:  issues,
		Summary: summary,
	}
}

func (v *Validator) validateNodeIDs(wf *workflow.Workflow) []Issue {
	var issues []Issue
	seen := make(map[string]bool)
	for _, n := range wf.Nodes {
		if n.ID == "" {
			issues = append(issues, Issue{Level: "error", Message: "node has empty ID"})
			continue
		}
		if seen[n.ID] {
			issues = append(issues, Issue{Level: "error", Message: fmt.Sprintf("duplicate node ID %q", n.ID), NodeID: n.ID})
		}
		seen[n.ID] = true
	}
	return issues
}

func (v *Validator) validateNodeTypes(wf *workflow.Workflow) []Issue {
	var issues []Issue
	for _, n := range wf.Nodes {
		if n.Type == "" {
			issues = append(issues, Issue{Level: "error", Message: fmt.Sprintf("node %q has no type", n.ID), NodeID: n.ID})
			continue
		}
		if _, ok := v.nodeMeta[n.Type]; !ok {
			// Also check registry for dynamically registered types
			if v.registry != nil && !v.registry.HasExecutor(n.Type) {
				issues = append(issues, Issue{Level: "error", Message: fmt.Sprintf("unknown node type %q", n.Type), NodeID: n.ID})
			}
		}
	}
	return issues
}

func (v *Validator) validateEdges(wf *workflow.Workflow) []Issue {
	var issues []Issue
	nodeSet := make(map[string]bool)
	for _, n := range wf.Nodes {
		nodeSet[n.ID] = true
	}
	for i, e := range wf.Edges {
		edgeID := fmt.Sprintf("edge-%d (%s -> %s)", i, e.From, e.To)
		if e.From == "" {
			issues = append(issues, Issue{Level: "error", Message: fmt.Sprintf("%s has no source", edgeID), EdgeID: edgeID})
		} else if !nodeSet[e.From] {
			issues = append(issues, Issue{Level: "error", Message: fmt.Sprintf("%s references unknown source node %q", edgeID, e.From), EdgeID: edgeID})
		}
		if e.To == "" {
			issues = append(issues, Issue{Level: "error", Message: fmt.Sprintf("%s has no target", edgeID), EdgeID: edgeID})
		} else if !nodeSet[e.To] {
			issues = append(issues, Issue{Level: "error", Message: fmt.Sprintf("%s references unknown target node %q", edgeID, e.To), EdgeID: edgeID})
		}
		if e.From == e.To && e.From != "" {
			issues = append(issues, Issue{Level: "warning", Message: fmt.Sprintf("%s is a self-loop", edgeID), EdgeID: edgeID})
		}
	}
	return issues
}

func (v *Validator) validateNodeConfig(wf *workflow.Workflow) []Issue {
	var issues []Issue
	for _, n := range wf.Nodes {
		meta, ok := v.nodeMeta[n.Type]
		if !ok || meta.InputSchema == nil {
			continue
		}
		props, _ := meta.InputSchema["properties"].(map[string]interface{})
		required, _ := meta.InputSchema["required"].([]interface{})

		// Check required fields
		for _, r := range required {
			key, ok := r.(string)
			if !ok {
				continue
			}
			if _, has := n.Config[key]; !has {
				issues = append(issues, Issue{
					Level:   "error",
					Message: fmt.Sprintf("node %q: missing required config field %q", n.ID, key),
					NodeID:  n.ID,
				})
			}
		}

		// Check unknown fields
		if props != nil {
			for key := range n.Config {
				if _, known := props[key]; !known {
					// It's a warning, not an error — schemas can be incomplete
					issues = append(issues, Issue{
						Level:   "warning",
						Message: fmt.Sprintf("node %q: unknown config field %q", n.ID, key),
						NodeID:  n.ID,
					})
				}
			}
		}
	}
	return issues
}

func (v *Validator) validateGraph(wf *workflow.Workflow) []Issue {
	var issues []Issue

	// Detect cycles
	adj := make(map[string][]string)
	for _, e := range wf.Edges {
		adj[e.From] = append(adj[e.From], e.To)
	}

	visited := make(map[string]bool)
	recStack := make(map[string]bool)
	var cycleNodes []string

	var dfs func(node string) bool
	dfs = func(node string) bool {
		visited[node] = true
		recStack[node] = true
		for _, neighbor := range adj[node] {
			if !visited[neighbor] {
				if dfs(neighbor) {
					return true
				}
			} else if recStack[neighbor] {
				cycleNodes = append(cycleNodes, neighbor)
				return true
			}
		}
		recStack[node] = false
		return false
	}

	for _, n := range wf.Nodes {
		if !visited[n.ID] {
			if dfs(n.ID) {
				issues = append(issues, Issue{
					Level:   "error",
					Message: fmt.Sprintf("cycle detected in workflow graph (involves %s)", strings.Join(cycleNodes, ", ")),
				})
				break
			}
		}
	}

	// Check for orphaned nodes (no incoming or outgoing edges)
	if len(wf.Nodes) > 1 {
		hasIncoming := make(map[string]bool)
		hasOutgoing := make(map[string]bool)
		for _, e := range wf.Edges {
			hasIncoming[e.To] = true
			hasOutgoing[e.From] = true
		}
		for _, n := range wf.Nodes {
			if !hasIncoming[n.ID] && !hasOutgoing[n.ID] {
				issues = append(issues, Issue{
					Level:   "warning",
					Message: fmt.Sprintf("node %q is orphaned (no edges)", n.ID),
					NodeID:  n.ID,
				})
			}
		}
	}

	return issues
}
