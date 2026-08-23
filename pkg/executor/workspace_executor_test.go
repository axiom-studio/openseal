package executor

import (
	"context"
	"testing"
)

type workspaceClientCapture struct{ request WorkspaceActionRequest }

func (c *workspaceClientCapture) ExecuteWorkspaceAction(_ context.Context, request WorkspaceActionRequest) (map[string]interface{}, error) {
	c.request = request
	return map[string]interface{}{"path": "README.md", "content": "hello", "size": 5, "truncated": false}, nil
}

func TestWorkspaceExecutorUsesTrustedBindingAndHidesItFromHostInput(t *testing.T) {
	registry := NewEmptyRegistry()
	client := &workspaceClientCapture{}
	if err := RegisterWorkspaceExecutors(registry, client); err != nil {
		t.Fatal(err)
	}
	executor, err := registry.Get("workspace-read")
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Execute(context.Background(), &StepDefinition{Config: map[string]interface{}{
		workspaceConfigKey: "agent-one:default", "path": "README.md",
	}}, nil)
	if err != nil || result.Output["content"] != "hello" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if client.request.WorkspaceID != "agent-one:default" || client.request.Operation != "read_file" || client.request.Input[workspaceConfigKey] != nil {
		t.Fatalf("request=%#v", client.request)
	}
}

func TestWorkspaceExecutorFailsClosedWithoutBoundWorkspace(t *testing.T) {
	registry := NewEmptyRegistry()
	client := &workspaceClientCapture{}
	if err := RegisterWorkspaceExecutors(registry, client); err != nil {
		t.Fatal(err)
	}
	executor, _ := registry.Get("workspace-read")
	if result, err := executor.Execute(context.Background(), &StepDefinition{Config: map[string]interface{}{"path": "README.md"}}, nil); err == nil || result != nil {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}
