// Package workflow provides a programmatic API for building and manipulating
// OpenSeal workflows. It offers a fluent builder pattern for constructing
// workflows in Go code, with support for HCL DSL integration, validation,
// and multiple export formats.
//
// Example usage:
//
//	wf := workflow.New("deploy_service").
//		SetDescription("Deploy a service to Kubernetes").
//		AddNode(workflow.NewNode("webhook", "trigger").
//			SetParam("path", "/deploy").
//			SetParam("method", "POST")).
//		AddNode(workflow.NewNode("k8s-apply", "apply_manifest").
//			SetParam("manifest", "${trigger.input.manifest}")).
//		AddEdge("trigger", "apply_manifest").
//		Build()
package workflow

import (
	"fmt"
	"sort"
	"strings"
)

// Workflow is the public representation of an OpenSeal workflow.
// It is the unified type used by both the programmatic builder
// and the HCL parser.
type Workflow struct {
	Name        string
	Description string
	Nodes       []Node
	Edges       []Edge
	InputSchema  map[string]InputField
	OutputSchema map[string]OutputField
	SourceFile   string
}

// Node represents a single step in a workflow.
type Node struct {
	Type   string
	ID     string
	Config map[string]interface{}
}

// Edge represents a directed connection between two nodes.
type Edge struct {
	From      string
	To        string
	Condition string
}

// InputField defines an input parameter for a workflow.
type InputField struct {
	Type        string
	Description string
	Required    bool
	Default     interface{}
}

// OutputField defines an output parameter for a workflow.
type OutputField struct {
	Type        string
	Description string
	Template    string
}

// WorkflowBuilder provides a fluent API for constructing workflows programmatically.
// It validates invariants as methods are called and produces a valid Workflow
// when Build() is invoked.
type WorkflowBuilder struct {
	name        string
	description string
	nodes       []Node
	edges       []Edge
	inputSchema map[string]InputField
	outputSchema map[string]OutputField
	seenNodeIDs map[string]bool
	err         error
}

// New creates a new WorkflowBuilder with the given workflow name.
func New(name string) *WorkflowBuilder {
	return &WorkflowBuilder{
		name:        name,
		seenNodeIDs: make(map[string]bool),
	}
}

// SetDescription sets the workflow description.
func (b *WorkflowBuilder) SetDescription(desc string) *WorkflowBuilder {
	if b.err != nil {
		return b
	}
	b.description = desc
	return b
}

// AddNode adds a node to the workflow. Returns the builder for chaining.
// Panics if a node with the same ID already exists.
func (b *WorkflowBuilder) AddNode(n Node) *WorkflowBuilder {
	if b.err != nil {
		return b
	}
	if n.ID == "" {
		b.err = fmt.Errorf("node ID cannot be empty")
		return b
	}
	if n.Type == "" {
		b.err = fmt.Errorf("node %q: type cannot be empty", n.ID)
		return b
	}
	if b.seenNodeIDs[n.ID] {
		b.err = fmt.Errorf("duplicate node ID %q", n.ID)
		return b
	}
	b.seenNodeIDs[n.ID] = true
	b.nodes = append(b.nodes, n)
	return b
}

// AddEdge adds a directed edge from one node to another.
// The optional condition is an expression that must evaluate to true
// for the edge to be followed at runtime.
func (b *WorkflowBuilder) AddEdge(from, to string, condition ...string) *WorkflowBuilder {
	if b.err != nil {
		return b
	}
	cond := ""
	if len(condition) > 0 {
		cond = condition[0]
	}
	b.edges = append(b.edges, Edge{From: from, To: to, Condition: cond})
	return b
}

// SetInput adds an input field to the workflow input schema.
func (b *WorkflowBuilder) SetInput(name string, field InputField) *WorkflowBuilder {
	if b.err != nil {
		return b
	}
	if b.inputSchema == nil {
		b.inputSchema = make(map[string]InputField)
	}
	field.Type = defaultFieldType(field.Type)
	b.inputSchema[name] = field
	return b
}

// SetOutput adds an output field to the workflow output schema.
func (b *WorkflowBuilder) SetOutput(name string, field OutputField) *WorkflowBuilder {
	if b.err != nil {
		return b
	}
	if b.outputSchema == nil {
		b.outputSchema = make(map[string]OutputField)
	}
	field.Type = defaultFieldType(field.Type)
	b.outputSchema[name] = field
	return b
}

// Build finalizes the builder and returns the constructed Workflow.
// Returns any validation error accumulated during building.
func (b *WorkflowBuilder) Build() (*Workflow, error) {
	if b.err != nil {
		return nil, b.err
	}
	if b.name == "" {
		return nil, fmt.Errorf("workflow name cannot be empty")
	}
	return &Workflow{
		Name:         b.name,
		Description:  b.description,
		Nodes:        b.nodes,
		Edges:        b.edges,
		InputSchema:  b.inputSchema,
		OutputSchema: b.outputSchema,
	}, nil
}

// MustBuild is like Build but panics on error.
// Useful for inline workflow definitions in tests or initialization.
func (b *WorkflowBuilder) MustBuild() *Workflow {
	wf, err := b.Build()
	if err != nil {
		panic(err)
	}
	return wf
}

// NodeBuilder provides a fluent API for constructing individual nodes.
type NodeBuilder struct {
	nodeType string
	id       string
	config   map[string]interface{}
	err      error
}

// NewNode creates a NodeBuilder for a node with the given type and ID.
func NewNode(nodeType, id string) Node {
	return Node{
		Type:   nodeType,
		ID:     id,
		Config: make(map[string]interface{}),
	}
}

