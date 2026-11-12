package agentrun

import (
	"context"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/skill"
)

type TurnRunnerBinding struct {
	ProgressAcknowledgementRenderer RunProgressAcknowledgementRenderer
	DeploymentID                    string
	// ActionDeploymentID is the resource whose Skill bindings authorized the
	// projected actions. Team conversations keep the roster Agent as the Turn
	// identity while resolving governed mutations against the owning Team.
	ActionDeploymentID string
	DefinitionID       string
	DefinitionVersion  string
	ModelProvider      string
	Model              string
	ModelActions       []capability.ModelAction
	RunbookOperations  []HostedRunbookOperation
	PreparedRuntimes   []PreparedSkillRuntime
	InputContextRefs   []string
	BudgetReservation  models.BudgetUsage
	// ModelCredential is the deployment's model binding
	// (deploymentModelCredential); kernel-issued model calls outside the
	// step, such as the evidence grounding review, send it. nil on desktop.
	ModelCredential *capability.CredentialReference

	// RunLoop seams (SPEC §2.4).
	Producer    StepProducer
	Session     *models.AgentSession // nil only for producers with no tool calls (routing)
	Preparers   []StepPreparer       // run-start, then conversation
	Tools       LoopToolExecutor     // disp-1
	Compactor   StepCompactor        // G3
	Final       FinalAnswerHandler   // CONV-08 for chats, runOutputFinalHandler otherwise
	FinalOutput FinalOutputPublisher // nil = none
	Host        StepHost
	Authority   ControlAuthority   // section 04 §4, incl. the conversation flags of section 05 §7
	Prompt      PromptContribution // kernel-authored system parts from the owning section
	ToolCheck   ToolCallCheck      // optional (conversation CheckToolCall)
	AfterTool   AfterToolEntryHook // optional (conversation artifact receipts)
}

// PreparedSkillRuntime binds one activation-time immutable runtime to the
// exact Skill binding revision exposed during a Turn. It stays outside the
// model action catalog and is attached only after a proposal selects that
// authorized binding.
type PreparedSkillRuntime struct {
	DeploymentID    string
	BindingID       string
	BindingRevision int64
	SkillID         string
	SkillVersion    string
	Runtime         skill.PreparedRuntime
}

// StepHost runs one model call or one host-native tool for the kernel loop.
// Atlas implements it over HTTP (sentinel → atlas), the OSS daemon in
// process. Neither call executes governed actions: those run inline in the
// kernel.
type StepHost interface {
	ExecuteModelStep(ctx context.Context, req models.ModelStepRequest) (models.ModelStepResult, error)
	ExecuteHostTool(ctx context.Context, req models.HostToolRequest) (models.HostToolResult, error)
}

// HostedAgentTarget is one active, same-scope Agent deployment eligible for
// bounded delegation. ID is the only durable identity; DisplayName and Purpose
// are model-facing discovery labels and are never persisted as substitutes.
type HostedAgentTarget struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Purpose     string `json:"purpose,omitempty"`
}

// HostedRunbookOperation is one immutable deterministic operation the current
// cognitive Agent may invoke. It contains no graph internals or credentials;
// the kernel resolves the exact active definition again at materialization.
type HostedRunbookOperation struct {
	DefinitionID      string                 `json:"-"`
	DefinitionVersion string                 `json:"-"`
	Entrypoint        string                 `json:"entrypoint"`
	Name              string                 `json:"name"`
	Description       string                 `json:"description"`
	InputSchema       map[string]interface{} `json:"inputSchema"`
	OutputSchema      map[string]interface{} `json:"outputSchema,omitempty"`
}
