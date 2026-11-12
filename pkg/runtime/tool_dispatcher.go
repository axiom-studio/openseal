package runtime

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"strings"

	"github.com/axiom-studio/openseal/pkg/fileinput"
	"github.com/axiom-studio/openseal/pkg/skill"
)

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
	if d == nil || d.invoker == nil || input.Bound == nil || input.Bound.Definition == nil || input.Bound.Binding == nil {
		return nil, errors.New("tool action dispatcher is not configured")
	}
	transportEndpoint, transportErr := boundActionToolTransport(input.Bound)
	if transportErr != nil {
		return nil, transportErr
	}
	sourceIdentity := skill.DefinitionSourceIdentity(input.Bound.Definition)
	// A legacy unqualified binding may resolve to one unique source through
	// the Catalog. The resolved definition remains authoritative in that case.
	if input.Bound.Binding.SourceIdentity != "" && input.Bound.Binding.SourceIdentity != sourceIdentity {
		return nil, errors.New("Skill binding source does not match its bound definition")
	}
	definitionDigest, err := skill.DefinitionDigest(input.Bound.Definition)
	if err != nil {
		return nil, fmt.Errorf("identify bound Skill definition: %w", err)
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
	fileReferences, err := skill.FileReferences(input.Bound, input.Arguments)
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
		// A Run executes its acting Agent's bindings, plus its owning Team's.
		if owner := input.Bound.Binding.DeploymentID; owner != input.Run.AssignedAgentID && !(input.Run.Owner.Type == OwnerTypeTeam && owner == input.Run.Owner.ID) {
			return nil, errors.New("Run cannot execute a Skill binding owned outside its acting Agent or owning Team")
		}
		executionDeploymentID = input.Run.AssignedAgentID
	}
	invocation := ToolInvocation{
		Name: transportEndpoint, Scope: input.Bound.Binding.Scope, DeploymentID: input.Bound.Binding.DeploymentID,
		ExecutionDeploymentID: executionDeploymentID,
		SkillID:               input.Bound.Definition.ID, SkillVersion: input.Bound.Definition.Version,
		SkillSourceIdentity: sourceIdentity, SkillDefinitionDigest: definitionDigest,
		Action: input.Bound.Action.Name, Arguments: arguments, FileArguments: fileinput.Properties(input.Bound.Action.InputSchema),
		FileReferences: fileReferences, OutputSchema: maps.Clone(input.Bound.Action.OutputSchema), BindingConfig: maps.Clone(input.Bound.Binding.Config), Credentials: input.Credentials,
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
