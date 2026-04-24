package agent

import (
	"context"
	"io"
	"time"

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

// FileStore manages temporary file storage for agent workflows.
// This interface defines only the methods needed by the runtime package.
type FileStore interface {
	// Store saves data and returns a unique file ID
	Store(data []byte, filename string, mimeType string) (*StoredFile, error)

	// Get retrieves file metadata by ID
	Get(fileId string) (*StoredFile, error)

	// GetReader returns a reader for the file content
	GetReader(fileId string) (io.ReadCloser, error)
}

// StoredFile represents metadata about a stored file
type StoredFile struct {
	Id        string    `json:"id"`
	Filename  string    `json:"filename"`
	MimeType  string    `json:"mimeType"`
	Size      int64     `json:"size"`
	Path      string    `json:"-"` // Internal path, not exposed
	RunId     string    `json:"runId,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// AgentInstanceService manages agent instances.
// This interface defines only the methods needed by the runtime package.
type AgentInstanceService interface {
	// GetInstance retrieves an agent instance with details
	GetInstance(id int) (*types.AgentInstanceBean, error)

	// GetTriggersForInstance retrieves all triggers for an agent instance
	GetTriggersForInstance(instanceId int) ([]*types.AgentTriggerBean, error)
}

// AgentOrchestrator handles pipeline execution for agents.
// This interface defines only the methods needed by the runtime package.
type AgentOrchestrator interface {
	// TriggerAgent starts a new agent run
	TriggerAgent(ctx context.Context, req *types.TriggerAgentRequest) (*types.AgentRunBean, error)

	// WaitForRunCompletion waits for a run to complete (for synchronous webhook execution)
	WaitForRunCompletion(ctx context.Context, runId int, timeout time.Duration) (*types.AgentRunBean, error)

	// GetFileStore returns the file store for serving files
	GetFileStore() FileStore
}