package workflow

import (
	"encoding/json"
	"fmt"
)

// jsonWorkflow is the JSON representation of a workflow.
type jsonWorkflow struct {
	Name         string                 `json:"name"`
	Description  string                 `json:"description,omitempty"`
	Nodes        []jsonNode             `json:"nodes"`
	Edges        []jsonEdge             `json:"edges,omitempty"`
	InputSchema  map[string]InputField  `json:"inputSchema,omitempty"`
	OutputSchema map[string]OutputField `json:"outputSchema,omitempty"`
}

type jsonNode struct {
	ID     string                 `json:"id"`
	Type   string                 `json:"type"`
	Config map[string]interface{} `json:"config,omitempty"`
}

type jsonEdge struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Condition string `json:"condition,omitempty"`
}

// ToJSON serializes the workflow to JSON bytes.
func (w *Workflow) ToJSON() ([]byte, error) {
	jw := jsonWorkflow{
		Name:         w.Name,
		Description:  w.Description,
		Nodes:        make([]jsonNode, len(w.Nodes)),
		Edges:        make([]jsonEdge, len(w.Edges)),
		InputSchema:  w.InputSchema,
		OutputSchema: w.OutputSchema,
	}
	for i, n := range w.Nodes {
		jw.Nodes[i] = jsonNode{ID: n.ID, Type: n.Type, Config: n.Config}
	}
	for i, e := range w.Edges {
		jw.Edges[i] = jsonEdge{From: e.From, To: e.To, Condition: e.Condition}
	}
	return json.MarshalIndent(jw, "", "  ")
}

// FromJSON deserializes a workflow from JSON bytes.
func FromJSON(data []byte) (*Workflow, error) {
	var jw jsonWorkflow
	if err := json.Unmarshal(data, &jw); err != nil {
		return nil, fmt.Errorf("unmarshal workflow JSON: %w", err)
	}
	w := &Workflow{
		Name:         jw.Name,
		Description:  jw.Description,
		Nodes:        make([]Node, len(jw.Nodes)),
		Edges:        make([]Edge, len(jw.Edges)),
		InputSchema:  jw.InputSchema,
		OutputSchema: jw.OutputSchema,
	}
	for i, n := range jw.Nodes {
		w.Nodes[i] = Node{ID: n.ID, Type: n.Type, Config: n.Config}
	}
	for i, e := range jw.Edges {
		w.Edges[i] = Edge{From: e.From, To: e.To, Condition: e.Condition}
	}
	return w, nil
}
