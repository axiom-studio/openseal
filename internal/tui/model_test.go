package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/runtime"
)

type fakeKernelClient struct {
	document       kernelapi.CapabilityDocument
	runs           []*runtime.AgentRun
	createErrors   []error
	createKeys     []string
	createRequests []kernelapi.CreateAgentRunRequest
	commands       []kernelapi.AgentRunCommandRequest
}

func (f *fakeKernelClient) Capabilities(context.Context) (kernelapi.CapabilityDocument, error) {
	return f.document, nil
}

func (f *fakeKernelClient) CreateAgentRun(_ context.Context, request kernelapi.CreateAgentRunRequest, key string) (*runtime.AgentRunCommandResult, error) {
	f.createKeys = append(f.createKeys, key)
	f.createRequests = append(f.createRequests, request)
	if len(f.createErrors) > 0 {
		err := f.createErrors[0]
		f.createErrors = f.createErrors[1:]
		if err != nil {
			return nil, err
		}
	}
	run := testRun("created", runtime.AgentRunStatusQueued, 1)
	f.runs = append([]*runtime.AgentRun{run}, f.runs...)
	return &runtime.AgentRunCommandResult{Run: run}, nil
}

func (f *fakeKernelClient) ListAgentRuns(context.Context, runtime.AgentRunFilter) ([]*runtime.AgentRun, error) {
	return f.runs, nil
}

func (f *fakeKernelClient) GetAgentRun(context.Context, runtime.Scope, string) (*runtime.AgentRun, error) {
	if len(f.runs) == 0 {
		return nil, runtime.ErrRunNotFound
	}
	return f.runs[0], nil
}

func (f *fakeKernelClient) CommandAgentRun(_ context.Context, _ runtime.Scope, id string, request kernelapi.AgentRunCommandRequest) (*runtime.AgentRunCommandResult, error) {
	f.commands = append(f.commands, request)
	status := runtime.AgentRunStatusPaused
	if request.Kind == runtime.AgentRunCommandResume {
		status = runtime.AgentRunStatusQueued
	} else if request.Kind == runtime.AgentRunCommandCancel {
		status = runtime.AgentRunStatusCanceled
	}
	run := testRun(id, status, request.ExpectedRevision+1)
	return &runtime.AgentRunCommandResult{Run: run}, nil
}

func TestModelDiscoversCapabilitiesBeforeRenderingActions(t *testing.T) {
	limited := kernelapi.Capability{
		ID: kernelapi.AgentRunsCapabilityID, Version: kernelapi.AgentRunsCapabilityVersion,
		Available: true, Operations: []string{kernelapi.OperationCreate, kernelapi.OperationList},
	}
	fake := &fakeKernelClient{
		document: kernelapi.CapabilityDocument{Version: kernelapi.Version, Capabilities: []kernelapi.Capability{limited}},
		runs:     []*runtime.AgentRun{testRun("run-1", runtime.AgentRunStatusQueued, 1)},
	}
	model := newTestModel(t, fake)
	applyCommand(t, model, model.loadCapabilities())
	view := model.View().Content
	if !strings.Contains(view, "Current work") || !strings.Contains(view, "First durable outcome") {
		t.Fatalf("work view missing canonical run:\n%s", view)
	}
	for _, action := range []string{"p pause", "g guide", "x stop"} {
		if strings.Contains(view, action) {
			t.Fatalf("unadvertised action %q was rendered:\n%s", action, view)
		}
	}
}

func TestCreateRetryPreservesIdempotencyAndPrompt(t *testing.T) {
	fake := &fakeKernelClient{
		document:     kernelapi.Capabilities(),
		createErrors: []error{errors.New("temporary disconnect"), nil},
	}
	model := newTestModel(t, fake)
	model.ready = true
	model.capability = kernelapi.AgentRunsCapability()
	model.editor.SetValue("Monitor Kubernetes events and remediate safely")

	applyCommand(t, model, model.submitRun())
	if model.editor.Value() == "" || model.pendingKey == "" {
		t.Fatal("failed creation did not preserve prompt and idempotency key")
	}
	applyCommand(t, model, model.submitRun())
	if len(fake.createKeys) != 2 || fake.createKeys[0] == "" || fake.createKeys[0] != fake.createKeys[1] {
		t.Fatalf("idempotency keys = %#v", fake.createKeys)
	}
	if model.editor.Value() != "" || model.pendingKey != "" {
		t.Fatal("successful creation did not clear composer state")
	}
	if fake.createRequests[0].AssignedAgentID != "operator" || fake.createRequests[0].Source != runtime.RunSourceManual {
		t.Fatalf("create request = %#v", fake.createRequests[0])
	}
}

func TestLifecycleCommandUsesSelectedRevisionAndAdvertisedOperation(t *testing.T) {
	fake := &fakeKernelClient{document: kernelapi.Capabilities()}
	model := newTestModel(t, fake)
	model.ready = true
	model.capability = kernelapi.AgentRunsCapability()
	model.runs = []*runtime.AgentRun{testRun("run-7", runtime.AgentRunStatusRunning, 7)}
	model.selectedID = "run-7"

	applyCommand(t, model, model.pauseOrResume())
	if len(fake.commands) != 1 {
		t.Fatalf("commands = %#v", fake.commands)
	}
	command := fake.commands[0]
	if command.Kind != runtime.AgentRunCommandPause || command.ExpectedRevision != 7 {
		t.Fatalf("command = %#v", command)
	}

	model.capability.Operations = []string{kernelapi.OperationList}
	if command := model.commandSelected(runtime.AgentRunCommandCancel, ""); command != nil {
		t.Fatal("TUI dispatched an unadvertised cancel operation")
	}
}

func TestContractMismatchFailsClosed(t *testing.T) {
	fake := &fakeKernelClient{document: kernelapi.CapabilityDocument{Version: "99"}}
	model := newTestModel(t, fake)
	applyCommand(t, model, model.loadCapabilities())
	if model.ready || !strings.Contains(model.View().Content, "Capability unavailable") {
		t.Fatalf("mismatch did not fail closed:\n%s", model.View().Content)
	}
}

func newTestModel(t *testing.T, fake *fakeKernelClient) *Model {
	t.Helper()
	config := DefaultConfig()
	config.PollInterval = -1
	model, err := NewModel(context.Background(), fake, config)
	if err != nil {
		t.Fatal(err)
	}
	model.width, model.height = 120, 36
	return model
}

func applyCommand(t *testing.T, model *Model, command tea.Cmd) {
	t.Helper()
	if command == nil {
		t.Fatal("expected command")
	}
	message := command()
	updated, followup := model.Update(message)
	if updated != model {
		t.Fatal("model identity changed unexpectedly")
	}
	if followup != nil {
		second := followup()
		if second != nil {
			model.Update(second)
		}
	}
}

func testRun(id string, status runtime.AgentRunStatus, revision int64) *runtime.AgentRun {
	return &runtime.AgentRun{
		ID: id, Scope: runtime.Scope{Kind: "local", ID: "default"},
		Owner:           runtime.ObjectiveOwner{Type: runtime.OwnerTypeAgent, ID: "operator"},
		AssignedAgentID: "operator", Goal: "First durable outcome", Source: runtime.RunSourceManual,
		Status: status, Revision: revision, UpdatedAt: time.Now(),
	}
}
