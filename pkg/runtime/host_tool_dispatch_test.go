package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/workspace"
)

// fakeHostToolHost answers ExecuteHostTool with scripted replies, one per call.
type fakeHostToolHost struct {
	mu       sync.Mutex
	requests []HostToolRequest
	replies  []func(context.Context, HostToolRequest) (HostToolResult, error)
}

func (h *fakeHostToolHost) ExecuteHostTool(ctx context.Context, request HostToolRequest) (HostToolResult, error) {
	h.mu.Lock()
	h.requests = append(h.requests, request)
	index := len(h.requests) - 1
	h.mu.Unlock()
	if index >= len(h.replies) {
		return HostToolResult{}, errors.New("unexpected host tool call")
	}
	return h.replies[index](ctx, request)
}

func hostReplyJSON(value interface{}, isError bool) func(context.Context, HostToolRequest) (HostToolResult, error) {
	return func(_ context.Context, request HostToolRequest) (HostToolResult, error) {
		encoded, _ := json.Marshal(value)
		return HostToolResult{APIVersion: HostToolAPIVersion, InvocationID: request.InvocationID,
			Content: []ContentPart{{Type: ContentPartJSON, Value: encoded}}, IsError: isError}, nil
	}
}

func hostReplyError(err error) func(context.Context, HostToolRequest) (HostToolResult, error) {
	return func(context.Context, HostToolRequest) (HostToolResult, error) { return HostToolResult{}, err }
}

type hostToolDispatchFixture struct {
	host       *fakeHostToolHost
	actions    *ActionCoordinator
	dispatcher *HostToolDispatcher
	slept      []time.Duration
}

func newHostToolDispatchFixture(replies ...func(context.Context, HostToolRequest) (HostToolResult, error)) *hostToolDispatchFixture {
	f := &hostToolDispatchFixture{host: &fakeHostToolHost{replies: replies}, actions: NewActionCoordinator(nil, nil, nil, nil, nil)}
	f.dispatcher = NewHostToolDispatcher(f.host, f.actions, nil)
	f.dispatcher.sleep = func(ctx context.Context, wait time.Duration) error {
		f.slept = append(f.slept, wait)
		return ctx.Err()
	}
	return f
}

func hostToolSpec(t *testing.T, operation string) HostToolSpec {
	t.Helper()
	spec, ok := LookupHostTool(HostToolSpecs(workspace.ChatAuthority("conv_1")), operation)
	if !ok {
		t.Fatalf("no host tool %s", operation)
	}
	return spec
}

func hostToolCallFixture(t *testing.T, operation string, arguments map[string]interface{}) HostToolCall {
	t.Helper()
	scope := Scope{Kind: "tenant", ID: "42"}
	expires := time.Now().Add(time.Hour)
	return HostToolCall{
		Run: &AgentRun{ID: "run_1", Scope: scope, Status: AgentRunStatusRunning, AssignedAgentID: "agent_1",
			LeaseOwner: "worker_1", LeaseExpiresAt: &expires},
		Step: &AgentTurn{ID: "step_1", Scope: scope, RunID: "run_1", Status: AgentTurnStatusRunning,
			LeaseOwner: "worker_1", LeaseExpiresAt: &expires},
		Binding: &TurnRunnerBinding{DefinitionID: "def_1", DefinitionVersion: "3", ModelActions: []capability.ModelAction{
			{Name: "zeta.send", Description: "Send", Risk: capability.RiskLevel("high"), InputSchema: map[string]interface{}{"type": "object", "required": []interface{}{"to"}}},
			{Name: "alpha.lookup", Description: "Look up", Risk: capability.RiskLevel("low")},
		}},
		SessionID: "ses_1", ToolEntryID: "ent_1", ToolCallID: "toolu_1",
		Spec: hostToolSpec(t, operation), Arguments: arguments,
		Workspace: *workspace.ChatAuthority("conv_1"),
	}
}

func hostToolErrorText(t *testing.T, result *ToolResult) string {
	t.Helper()
	if result == nil || len(result.Content) != 1 || result.Content[0].Type != ContentPartJSON {
		t.Fatalf("result = %#v, want one json part", result)
	}
	var value struct{ Error string }
	if err := json.Unmarshal(result.Content[0].Value, &value); err != nil {
		t.Fatal(err)
	}
	return value.Error
}

