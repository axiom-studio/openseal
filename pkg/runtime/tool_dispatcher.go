package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/axiom-studio/openseal/pkg/skill"
)

// ToolInvoker is the product-neutral boundary for a registered typed tool.
// Resolved credentials are separate from the model-visible argument envelope.
type ToolInvoker interface {
	InvokeTool(context.Context, string, map[string]interface{}, map[string]string) (map[string]interface{}, error)
}

type ToolInvokerFunc func(context.Context, string, map[string]interface{}, map[string]string) (map[string]interface{}, error)

func (f ToolInvokerFunc) InvokeTool(ctx context.Context, name string, arguments map[string]interface{}, credentials map[string]string) (map[string]interface{}, error) {
	return f(ctx, name, arguments, credentials)
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
	return d.invoker.InvokeTool(ctx, transport.Endpoint, arguments, input.Credentials)
}
