package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
	"github.com/axiom-studio/openseal/pkg/workspace"
)

func TestHostToolSpecsChatWorkspaceOffersTenToolsWithoutBatchRead(t *testing.T) {
	authority := workspace.ChatAuthority("conv_1")
	specs := modelstep.HostToolSpecs(authority)
	want := []string{
		workspace.OperationListDirectory, workspace.OperationReadFile, workspace.OperationSearchFiles,
		workspace.OperationWriteFile, workspace.OperationEditFile, workspace.OperationDeleteFile,
		workspace.OperationMoveFile, workspace.OperationRunCommand, workspace.OperationListChats,
		workspace.OperationCopyFromChat,
	}
	if len(specs) != len(want) {
		t.Fatalf("got %d specs, want %d", len(specs), len(want))
	}
	operations := workspace.Operations(authority)
	byName := map[string]workspace.Operation{}
	for _, operation := range operations {
		byName[operation.Name] = operation
	}
	for i, spec := range specs {
		if spec.Operation != want[i] {
			t.Fatalf("spec %d = %s, want %s", i, spec.Operation, want[i])
		}
		if spec.Operation == workspace.OperationBatchRead {
			t.Fatal("batch_read is offered")
		}
		if spec.ToolName != strings.ReplaceAll(spec.Operation, ".", "_") || strings.Contains(spec.ToolName, ".") {
			t.Fatalf("tool name %q for %s", spec.ToolName, spec.Operation)
		}
		operation := byName[spec.Operation]
		if spec.Description != operation.Description || spec.Description == "" {
			t.Fatalf("description of %s is not the catalog's", spec.Operation)
		}
		got, _ := json.Marshal(spec.InputSchema)
		expected, _ := json.Marshal(operation.InputSchema)
		if string(got) != string(expected) {
			t.Fatalf("schema of %s is not the catalog's", spec.Operation)
		}
	}
}

func TestHostToolSpecsNilAuthorityOffersNone(t *testing.T) {
	if specs := modelstep.HostToolSpecs(nil); specs != nil {
		t.Fatalf("nil authority offered %d tools", len(specs))
	}
	if specs := modelstep.HostToolSpecs(&workspace.Authority{}); specs != nil {
		t.Fatalf("invalid authority offered %d tools", len(specs))
	}
}

func TestHostToolSpecsSideEffectsAndConcurrency(t *testing.T) {
	reads := map[string]bool{
		workspace.OperationListDirectory: true, workspace.OperationReadFile: true,
		workspace.OperationSearchFiles: true, workspace.OperationListChats: true,
	}
	for _, spec := range modelstep.HostToolSpecs(workspace.ChatAuthority("conv_1")) {
		if reads[spec.Operation] {
			if spec.SideEffect != capability.SideEffectRead || !spec.Concurrent {
				t.Fatalf("%s: side effect %s concurrent %v, want read and concurrent", spec.Operation, spec.SideEffect, spec.Concurrent)
			}
			continue
		}
		if spec.SideEffect != capability.SideEffectWrite || spec.Concurrent {
			t.Fatalf("%s: side effect %s concurrent %v, want write and serial", spec.Operation, spec.SideEffect, spec.Concurrent)
		}
	}
}

func TestLookupHostToolByProviderNameAndOperation(t *testing.T) {
	specs := modelstep.HostToolSpecs(workspace.ChatAuthority("conv_1"))
	byName, ok := modelstep.LookupHostTool(specs, "workspace_run_command")
	if !ok || byName.Operation != workspace.OperationRunCommand {
		t.Fatalf("lookup by tool name = %+v, %v", byName, ok)
	}
	byOperation, ok := modelstep.LookupHostTool(specs, workspace.OperationReadFile)
	if !ok || byOperation.ToolName != "workspace_read_file" {
		t.Fatalf("lookup by operation = %+v, %v", byOperation, ok)
	}
	for _, name := range []string{"", workspace.OperationBatchRead, "workspace_batch_read", "gmail.send"} {
		if _, ok := modelstep.LookupHostTool(specs, name); ok {
			t.Fatalf("lookup %q found a host tool", name)
		}
	}
	if _, ok := modelstep.LookupHostTool(nil, "workspace_read_file"); ok {
		t.Fatal("lookup in no specs found a host tool")
	}
}

