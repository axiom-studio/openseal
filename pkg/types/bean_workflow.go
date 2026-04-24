package types

import "time"

// AgentWorkflow represents a workflow instance stored in the database
type AgentWorkflow struct {
	Id                    int       `json:"id,omitempty" db:"id"`
	AgentLibraryVersionId int       `json:"agentLibraryVersionId" db:"agent_library_version_id" validate:"required"`
	Name                  string    `json:"name" db:"name" validate:"required,min=1,max=100"`
	Description           string    `json:"description,omitempty" db:"description"`
	IsDefault             bool      `json:"isDefault" db:"is_default"`
	CreatedAt             time.Time `json:"createdAt" db:"created_at"`
	UpdatedAt             time.Time `json:"updatedAt" db:"updated_at"`
	CreatedBy             int32     `json:"-" db:"created_by"`
	UpdatedBy             int32     `json:"-" db:"updated_by"`
}

// AgentWorkflowBean is the API response bean for a workflow
type AgentWorkflowBean struct {
	Id                    int       `json:"id,omitempty"`
	AgentLibraryVersionId int       `json:"agentLibraryVersionId" validate:"required"`
	AgentLibraryVersion   string    `json:"agentLibraryVersion,omitempty"`
	AgentLibraryName      string    `json:"agentLibraryName,omitempty"`
	Name                  string    `json:"name" validate:"required,min=1,max=100"`
	Description           string    `json:"description,omitempty"`
	IsDefault             bool      `json:"isDefault"`
	CreatedAt             time.Time `json:"createdAt"`
	UpdatedAt             time.Time `json:"updatedAt"`
	CreatedBy             int32     `json:"-"`
	UpdatedBy             int32     `json:"-"`
}

// AgentWorkflowListBean is a summary view for listing workflows
type AgentWorkflowListBean struct {
	Id                    int    `json:"id"`
	AgentLibraryVersionId int    `json:"agentLibraryVersionId"`
	AgentLibraryVersion   string `json:"agentLibraryVersion,omitempty"`
	AgentLibraryName      string `json:"agentLibraryName,omitempty"`
	Name                  string `json:"name"`
	Description           string `json:"description,omitempty"`
	IsDefault             bool   `json:"isDefault"`
	CreatedAt             string `json:"createdAt"`
	UpdatedAt             string `json:"updatedAt"`
}

// AgentWorkflowsListResponse is the paginated response for listing workflows
type AgentWorkflowsListResponse struct {
	Workflows  []*AgentWorkflowListBean `json:"workflows"`
	TotalCount int                      `json:"totalCount"`
}

// CreateWorkflowRequest is the request to create a new workflow
type CreateWorkflowRequest struct {
	AgentLibraryVersionId int    `json:"agentLibraryVersionId" validate:"required"`
	Name                  string `json:"name" validate:"required,min=1,max=100"`
	Description           string `json:"description,omitempty"`
	IsDefault             bool   `json:"isDefault"`
	UserId                int32  `json:"-"`
}

// UpdateWorkflowRequest is the request to update an existing workflow
type UpdateWorkflowRequest struct {
	Name        string `json:"name" validate:"required,min=1,max=100"`
	Description string `json:"description,omitempty"`
	IsDefault   bool   `json:"isDefault"`
	UserId      int32  `json:"-"`
}

// Workflow constants
const (
	WorkflowDefaultName = "default"
)