// NewNodeBuilder creates a NodeBuilder for fluent node construction.
func NewNodeBuilder(nodeType, id string) *NodeBuilder {
	return &NodeBuilder{
		nodeType: nodeType,
		id:       id,
		config:   make(map[string]interface{}),
	}
}

// SetParam sets a single configuration parameter on the node.
func (nb *NodeBuilder) SetParam(key string, value interface{}) *NodeBuilder {
	if nb.err != nil {
		return nb
	}
	nb.config[key] = value
	return nb
}

// SetParams sets multiple configuration parameters at once.
func (nb *NodeBuilder) SetParams(params map[string]interface{}) *NodeBuilder {
	if nb.err != nil {
		return nb
	}
	for k, v := range params {
		nb.config[k] = v
	}
	return nb
}

// Build returns the constructed Node.
func (nb *NodeBuilder) Build() (Node, error) {
	if nb.err != nil {
		return Node{}, nb.err
	}
	if nb.id == "" {
		return Node{}, fmt.Errorf("node ID cannot be empty")
	}
	if nb.nodeType == "" {
		return Node{}, fmt.Errorf("node %q: type cannot be empty", nb.id)
	}
	return Node{
		Type:   nb.nodeType,
		ID:     nb.id,
		Config: nb.config,
	}, nil
}

// GetParam returns a config parameter by key, or the provided default.
func (n Node) GetParam(key string, defaultVal ...interface{}) interface{} {
	if v, ok := n.Config[key]; ok {
		return v
	}
	if len(defaultVal) > 0 {
		return defaultVal[0]
	}
	return nil
}

// ParamString returns a config parameter as a string.
func (n Node) ParamString(key string) (string, bool) {
	v, ok := n.Config[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

// ParamInt returns a config parameter as an int.
func (n Node) ParamInt(key string) (int, bool) {
	v, ok := n.Config[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case int:
		return n, true
	case float64:
		return int(n), true
	default:
		return 0, false
	}
}

// ParamBool returns a config parameter as a bool.
func (n Node) ParamBool(key string) (bool, bool) {
	v, ok := n.Config[key]
	if !ok {
		return false, false
	}
	b, ok := v.(bool)
	return b, ok
}

// HasCondition returns whether the edge has a condition expression.
func (e Edge) HasCondition() bool {
	return e.Condition != ""
}

// FindNode returns a node by ID, or nil if not found.
func (w *Workflow) FindNode(id string) *Node {
	for i := range w.Nodes {
		if w.Nodes[i].ID == id {
			return &w.Nodes[i]
		}
	}
	return nil
}

// OutgoingEdges returns all edges originating from the given node ID.
func (w *Workflow) OutgoingEdges(nodeID string) []Edge {
	var result []Edge
	for _, e := range w.Edges {
		if e.From == nodeID {
			result = append(result, e)
		}
	}
	return result
}

// IncomingEdges returns all edges targeting the given node ID.
func (w *Workflow) IncomingEdges(nodeID string) []Edge {
	var result []Edge
	for _, e := range w.Edges {
		if e.To == nodeID {
			result = append(result, e)
		}
	}
	return result
}

// EntryNodes returns nodes with no incoming edges (workflow entry points).
func (w *Workflow) EntryNodes() []Node {
	hasIncoming := make(map[string]bool)
	for _, e := range w.Edges {
		hasIncoming[e.To] = true
	}
	var result []Node
	for _, n := range w.Nodes {
		if !hasIncoming[n.ID] {
			result = append(result, n)
		}
	}
	return result
}

// ExitNodes returns nodes with no outgoing edges (workflow exit points).
func (w *Workflow) ExitNodes() []Node {
	hasOutgoing := make(map[string]bool)
	for _, e := range w.Edges {
		hasOutgoing[e.From] = true
	}
	var result []Node
	for _, n := range w.Nodes {
		if !hasOutgoing[n.ID] {
			result = append(result, n)
		}
	}
	return result
}

// NodeIDs returns all node IDs in the workflow.
func (w *Workflow) NodeIDs() []string {
	ids := make([]string, len(w.Nodes))
	for i, n := range w.Nodes {
		ids[i] = n.ID
	}
	return ids
}

// defaultFieldType normalizes empty types to "string".
func defaultFieldType(t string) string {
	if t == "" {
		return "string"
	}
	return t
}

// formatHCLValue formats a Go value as an HCL attribute value.
func formatHCLValue(val interface{}) string {
	switch v := val.(type) {
	case string:
		if ContainsInterpolation(v) {
			return fmt.Sprintf("%q", escapeHCLString(v))
		}
		return fmt.Sprintf("%q", v)
	case bool:
		if v {
			return "true"
		}
		return "false"
	case int:
		return fmt.Sprintf("%d", v)
	case float64:
		return fmt.Sprintf("%g", v)
	case map[string]interface{}:
		return formatHCLMap(v)
	case []interface{}:
		return formatHCLList(v)
	default:
		return fmt.Sprintf("%q", fmt.Sprintf("%v", v))
	}
}

func formatHCLMap(m map[string]interface{}) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%q = %s", k, formatHCLValue(m[k])))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

func formatHCLList(items []interface{}) string {
	var parts []string
	for _, item := range items {
		parts = append(parts, formatHCLValue(item))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// escapeHCLString escapes special characters in strings for HCL output.
func escapeHCLString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}

// ContainsInterpolation checks whether a string contains ${...} interpolation expressions.
func ContainsInterpolation(s string) bool {
	for i := 0; i < len(s)-2; i++ {
		if s[i] == '$' && s[i+1] == '{' {
			return true
		}
	}
	return false
}