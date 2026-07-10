// Package kernelapi defines the versioned HTTP contract shared by the
// standalone OpenSeal daemon and thin clients such as the terminal UI.
package kernelapi

import (
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime"
)

const (
	Version                    = "1"
	AgentRunsCapabilityID      = "agent-runs"
	AgentRunsCapabilityVersion = "1"
)

const (
	OperationCreate    = "create"
	OperationGet       = "get"
	OperationList      = "list"
	OperationPause     = "pause"
	OperationResume    = "resume"
	OperationCancel    = "cancel"
	OperationIntervene = "intervene"
)

// CapabilityDocument is the authoritative product surface advertised by an
// OpenSeal server. Clients must not infer operations that are absent here.
type CapabilityDocument struct {
	Version      string       `json:"version"`
	Capabilities []Capability `json:"capabilities"`
}

type Capability struct {
	ID         string   `json:"id"`
	Version    string   `json:"version"`
	Available  bool     `json:"available"`
	Operations []string `json:"operations"`
}

func (d CapabilityDocument) Find(id, version string) (Capability, bool) {
	for _, candidate := range d.Capabilities {
		if candidate.ID == id && candidate.Version == version {
			return candidate, true
		}
	}
	return Capability{}, false
}

func (c Capability) Supports(operation string) bool {
	if !c.Available {
		return false
	}
	for _, candidate := range c.Operations {
		if candidate == operation {
			return true
		}
	}
	return false
}

func AgentRunsCapability() Capability {
	return Capability{
		ID: AgentRunsCapabilityID, Version: AgentRunsCapabilityVersion, Available: true,
		Operations: []string{
			OperationCreate, OperationGet, OperationList, OperationPause,
			OperationResume, OperationCancel, OperationIntervene,
		},
	}
}

func Capabilities() CapabilityDocument {
	return CapabilityDocument{
		Version:      Version,
		Capabilities: []Capability{AgentRunsCapability()},
	}
}

// CreateAgentRunRequest is the public request body for canonical durable work.
// Scope is explicit because standalone OpenSeal can host more than one local
// workspace even though the TUI defaults to local/default.
type CreateAgentRunRequest struct {
	Scope           runtime.Scope              `json:"scope"`
	ObjectiveID     string                     `json:"objectiveId,omitempty"`
	ParentRunID     string                     `json:"parentRunId,omitempty"`
	Owner           runtime.ObjectiveOwner     `json:"owner"`
	AssignedAgentID string                     `json:"assignedAgentId,omitempty"`
	Goal            string                     `json:"goal"`
	Source          runtime.RunSource          `json:"source"`
	Priority        int                        `json:"priority,omitempty"`
	Deadline        *time.Time                 `json:"deadline,omitempty"`
	AvailableAt     *time.Time                 `json:"availableAt,omitempty"`
	Context         map[string]interface{}     `json:"context,omitempty"`
	Plan            map[string]interface{}     `json:"plan,omitempty"`
	Checkpoint      map[string]interface{}     `json:"checkpoint,omitempty"`
	WakeCondition   *runtime.WakeCondition     `json:"wakeCondition,omitempty"`
	Budget          map[string]interface{}     `json:"budget,omitempty"`
	Policy          map[string]interface{}     `json:"policy,omitempty"`
	IdempotencyKey  string                     `json:"idempotencyKey,omitempty"`
	Actor           runtime.ActivityActor      `json:"actor,omitempty"`
	Visibility      runtime.ActivityVisibility `json:"visibility,omitempty"`
}

type AgentRunCommandRequest struct {
	ExpectedRevision int64                       `json:"expectedRevision"`
	Kind             runtime.AgentRunCommandKind `json:"kind"`
	Actor            runtime.ActivityActor       `json:"actor,omitempty"`
	Summary          string                      `json:"summary,omitempty"`
	Instruction      string                      `json:"instruction,omitempty"`
	Visibility       runtime.ActivityVisibility  `json:"visibility,omitempty"`
}
