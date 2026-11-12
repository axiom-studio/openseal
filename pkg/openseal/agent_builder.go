package openseal

import (
	"context"
	"errors"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/agentbuilder"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
)

type (
	AgentBuilderHost               = agentbuilder.AgentBuilderHost
	AgentModelRequest              = agentbuilder.AgentModelRequest
	AgentCreated                   = agentbuilder.AgentCreated
	AgentBuilderDraft              = agentbuilder.AgentBuilderDraft
	AgentBuilderDraftStatus        = agentbuilder.AgentBuilderDraftStatus
	AgentBuilderDraftFilter        = agentbuilder.AgentBuilderDraftFilter
	AgentBuilderApprovalAuthorizer = action.AgentBuilderApprovalAuthorizer
	StartAgentBuilderRequest       = models.StartAgentBuilderRequest
	StartAgentBuilderResult        = agentbuilder.StartAgentBuilderResult
)

const (
	AgentTrialModelContextKey = agentbuilder.AgentTrialModelContextKey
	AgentBuilderDeploymentID  = conversationrun.AgentBuilderDeploymentID
)

var ErrAgentBuilderApprovalForbidden = action.ErrAgentBuilderApprovalForbidden

// StartAgentBuilderConversation opens a builder conversation owned by the
// builder Agent and posts the person's request as its first message, with
// name and model as builder parameters (never in the text). The conversation
// scheduler starts the builder's Run. A repeated idempotency key replays.
func (e *Engine) StartAgentBuilderConversation(ctx context.Context, req StartAgentBuilderRequest) (*StartAgentBuilderResult, error) {
	if e.conversations == nil {
		return nil, errors.New("conversation store is not configured")
	}
	return agentbuilder.StartAgentBuilderConversation(ctx, e, req)
}

// ListAgentBuilderDrafts lists the viewer's builder conversations that have
// not produced their Agent or Team yet, with the drafts rail status (D5).
func (e *Engine) ListAgentBuilderDrafts(ctx context.Context, filter AgentBuilderDraftFilter) ([]AgentBuilderDraft, error) {
	if e.conversations == nil {
		return nil, errors.New("conversation store is not configured")
	}
	return agentbuilder.ListAgentBuilderDrafts(ctx, e, filter)
}

// WithAgentBuilderHost adds the host's model choice and post-creation work to
// the builder actions (create_agent, update_agent, try_agent). Without it a
// model choice is refused and nothing runs after creation.
func WithAgentBuilderHost(host AgentBuilderHost) Option {
	return func(e *Engine) error {
		if host == nil {
			return errors.New("agent builder host is required")
		}
		e.agentBuilderHost = host
		return nil
	}
}

// EnsureAgentControlConversation returns the Agent's control conversation,
// creating it when it is missing.
func (e *Engine) EnsureAgentControlConversation(ctx context.Context, deployment *kernelagent.AgentDeployment) (*models.Conversation, error) {
	return e.ensureAgentControlConversation(ctx, deployment, "")
}

// configureAgentBuilderActions adds the builder validator and wraps every
// action dispatcher with the builder executor. It runs with the Agent
// management actions.
func (e *Engine) configureAgentBuilderActions() error {
	if e.spaces == nil {
		return errors.New("agent builder actions require space storage")
	}
	conversations, _ := e.store.(models.ConversationStore)
	validator, err := agentbuilder.NewAgentBuilderActionValidator(e.agents, e.teams, e.skills, e.spaces, conversations, e.store, e.agentBuilderHost)
	if err != nil {
		return err
	}
	e.actionValidators = append(e.actionValidators, validator)
	installer, err := agentbuilder.NewAgentInstaller(e.store, e.agents, e.teams, e.skills, e, e.agentBuilderHost)
	if err != nil {
		return err
	}
	e.agentInstaller = installer
	for index := range e.actionPoolSpecs {
		dispatcher, dispatchErr := agentbuilder.NewAgentBuilderActionDispatcher(installer, e.actionPoolSpecs[index].dispatcher)
		if dispatchErr != nil {
			return dispatchErr
		}
		e.actionPoolSpecs[index].dispatcher = dispatcher
	}
	for index := range e.actionSupervisorSpecs {
		dispatcher, dispatchErr := agentbuilder.NewAgentBuilderActionDispatcher(installer, e.actionSupervisorSpecs[index].dispatcher)
		if dispatchErr != nil {
			return dispatchErr
		}
		e.actionSupervisorSpecs[index].dispatcher = dispatcher
	}
	return nil
}
