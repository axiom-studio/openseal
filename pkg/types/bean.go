package types

import "time"

// AgentLibraryBean represents an agent library for API responses
type AgentLibraryBean struct {
	Id          int                        `json:"id,omitempty"`
	Name        string                     `json:"name" validate:"required,min=1,max=200"`
	Description string                     `json:"description,omitempty"`
	Icon        string                     `json:"icon,omitempty"`
	Category    string                     `json:"category,omitempty"`
	Versions    []*AgentLibraryVersionBean `json:"versions,omitempty"`
	UserId      int32                      `json:"-"`
}

// AgentLibraryVersionBean represents a specific version of an agent template
type AgentLibraryVersionBean struct {
	Id             int                    `json:"id,omitempty"`
	AgentLibraryId int                    `json:"agentLibraryId,omitempty"`
	Version        string                 `json:"version" validate:"required,min=1,max=50"`
	Nodes          []*AgentNodeDefinition `json:"nodes" validate:"required,min=1"`
	Connections    []*AgentConnection     `json:"connections,omitempty"`
	InputSchema    map[string]InputField  `json:"inputSchema,omitempty"`
	OutputSchema   map[string]OutputField `json:"outputSchema,omitempty"`
	PersonaConfig  string                 `json:"personaConfig,omitempty"` // JSON: LLM persona config for autonomous agents
	UserId         int32                  `json:"-"`
}

// AgentNodeDefinition defines a single node in the visual workflow
type AgentNodeDefinition struct {
	Id          string                 `json:"id" validate:"required"`
	Name        string                 `json:"name" validate:"required,min=1,max=100"`
	Description string                 `json:"description,omitempty"`
	Type        string                 `json:"type" validate:"required"`
	PositionX   float64                `json:"positionX"`
	PositionY   float64                `json:"positionY"`
	Config      map[string]interface{} `json:"config,omitempty"`
	WorkflowId  *int                   `json:"workflowId,omitempty"`
}

// GetId implements executor.NodeDefinition
func (n *AgentNodeDefinition) GetId() string { return n.Id }

// GetType implements executor.NodeDefinition
func (n *AgentNodeDefinition) GetType() string { return n.Type }

// AgentConnection defines a connection between nodes
type AgentConnection struct {
	Id           string `json:"id" validate:"required"`
	SourceNodeId string `json:"sourceNodeId" validate:"required"`
	TargetNodeId string `json:"targetNodeId" validate:"required"`
	SourceHandle string `json:"sourceHandle,omitempty"`
	TargetHandle string `json:"targetHandle,omitempty"`
	Label        string `json:"label,omitempty"`
	WorkflowId   int    `json:"workflowId,omitempty"`
}

// GetSourceNodeId implements executor.Connection
func (c *AgentConnection) GetSourceNodeId() string { return c.SourceNodeId }

// GetTargetNodeId implements executor.Connection
func (c *AgentConnection) GetTargetNodeId() string { return c.TargetNodeId }

// GetSourceHandle implements executor.Connection
func (c *AgentConnection) GetSourceHandle() string { return c.SourceHandle }

// GetTargetHandle implements executor.Connection
func (c *AgentConnection) GetTargetHandle() string { return c.TargetHandle }

// GetLabel implements executor.Connection
func (c *AgentConnection) GetLabel() string { return c.Label }