type fakeChatWorkspaces struct {
	conversationID string
	err            error
	calls          int
}

func (f *fakeChatWorkspaces) PrepareChatWorkspace(context.Context, *models.AgentRun) (string, error) {
	f.calls++
	return f.conversationID, f.err
}

func TestNoChatWorkspacesResolverOffersNoHostTools(t *testing.T) {
	run := &models.AgentRun{ID: "run_1", Context: map[string]interface{}{"conversationId": "conv_1"}}
	authority, err := modelstep.PrepareChatWorkspace(context.Background(), nil, run)
	if err != nil || authority != nil || modelstep.HostToolSpecs(authority) != nil {
		t.Fatalf("no resolver gave authority %+v, err %v", authority, err)
	}

	noChat := &fakeChatWorkspaces{}
	authority, err = modelstep.PrepareChatWorkspace(context.Background(), noChat, run)
	if err != nil || authority != nil || noChat.calls != 1 {
		t.Fatalf("no chat gave authority %+v, err %v, calls %d", authority, err, noChat.calls)
	}

	failing := &fakeChatWorkspaces{conversationID: "conv_1", err: errors.New("chat files down")}
	authority, err = modelstep.PrepareChatWorkspace(context.Background(), failing, run)
	if err == nil || authority != nil {
		t.Fatalf("resolver error gave authority %+v, err %v", authority, err)
	}

	chat := &fakeChatWorkspaces{conversationID: " conv_root "}
	authority, err = modelstep.PrepareChatWorkspace(context.Background(), chat, run)
	if err != nil || authority == nil || authority.ConversationID != "conv_root" {
		t.Fatalf("chat gave authority %+v, err %v", authority, err)
	}
	if len(modelstep.HostToolSpecs(authority)) != 10 {
		t.Fatal("chat Workspace does not offer the host tools")
	}
}

func validHostToolRequest(operation string) models.HostToolRequest {
	return models.HostToolRequest{
		APIVersion: models.HostToolAPIVersion, InvocationID: "ent_1", Scope: models.Scope{Kind: "tenant", ID: "42"},
		RunID: "run_1", StepID: "turn_1", SessionID: "ses_1", AgentID: "dep_1",
		DefinitionID: "def_1", DefinitionVersion: "7", ToolEntryID: "ent_1", ToolCallID: "toolu_1",
		Operation: operation, Arguments: map[string]interface{}{"path": "a.txt"},
		Workspace: workspace.Authority{ConversationID: "conv_1"},
	}
}

