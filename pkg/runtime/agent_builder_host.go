package runtime

import (
	"context"
	"errors"

	"github.com/axiom-studio/openseal/pkg/skill"
)

// ErrAgentChangeForbidden is what AuthorizeAgentChange returns when the
// person may not change the Agent.
var ErrAgentChangeForbidden = errors.New("the person can't change this Agent")

// AgentBuilderHost is what a host adds to the builder actions: model choice
// and the work that follows a created Agent. A nil host (the OSS daemon)
// refuses a model choice and does nothing after creation.
type AgentBuilderHost interface {
	// ValidateAgentModel runs at proposal time for create_agent, update_agent
	// and try_agent. An error becomes an is_error tool result and no card.
	ValidateAgentModel(ctx context.Context, scope skill.ScopeReference, model string) error
	// AuthorizeAgentChange runs at proposal time for update_agent, before
	// the card shows anything of the target Agent: actingUserID (the Run's
	// ActingUser) must be allowed to change deploymentID. A refusal is
	// ErrAgentChangeForbidden; any error means no card.
	AuthorizeAgentChange(ctx context.Context, scope skill.ScopeReference, actingUserID, deploymentID string) error
	// PrepareAgentModel runs at execution time. For create_agent it runs
	// before the deployment exists (DeploymentID is the id it will have),
	// so a failure leaves no Agent behind. An error fails the call.
	PrepareAgentModel(ctx context.Context, request AgentModelRequest) error
	// AfterAgentCreated runs synchronously after PrepareAgentModel, before
	// the result returns. It is best effort: failures are only logged.
	AfterAgentCreated(ctx context.Context, created AgentCreated)
}

// AgentModelRequest asks the host to bill and route an Agent's model.
type AgentModelRequest struct {
	Scope        skill.ScopeReference
	DeploymentID string
	Model        string // "" = host default
	OwnerUserID  string // the approving person (ApprovalCheckpoint.DecisionBy.ID, type user)
	ActionCallID string
	Updated      bool // update_agent with a changed model
}

// AgentCreated reports an Agent a builder action created or changed.
type AgentCreated struct {
	Scope         Scope
	DeploymentID  string
	DisplayName   string
	Purpose       string
	ActionCallID  string
	RunID         string
	BillingUserID string
	Updated       bool
}

// AgentBuilderCutoverHost converts the retired change-set drafts into
// builder conversations at engine start.
type AgentBuilderCutoverHost interface {
	// EnsureBuilderAgent makes the builder Agent exist and be builder-ready
	// (current definition and bindings with the agent builder actions). It
	// is idempotent and runs before a scope's drafts convert, whether or not
	// the builder already exists.
	EnsureBuilderAgent(ctx context.Context, scope Scope, requesterUserID string) error
	// DraftModel is the model the draft was billed with; "" = default.
	DraftModel(ctx context.Context, scope Scope, idempotencyKey string) (string, error)
	// AfterAgentBuilderCutover runs once, after the cutover table is dropped.
	AfterAgentBuilderCutover(ctx context.Context) error
}

const (
	// AgentTrialModelContextKey holds the drafted model in a try_agent child
	// Run's Context ("" = default). Hosts read it for billing.
	AgentTrialModelContextKey = "_opensealAgentTrialModel"
	// AgentTrialContextKey holds the AgentTrialContext of a try_agent child Run.
	AgentTrialContextKey = "_opensealAgentTrial"
	// AgentTrialWakeType is the wake a parent waits on while its trial runs.
	AgentTrialWakeType = "agent_trial"
)

// AgentTrialContext is the drafted profile a try_agent child Run speaks as.
// Profile carries no model; the model is under AgentTrialModelContextKey.
type AgentTrialContext struct {
	Profile      agentProfileArguments `json:"profile"`
	ParentRunID  string                `json:"parentRunId"`
	ActionCallID string                `json:"actionCallId"`
}