func runHostTool(t *testing.T, f *hostToolDispatchFixture, call HostToolCall) HostToolOutcome {
	t.Helper()
	outcome, ok := f.dispatcher.Run(context.Background(), call)
	if !ok {
		t.Fatal("Run returned ok=false")
	}
	return outcome
}

func TestHostToolReadCompleted(t *testing.T) {
	f := newHostToolDispatchFixture(hostReplyJSON(map[string]interface{}{"content": "hello"}, false))
	call := hostToolCallFixture(t, workspace.OperationReadFile, map[string]interface{}{"path": "a.txt"})
	outcome := runHostTool(t, f, call)
	if outcome.State != ToolEntryCompleted || outcome.Result.Status != ToolResultSucceeded || outcome.Result.IsError {
		t.Fatalf("outcome = %#v", outcome)
	}
	if got := string(outcome.Result.Content[0].Value); got != `{"content":"hello"}` {
		t.Fatalf("content = %s", got)
	}
	request := f.host.requests[0]
	want := HostToolRequest{
		APIVersion: HostToolAPIVersion, InvocationID: "ent_1", Scope: call.Run.Scope, RunID: "run_1", StepID: "step_1",
		SessionID: "ses_1", AgentID: "agent_1", DefinitionID: "def_1", DefinitionVersion: "3", ToolEntryID: "ent_1",
		ToolCallID: "toolu_1", Operation: workspace.OperationReadFile, Arguments: map[string]interface{}{"path": "a.txt"},
		Workspace: workspace.Authority{ConversationID: "conv_1"},
	}
	if !reflect.DeepEqual(request, want) {
		t.Fatalf("request = %#v\nwant %#v", request, want)
	}
	if outcome.OriginRef != "" || outcome.PinnedRun != nil {
		t.Fatalf("read carries command fields: %#v", outcome)
	}
	patch, err := outcome.KernelPatch()
	if err != nil || patch.ResultDigest == "" || patch.ResultDigest != toolResultDigest(outcome.Result) || patch.PinnedRun != nil {
		t.Fatalf("patch = %#v, %v", patch, err)
	}
}

func TestHostToolIsErrorResultIsErrorEntry(t *testing.T) {
	f := newHostToolDispatchFixture(hostReplyJSON(map[string]interface{}{"error": "no such file"}, true))
	outcome := runHostTool(t, f, hostToolCallFixture(t, workspace.OperationEditFile, map[string]interface{}{"path": "a.txt"}))
	if outcome.State != ToolEntryError || outcome.Result.Status != ToolResultFailed || !outcome.Result.IsError {
		t.Fatalf("outcome = %#v", outcome)
	}
	if hostToolErrorText(t, outcome.Result) != "no such file" {
		t.Fatalf("content = %s", outcome.Result.Content[0].Value)
	}
}

func routineTestPin() workspace.CommandPin {
	return workspace.CommandPin{Path: "scripts/report.py", Digest: "sha256:abc", FilesVersion: 4, Command: "python3 scripts/report.py", TimeoutSeconds: 300}
}

func TestHostToolRunCommandAttachesPinAndRecordsCommand(t *testing.T) {
	pin := routineTestPin()
	f := newHostToolDispatchFixture(hostReplyJSON(map[string]interface{}{
		"exitStatus": 0, "stdout": "done\n",
		workspace.ExpectedDigestArgument: map[string]interface{}{"path": pin.Path, "digest": pin.Digest},
	}, false))
	call := hostToolCallFixture(t, workspace.OperationRunCommand, map[string]interface{}{"command": pin.Command})
	call.CommandPins = []workspace.CommandPin{pin}
	outcome := runHostTool(t, f, call)
	sent := f.host.requests[0].Arguments
	expected, ok := workspace.ExpectedDigestOf(sent)
	if !ok || expected.Path != pin.Path || expected.Digest != pin.Digest || expected.FilesVersion != pin.FilesVersion {
		t.Fatalf("sent arguments = %#v", sent)
	}
	if sent["timeoutSeconds"] != 300 {
		t.Fatalf("pin timeout not attached: %#v", sent)
	}
	if !reflect.DeepEqual(outcome.Arguments, sent) {
		t.Fatalf("outcome arguments = %#v, sent %#v", outcome.Arguments, sent)
	}
	if outcome.State != ToolEntryCompleted || outcome.PinnedRun == nil {
		t.Fatalf("outcome = %#v", outcome)
	}
	record := *outcome.PinnedRun
	if err := record.Validate(call.CommandPins); err != nil || !record.Succeeded(pin.Digest) || !record.Runs(pin) || record.OutputTail != "done\n" {
		t.Fatalf("record = %#v (%v)", record, err)
	}
	patch, err := outcome.KernelPatch()
	if err != nil || patch.OriginRef != "command:ent_1" || len(patch.PinnedRun) == 0 {
		t.Fatalf("patch = %#v, %v", patch, err)
	}
	if _, ok := call.Arguments[workspace.ExpectedDigestArgument]; ok {
		t.Fatal("the model's arguments were changed in place")
	}
}

