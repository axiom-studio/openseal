package openseal

import (
	"context"
	"errors"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/runtime"
)

type (
	AgentBuilderHost  = runtime.AgentBuilderHost
	AgentModelRequest = runtime.AgentModelRequest
	AgentCreated      = runtime.AgentCreated
)

const AgentTrialModelContextKey = runtime.AgentTrialModelContextKey

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
func (e *Engine) EnsureAgentControlConversation(ctx context.Context, deployment *kernelagent.AgentDeployment) (*runtime.Conversation, error) {
	return e.ensureAgentControlConversation(ctx, deployment)
}

// configureAgentBuilderActions adds the builder validator and wraps every
// action dispatcher with the builder executor. It runs with the Agent
// management actions.
func (e *Engine) configureAgentBuilderActions() error {
	if e.spaces == nil {
		return errors.New("agent builder actions require space storage")
	}
	conversations, _ := e.store.(runtime.ConversationStore)
	validator, err := runtime.NewAgentBuilderActionValidator(e.agents, e.teams, e.skills, e.spaces, conversations, e.store, e.agentBuilderHost)
	if err != nil {
		return err
	}
	e.actionValidators = append(e.actionValidators, validator)
	for index := range e.actionPoolSpecs {
		dispatcher, dispatchErr := runtime.NewAgentBuilderActionDispatcher(e.store, e.agents, e.teams, e.skills, e, e.agentBuilderHost, e.actionPoolSpecs[index].dispatcher)
		if dispatchErr != nil {
			return dispatchErr
		}
		e.actionPoolSpecs[index].dispatcher = dispatcher
	}
	for index := range e.actionSupervisorSpecs {
		dispatcher, dispatchErr := runtime.NewAgentBuilderActionDispatcher(e.store, e.agents, e.teams, e.skills, e, e.agentBuilderHost, e.actionSupervisorSpecs[index].dispatcher)
		if dispatchErr != nil {
			return dispatchErr
		}
		e.actionSupervisorSpecs[index].dispatcher = dispatcher
	}
	return nil
}
