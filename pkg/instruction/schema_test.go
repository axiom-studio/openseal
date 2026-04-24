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
	"testing"
	"time"
)

func TestAddNodePayload_Validate(t *testing.T) {
	tests := []struct {
		name    string
		payload *AddNodePayload
		wantErr bool
	}{
		{
			name: "valid add node payload",
			payload: &AddNodePayload{
				Node: NodeConfig{
					ID:          "123e4567-e89b-12d3-a456-426614174000",
					Type:        "webhook",
					Name:        "Webhook Node",
					PositionX:   100.0,
					PositionY:   200.0,
					Description: "A webhook trigger node",
				},
			},
			wantErr: false,
		},
		{
			name: "missing node type",
			payload: &AddNodePayload{
				Node: NodeConfig{
					ID:          "123e4567-e89b-12d3-a456-426614174000",
					Name:        "Webhook Node",
					PositionX:   100.0,
					PositionY:   200.0,
					Description: "A webhook trigger node",
				},
			},
			wantErr: true,
		},
		{
			name: "missing node name",
			payload: &AddNodePayload{
				Node: NodeConfig{
					ID:          "123e4567-e89b-12d3-a456-426614174000",
					Type:        "webhook",
					PositionX:   100.0,
					PositionY:   200.0,
					Description: "A webhook trigger node",
				},
			},
			wantErr: true,
		},
		{
			name: "negative positionX",
			payload: &AddNodePayload{
				Node: NodeConfig{
					ID:          "123e4567-e89b-12d3-a456-426614174000",
					Type:        "webhook",
					Name:        "Webhook Node",
					PositionX:   -10.0,
					PositionY:   200.0,
					Description: "A webhook trigger node",
				},
			},
			wantErr: true,
		},
		{
			name: "negative positionY",
			payload: &AddNodePayload{
				Node: NodeConfig{
					ID:          "123e4567-e89b-12d3-a456-426614174000",
					Type:        "webhook",
					Name:        "Webhook Node",
					PositionX:   100.0,
					PositionY:   -20.0,
					Description: "A webhook trigger node",
				},
			},
			wantErr: true,
		},
		{
			name: "empty config map",
			payload: &AddNodePayload{
				Node: NodeConfig{
					ID:          "123e4567-e89b-12d3-a456-426614174000",
					Type:        "webhook",
					Name:        "Webhook Node",
					PositionX:   100.0,
					PositionY:   200.0,
					Config:      map[string]interface{}{},
					Description: "A webhook trigger node",
				},
			},
			wantErr: false,
		},
		{
			name: "node name too long",
			payload: &AddNodePayload{
				Node: NodeConfig{
					ID:          "123e4567-e89b-12d3-a456-426614174000",
					Type:        "webhook",
					Name:        "ThisIsAVeryLongNodeNameThatExceedsTheMaximumLengthOfOneHundredCharactersWhichShouldTriggerValidationError",
					PositionX:   100.0,
					PositionY:   200.0,
					Description: "A webhook trigger node",
				},
			},
			wantErr: true,
		},
		{
			name: "description too long",
			payload: &AddNodePayload{
				Node: NodeConfig{
					ID:          "123e4567-e89b-12d3-a456-426614174000",
					Type:        "webhook",
					Name:        "Webhook Node",
					PositionX:   100.0,
					PositionY:   200.0,
					Description: "This is a very long description that exceeds the maximum length of five hundred characters. " + repeatString("x", 500),
				},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.payload.Validate(); (err != nil) != tt.wantErr {
				t.Errorf("AddNodePayload.Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestRemoveNodePayload_Validate(t *testing.T) {
	tests := []struct {
		name    string
		payload *RemoveNodePayload
		wantErr bool
	}{
		{
			name: "valid remove node payload",
			payload: &RemoveNodePayload{
				NodeID: "123e4567-e89b-12d3-a456-426614174000",
			},
			wantErr: false,
		},
		{
			name:    "missing node ID",
			payload: &RemoveNodePayload{},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.payload.Validate(); (err != nil) != tt.wantErr {
				t.Errorf("RemoveNodePayload.Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestAddEdgePayload_Validate(t *testing.T) {
	tests := []struct {
		name    string
		payload *AddEdgePayload
		wantErr bool
	}{
		{
			name: "valid add edge payload",
			payload: &AddEdgePayload{
				Edge: EdgeConfig{
					ID:           "123e4567-e89b-12d3-a456-426614174000",
					SourceNodeID: "123e4567-e89b-12d3-a456-426614174001",
					TargetNodeID: "123e4567-e89b-12d3-a456-426614174002",
					SourceHandle: "output",
					TargetHandle: "input",
					Label:        "Success",
				},
			},
			wantErr: false,
		},
		{
			name: "missing source node ID",
			payload: &AddEdgePayload{
				Edge: EdgeConfig{
					ID:           "123e4567-e89b-12d3-a456-426614174000",
					TargetNodeID: "123e4567-e89b-12d3-a456-426614174002",
				},
			},
			wantErr: true,
		},
		{
			name: "missing target node ID",
			payload: &AddEdgePayload{
				Edge: EdgeConfig{
					ID:           "123e4567-e89b-12d3-a456-426614174000",
					SourceNodeID: "123e4567-e89b-12d3-a456-426614174001",
				},
			},
			wantErr: true,
		},
		{
			name: "same source and target node",
			payload: &AddEdgePayload{
				Edge: EdgeConfig{
					ID:           "123e4567-e89b-12d3-a456-426614174000",
					SourceNodeID: "123e4567-e89b-12d3-a456-426614174001",
					TargetNodeID: "123e4567-e89b-12d3-a456-426614174001",
				},
			},
			wantErr: true,
		},
		{
			name: "with optional fields",
			payload: &AddEdgePayload{
				Edge: EdgeConfig{
					ID:           "123e4567-e89b-12d3-a456-426614174000",
					SourceNodeID: "123e4567-e89b-12d3-a456-426614174001",
					TargetNodeID: "123e4567-e89b-12d3-a456-426614174002",
				},
			},
			wantErr: false,
		},
		{
			name: "source handle too long",
			payload: &AddEdgePayload{
				Edge: EdgeConfig{
					ID:           "123e4567-e89b-12d3-a456-426614174000",
					SourceNodeID: "123e4567-e89b-12d3-a456-426614174001",
					TargetNodeID: "123e4567-e89b-12d3-a456-426614174002",
					SourceHandle: repeatString("x", 101),
				},
			},
			wantErr: true,
		},
		{
			name: "label too long",
			payload: &AddEdgePayload{
				Edge: EdgeConfig{
					ID:           "123e4567-e89b-12d3-a456-426614174000",
					SourceNodeID: "123e4567-e89b-12d3-a456-426614174001",
					TargetNodeID: "123e4567-e89b-12d3-a456-426614174002",
					Label:        repeatString("x", 101),
				},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.payload.Validate(); (err != nil) != tt.wantErr {
				t.Errorf("AddEdgePayload.Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestRemoveEdgePayload_Validate(t *testing.T) {
	tests := []struct {
		name    string
		payload *RemoveEdgePayload
		wantErr bool
	}{
		{
			name: "valid remove edge payload",
			payload: &RemoveEdgePayload{
				EdgeID: "123e4567-e89b-12d3-a456-426614174000",
			},
			wantErr: false,
		},
		{
			name:    "missing edge ID",
			payload: &RemoveEdgePayload{},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.payload.Validate(); (err != nil) != tt.wantErr {
				t.Errorf("RemoveEdgePayload.Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestUpdateNodeConfigPayload_Validate(t *testing.T) {
	tests := []struct {
		name    string
		payload *UpdateNodeConfigPayload
		wantErr bool
	}{
		{
			name: "valid update node config payload",
			payload: &UpdateNodeConfigPayload{
				NodeID: "123e4567-e89b-12d3-a456-426614174000",
				Config: map[string]interface{}{
					"method": "POST",
					"url":    "https://example.com",
				},
			},
			wantErr: false,
		},
		{
			name:    "missing node ID",
			payload: &UpdateNodeConfigPayload{Config: map[string]interface{}{"key": "value"}},
			wantErr: true,
		},
		{
			name: "empty config",
			payload: &UpdateNodeConfigPayload{
				NodeID: "123e4567-e89b-12d3-a456-426614174000",
				Config: map[string]interface{}{},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.payload.Validate(); (err != nil) != tt.wantErr {
				t.Errorf("UpdateNodeConfigPayload.Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestUpdateNodeNamePayload_Validate(t *testing.T) {
	tests := []struct {
		name    string
		payload *UpdateNodeNamePayload
		wantErr bool
	}{
		{
			name: "valid update node name payload",
			payload: &UpdateNodeNamePayload{
				NodeID: "123e4567-e89b-12d3-a456-426614174000",
				Name:   "New Node Name",
			},
			wantErr: false,
		},
		{
			name:    "missing node ID",
			payload: &UpdateNodeNamePayload{Name: "New Name"},
			wantErr: true,
		},
		{
			name: "missing name",
			payload: &UpdateNodeNamePayload{
				NodeID: "123e4567-e89b-12d3-a456-426614174000",
				Name:   "",
			},
			wantErr: true,
		},
		{
			name: "name too long",
			payload: &UpdateNodeNamePayload{
				NodeID: "123e4567-e89b-12d3-a456-426614174000",
				Name:   repeatString("x", 101),
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.payload.Validate(); (err != nil) != tt.wantErr {
				t.Errorf("UpdateNodeNamePayload.Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestUpdateNodeDescriptionPayload_Validate(t *testing.T) {
	tests := []struct {
		name    string
		payload *UpdateNodeDescriptionPayload
		wantErr bool
	}{
		{
			name: "valid update node description payload",
			payload: &UpdateNodeDescriptionPayload{
				NodeID:      "123e4567-e89b-12d3-a456-426614174000",
				Description: "New description",
			},
			wantErr: false,
		},
		{
			name:    "missing node ID",
			payload: &UpdateNodeDescriptionPayload{Description: "New description"},
			wantErr: true,
		},
		{
			name: "description too long",
			payload: &UpdateNodeDescriptionPayload{
				NodeID:      "123e4567-e89b-12d3-a456-426614174000",
				Description: repeatString("x", 501),
			},
			wantErr: true,
		},
		{
			name: "empty description (allowed)",
			payload: &UpdateNodeDescriptionPayload{
				NodeID:      "123e4567-e89b-12d3-a456-426614174000",
				Description: "",
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.payload.Validate(); (err != nil) != tt.wantErr {
				t.Errorf("UpdateNodeDescriptionPayload.Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestClearWorkflowPayload_Validate(t *testing.T) {
	tests := []struct {
		name    string
		payload *ClearWorkflowPayload
		wantErr bool
	}{
		{
			name:    "valid clear workflow payload",
			payload: &ClearWorkflowPayload{},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.payload.Validate(); (err != nil) != tt.wantErr {
				t.Errorf("ClearWorkflowPayload.Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestInstruction_Validate(t *testing.T) {
	tests := []struct {
		name        string
		instruction *Instruction
		wantErr     bool
	}{
		{
			name: "valid instruction",
			instruction: &Instruction{
				Type: AddNode,
				Payload: &AddNodePayload{
					Node: NodeConfig{
						ID:        "123e4567-e89b-12d3-a456-426614174000",
						Type:      "webhook",
						Name:      "Webhook Node",
						PositionX: 100.0,
						PositionY: 200.0,
					},
				},
			},
			wantErr: false,
		},
		{
			name: "missing type",
			instruction: &Instruction{
				Payload: &AddNodePayload{},
			},
			wantErr: true,
		},
		{
			name: "missing payload",
			instruction: &Instruction{
				Type: AddNode,
			},
			wantErr: true,
		},
		{
			name: "invalid payload",
			instruction: &Instruction{
				Type:    AddNode,
				Payload: &AddNodePayload{},
			},
			wantErr: true,
		},
		{
			name: "invalid metadata",
			instruction: &Instruction{
				Type:    AddNode,
				Payload: &AddNodePayload{},
				Metadata: &InstructionMetadata{
					ID:        "invalid-uuid",
					Timestamp: time.Now(),
					Version:   0,
				},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.instruction.Validate(); (err != nil) != tt.wantErr {
				t.Errorf("Instruction.Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestInstructionMetadata_Validate(t *testing.T) {
	tests := []struct {
		name     string
		metadata *InstructionMetadata
		wantErr  bool
	}{
		{
			name: "valid metadata",
			metadata: &InstructionMetadata{
				ID:        "123e4567-e89b-12d3-a456-426614174000",
				Timestamp: time.Now(),
				Version:   1,
			},
			wantErr: false,
		},
		{
			name:     "missing ID",
			metadata: &InstructionMetadata{},
			wantErr:  true,
		},
		{
			name: "invalid UUID",
			metadata: &InstructionMetadata{
				ID:        "invalid-uuid",
				Timestamp: time.Now(),
				Version:   1,
			},
			wantErr: true,
		},
		{
			name: "zero timestamp",
			metadata: &InstructionMetadata{
				ID:        "123e4567-e89b-12d3-a456-426614174000",
				Timestamp: time.Time{},
				Version:   1,
			},
			wantErr: true,
		},
		{
			name: "version less than 1",
			metadata: &InstructionMetadata{
				ID:        "123e4567-e89b-12d3-a456-426614174000",
				Timestamp: time.Now(),
				Version:   0,
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.metadata.Validate(); (err != nil) != tt.wantErr {
				t.Errorf("InstructionMetadata.Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestNewInstruction(t *testing.T) {
	payload := &AddNodePayload{
		Node: NodeConfig{
			ID:        "123e4567-e89b-12d3-a456-426614174000",
			Type:      "webhook",
			Name:      "Webhook Node",
			PositionX: 100.0,
			PositionY: 200.0,
		},
	}

	instruction := NewInstruction(AddNode, payload)

	if instruction.Type != AddNode {
		t.Errorf("Expected type %v, got %v", AddNode, instruction.Type)
	}

	if instruction.Payload == nil {
		t.Error("Expected payload to be set")
	}

	if instruction.Metadata == nil {
		t.Error("Expected metadata to be set")
	}

	if instruction.Metadata.ID == "" {
		t.Error("Expected metadata ID to be set")
	}

	if instruction.Metadata.Timestamp.IsZero() {
		t.Error("Expected metadata timestamp to be set")
	}

	if instruction.Metadata.Version != 1 {
		t.Errorf("Expected metadata version to be 1, got %d", instruction.Metadata.Version)
	}
}

func TestNewInstructionMetadata(t *testing.T) {
	metadata := NewInstructionMetadata()

	if metadata.ID == "" {
		t.Error("Expected ID to be set")
	}

	if metadata.Timestamp.IsZero() {
		t.Error("Expected timestamp to be set")
	}

	if metadata.Version != 1 {
		t.Errorf("Expected version to be 1, got %d", metadata.Version)
	}
}

func TestGetInstructionType(t *testing.T) {
	payload := &AddNodePayload{
		BaseInstructionPayload: BaseInstructionPayload{
			Type: AddNode,
		},
		Node: NodeConfig{
			ID:        "123e4567-e89b-12d3-a456-426614174000",
			Type:      "webhook",
			Name:      "Webhook Node",
			PositionX: 100.0,
			PositionY: 200.0,
		},
	}

	instructionType := GetInstructionType(payload)

	if instructionType != AddNode {
		t.Errorf("Expected instruction type %v, got %v", AddNode, instructionType)
	}
}

// Helper function to repeat a string
func repeatString(s string, n int) string {
	result := ""
	for i := 0; i < n; i++ {
		result += s
	}
	return result
}