func TestHostToolRunCommandPinnedWrongVersionRecordsRefusal(t *testing.T) {
	pin := routineTestPin()
	f := newHostToolDispatchFixture(hostReplyJSON(map[string]interface{}{"exitStatus": 0}, false))
	call := hostToolCallFixture(t, workspace.OperationRunCommand, map[string]interface{}{"command": pin.Command})
	call.CommandPins = []workspace.CommandPin{pin}
	outcome := runHostTool(t, f, call)
	if outcome.PinnedRun == nil || outcome.PinnedRun.Refused == "" || outcome.PinnedRun.Succeeded(pin.Digest) {
		t.Fatalf("record = %#v", outcome.PinnedRun)
	}
}

func TestHostToolRunCommandStripsModelExpectedDigest(t *testing.T) {
	f := newHostToolDispatchFixture(hostReplyJSON(map[string]interface{}{"exitStatus": 0}, false))
	call := hostToolCallFixture(t, workspace.OperationRunCommand, map[string]interface{}{
		"command": "ls", workspace.ExpectedDigestArgument: map[string]interface{}{"path": "x.py", "digest": "sha256:forged"},
	})
	outcome := runHostTool(t, f, call)
	if _, ok := f.host.requests[0].Arguments[workspace.ExpectedDigestArgument]; ok {
		t.Fatalf("model expectedDigest reached the host: %#v", f.host.requests[0].Arguments)
	}
	if outcome.PinnedRun != nil {
		t.Fatalf("unpinned command recorded: %#v", outcome.PinnedRun)
	}
}

func TestHostToolRunCommandSetsCommandRefAndSortedVMTools(t *testing.T) {
	f := newHostToolDispatchFixture(hostReplyJSON(map[string]interface{}{"exitStatus": 0}, false))
	outcome := runHostTool(t, f, hostToolCallFixture(t, workspace.OperationRunCommand, map[string]interface{}{"command": "ls"}))
	request := f.host.requests[0]
	if request.CommandRef != "command:ent_1" || outcome.OriginRef != "command:ent_1" {
		t.Fatalf("commandRef = %q, originRef = %q", request.CommandRef, outcome.OriginRef)
	}
	want := []HostVMTool{
		{Name: "alpha.lookup", Description: "Look up", Risk: capability.RiskLevel("low"), InputSchema: map[string]interface{}{"type": "object"}},
		{Name: "zeta.send", Description: "Send", Risk: capability.RiskLevel("high"), InputSchema: map[string]interface{}{"type": "object", "required": []interface{}{"to"}}},
	}
	if !reflect.DeepEqual(request.VMTools, want) {
		t.Fatalf("vmTools = %#v", request.VMTools)
	}
}

// proposeNested asks the fixture's coordinator for a nested call of call's
// step as runID, the way the VM does, and returns its error. The coordinator
// has no stores, so a call the step's authority allows fails as not
// configured, after the authority checks.
func (f *hostToolDispatchFixture) proposeNested(call HostToolCall, runID, capability string) error {
	_, err := f.actions.ProposeNested(context.Background(), NestedActionRequest{
		Scope: call.Step.Scope, Origin: ActionOriginVM, RunID: runID, TurnID: call.Step.ID,
		Capability: capability, IdempotencyKey: "probe",
	})
	return err
}

