package runtime

import (
	"time"
)

// ConversationalRequest is a request for team chat agent execution with custom system prompt
type ConversationalRequest struct {
	// Persona to execute
	PersonaID int `json:"personaId"`

	// Message content
	Message string `json:"message"`

	// Custom system prompt (overrides persona's default)
	SystemPrompt string `json:"systemPrompt,omitempty"`

	// Message history for multi-turn
	History []*ConversationMessage `json:"history,omitempty"`

	// Conversation context
	ConversationID int `json:"conversationId,omitempty"`

	// User context
	UserID int32 `json:"userId,omitempty"`

	// Whether this agent is the team leader
	IsLeader bool `json:"isLeader,omitempty"`

	// Team members (for leader delegation)
	TeamMembers []TeamMemberInfo `json:"teamMembers,omitempty"`
}

// TeamMemberInfo represents a team member for delegation purposes
type TeamMemberInfo struct {
	PersonaID   int    `json:"personaId"`
	PersonaName string `json:"personaName"`
	Autonomous  bool   `json:"autonomous"`
}

// RuntimeRequest is the request to execute an autonomous agent
type RuntimeRequest struct {
	// Persona to execute
	PersonaId int `json:"personaId"`

	// Conversation context
	ConversationId int               `json:"conversationId,omitempty"`
	Message        string            `json:"message"`
	Attachments    []*FileAttachment `json:"attachments,omitempty"`

	// Additional context
	Context map[string]interface{} `json:"context,omitempty"`

	// Message history for multi-turn
	History []*ConversationMessage `json:"history,omitempty"`

	// Execution mode
	Synchronous bool `json:"synchronous,omitempty"`

	// User context
	UserId int32 `json:"userId,omitempty"`
}

// ConversationMessage represents a message in the conversation history
type ConversationMessage struct {
	Role      string                 `json:"role"` // user, assistant, system
	Content   string                 `json:"content"`
	Timestamp time.Time              `json:"timestamp,omitempty"`
	Metadata  map[string]interface{} `json:"metadata,omitempty"`
}

// FileAttachment represents a file attached to a chat message
type FileAttachment struct {
	Id       string `json:"id"`
	Name     string `json:"name"`
	MimeType string `json:"mimeType"`
	Size     int64  `json:"size"`
}

// RuntimeResponse is the response from agent execution
type RuntimeResponse struct {
	// The response content
	Response string `json:"response"`

	// Task tracking
	TaskId string `json:"taskId,omitempty"`
	State  string `json:"state"` // completed, input_required, working, failed

	// If state == input_required
	InputRequest *InputRequest `json:"inputRequest,omitempty"`

	// Tool execution details
	ToolCalls []*ToolCallResult `json:"toolCalls,omitempty"`

	// Token usage
	TokenUsage *TokenUsage `json:"tokenUsage,omitempty"`

	// Error if failed
	Error string `json:"error,omitempty"`
}

// InputRequest represents a request for additional input from the user
type InputRequest struct {
	Prompt   string                 `json:"prompt"`            // What to ask the user
	Schema   map[string]interface{} `json:"schema,omitempty"`  // JSON schema for expected input
	Options  []string               `json:"options,omitempty"` // If multiple choice
	Required bool                   `json:"required,omitempty"`
}

// ToolCallResult represents the result of a tool execution
type ToolCallResult struct {
	ToolCallID string                 `json:"toolCallId,omitempty"`
	ToolName   string                 `json:"toolName"`
	Arguments  map[string]interface{} `json:"arguments,omitempty"`
	Result     interface{}            `json:"result,omitempty"`
	Error      string                 `json:"error,omitempty"`
	Duration   int64                  `json:"durationMs,omitempty"`
}

// TokenUsage represents token usage for AI operations
type TokenUsage struct {
	InputTokens  int `json:"inputTokens"`
	OutputTokens int `json:"outputTokens"`
	TotalTokens  int `json:"totalTokens"`
}

// TaskStatus represents the status of an async task
type TaskStatus struct {
	TaskId      string           `json:"taskId"`
	PersonaId   int              `json:"personaId"`
	State       string           `json:"state"`
	Progress    float64          `json:"progress,omitempty"`
	Message     string           `json:"message,omitempty"`
	CreatedAt   time.Time        `json:"createdAt"`
	UpdatedAt   time.Time        `json:"updatedAt"`
	CompletedAt *time.Time       `json:"completedAt,omitempty"`
	Result      *RuntimeResponse `json:"result,omitempty"`
}

// ToolDefinition represents a tool available to the agent
type ToolDefinition struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	InputSchema map[string]interface{} `json:"inputSchema,omitempty"`
	Config      map[string]interface{} `json:"config,omitempty"` // Internal config for execution
}

// TeamExecutionRequest is the request to execute a team of agents
type TeamExecutionRequest struct {
	// Team external ID
	TeamId string `json:"teamId"`

	// Conversation context
	ConversationId int                    `json:"conversationId,omitempty"`
	Message        string                 `json:"message"`
	Context        map[string]interface{} `json:"context,omitempty"`

	// Message history for multi-turn
	History []*ConversationMessage `json:"history,omitempty"`

	// Execution mode: "broadcast", "sequential", "orchestrated"
	Mode string `json:"mode,omitempty"`

	// User context
	UserId int32 `json:"userId,omitempty"`
}

// TeamExecutionResponse is the response from team execution
type TeamExecutionResponse struct {
	// Team ID
	TeamId string `json:"teamId"`

	// Conversation ID
	ConversationId int `json:"conversationId,omitempty"`

	// Combined/summarized response
	FinalResponse string `json:"finalResponse"`

	// Individual responses from each team member
	MemberResponses []*TeamMemberResponse `json:"memberResponses"`

	// Total participants
	Participants int `json:"participants"`

	// Execution time
	DurationMs int64 `json:"durationMs,omitempty"`
}

// TeamMemberResponse is the response from a single team member
type TeamMemberResponse struct {
	PersonaId   int               `json:"personaId"`
	PersonaName string            `json:"personaName"`
	Role        string            `json:"role"` // leader, worker, reviewer
	Response    string            `json:"response"`
	ToolCalls   []*ToolCallResult `json:"toolCalls,omitempty"`
	TokenUsage  *TokenUsage       `json:"tokenUsage,omitempty"`
	Error       string            `json:"error,omitempty"`
}

// ToolCall represents a tool call from the LLM
type ToolCall struct {
	ID        string                 `json:"id"`
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments"`
}

// ToolResult represents the result of executing a tool
type ToolResult struct {
	ToolCallID string      `json:"toolCallId"`
	Content    interface{} `json:"content"`
	IsError    bool        `json:"isError,omitempty"`
}

// ============ Constants ============

const (
	// Task states
	TaskStateCompleted     = "completed"
	TaskStateInputRequired = "input_required"
	TaskStateWorking       = "working"
	TaskStateFailed        = "failed"
	TaskStateCanceled      = "canceled"

	// Message roles
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleSystem    = "system"

	// Special tool names
	ToolAskUser     = "ask_user"     // Built-in tool for requesting input
	ToolRunWorkflow = "run_workflow" // Auto-discovered workflow tool
)
