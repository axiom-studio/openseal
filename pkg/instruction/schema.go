/*
 * Copyright (c) 2025. Axiom Studio
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package instruction

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// InstructionType defines the type of instruction
type InstructionType string

const (
	// AddNode adds a new node to the workflow
	// Payload: AddNodePayload
	// Execution: Creates a new node in the workflow with specified configuration
	AddNode InstructionType = "add_node"

	// RemoveNode removes a node from the workflow
	// Payload: RemoveNodePayload
	// Execution: Removes the specified node and all connected edges from the workflow
	RemoveNode InstructionType = "remove_node"

	// AddEdge adds a connection between nodes
	// Payload: AddEdgePayload
	// Execution: Creates an edge connecting two nodes in the workflow
	AddEdge InstructionType = "add_edge"

	// RemoveEdge removes a connection between nodes
	// Payload: RemoveEdgePayload
	// Execution: Removes the specified edge from the workflow
	RemoveEdge InstructionType = "remove_edge"

	// UpdateNodeConfig updates a node's configuration
	// Payload: UpdateNodeConfigPayload
	// Execution: Updates the configuration parameters of an existing node
	UpdateNodeConfig InstructionType = "update_node_config"

	// UpdateNodeName updates a node's name
	// Payload: UpdateNodeNamePayload
	// Execution: Changes the display name of an existing node
	UpdateNodeName InstructionType = "update_node_name"

	// UpdateNodeDescription updates a node's description
	// Payload: UpdateNodeDescriptionPayload
	// Execution: Updates the description text of an existing node
	UpdateNodeDescription InstructionType = "update_node_description"

	// ClearWorkflow removes all nodes and edges from the workflow
	// Payload: ClearWorkflowPayload
	// Execution: Resets the workflow to an empty state
	ClearWorkflow InstructionType = "clear_workflow"
)

// InstructionMetadata contains metadata about an instruction
type InstructionMetadata struct {
	ID        string    `json:"id" validate:"required,uuid"`
	Timestamp time.Time `json:"timestamp" validate:"required"`
	Version   int       `json:"version" validate:"required,min=1"`
}

// NewInstructionMetadata creates a new metadata instance with auto-generated ID and timestamp
func NewInstructionMetadata() *InstructionMetadata {
	return &InstructionMetadata{
		ID:        uuid.New().String(),
		Timestamp: time.Now().UTC(),
		Version:   1,
	}
}

// InstructionPayload is the interface for all instruction payloads
type InstructionPayload interface {
	// Validate validates the payload
	Validate() error
	// GetType returns the instruction type
	GetType() InstructionType
}

// BaseInstructionPayload provides common implementation for payloads
type BaseInstructionPayload struct {
	Type InstructionType `json:"type"`
}

// GetType returns the instruction type
func (p *BaseInstructionPayload) GetType() InstructionType {
	return p.Type
}

// AddNodePayload represents the payload for adding a node
type AddNodePayload struct {
	BaseInstructionPayload `json:",inline"`
	Node                   NodeConfig `json:"node" validate:"required,dive"`
}

// NodeConfig represents the configuration for a node
type NodeConfig struct {
	ID          string                 `json:"id" validate:"required,uuid"`
	Type        string                 `json:"type" validate:"required,min=1,max=100"`
	Name        string                 `json:"name" validate:"required,min=1,max=100"`
	PositionX   float64                `json:"positionX" validate:"required"`
	PositionY   float64                `json:"positionY" validate:"required"`
	Config      map[string]interface{} `json:"config,omitempty"`
	Description string                 `json:"description,omitempty" validate:"max=500"`
}

// Validate validates the AddNodePayload
func (p *AddNodePayload) Validate() error {
	if p.Node.Type == "" {
		return fmt.Errorf("node type is required")
	}
	if p.Node.Name == "" {
		return fmt.Errorf("node name is required")
	}
	if len(p.Node.Name) > 100 {
		return fmt.Errorf("node name must be at most 100 characters")
	}
	if p.Node.PositionX < 0 {
		return fmt.Errorf("positionX must be non-negative")
	}
	if p.Node.PositionY < 0 {
		return fmt.Errorf("positionY must be non-negative")
	}
	if p.Node.Description != "" && len(p.Node.Description) > 500 {
		return fmt.Errorf("node description must be at most 500 characters")
	}
	return nil
}

// RemoveNodePayload represents the payload for removing a node
type RemoveNodePayload struct {
	BaseInstructionPayload `json:",inline"`
	NodeID                 string `json:"nodeId" validate:"required,uuid"`
}

// Validate validates the RemoveNodePayload
func (p *RemoveNodePayload) Validate() error {
	if p.NodeID == "" {
		return fmt.Errorf("node ID is required")
	}
	return nil
}

// AddEdgePayload represents the payload for adding an edge
type AddEdgePayload struct {
	BaseInstructionPayload `json:",inline"`
	Edge                   EdgeConfig `json:"edge" validate:"required,dive"`
}

// EdgeConfig represents the configuration for an edge
type EdgeConfig struct {
	ID           string `json:"id" validate:"required,uuid"`
	SourceNodeID string `json:"sourceNodeId" validate:"required,uuid"`
	TargetNodeID string `json:"targetNodeId" validate:"required,uuid"`
	SourceHandle string `json:"sourceHandle,omitempty" validate:"max=100"`
	TargetHandle string `json:"targetHandle,omitempty" validate:"max=100"`
	Label        string `json:"label,omitempty" validate:"max=100"`
}

// Validate validates the AddEdgePayload
func (p *AddEdgePayload) Validate() error {
	if p.Edge.SourceNodeID == "" {
		return fmt.Errorf("source node ID is required")
	}
	if p.Edge.TargetNodeID == "" {
		return fmt.Errorf("target node ID is required")
	}
	if p.Edge.SourceNodeID == p.Edge.TargetNodeID {
		return fmt.Errorf("source and target nodes cannot be the same")
	}
	if p.Edge.SourceHandle != "" && len(p.Edge.SourceHandle) > 100 {
		return fmt.Errorf("source handle must be at most 100 characters")
	}
	if p.Edge.TargetHandle != "" && len(p.Edge.TargetHandle) > 100 {
		return fmt.Errorf("target handle must be at most 100 characters")
	}
	if p.Edge.Label != "" && len(p.Edge.Label) > 100 {
		return fmt.Errorf("edge label must be at most 100 characters")
	}
	return nil
}

// RemoveEdgePayload represents the payload for removing an edge
type RemoveEdgePayload struct {
	BaseInstructionPayload `json:",inline"`
	EdgeID                 string `json:"edgeId" validate:"required,uuid"`
}

// Validate validates the RemoveEdgePayload
func (p *RemoveEdgePayload) Validate() error {
	if p.EdgeID == "" {
		return fmt.Errorf("edge ID is required")
	}
	return nil
}

// UpdateNodeConfigPayload represents the payload for updating node configuration
type UpdateNodeConfigPayload struct {
	BaseInstructionPayload `json:",inline"`
	NodeID                 string                 `json:"nodeId" validate:"required,uuid"`
	Config                 map[string]interface{} `json:"config" validate:"required"`
}

// Validate validates the UpdateNodeConfigPayload
func (p *UpdateNodeConfigPayload) Validate() error {
	if p.NodeID == "" {
		return fmt.Errorf("node ID is required")
	}
	if len(p.Config) == 0 {
		return fmt.Errorf("config must not be empty")
	}
	return nil
}

// UpdateNodeNamePayload represents the payload for updating node name
type UpdateNodeNamePayload struct {
	BaseInstructionPayload `json:",inline"`
	NodeID                 string `json:"nodeId" validate:"required,uuid"`
	Name                   string `json:"name" validate:"required,min=1,max=100"`
}

// Validate validates the UpdateNodeNamePayload
func (p *UpdateNodeNamePayload) Validate() error {
	if p.NodeID == "" {
		return fmt.Errorf("node ID is required")
	}
	if p.Name == "" {
		return fmt.Errorf("node name is required")
	}
	if len(p.Name) > 100 {
		return fmt.Errorf("node name must be at most 100 characters")
	}
	return nil
}

// UpdateNodeDescriptionPayload represents the payload for updating node description
type UpdateNodeDescriptionPayload struct {
	BaseInstructionPayload `json:",inline"`
	NodeID                 string `json:"nodeId" validate:"required,uuid"`
	Description            string `json:"description" validate:"max=500"`
}

// Validate validates the UpdateNodeDescriptionPayload
func (p *UpdateNodeDescriptionPayload) Validate() error {
	if p.NodeID == "" {
		return fmt.Errorf("node ID is required")
	}
	if p.Description != "" && len(p.Description) > 500 {
		return fmt.Errorf("node description must be at most 500 characters")
	}
	return nil
}

// ClearWorkflowPayload represents the payload for clearing the workflow
type ClearWorkflowPayload struct {
	BaseInstructionPayload `json:",inline"`
}

// Validate validates the ClearWorkflowPayload
func (p *ClearWorkflowPayload) Validate() error {
	return nil
}

// Instruction is the common envelope for all instructions
// This struct serves as the main container for instruction data that flows between
// the backend AI/agent system and the frontend workflow builder via SSE.
type Instruction struct {
	// Type specifies the type of instruction to execute
	Type InstructionType `json:"type" validate:"required,oneof=add_node remove_node add_edge remove_edge update_node_config update_node_name update_node_description clear_workflow"`
	// Payload contains the specific data required for the instruction
	Payload InstructionPayload `json:"payload" validate:"required"`
	// Metadata provides additional information about the instruction (optional)
	Metadata *InstructionMetadata `json:"metadata,omitempty"`
}

// MarshalJSON customizes JSON marshaling to handle the interface payload
func (i *Instruction) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type     InstructionType      `json:"type"`
		Payload  interface{}          `json:"payload"`
		Metadata *InstructionMetadata `json:"metadata,omitempty"`
	}{
		Type:     i.Type,
		Payload:  i.Payload,
		Metadata: i.Metadata,
	})
}

// UnmarshalJSON customizes JSON unmarshaling to handle the interface payload
func (i *Instruction) UnmarshalJSON(data []byte) error {
	aux := struct {
		Type     InstructionType      `json:"type"`
		Metadata *InstructionMetadata `json:"metadata,omitempty"`
	}{}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	i.Type = aux.Type
	i.Metadata = aux.Metadata

	var rawPayload map[string]interface{}
	if err := json.Unmarshal(data, &struct {
		Payload *map[string]interface{} `json:"payload"`
	}{
		Payload: &rawPayload,
	}); err != nil {
		return err
	}

	switch i.Type {
	case AddNode:
		var payload AddNodePayload
		payloadBytes, err := json.Marshal(rawPayload)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(payloadBytes, &payload); err != nil {
			return err
		}
		i.Payload = &payload
	case RemoveNode:
		var payload RemoveNodePayload
		payloadBytes, err := json.Marshal(rawPayload)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(payloadBytes, &payload); err != nil {
			return err
		}
		i.Payload = &payload
	case AddEdge:
		var payload AddEdgePayload
		payloadBytes, err := json.Marshal(rawPayload)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(payloadBytes, &payload); err != nil {
			return err
		}
		i.Payload = &payload
	case RemoveEdge:
		var payload RemoveEdgePayload
		payloadBytes, err := json.Marshal(rawPayload)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(payloadBytes, &payload); err != nil {
			return err
		}
		i.Payload = &payload
	case UpdateNodeConfig:
		var payload UpdateNodeConfigPayload
		payloadBytes, err := json.Marshal(rawPayload)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(payloadBytes, &payload); err != nil {
			return err
		}
		i.Payload = &payload
	case UpdateNodeName:
		var payload UpdateNodeNamePayload
		payloadBytes, err := json.Marshal(rawPayload)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(payloadBytes, &payload); err != nil {
			return err
		}
		i.Payload = &payload
	case UpdateNodeDescription:
		var payload UpdateNodeDescriptionPayload
		payloadBytes, err := json.Marshal(rawPayload)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(payloadBytes, &payload); err != nil {
			return err
		}
		i.Payload = &payload
	case ClearWorkflow:
		var payload ClearWorkflowPayload
		payloadBytes, err := json.Marshal(rawPayload)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(payloadBytes, &payload); err != nil {
			return err
		}
		i.Payload = &payload
	default:
		return fmt.Errorf("unknown instruction type: %s", i.Type)
	}

	return nil
}

// NewInstruction creates a new instruction with the given type and payload
// This is the primary factory function for creating instructions in the system.
// It automatically generates metadata for the instruction.
func NewInstruction(typ InstructionType, payload InstructionPayload) *Instruction {
	return &Instruction{
		Type:     typ,
		Payload:  payload,
		Metadata: NewInstructionMetadata(),
	}
}

// Validate validates the instruction
func (i *Instruction) Validate() error {
	// Validate instruction type
	if i.Type == "" {
		return fmt.Errorf("instruction type is required")
	}

	// Validate payload
	if i.Payload == nil {
		return fmt.Errorf("payload is required")
	}

	// Validate payload-specific rules
	if err := i.Payload.Validate(); err != nil {
		return fmt.Errorf("payload validation failed: %w", err)
	}

	// Validate metadata if present
	if i.Metadata != nil {
		if err := i.Metadata.Validate(); err != nil {
			return fmt.Errorf("metadata validation failed: %w", err)
		}
	}

	return nil
}

// Validate validates the InstructionMetadata
func (m *InstructionMetadata) Validate() error {
	if m.ID == "" {
		return fmt.Errorf("metadata ID is required")
	}
	if _, err := uuid.Parse(m.ID); err != nil {
		return fmt.Errorf("metadata ID must be a valid UUID: %w", err)
	}
	if m.Timestamp.IsZero() {
		return fmt.Errorf("metadata timestamp is required")
	}
	if m.Version < 1 {
		return fmt.Errorf("metadata version must be at least 1")
	}
	return nil
}

// GetInstructionType returns the instruction type from a payload
func GetInstructionType(payload InstructionPayload) InstructionType {
	return payload.GetType()
}