func TestHostToolRunCommandOpensNestedAuthorityForStepAndClosesIt(t *testing.T) {
	f := newHostToolDispatchFixture()
	call := hostToolCallFixture(t, workspace.OperationRunCommand, map[string]interface{}{"command": "seal call alpha.lookup"})
	var offered, notOffered, otherRun error
	f.host.replies = append(f.host.replies, func(ctx context.Context, request HostToolRequest) (HostToolResult, error) {
		offered = f.proposeNested(call, "run_1", "alpha.lookup")
		notOffered = f.proposeNested(call, "run_1", "nobody.else")
		otherRun = f.proposeNested(call, "run_2", "alpha.lookup")
		return hostReplyJSON(map[string]interface{}{"exitStatus": 0}, false)(ctx, request)
	})
	runHostTool(t, f, call)
	// During the call the step's authority is open for run_1 with the call's
	// binding: it offers alpha.lookup and nothing else.
	if offered == nil || errors.Is(offered, ErrNestedTurnNotActive) || errors.Is(offered, ErrNestedActionNotAuthorized) ||
		!errors.Is(notOffered, ErrNestedActionNotAuthorized) || !errors.Is(otherRun, ErrNestedTurnNotActive) {
		t.Fatalf("nested authority during the call: offered=%v notOffered=%v otherRun=%v", offered, notOffered, otherRun)
	}
	if after := f.proposeNested(call, "run_1", "alpha.lookup"); !errors.Is(after, ErrNestedTurnNotActive) {
		t.Fatalf("nested authority still open: %v", after)
	}
}

func TestHostToolRunCommandWithoutActionsOpensNoBridge(t *testing.T) {
	f := newHostToolDispatchFixture()
	call := hostToolCallFixture(t, workspace.OperationRunCommand, map[string]interface{}{"command": "ls"})
	call.Binding.ModelActions = nil
	f.host.replies = append(f.host.replies, func(ctx context.Context, request HostToolRequest) (HostToolResult, error) {
		if err := f.proposeNested(call, "run_1", "alpha.lookup"); !errors.Is(err, ErrNestedTurnNotActive) {
			t.Errorf("nested authority opened without VM tools: %v", err)
		}
		return hostReplyJSON(map[string]interface{}{"exitStatus": 0}, false)(ctx, request)
	})
	runHostTool(t, f, call)
	if f.host.requests[0].VMTools != nil || f.host.requests[0].CommandRef != "command:ent_1" {
		t.Fatalf("request = %#v", f.host.requests[0])
	}
}

func TestHostToolNonCommandNeverOpensNestedAuthority(t *testing.T) {
	f := newHostToolDispatchFixture()
	call := hostToolCallFixture(t, workspace.OperationWriteFile, map[string]interface{}{"path": "a.txt", "content": "x"})
	f.host.replies = append(f.host.replies, func(ctx context.Context, request HostToolRequest) (HostToolResult, error) {
		if err := f.proposeNested(call, "run_1", "alpha.lookup"); !errors.Is(err, ErrNestedTurnNotActive) {
			t.Errorf("nested authority opened for a non-command tool: %v", err)
		}
		return hostReplyJSON(map[string]interface{}{"path": "a.txt"}, false)(ctx, request)
	})
	runHostTool(t, f, call)
	request := f.host.requests[0]
	if request.CommandRef != "" || request.VMTools != nil {
		t.Fatalf("request = %#v", request)
	}
}

func TestHostToolUnavailableRetriesTwiceThenError(t *testing.T) {
	unavailable := &HostToolFailure{Code: HostToolFailureUnavailable, Message: "draining", NotStarted: true}
	f := newHostToolDispatchFixture(hostReplyError(unavailable), hostReplyError(unavailable), hostReplyError(unavailable))
	outcome := runHostTool(t, f, hostToolCallFixture(t, workspace.OperationWriteFile, map[string]interface{}{"path": "a.txt"}))
	if len(f.host.requests) != 3 || !reflect.DeepEqual(f.slept, []time.Duration{time.Second, 3 * time.Second}) {
		t.Fatalf("calls = %d, waits = %v", len(f.host.requests), f.slept)
	}
	for _, request := range f.host.requests {
		if request.InvocationID != "ent_1" {
			t.Fatalf("retry changed the invocation: %q", request.InvocationID)
		}
	}
	if outcome.State != ToolEntryError || hostToolErrorText(t, outcome.Result) != hostToolUnavailableMessage {
		t.Fatalf("outcome = %#v", outcome)
	}
}

func TestHostToolUnavailableThenSucceeds(t *testing.T) {
	unavailable := &HostToolFailure{Code: HostToolFailureUnavailable, NotStarted: true}
	f := newHostToolDispatchFixture(hostReplyError(unavailable), hostReplyJSON(map[string]interface{}{"entries": []interface{}{}}, false))
	outcome := runHostTool(t, f, hostToolCallFixture(t, workspace.OperationListDirectory, map[string]interface{}{}))
	if outcome.State != ToolEntryCompleted || len(f.slept) != 1 {
		t.Fatalf("outcome = %#v, waits = %v", outcome, f.slept)
	}
}