// InputField defines an input parameter schema
type InputField struct {
	Type        string `json:"type" validate:"required,oneof=string number boolean object array"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
	Default     any    `json:"default,omitempty"`
	BindingType string `json:"bindingType,omitempty"` // Constrains binding sources: "blueprint", "static", "any", or "" (empty means any)
}

// OutputField defines an output parameter schema
type OutputField struct {
	Type        string `json:"type" validate:"required,oneof=string number boolean object array"`
	Description string `json:"description,omitempty"`
	Template    string `json:"template,omitempty"` // e.g., {{step.output.stepName.field}}
}

// TriggerInputField defines an input that a trigger accepts at runtime
type TriggerInputField struct {
	Type            string           `json:"type" validate:"required,oneof=string number boolean object array file"`
	Description     string           `json:"description,omitempty"`
	Required        bool             `json:"required,omitempty"`
	Default         any              `json:"default,omitempty"`
	FileConstraints *FileConstraints `json:"fileConstraints,omitempty"`
}

// FileConstraints defines constraints for file-type inputs
type FileConstraints struct {
	AllowedMimeTypes []string `json:"allowedMimeTypes,omitempty"`
	MaxFileSizeMB    int      `json:"maxFileSizeMB,omitempty"`
	MaxFiles         int      `json:"maxFiles,omitempty"`
}

// AgentLibraryListBean is a summary view for listing
type AgentLibraryListBean struct {
	Id            int    `json:"id"`
	Name          string `json:"name"`
	Description   string `json:"description,omitempty"`
	Icon          string `json:"icon,omitempty"`
	Category      string `json:"category,omitempty"`
	LatestVersion string `json:"latestVersion,omitempty"`
	VersionCount  int    `json:"versionCount"`
}

// AgentInstanceBean represents a deployed agent instance
type AgentInstanceBean struct {
	Id                    int                    `json:"id,omitempty"`
	AgentLibraryVersionId int                    `json:"agentLibraryVersionId" validate:"required"`
	Name                  string                 `json:"name" validate:"required,min=1,max=200"`
	BlueprintInstanceId   int                    `json:"blueprintInstanceId,omitempty"`
	Bindings              []*AgentBinding        `json:"bindings,omitempty"`
	ResolvedBindings      map[string]interface{} `json:"resolvedBindings,omitempty"`
	EnvironmentId         int                    `json:"environmentId" validate:"required"`
	TeamId                int                    `json:"teamId" validate:"required"`
	Enabled               bool                   `json:"enabled"`
	WebhookPath           string                 `json:"webhookPath,omitempty"`
	WebhookUrl            string                 `json:"webhookUrl,omitempty"`
	CronExpression        string                 `json:"cronExpression,omitempty"`
	Status                string                 `json:"status,omitempty"`
	TimeoutSeconds        int                    `json:"timeoutSeconds,omitempty"`
	PersonaId             *int                   `json:"personaId,omitempty"` // If set, this agent has autonomous capabilities
	UserId                int32                  `json:"-"`
	AgentLibraryName      string                 `json:"agentLibraryName,omitempty"`
	AgentLibraryVersion   string                 `json:"agentLibraryVersion,omitempty"`
	// A2A (Agent-to-Agent) Protocol support
	A2AEnabled      bool                   `json:"a2aEnabled,omitempty"`
	A2AEndpoint     string                 `json:"a2aEndpoint,omitempty"`
	A2ACapabilities *A2ACapabilities       `json:"a2aCapabilities,omitempty"`
	A2AAgentCard    map[string]interface{} `json:"a2aAgentCard,omitempty"`
}

// A2ACapabilities describes A2A protocol capabilities for an agent instance
type A2ACapabilities struct {
	// Whether the agent supports A2A protocol
	Enabled bool `json:"enabled"`
	// Supported A2A methods
	SupportedMethods []string `json:"supportedMethods,omitempty"`
	// Skills exposed via A2A
	Skills []A2ASkill `json:"skills,omitempty"`
	// Authentication requirements
	AuthType string `json:"authType,omitempty"`
}

// A2ASkill describes a skill exposed via A2A
type A2ASkill struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// AgentBinding defines how an agent input is bound to a value source
type AgentBinding struct {
	InputName           string                 `json:"inputName" validate:"required"`
	SourceType          string                 `json:"sourceType" validate:"required,oneof=blueprint static env vault"`
	BlueprintNodeId     int                    `json:"blueprintNodeId,omitempty"`
	BlueprintName       string                 `json:"blueprintName,omitempty"`
	NodeName            string                 `json:"nodeName,omitempty"`
	OutputKey           string                 `json:"outputKey,omitempty"`          // Output key (for both helm-chart and virtual nodes)
	SelectedServiceUrl  string                 `json:"selectedServiceUrl,omitempty"` // Actual service URL from Helm resource tree
	NodeType            string                 `json:"nodeType,omitempty"`           // "helm-chart" or "virtual"
	VirtualOutputs      map[string]interface{} `json:"virtualOutputs,omitempty"`     // Outputs for virtual nodes
	StaticValue         string                 `json:"staticValue,omitempty"`
	EnvVar              string                 `json:"envVar,omitempty"`
	VaultCredentialName string                 `json:"vaultCredentialName,omitempty"` // Credential name in vault
	VaultFieldName      string                 `json:"vaultFieldName,omitempty"`      // Field to extract from credential
}

// DeployAgentRequest is the request to deploy a new agent instance
type DeployAgentRequest struct {
	AgentLibraryVersionId int             `json:"agentLibraryVersionId" validate:"required"`
	Name                  string          `json:"name" validate:"required,min=1,max=200"`
	BlueprintInstanceId   int             `json:"blueprintInstanceId,omitempty"`
	Bindings              []*AgentBinding `json:"bindings,omitempty"`
	EnvironmentId         int             `json:"environmentId" validate:"required"`
	TeamId                int             `json:"teamId" validate:"required"`
	CronExpression        string          `json:"cronExpression,omitempty"`
	TimeoutSeconds        int             `json:"timeoutSeconds,omitempty"`
	UserId                int32           `json:"-"`
}

// AgentInstanceListBean is a summary view for listing instances
type AgentInstanceListBean struct {
	Id                  int      `json:"id"`
	Name                string   `json:"name"`
	AgentLibraryName    string   `json:"agentLibraryName"`
	AgentLibraryVersion string   `json:"agentLibraryVersion"`
	BlueprintInstanceId int      `json:"blueprintInstanceId,omitempty"`
	EnvironmentId       int      `json:"environmentId"`
	EnvironmentName     string   `json:"environmentName,omitempty"`
	TeamId              int      `json:"teamId,omitempty"`
	TeamName            string   `json:"teamName,omitempty"`
	Enabled             bool     `json:"enabled"`
	Status              string   `json:"status"`
	WebhookUrl          string   `json:"webhookUrl,omitempty"`
	TriggerTypes        []string `json:"triggerTypes,omitempty"`
	LastRunAt           string   `json:"lastRunAt,omitempty"`
	LastRunStatus       string   `json:"lastRunStatus,omitempty"`
	RunCount            int      `json:"runCount"`
}

// AgentInstancesListResponse is the paginated response for listing agent instances
type AgentInstancesListResponse struct {
	Instances  []*AgentInstanceListBean `json:"instances"`
	TotalCount int                      `json:"totalCount"`
}

// AgentRunBean represents a single execution of an agent
type AgentRunBean struct {
	Id              int                 `json:"id"`
	AgentInstanceId int                 `json:"agentInstanceId"`
	TriggeredBy     string              `json:"triggeredBy"`
	TriggerData     map[string]any      `json:"triggerData,omitempty"`
	Status          string              `json:"status"`
	Error           string              `json:"error,omitempty"`
	StartedAt       time.Time           `json:"startedAt"`
	CompletedAt     *time.Time          `json:"completedAt,omitempty"`
	Duration        int64               `json:"duration,omitempty"` // milliseconds
	Ephemeral       bool                `json:"ephemeral"`
	StepRuns        []*AgentStepRunBean `json:"stepRuns,omitempty"`
}

// AgentStepRunBean represents a single step execution within a run
type AgentStepRunBean struct {
	Id          int            `json:"id"`
	AgentRunId  int            `json:"agentRunId"`
	StepName    string         `json:"stepName"`
	StepType    string         `json:"stepType"`
	StepIndex   int            `json:"stepIndex"`
	Status      string         `json:"status"`
	Input       map[string]any `json:"input,omitempty"`
	Output      map[string]any `json:"output,omitempty"`
	Error       string         `json:"error,omitempty"`
	StartedAt   *time.Time     `json:"startedAt,omitempty"`
	CompletedAt *time.Time     `json:"completedAt,omitempty"`
	Duration    int64          `json:"duration,omitempty"` // milliseconds
}

// TriggerAgentRequest is the request to manually trigger an agent
type TriggerAgentRequest struct {
	AgentInstanceId int            `json:"agentInstanceId" validate:"required"`
	TriggerNodeId   string         `json:"triggerNodeId,omitempty"`
	WorkflowId      *int           `json:"workflowId,omitempty"`
	TriggeredBy     string         `json:"triggeredBy,omitempty"` // webhook, cron, manual
	TriggerData     map[string]any `json:"triggerData,omitempty"`
	UserId          int32          `json:"-"`
}

// AgentRunListBean is a summary view for listing runs
type AgentRunListBean struct {
	Id          int        `json:"id"`
	TriggeredBy string     `json:"triggeredBy"`
	Status      string     `json:"status"`
	StartedAt   time.Time  `json:"startedAt"`
	CompletedAt *time.Time `json:"completedAt,omitempty"`
	Duration    int64      `json:"duration,omitempty"`
	StepCount   int        `json:"stepCount"`
}

// AgentOutputsBean represents computed outputs for an agent instance
type AgentOutputsBean struct {
	AgentInstanceId int                    `json:"agentInstanceId"`
	AgentName       string                 `json:"agentName"`
	Outputs         map[string]interface{} `json:"outputs"`
}

// Node type constants (triggers)
const (
	NodeTypeWebhook = "webhook"
	NodeTypeCron    = "cron"
	NodeTypeManual  = "manual"
)

// Node type constants (control flow)
const (
	NodeTypeIf     = "if"
	NodeTypeSwitch = "switch"
	NodeTypeDelay  = "delay"
)

// Node type constants (actions)
const (
	NodeTypeHTTP     = "http"
	NodeTypeCode     = "code"
	NodeTypeAI       = "ai"
	NodeTypePGVector = "pgvector"
)

// Node type constants (data)
const (
	NodeTypeTransform = "transform"
	NodeTypeSet       = "set"
	NodeTypeMerge     = "merge"
)

// Step type constants (legacy aliases)
const (
	StepTypeIf        = "if"
	StepTypeSwitch    = "switch"
	StepTypeTransform = "transform"
	StepTypeSet       = "set"
	StepTypeMerge     = "merge"
	StepTypeDelay     = "delay"
	StepTypeCode      = "code"
	StepTypeHTTP      = "http"
	StepTypeAI        = "ai"
)

// Trigger type constants
const (
	TriggerTypeWebhook  = "webhook"
	TriggerTypeSchedule = "schedule"
	TriggerTypeManual   = "manual"
	TriggerTypeEvent    = "event"
)

// Agent instance status constants
const (
	AgentStatusActive = "active"
	AgentStatusPaused = "paused"
	AgentStatusError  = "error"
)

// Agent run status constants
const (
	RunStatusPending   = "pending"
	RunStatusRunning   = "running"
	RunStatusStreaming = "streaming" // Node is actively streaming partial outputs
	RunStatusCompleted = "completed"
	RunStatusFailed    = "failed"
)

// Binding source type constants
const (
	BindingSourceBlueprint = "blueprint"
	BindingSourceStatic    = "static"
	BindingSourceEnv       = "env"
	BindingSourceVault     = "vault"
)

// Agent category constants
const (
	CategoryDataProcessing = "data-processing"
	CategoryMLInference    = "ml-inference"
	CategoryNotifications  = "notifications"
	CategoryIntegration    = "integration"
	CategoryCustom         = "custom"
)

// Skill category constants
const (
	SkillCategoryCore           = "core"
	SkillCategoryInfrastructure = "infrastructure"
	SkillCategoryData           = "data"
	SkillCategoryAI             = "ai"
	SkillCategoryCommunication  = "communication"
	SkillCategoryUtility        = "utility"
)

// TestWorkflowRequest is the request to test a workflow without deployment
type TestWorkflowRequest struct {
	Nodes          []*AgentNodeDefinition `json:"nodes" validate:"required,min=1"`
	Connections    []*AgentConnection     `json:"connections"`
	TestInput      map[string]interface{} `json:"testInput,omitempty"`
	StartNodeId    string                 `json:"startNodeId,omitempty"`
	WorkflowId     *int                   `json:"workflowId,omitempty"`
	TimeoutSeconds int                    `json:"timeoutSeconds,omitempty"`

	// Previous execution state for resume functionality (test-only)
	PreviousNodeOutputs  map[string]interface{}            `json:"previousNodeOutputs,omitempty"`  // nodeId -> output
	PreviousNodeMetadata map[string]map[string]interface{} `json:"previousNodeMetadata,omitempty"` // nodeId -> metadata

	// WARNING: DEPRECATED - VaultCredentials contains PLAINTEXT secrets!
	// This field exposes credentials in NATS messages and OpenSeal memory.
	// Use VaultCredentialRefs instead for secure credential handling.
	// TODO: Remove after migration to credential references (Phase 4)
	VaultCredentials map[string]map[string]interface{} `json:"vaultCredentials,omitempty"`

	// Secure credential references (recommended)
	// Contains only metadata, actual credentials resolved just-in-time
	VaultCredentialRefs []VaultCredentialRef `json:"vaultCredentialRefs,omitempty"`
}

// VaultCredentialRef is a secure reference to a vault credential
// Instead of sending plaintext credentials, we send references that are resolved just-in-time
type VaultCredentialRef struct {
	CredentialID   int    `json:"credentialId"`   // Database ID of the credential
	CredentialName string `json:"credentialName"` // Name for lookup (e.g., "GEMINI_KEY")
	Field          string `json:"field"`          // Field to access (e.g., "value", "apiKey")
	TemplatePath   string `json:"templatePath"`   // Original template (e.g., "{{vault.GEMINI_KEY.value}}")
	TeamID         *int   `json:"teamId"`         // For access control
}

// TestWorkflowResponse is the response from testing a workflow
type TestWorkflowResponse struct {
	Status      string                     `json:"status"` // completed, failed
	Error       string                     `json:"error,omitempty"`
	Duration    int64                      `json:"duration"` // milliseconds
	NodeResults map[string]*TestNodeResult `json:"nodeResults"`
	StartedAt   time.Time                  `json:"startedAt"`
	CompletedAt time.Time                  `json:"completedAt"`
}

// TestNodeResult represents the execution result of a single node
type TestNodeResult struct {
	NodeId      string                 `json:"nodeId"`
	NodeName    string                 `json:"nodeName"`
	NodeType    string                 `json:"nodeType"`
	Status      string                 `json:"status"` // pending, running, streaming, completed, failed, skipped
	Input       map[string]interface{} `json:"input,omitempty"`
	Output      map[string]interface{} `json:"output,omitempty"`
	Partial     interface{}            `json:"partial,omitempty"`  // Incremental output chunk for streaming nodes
	Progress    float64                `json:"progress,omitempty"` // 0.0-1.0 progress indicator for streaming
	Error       string                 `json:"error,omitempty"`
	StartedAt   *time.Time             `json:"startedAt,omitempty"`
	CompletedAt *time.Time             `json:"completedAt,omitempty"`
	Duration    int64                  `json:"duration,omitempty"` // milliseconds
}

// AgentBlueprintConnectionBean represents a connection from agent input to blueprint node output
type AgentBlueprintConnectionBean struct {
	Id              int    `json:"id,omitempty"`
	AgentInstanceId int    `json:"agentInstanceId"`
	BlueprintNodeId int    `json:"blueprintNodeId"`
	InputName       string `json:"inputName"`
	OutputKey       string `json:"outputKey"`
}

// BlueprintAgentInfoBean contains agent information for display in blueprint view
type BlueprintAgentInfoBean struct {
	AgentInstanceId     int                             `json:"agentInstanceId"`
	AgentName           string                          `json:"agentName"`
	AgentLibraryName    string                          `json:"agentLibraryName"`
	AgentLibraryVersion string                          `json:"agentLibraryVersion"`
	Status              string                          `json:"status"`
	Enabled             bool                            `json:"enabled"`
	BlueprintInstanceId int                             `json:"blueprintInstanceId,omitempty"`
	Connections         []*AgentBlueprintConnectionBean `json:"connections"`
}

// BlueprintAgentsResponse contains all agents connected to a blueprint
type BlueprintAgentsResponse struct {
	BlueprintId int                       `json:"blueprintId"`
	Agents      []*BlueprintAgentInfoBean `json:"agents"`
}

// AgentTriggerBean represents a trigger for an agent instance
type AgentTriggerBean struct {
	Id              int                          `json:"id,omitempty"`
	AgentInstanceId int                          `json:"agentInstanceId"`
	NodeId          string                       `json:"nodeId"`
	TriggerType     string                       `json:"triggerType"`
	Config          map[string]interface{}       `json:"config,omitempty"`
	InputSchema     map[string]TriggerInputField `json:"inputSchema,omitempty"`
	WebhookPath     string                       `json:"webhookPath,omitempty"`
	CronExpression  string                       `json:"cronExpression,omitempty"`
	Enabled         bool                         `json:"enabled"`
	WorkflowId      *int                         `json:"workflowId,omitempty"`
}
