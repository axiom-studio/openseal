package validation

// Workflow is the minimal representation a validator needs.
// Callers convert from their own workflow type to this.
type Workflow struct {
	Name   string
	Nodes  []Node
	Edges  []Edge
}

// Node is the minimal node representation for validation.
type Node struct {
	ID     string
	Type   string
	Config map[string]interface{}
}

// Edge is the minimal edge representation for validation.
type Edge struct {
	From      string
	To        string
	Condition string
}