func TestHostToolNotStartedFailureShowsItsMessage(t *testing.T) {
	f := newHostToolDispatchFixture(hostReplyError(&HostToolFailure{Code: HostToolFailureAccessDenied, Message: "This chat's files are not yours.", NotStarted: true}))
	outcome := runHostTool(t, f, hostToolCallFixture(t, workspace.OperationDeleteFile, map[string]interface{}{"path": "a.txt"}))
	if outcome.State != ToolEntryError || hostToolErrorText(t, outcome.Result) != "This chat's files are not yours." || len(f.slept) != 0 {
		t.Fatalf("outcome = %#v", outcome)
	}
}

func TestHostToolWriteTransportFailureIsInterrupted(t *testing.T) {
	f := newHostToolDispatchFixture(hostReplyError(errors.New("connection reset")))
	outcome := runHostTool(t, f, hostToolCallFixture(t, workspace.OperationWriteFile, map[string]interface{}{"path": "a.txt"}))
	if outcome.State != ToolEntryInterrupted || outcome.Result.Status != ToolResultInterrupted || !outcome.Result.IsError ||
		hostToolErrorText(t, outcome.Result) != HostToolInterruptedMessage {
		t.Fatalf("outcome = %#v", outcome)
	}
}

func TestHostToolPinnedCommandTransportFailureRecordsUnknownRun(t *testing.T) {
	pin := routineTestPin()
	f := newHostToolDispatchFixture(hostReplyError(&HostToolFailure{Code: HostToolFailureFailed, Message: "stream ended"}))
	call := hostToolCallFixture(t, workspace.OperationRunCommand, map[string]interface{}{"command": pin.Command})
	call.CommandPins = []workspace.CommandPin{pin}
	outcome := runHostTool(t, f, call)
	if outcome.State != ToolEntryInterrupted || outcome.PinnedRun == nil || outcome.PinnedRun.Refused == "" {
		t.Fatalf("outcome = %#v", outcome)
	}
}

func TestHostToolReadTransportFailureIsError(t *testing.T) {
	f := newHostToolDispatchFixture(hostReplyError(&HostToolFailure{Code: HostToolFailureFailed, Message: "stream ended"}))
	outcome := runHostTool(t, f, hostToolCallFixture(t, workspace.OperationSearchFiles, map[string]interface{}{"query": "x"}))
	if outcome.State != ToolEntryError || outcome.Result.Status != ToolResultFailed || hostToolErrorText(t, outcome.Result) != hostToolCouldNotRunMessage {
		t.Fatalf("outcome = %#v", outcome)
	}
}

func TestHostToolUnsupportedHostIsError(t *testing.T) {
	f := newHostToolDispatchFixture(hostReplyError(ErrHostToolUnsupported))
	outcome := runHostTool(t, f, hostToolCallFixture(t, workspace.OperationWriteFile, map[string]interface{}{"path": "a.txt"}))
	if outcome.State != ToolEntryError || hostToolErrorText(t, outcome.Result) != hostToolUnsupportedMessage {
		t.Fatalf("outcome = %#v", outcome)
	}
}

func oversizedHostReply(_ context.Context, request HostToolRequest) (HostToolResult, error) {
	return HostToolResult{APIVersion: HostToolAPIVersion, InvocationID: request.InvocationID,
		Content: []ContentPart{{Type: ContentPartText, Text: strings.Repeat("x", MaxToolResultContentBytes+1)}}}, nil
}

func TestHostToolOversizedContentRead(t *testing.T) {
	f := newHostToolDispatchFixture(oversizedHostReply)
	outcome := runHostTool(t, f, hostToolCallFixture(t, workspace.OperationReadFile, map[string]interface{}{"path": "a.txt"}))
	if outcome.State != ToolEntryError || hostToolErrorText(t, outcome.Result) != hostToolUnusableResultMessage {
		t.Fatalf("outcome = %#v", outcome)
	}
}