func TestHostToolRequestValidate(t *testing.T) {
	if err := validHostToolRequest(workspace.OperationReadFile).Validate(); err != nil {
		t.Fatalf("valid read: %v", err)
	}
	command := validHostToolRequest(workspace.OperationRunCommand)
	command.CommandRef = models.HostToolCommandRef(command.ToolEntryID)
	command.VMTools = []models.HostVMTool{{Name: "gmail.send", Risk: capability.RiskLevelExternal, InputSchema: map[string]interface{}{"type": "object"}}}
	if err := command.Validate(); err != nil {
		t.Fatalf("valid run_command: %v", err)
	}
	if command.CommandRef != "command:ent_1" {
		t.Fatalf("command ref %q", command.CommandRef)
	}

	cases := map[string]func(*models.HostToolRequest){
		"api version":      func(r *models.HostToolRequest) { r.APIVersion = "openseal.host-tool/v0" },
		"identity":         func(r *models.HostToolRequest) { r.InvocationID = "ent_2" },
		"run":              func(r *models.HostToolRequest) { r.RunID = "" },
		"step":             func(r *models.HostToolRequest) { r.StepID = " " },
		"agent":            func(r *models.HostToolRequest) { r.AgentID = "" },
		"operation":        func(r *models.HostToolRequest) { r.Operation = "" },
		"scope":            func(r *models.HostToolRequest) { r.Scope = models.Scope{} },
		"workspace":        func(r *models.HostToolRequest) { r.Workspace = workspace.Authority{} },
		"ref on read":      func(r *models.HostToolRequest) { r.CommandRef = "command:ent_1" },
		"vm tools on read": func(r *models.HostToolRequest) { r.VMTools = []models.HostVMTool{{Name: "gmail.send"}} },
		"long command ref": func(r *models.HostToolRequest) {
			r.Operation = workspace.OperationRunCommand
			r.CommandRef = strings.Repeat("x", 129)
		},
		"unnamed vm tool": func(r *models.HostToolRequest) {
			r.Operation = workspace.OperationRunCommand
			r.VMTools = []models.HostVMTool{{}}
		},
		"duplicate vm tool": func(r *models.HostToolRequest) {
			r.Operation = workspace.OperationRunCommand
			r.VMTools = []models.HostVMTool{{Name: "a"}, {Name: "a"}}
		},
	}
	for name, mutate := range cases {
		request := validHostToolRequest(workspace.OperationReadFile)
		mutate(&request)
		err := request.Validate()
		var failure *models.HostToolFailure
		if !errors.As(err, &failure) || failure.Code != models.HostToolFailureInvalidRequest || !failure.NotStarted {
			t.Fatalf("%s: err %v, want invalid request not started", name, err)
		}
	}
}

func TestHostToolResultValidate(t *testing.T) {
	request := validHostToolRequest(workspace.OperationReadFile)
	result := models.HostToolResult{
		APIVersion: models.HostToolAPIVersion, InvocationID: "ent_1",
		Content: []models.ContentPart{{Type: models.ContentPartJSON, Value: json.RawMessage(`{"content":"hi"}`)}},
	}
	if err := result.Validate(request); err != nil {
		t.Fatalf("valid result: %v", err)
	}
	encoded, err := json.Marshal(result)
	if err != nil || !strings.Contains(string(encoded), `"content":[{"type":"json","value":{"content":"hi"}}]`) {
		t.Fatalf("encoded %s, %v", encoded, err)
	}

	wrongVersion := result
	wrongVersion.APIVersion = ""
	if wrongVersion.Validate(request) == nil {
		t.Fatal("result without apiVersion accepted")
	}
	wrongInvocation := result
	wrongInvocation.InvocationID = "ent_2"
	if wrongInvocation.Validate(request) == nil {
		t.Fatal("result for another invocation accepted")
	}
	empty := result
	empty.Content = nil
	if empty.Validate(request) == nil {
		t.Fatal("result without content accepted")
	}
	oversized := result
	oversized.Content = []models.ContentPart{{Type: models.ContentPartText, Text: strings.Repeat("x", models.MaxToolResultContentBytes+1)}}
	if err := oversized.Validate(request); !errors.Is(err, models.ErrSessionEntryTooLarge) {
		t.Fatalf("oversized result: %v", err)
	}
	inline := result
	inline.Content = []models.ContentPart{{Type: models.ContentPartImage, MediaType: "image/png", DataBase64: "AAAA"}}
	if err := inline.Validate(request); !errors.Is(err, models.ErrInvalidSessionEntry) {
		t.Fatalf("inline image result: %v", err)
	}
}

func TestHostToolFailureError(t *testing.T) {
	failure := &models.HostToolFailure{Code: models.HostToolFailureUnavailable, Message: "draining", NotStarted: true}
	if failure.Error() != "host_tool_unavailable: draining" {
		t.Fatalf("error %q", failure.Error())
	}
	if (&models.HostToolFailure{Code: models.HostToolFailureFailed}).Error() != models.HostToolFailureFailed {
		t.Fatal("code-only error")
	}
	var nilFailure *models.HostToolFailure
	if nilFailure.Error() == "" {
		t.Fatal("nil failure has no text")
	}
}
