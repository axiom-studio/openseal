package runtime

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/axiom-studio/openseal/pkg/skill"
)

// ToolInvocation is the complete, tenant-scoped transport envelope. Resolved
// credentials are deliberately separate from model-visible arguments and must
// never be persisted or logged by an invoker.
type ToolInvocation struct {
	Name  string
	Scope skill.ScopeReference
	// DeploymentID is the Skill binding authority owner. For Team-owned
	// bindings it remains the Team deployment throughout policy, credential,
	// idempotency, and audit handling.
	DeploymentID string
	// ExecutionDeploymentID is the kernel-derived Agent deployment that
	// supplies placement for this invocation. It is never model writable.
	ExecutionDeploymentID string
	SkillID               string
	SkillVersion          string
	Action                string
	ActionCallID          string
	RunID                 string
	Arguments             map[string]interface{}
	Credentials           map[string]string
	CredentialLease       *SignedActionCredentialLease
	CredentialReferences  map[string]skill.CredentialReference
	PreparedRuntime       *skill.PreparedRuntime
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
	transportEndpoint, transportErr := boundActionToolTransport(input.Bound)
	if transportErr != nil {
		return nil, transportErr
	}
	if len(input.Credentials) > 0 && input.CredentialLease != nil {
		return nil, errors.New("plaintext credentials and an opaque credential lease are mutually exclusive")
	}
	if input.CredentialLease != nil {
		if input.Call == nil || input.Run == nil {
			return nil, errors.New("opaque credential lease requires its durable ActionCall and Run")
		}
		if err := MatchActionCredentialLeaseReferences(input.CredentialLease, input.Call, input.Run, transportEndpoint); err != nil {
			return nil, fmt.Errorf("validate opaque credential lease transport: %w", err)
		}
		if !reflect.DeepEqual(input.CredentialReferences, input.Call.CredentialRefs) {
			return nil, errors.New("opaque credential references do not match the durable ActionCall")
		}
	}
	arguments, err := skill.MaterializeTransportArguments(input.Bound, input.Arguments)
	if err != nil {
		return nil, err
	}
	executionDeploymentID := input.Bound.Binding.DeploymentID
	if input.Run != nil {
		if input.Run.Scope != (Scope{Kind: input.Bound.Binding.Scope.Kind, ID: input.Bound.Binding.Scope.ID}) {
			return nil, errors.New("action Run scope does not match the Skill binding")
		}
		if strings.TrimSpace(input.Run.AssignedAgentID) == "" {
			return nil, errors.New("action Run has no assigned execution Agent")
		}
		if input.Run.Owner.Type == OwnerTypeTeam && input.Bound.Binding.DeploymentID != input.Run.Owner.ID && input.Bound.Binding.DeploymentID != input.Run.AssignedAgentID {
			return nil, errors.New("Team Run cannot execute a Skill binding owned outside its Team or assigned Agent")
		}
		executionDeploymentID = input.Run.AssignedAgentID
	}
	invocation := ToolInvocation{
		Name: transportEndpoint, Scope: input.Bound.Binding.Scope, DeploymentID: input.Bound.Binding.DeploymentID,
		ExecutionDeploymentID: executionDeploymentID,
		SkillID:               input.Bound.Definition.ID, SkillVersion: input.Bound.Definition.Version,
		Action: input.Bound.Action.Name, Arguments: arguments, Credentials: input.Credentials,
		CredentialLease: cloneSignedActionCredentialLease(input.CredentialLease), CredentialReferences: cloneCredentialReferences(input.CredentialReferences),
	}
	if input.Call != nil {
		invocation.ActionCallID = input.Call.ID
		invocation.RunID = input.Call.RunID
		invocation.PreparedRuntime = clonePreparedRuntime(input.Call.PreparedRuntime)
	}
	return d.invoker.InvokeTool(ctx, invocation)
}

func boundActionToolTransport(bound *skill.BoundAction) (string, error) {
	if bound == nil || bound.Definition == nil {
		return "", errors.New("bound action is required")
	}
	transport := bound.Definition.Transport
	if bound.Action.Transport != nil {
		transport = *bound.Action.Transport
	}
	endpoint := strings.TrimSpace(transport.Endpoint)
	if transport.Kind != "tool" || endpoint == "" {
		return "", fmt.Errorf("unsupported tool transport %q", transport.Kind)
	}
	return endpoint, nil
}