func TestHostToolOversizedContentWrite(t *testing.T) {
	f := newHostToolDispatchFixture(oversizedHostReply)
	outcome := runHostTool(t, f, hostToolCallFixture(t, workspace.OperationRunCommand, map[string]interface{}{"command": "ls"}))
	if outcome.State != ToolEntryInterrupted || hostToolErrorText(t, outcome.Result) != HostToolInterruptedMessage {
		t.Fatalf("outcome = %#v", outcome)
	}
}

func TestHostToolMismatchedInvocationIsUnusable(t *testing.T) {
	f := newHostToolDispatchFixture(func(context.Context, HostToolRequest) (HostToolResult, error) {
		return HostToolResult{APIVersion: HostToolAPIVersion, InvocationID: "other", Content: []ContentPart{{Type: ContentPartText, Text: "x"}}}, nil
	})
	outcome := runHostTool(t, f, hostToolCallFixture(t, workspace.OperationListChats, map[string]interface{}{}))
	if outcome.State != ToolEntryError || hostToolErrorText(t, outcome.Result) != hostToolUnusableResultMessage {
		t.Fatalf("outcome = %#v", outcome)
	}
}

func TestHostToolContextCanceledLeavesEntryRunning(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := newHostToolDispatchFixture(func(ctx context.Context, request HostToolRequest) (HostToolResult, error) {
		cancel()
		return HostToolResult{}, ctx.Err()
	})
	if outcome, ok := f.dispatcher.Run(ctx, hostToolCallFixture(t, workspace.OperationWriteFile, map[string]interface{}{"path": "a.txt"})); ok {
		t.Fatalf("ok = true, outcome = %#v", outcome)
	}
	// A cancel during the unavailable back-off is the same.
	ctx, cancel = context.WithCancel(context.Background())
	f = newHostToolDispatchFixture(hostReplyError(&HostToolFailure{Code: HostToolFailureUnavailable, NotStarted: true}))
	f.dispatcher.sleep = func(context.Context, time.Duration) error { cancel(); return context.Canceled }
	if _, ok := f.dispatcher.Run(ctx, hostToolCallFixture(t, workspace.OperationReadFile, map[string]interface{}{"path": "a.txt"})); ok {
		t.Fatal("ok = true after a cancel during back-off")
	}
}

func TestHostToolResumeReadRerunsWriteInterrupted(t *testing.T) {
	for _, operation := range []string{workspace.OperationListDirectory, workspace.OperationReadFile, workspace.OperationSearchFiles, workspace.OperationListChats} {
		state, result := HostToolResumeState(hostToolSpec(t, operation))
		if state != ToolEntryPending || result != nil {
			t.Fatalf("%s resumes as %s %#v", operation, state, result)
		}
	}
	for _, operation := range []string{workspace.OperationWriteFile, workspace.OperationEditFile, workspace.OperationDeleteFile,
		workspace.OperationMoveFile, workspace.OperationCopyFromChat, workspace.OperationRunCommand} {
		state, result := HostToolResumeState(hostToolSpec(t, operation))
		if state != ToolEntryInterrupted || result.Status != ToolResultInterrupted || !result.IsError || hostToolErrorText(t, result) != HostToolInterruptedMessage {
			t.Fatalf("%s resumes as %s %#v", operation, state, result)
		}
	}
}

func TestHostToolNeverSpendsActionsBudget(t *testing.T) {
	f := newHostToolDispatchFixture(hostReplyJSON(map[string]interface{}{"path": "a.txt"}, false), hostReplyJSON(map[string]interface{}{"exitStatus": 0}, false))
	call := hostToolCallFixture(t, workspace.OperationWriteFile, map[string]interface{}{"path": "a.txt", "content": "x"})
	call.Run.Budget = &BudgetPolicy{MaxActions: 1}
	call.Run.BudgetUsage = BudgetUsage{Actions: 1}
	before, _ := json.Marshal(call.Run)
	if outcome := runHostTool(t, f, call); outcome.State != ToolEntryCompleted {
		t.Fatalf("write with an exhausted actions budget: %#v", outcome)
	}
	command := hostToolCallFixture(t, workspace.OperationRunCommand, map[string]interface{}{"command": "ls"})
	command.Run = call.Run
	if outcome := runHostTool(t, f, command); outcome.State != ToolEntryCompleted {
		t.Fatalf("command with an exhausted actions budget: %#v", outcome)
	}
	if after, _ := json.Marshal(call.Run); string(after) != string(before) {
		t.Fatalf("Run changed:\n%s\nwas\n%s", after, before)
	}
}
