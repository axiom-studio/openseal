package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/axiom-studio/openseal/pkg/skill"
)

// ToolInvocation is the complete, tenant-scoped transport envelope. Resolved
// credentials are deliberately separate from model-visible arguments and must
// never be persisted or logged by an invoker.
type ToolInvocation struct {
	Name         string
	Scope        skill.ScopeReference
	DeploymentID string
	SkillID      string
	SkillVersion string
	Action       string
	ActionCallID string
	RunID        string
	Arguments    map[string]interface{}
	Credentials  map[string]string
}

// ToolInvoker is the product-neutral boundary for a registered typed tool.
type ToolInvoker interface {
	InvokeTool(context.Context, ToolInvocation) (map[string]interface{}, error)
}

type ToolInvokerFunc func(context.Context, ToolInvocation) (map[string]interface{}, error)

func (f ToolInvokerFunc) InvokeTool(ctx context.Context, invocation ToolInvocation) (map[string]interface{}, error) {
	return f(ctx, invocation)
}

// ToolActionDispatcher executes canonical tool transports, including compiled
// OpenClaw raw-command mappings, through an embedding application's tool host.
type ToolActionDispatcher struct {
	invoker ToolInvoker
}

func NewToolActionDispatcher(invoker ToolInvoker) (*ToolActionDispatcher, error) {
	if invoker == nil {
		return nil, errors.New("tool invoker is required")
	}
	return &ToolActionDispatcher{invoker: invoker}, nil
}

func (d *ToolActionDispatcher) DispatchAction(ctx context.Context, input ActionDispatchInput) (map[string]interface{}, error) {
	if d == nil || d.invoker == nil || input.Bound == nil || input.Bound.Definition == nil {
		return nil, errors.New("tool action dispatcher is not configured")
	}
	transport := input.Bound.Definition.Transport
	if input.Bound.Action.Transport != nil {
		transport = *input.Bound.Action.Transport
	}
	if transport.Kind != "tool" || strings.TrimSpace(transport.Endpoint) == "" {
		return nil, fmt.Errorf("unsupported tool transport %q", transport.Kind)
	}
	arguments, err := skill.MaterializeTransportArguments(input.Bound, input.Arguments)
	if err != nil {
		return nil, err
	}
	invocation := ToolInvocation{
		Name: transport.Endpoint, Scope: input.Bound.Binding.Scope, DeploymentID: input.Bound.Binding.DeploymentID,
		SkillID: input.Bound.Definition.ID, SkillVersion: input.Bound.Definition.Version,
		Action: input.Bound.Action.Name, Arguments: arguments, Credentials: input.Credentials,
	}
	if input.Call != nil {
		invocation.ActionCallID = input.Call.ID
		invocation.RunID = input.Call.RunID
	}
	return d.invoker.InvokeTool(ctx, invocation)
}
