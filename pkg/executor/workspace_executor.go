package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

const workspaceConfigKey = "workspaceId"

// WorkspaceClient is implemented by the embedding host. OpenSeal owns the
// typed action contract while the host owns storage, child-job scheduling,
// isolation, and artifact transport.
type WorkspaceClient interface {
	ExecuteWorkspaceAction(context.Context, WorkspaceActionRequest) (map[string]interface{}, error)
}

type WorkspaceActionRequest struct {
	WorkspaceID string
	Operation   string
	Input       map[string]interface{}
}

type workspaceExecutor struct {
	typeName  string
	operation string
	client    WorkspaceClient
}

func (e *workspaceExecutor) Type() string { return e.typeName }

func (e *workspaceExecutor) Execute(ctx context.Context, step *StepDefinition, resolver TemplateResolver) (*StepResult, error) {
	if e == nil || e.client == nil {
		return nil, errors.New("Workspace client is not configured")
	}
	if step == nil || step.Config == nil {
		return nil, errors.New("Workspace action configuration is required")
	}
	workspaceID, _ := step.Config[workspaceConfigKey].(string)
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID == "" || len(workspaceID) > 128 {
		return nil, errors.New("trusted Workspace binding is required")
	}
	input := make(map[string]interface{}, len(step.Config)-1)
	for key, value := range step.Config {
		if key == workspaceConfigKey {
			continue
		}
		if resolver != nil {
			if text, ok := value.(string); ok {
				value = resolver.ResolveString(text)
			}
		}
		input[key] = value
	}
	result, err := e.client.ExecuteWorkspaceAction(ctx, WorkspaceActionRequest{WorkspaceID: workspaceID, Operation: e.operation, Input: input})
	if err != nil {
		return nil, fmt.Errorf("Workspace %s failed: %w", e.operation, err)
	}
	return &StepResult{Output: result}, nil
}

// RegisterWorkspaceExecutors installs the host-backed portable Workspace
// transports into one worker registry.
func RegisterWorkspaceExecutors(registry *Registry, client WorkspaceClient) error {
	if registry == nil || client == nil {
		return errors.New("Workspace executor registry and client are required")
	}
	for _, item := range []struct{ transport, operation string }{
		{"workspace-list", "list_directory"},
		{"workspace-read", "read_file"},
		{"workspace-search", "search_files"},
		{"workspace-write", "write_file"},
		{"workspace-patch", "apply_patch"},
		{"workspace-run", "run_command"},
	} {
		registry.Register(&workspaceExecutor{typeName: item.transport, operation: item.operation, client: client})
	}
	return nil
}
