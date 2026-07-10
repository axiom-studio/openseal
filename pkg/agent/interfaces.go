package agent

import (
	"github.com/axiom-studio/openseal/pkg/types"
)

// Type aliases for types package - allows runtime to use agent.X instead of types.X
type TriggerAgentRequest = types.TriggerAgentRequest
type AgentRunBean = types.AgentRunBean
type TriggerInputField = types.TriggerInputField
type AgentTriggerBean = types.AgentTriggerBean

// Run status constants - re-exported from types
const (
	RunStatusPending   = types.RunStatusPending
	RunStatusRunning   = types.RunStatusRunning
	RunStatusStreaming = types.RunStatusStreaming
	RunStatusCompleted = types.RunStatusCompleted
	RunStatusFailed    = types.RunStatusFailed
)

// FileStore is an alias so runtime code can continue using agent.FileStore.
type FileStore = types.FileStore

// StoredFile is an alias so runtime code can continue using agent.StoredFile.
type StoredFile = types.StoredFile

// AgentInstanceService manages agent instances.
// This interface defines only the methods needed by the runtime package.
type AgentInstanceService interface {
	// GetInstance retrieves an agent instance with details
	GetInstance(id int) (*types.AgentInstanceBean, error)

	// GetTriggersForInstance retrieves all triggers for an agent instance
	GetTriggersForInstance(instanceId int) ([]*types.AgentTriggerBean, error)
}

// AgentOrchestrator is defined in pkg/types.
// Runtime code should use types.AgentOrchestrator directly.
type AgentOrchestrator = types.AgentOrchestrator
