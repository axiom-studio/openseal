package types

type NodeDefinition struct {
	Id          string                 `json:"id" validate:"required"`
	Name        string                 `json:"name" validate:"required,min=1,max=100"`
	Description string                 `json:"description,omitempty"`
	Type        string                 `json:"type" validate:"required"`
	PositionX   float64                `json:"positionX"`
	PositionY   float64                `json:"positionY"`
	Config      map[string]interface{} `json:"config,omitempty"`
	WorkflowId  *int                   `json:"workflowId,omitempty"`
}

func (n *NodeDefinition) GetId() string { return n.Id }

func (n *NodeDefinition) GetType() string { return n.Type }

type ConnectionDefinition struct {
	Id           string `json:"id" validate:"required"`
	SourceNodeId string `json:"sourceNodeId" validate:"required"`
	TargetNodeId string `json:"targetNodeId" validate:"required"`
	SourceHandle string `json:"sourceHandle,omitempty"`
	TargetHandle string `json:"targetHandle,omitempty"`
	Label        string `json:"label,omitempty"`
	WorkflowId   int    `json:"workflowId,omitempty"`
}

func (c *ConnectionDefinition) GetSourceNodeId() string { return c.SourceNodeId }

func (c *ConnectionDefinition) GetTargetNodeId() string { return c.TargetNodeId }

func (c *ConnectionDefinition) GetSourceHandle() string { return c.SourceHandle }

func (c *ConnectionDefinition) GetTargetHandle() string { return c.TargetHandle }

func (c *ConnectionDefinition) GetLabel() string { return c.Label }
