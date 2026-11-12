package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

func runbookLiteral(value interface{}) runbook.Value {
	encoded, _ := json.Marshal(value)
	return runbook.Value{Literal: encoded}
}
func runbookRef(pointer string) *runbook.Value {
	value := runbook.Value{Ref: runbook.JSONPointer(pointer)}
	return &value
}

func newTestRunbookProducer(t *testing.T, definition *runbook.Definition, entrypoint string, calls ActionCallReader) *RunbookStepProducer {
	t.Helper()
	if calls == nil {
		calls = fixedActionCalls{}
	}
	producer, err := NewRunbookStepProducer(definition, entrypoint, calls)
	if err != nil {
		t.Fatal(err)
	}
	return producer
}

// runbookInput is the StepInput of step sequence of run, with binding and
// the Run's session path.
func runbookInput(run *AgentRun, sequence int64, binding *TurnRunnerBinding, path *AgentSessionPath) StepInput {
	if binding == nil {
		binding = &TurnRunnerBinding{}
	}
	if path == nil {
		path = &AgentSessionPath{}
	}
	return StepInput{Run: run, Step: &AgentTurn{ID: fmt.Sprintf("step-%d", sequence), Sequence: sequence}, Binding: binding, Path: path, Mode: StepModeNormal}
}

// produceRunbook runs one step and checks it is a valid deterministic output.
func produceRunbook(t *testing.T, producer *RunbookStepProducer, in StepInput) StepOutput {
	t.Helper()
	out, err := producer.Produce(t.Context(), in)
	if err != nil {
		t.Fatalf("produce step %s: %v", in.Step.ID, err)
	}
	requireValidDeterministicOutput(t, out)
	if out.KernelState == nil {
		t.Fatalf("step %s returned no engine state: %#v", in.Step.ID, out)
	}
	return out
}

// appendRunbookToolResult adds the terminal tool entry of a governed call to
// path, as the inline executor would record it.
func appendRunbookToolResult(path *AgentSessionPath, runID, toolCallID string, state ToolEntryState, actionCallID string) {
	path.Entries = append(path.Entries, producerToolPath(runID, toolCallID, state, actionCallID).Entries...)
}

func webFetchAction() capability.ModelAction {
	return capability.ModelAction{Name: "web_fetch", SkillID: "web", Version: "1", Action: "fetch", BindingID: "web-binding", BindingRevision: 3}
}

func TestRunbookProducerReportsDelegatedAgentFailureReason(t *testing.T) {
	producer := &RunbookStepProducer{definition: &runbook.Definition{}}
	run := &AgentRun{ID: "run-1", Scope: Scope{Kind: "tenant", ID: "1"}, Output: map[string]interface{}{}}
	stepID := "perform-hourly"
	requestID := stableCollaborationID(run.Scope, delegationIdempotencyKey(run.ID, stepID), "request")
	run.Output["collaborationResults"] = map[string]interface{}{
		requestID: map[string]interface{}{
			"status": string(AgentRequestStatusFailed),
			"reason": "Model provider quota is exhausted; choose a credential with available quota.",
		},
	}
	state := &runbookExecutionState{PendingDelegation: stepID}
	consumed, failure, err := producer.consumeDelegationResult(map[string]interface{}{}, state, run)
	if err != nil || consumed || !strings.Contains(failure, "provider quota is exhausted") {
		t.Fatalf("consumed=%t failure=%q err=%v", consumed, failure, err)
	}
}

func TestRunbookProducerEmitsPinnedGovernedCallAndConsumesActionCall(t *testing.T) {
	definition := &runbook.Definition{APIVersion: runbook.APIVersion, ID: "health", Version: "1", Name: "Health", Entrypoints: map[string]string{"manual": "fetch"}, Steps: map[string]runbook.Step{
		"fetch": {Kind: runbook.StepAction, Action: &runbook.ActionStep{SkillID: "web", SkillVersion: "1", Action: "fetch", Arguments: map[string]runbook.Value{"url": {Ref: "/input/url"}}, ResultPath: "/steps/fetch", Next: "done"}},
		"done":  {Kind: runbook.StepEnd, End: &runbook.EndStep{Outputs: map[string]runbook.Value{"content": {Ref: "/steps/fetch/content"}}}},
	}}
	calls := fixedActionCalls{}
	producer := newTestRunbookProducer(t, definition, "manual", calls)
	if producer.Kind() != StepProducerDeterministic {
		t.Fatalf("kind = %s", producer.Kind())
	}
	action := webFetchAction()
	binding := &TurnRunnerBinding{ModelActions: []capability.ModelAction{action}}
	run := &AgentRun{ID: "run-1", Context: map[string]interface{}{"url": "https://example.test/health"}}
	path := &AgentSessionPath{}
	first := produceRunbook(t, producer, runbookInput(run, 1, binding, path))
	if len(first.ToolCalls) != 1 {
		t.Fatalf("first = %#v", first)
	}
	call := first.ToolCalls[0]
	if call.ID != "runbook.1.fetch" || call.Name != action.Name || call.Directive == nil ||
		call.Directive.IdempotencyKey != "runbook:health:run-1:fetch:1" || call.Directive.BindingID != "web-binding" ||
		call.Directive.BindingRevision != 3 || call.Directive.ExpectedSkillVersion != "1" || call.Directive.Summary != "Execute web.fetch" {
		t.Fatalf("call = %#v directive = %#v", call, call.Directive)
	}
	var arguments map[string]interface{}
	if err := json.Unmarshal(call.Arguments, &arguments); err != nil || arguments["url"] != "https://example.test/health" {
		t.Fatalf("arguments = %s err = %v", call.Arguments, err)
	}
	if _, legacy := first.KernelState["runbookActionInputs"]; legacy {
		t.Fatal("action inputs were copied into the engine state")
	}
	firstTrace, err := RunbookTraceFromCheckpoint(first.KernelState)
	if err != nil || len(firstTrace.Entries) != 1 || firstTrace.Entries[0].StepID != "fetch" || firstTrace.Entries[0].Status != RunbookStepTraceWaiting || firstTrace.Entries[0].Visit != 1 || firstTrace.Entries[0].TurnID != "step-1" || !reflect.DeepEqual(firstTrace.Entries[0].InputRefs, []string{"/input/url"}) || !reflect.DeepEqual(firstTrace.Entries[0].OutputRefs, []string{"/steps/fetch"}) {
		t.Fatalf("first trace=%#v err=%v", firstTrace, err)
	}
	if len(firstTrace.Entries[0].Inputs) != 1 || firstTrace.Entries[0].Inputs[0].Shape != "string" || len(firstTrace.Entries[0].Outputs) != 1 || firstTrace.Entries[0].Outputs[0].Availability != RunbookDataMissing {
		t.Fatalf("first data observations=%#v", firstTrace.Entries[0])
	}
	// The loop applies the step, the inline executor records the call.
	run.Checkpoint = first.KernelState
	calls["call-1"] = &ActionCall{ID: "call-1", RunID: run.ID, SkillID: "web", SkillVersion: "1", Action: "fetch", ApprovalID: "approval-1", Status: ActionCallStatusSucceeded, Output: map[string]interface{}{"content": "ok"}}
	appendRunbookToolResult(path, run.ID, call.ID, ToolEntryCompleted, "call-1")
	second := produceRunbook(t, producer, runbookInput(run, 2, binding, path))
	if second.Final == nil || second.Final.Status != AgentRunStatusCompleted || second.Final.Output["content"] != "ok" || len(second.ToolCalls) != 0 {
		t.Fatalf("second=%#v", second)
	}
	state, err := decodeRunbookState(second.KernelState)
	if err != nil || state.PendingAction != "" || state.PendingCall != "" {
		t.Fatalf("pending call survived: %#v err=%v", state, err)
	}
	trace, err := RunbookTraceFromCheckpoint(second.KernelState)
	if err != nil || len(trace.Entries) != 2 || trace.Entries[0].Status != RunbookStepTraceSucceeded || trace.Entries[0].ActionCallID != "call-1" || trace.Entries[0].ApprovalID != "approval-1" || trace.Entries[0].SelectedNext != "done" || trace.Entries[1].StepID != "done" || trace.Entries[1].Status != RunbookStepTraceSucceeded {
		t.Fatalf("completed trace=%#v err=%v", trace, err)
	}
	if trace.Entries[0].Outputs[0].Shape != "object" || trace.Entries[1].Inputs[0].Shape != "string" || trace.Entries[1].Outputs[0].Shape != "string" {
		t.Fatalf("completed data observations=%#v", trace.Entries)
	}
	encodedTrace, _ := json.Marshal(trace)
	if strings.Contains(string(encodedTrace), "https://example.test/health") || strings.Contains(string(encodedTrace), `"content":"ok"`) {
		t.Fatalf("trace copied governed values: %s", encodedTrace)
	}
}

func TestRunbookProducerFailsOnUnsuccessfulOrRefusedAction(t *testing.T) {
	definition := &runbook.Definition{APIVersion: runbook.APIVersion, ID: "health", Version: "1", Name: "Health", Entrypoints: map[string]string{"manual": "fetch"}, Steps: map[string]runbook.Step{
		"fetch": {Kind: runbook.StepAction, Action: &runbook.ActionStep{SkillID: "web", SkillVersion: "1", Action: "fetch", ResultPath: "/steps/fetch", Next: "done"}},
		"done":  {Kind: runbook.StepEnd, End: &runbook.EndStep{}},
	}}
	binding := &TurnRunnerBinding{ModelActions: []capability.ModelAction{webFetchAction()}}
	for _, item := range []struct {
		name   string
		call   *ActionCall
		expect string
	}{
		{name: "denied", call: &ActionCall{ID: "call-1", Status: ActionCallStatusDenied}, expect: "runbook action fetch denied"},
		{name: "refused before a row", expect: "was refused before it ran"},
	} {
		t.Run(item.name, func(t *testing.T) {
			calls := fixedActionCalls{}
			producer := newTestRunbookProducer(t, definition, "manual", calls)
			run := &AgentRun{ID: "run-1", Context: map[string]interface{}{}}
			path := &AgentSessionPath{}
			first := produceRunbook(t, producer, runbookInput(run, 1, binding, path))
			run.Checkpoint = first.KernelState
			actionID := ""
			state := ToolEntryError
			if item.call != nil {
				item.call.RunID = run.ID
				calls[item.call.ID] = item.call
				actionID = item.call.ID
			}
			appendRunbookToolResult(path, run.ID, first.ToolCalls[0].ID, state, actionID)
			second := produceRunbook(t, producer, runbookInput(run, 2, binding, path))
			if second.Failure == nil || !strings.Contains(second.Failure.Message, item.expect) || second.Failure.Kind != RunFailureKindExecution {
				t.Fatalf("second=%#v", second)
			}
			trace, err := RunbookTraceFromCheckpoint(second.KernelState)
			if err != nil || len(trace.Entries) != 1 || trace.Entries[0].Status != RunbookStepTraceFailed || !strings.Contains(trace.Entries[0].Error, item.expect) {
				t.Fatalf("trace=%#v err=%v", trace, err)
			}
		})
	}
}

func TestRunbookProducerFailsActionNodeWithoutAuthorizedBinding(t *testing.T) {
	definition := &runbook.Definition{APIVersion: runbook.APIVersion, ID: "health", Version: "1", Name: "Health", Entrypoints: map[string]string{"manual": "fetch"}, Steps: map[string]runbook.Step{
		"fetch": {Kind: runbook.StepAction, Action: &runbook.ActionStep{SkillID: "web", SkillVersion: "2", Action: "fetch", ResultPath: "/steps/fetch", Next: "done"}},
		"done":  {Kind: runbook.StepEnd, End: &runbook.EndStep{}},
	}}
	producer := newTestRunbookProducer(t, definition, "manual", nil)
	binding := &TurnRunnerBinding{ModelActions: []capability.ModelAction{webFetchAction()}}
	out := produceRunbook(t, producer, runbookInput(&AgentRun{ID: "run-1"}, 1, binding, nil))
	if out.Failure == nil || !strings.Contains(out.Failure.Message, "not authorized") || len(out.ToolCalls) != 0 {
		t.Fatalf("out=%#v", out)
	}
}

func TestRunbookProducerThreadsActionOutputAcrossBrowserLifecycle(t *testing.T) {
	definition := &runbook.Definition{
		APIVersion: runbook.APIVersion, ID: "browser", Version: "1", Name: "Browser", Entrypoints: map[string]string{"daily": "start"},
		Steps: map[string]runbook.Step{
			"start": {Kind: runbook.StepAction, Action: &runbook.ActionStep{
				SkillID: "browser", SkillVersion: "1", Action: "start", Arguments: map[string]runbook.Value{"profile": runbookLiteral("rowan")}, ResultPath: "/results/session", Next: "navigate",
			}},
			"navigate": {Kind: runbook.StepAction, Action: &runbook.ActionStep{
				SkillID: "browser", SkillVersion: "1", Action: "navigate", Arguments: map[string]runbook.Value{
					"sessionId": {Ref: "/results/session/sessionId"}, "url": runbookLiteral("https://example.test"),
				}, ResultPath: "/results/navigation", Next: "close",
			}},
			"close": {Kind: runbook.StepAction, Action: &runbook.ActionStep{
				SkillID: "browser", SkillVersion: "1", Action: "close", Arguments: map[string]runbook.Value{
					"sessionId": {Ref: "/results/session/sessionId"},
				}, ResultPath: "/results/close", Next: "done",
			}},
			"done": {Kind: runbook.StepEnd, End: &runbook.EndStep{}},
		},
	}
	calls := fixedActionCalls{}
	producer := newTestRunbookProducer(t, definition, "daily", calls)
	binding := &TurnRunnerBinding{ModelActions: []capability.ModelAction{
		{Name: "browser_start", SkillID: "browser", Version: "1", Action: "start"},
		{Name: "browser_navigate", SkillID: "browser", Version: "1", Action: "navigate"},
		{Name: "browser_close", SkillID: "browser", Version: "1", Action: "close"},
	}}
	run := &AgentRun{ID: "run-browser", Context: map[string]interface{}{}}
	path := &AgentSessionPath{}
	step := func(sequence int64) StepOutput {
		out := produceRunbook(t, producer, runbookInput(run, sequence, binding, path))
		run.Checkpoint = out.KernelState
		return out
	}
	complete := func(call LoopToolCall, id string, output map[string]interface{}) {
		calls[id] = &ActionCall{ID: id, RunID: run.ID, Status: ActionCallStatusSucceeded, Output: output}
		appendRunbookToolResult(path, run.ID, call.ID, ToolEntryCompleted, id)
	}
	arguments := func(call LoopToolCall) map[string]interface{} {
		var value map[string]interface{}
		if err := json.Unmarshal(call.Arguments, &value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	start := step(1)
	if start.ToolCalls[0].Name != "browser_start" {
		t.Fatalf("start=%#v", start)
	}
	complete(start.ToolCalls[0], "start-call", map[string]interface{}{"sessionId": "session-1"})
	navigate := step(2)
	if args := arguments(navigate.ToolCalls[0]); navigate.ToolCalls[0].Name != "browser_navigate" || args["sessionId"] != "session-1" || args["url"] != "https://example.test" {
		t.Fatalf("navigate=%#v", navigate.ToolCalls)
	}
	complete(navigate.ToolCalls[0], "navigate-call", map[string]interface{}{"url": "https://example.test"})
	closeStep := step(3)
	if args := arguments(closeStep.ToolCalls[0]); args["sessionId"] != "session-1" {
		t.Fatalf("close=%#v", closeStep.ToolCalls)
	}
	if ids := []string{start.ToolCalls[0].ID, navigate.ToolCalls[0].ID, closeStep.ToolCalls[0].ID}; ids[0] == ids[1] || ids[1] == ids[2] {
		t.Fatalf("tool call ids repeat across nodes: %v", ids)
	}
	complete(closeStep.ToolCalls[0], "close-call", map[string]interface{}{"closed": true})
	completed := step(4)
	if completed.Final == nil || completed.Final.Status != AgentRunStatusCompleted {
		t.Fatalf("completed=%#v", completed)
	}
}

func TestRunbookProducerExposesKernelRunIdentityToActionArguments(t *testing.T) {
	definition := &runbook.Definition{APIVersion: runbook.APIVersion, ID: "session", Version: "1", Name: "Session", Entrypoints: map[string]string{"start": "open"}, Steps: map[string]runbook.Step{
		"open": {Kind: runbook.StepAction, Action: &runbook.ActionStep{SkillID: "browser", SkillVersion: "1", Action: "start", Arguments: map[string]runbook.Value{
			"sessionId": {Ref: "/runtime/runId"},
		}, ResultPath: "/steps/open", Next: "done"}},
		"done": {Kind: runbook.StepEnd, End: &runbook.EndStep{}},
	}}
	producer := newTestRunbookProducer(t, definition, "start", nil)
	binding := &TurnRunnerBinding{ModelActions: []capability.ModelAction{{Name: "browser_start", SkillID: "browser", Version: "1", Action: "start"}}}
	run := &AgentRun{ID: "run-123", RootRunID: "root-456", ObjectiveID: "objective-789", Checkpoint: map[string]interface{}{
		"runtime": map[string]interface{}{"runId": "spoofed"},
	}}
	out := produceRunbook(t, producer, runbookInput(run, 1, binding, nil))
	var arguments map[string]interface{}
	if err := json.Unmarshal(out.ToolCalls[0].Arguments, &arguments); err != nil || arguments["sessionId"] != run.ID {
		t.Fatalf("arguments=%#v err=%v", arguments, err)
	}
	runtimeContext, _ := out.KernelState["runtime"].(map[string]interface{})
	if runtimeContext["rootRunId"] != run.RootRunID || runtimeContext["objectiveId"] != run.ObjectiveID {
		t.Fatalf("runtime context=%#v", runtimeContext)
	}
}

func TestRunbookProducerValidatesCallableOutput(t *testing.T) {
	definition := &runbook.Definition{
		APIVersion: runbook.APIVersion, ID: "typed", Version: "1", Name: "Typed",
		Entrypoints: map[string]string{"manual": "done"},
		Interfaces: map[string]runbook.Interface{"manual": {
			Description: "Return a typed result.", InputSchema: map[string]interface{}{"type": "object"},
			OutputSchema: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{"result": map[string]interface{}{"type": "string"}},
				"required":   []interface{}{"result"},
			},
		}},
		Steps: map[string]runbook.Step{"done": {
			Kind: runbook.StepEnd,
			End:  &runbook.EndStep{Outputs: map[string]runbook.Value{"result": {Literal: []byte(`42`)}}},
		}},
	}
	producer := newTestRunbookProducer(t, definition, "manual", nil)
	out := produceRunbook(t, producer, runbookInput(&AgentRun{ID: "run", Context: map[string]interface{}{}}, 1, nil, nil))
	if out.Failure == nil || out.Failure.Message == "" {
		t.Fatalf("output mismatch out=%#v", out)
	}
	trace, traceErr := RunbookTraceFromCheckpoint(out.KernelState)
	if traceErr != nil || len(trace.Entries) != 1 || trace.Entries[0].StepID != "done" || trace.Entries[0].Status != RunbookStepTraceFailed || trace.Entries[0].Error == "" {
		t.Fatalf("failure trace=%#v err=%v", trace, traceErr)
	}
}

func concurrentRunbookDefinition() *runbook.Definition {
	return &runbook.Definition{APIVersion: runbook.APIVersion, ID: "deterministic", Version: "1", Name: "Deterministic", Entrypoints: map[string]string{"manual": "choose"}, Steps: map[string]runbook.Step{
		"choose": {Kind: runbook.StepDecision, Decision: &runbook.DecisionStep{Cases: []runbook.DecisionCase{{When: runbook.Predicate{Operator: runbook.PredicateTruthy, Left: runbookRef("/input/parallel")}, Next: "fork"}}, Default: "loop"}},
		"fork":   {Kind: runbook.StepFork, Fork: &runbook.ForkStep{Branches: map[string]string{"a": "a", "b": "b"}, BranchBudgets: map[string]runbook.BudgetAllocation{"a": {MaxTurns: 2}, "b": {MaxActions: 3}}, Join: "join"}},
		"a":      {Kind: runbook.StepTransform, Transform: &runbook.TransformStep{Assignments: map[string]runbook.Value{"/state/a": runbookLiteral(true)}, Next: "join"}},
		"b":      {Kind: runbook.StepTransform, Transform: &runbook.TransformStep{Assignments: map[string]runbook.Value{"/state/b": runbookLiteral(true)}, Next: "join"}},
		"join":   {Kind: runbook.StepJoin, Join: &runbook.JoinStep{Fork: "fork", Mode: runbook.JoinAll, Next: "done"}},
		"loop":   {Kind: runbook.StepForEach, ForEach: &runbook.ForEachStep{Items: runbook.Value{Ref: "/input/items"}, ItemName: "item", MaxIterations: 3, Body: "copy", Next: "done"}},
		"copy":   {Kind: runbook.StepTransform, Transform: &runbook.TransformStep{Assignments: map[string]runbook.Value{"/state/last": {Ref: "/loop/item"}}, Next: "again"}},
		"again":  {Kind: runbook.StepLoopReturn, LoopReturn: &runbook.LoopReturnStep{ForEach: "loop"}},
		"done":   {Kind: runbook.StepEnd, End: &runbook.EndStep{Outputs: map[string]runbook.Value{"state": {Ref: "/state"}}}},
	}}
}

func TestRunbookProducerEmitsKernelForkCallAndRunsBoundedForEach(t *testing.T) {
	definition := concurrentRunbookDefinition()
	parallel := newTestRunbookProducer(t, definition, "manual", nil)
	run := &AgentRun{ID: "parallel", Context: map[string]interface{}{"parallel": true}}
	out := produceRunbook(t, parallel, runbookInput(run, 1, nil, nil))
	if len(out.ToolCalls) != 1 || out.ToolCalls[0].Name != string(ControlToolRunbookFork) || out.ToolCalls[0].Directive == nil {
		t.Fatalf("parallel=%#v", out)
	}
	if string(out.ToolCalls[0].Arguments) != `{"step":"fork","visit":2}` || out.ToolCalls[0].ID != "runbook.2.fork" {
		t.Fatalf("fork call=%#v args=%s", out.ToolCalls[0], out.ToolCalls[0].Arguments)
	}
	state, err := decodeRunbookState(out.KernelState)
	if err != nil || state.Current != "fork" || state.PendingFork != "fork" || state.PendingCall != out.ToolCalls[0].ID {
		t.Fatalf("state=%#v err=%v", state, err)
	}
	// The settle rebuilds the spec from the applied engine state.
	spec, err := parallel.forkSpec(out.KernelState, state, run.ID)
	if err != nil || spec.ForkID != "fork" || spec.Policy.Mode != FanInModeAll || len(spec.Branches) != 2 || spec.SourceCheckpoint == nil || spec.ChildKind != "" {
		t.Fatalf("spec=%#v err=%v", spec, err)
	}
	if spec.Branches[0].Budget == nil || spec.Branches[0].Budget.MaxTurns != 2 || spec.Branches[1].Budget == nil || spec.Branches[1].Budget.MaxActions != 3 {
		t.Fatalf("branch budgets=%#v", spec.Branches)
	}
	for index, branch := range spec.Branches {
		if _, copied := branch.Checkpoint[runbookTraceCheckpointKey]; copied {
			t.Fatalf("branch %d copied the parent trace", index)
		}
		child := produceRunbook(t, parallel, runbookInput(&AgentRun{ID: "child", Checkpoint: branch.Checkpoint}, 1, nil, nil))
		if child.Final == nil || child.Final.Output["branchId"] != branch.ID || child.Final.Output["forkId"] != "fork" {
			t.Fatalf("branch %d=%#v", index, child)
		}
		trace, traceErr := RunbookTraceFromCheckpoint(child.KernelState)
		if traceErr != nil || len(trace.Entries) != 2 || trace.Entries[0].StepID != branch.ID || trace.Entries[1].StepID != "join" {
			t.Fatalf("branch %d trace=%#v error=%v", index, trace, traceErr)
		}
	}
	// The group resolution writes the joined branches into Run.Output.
	groupID := stableForkIdentifier(run.ID, "fork", "group")
	run.Checkpoint = out.KernelState
	run.Output = map[string]interface{}{"dependencyGroups": map[string]interface{}{groupID: map[string]interface{}{
		"status": string(RunDependencyGroupSatisfied),
		"dependencies": map[string]interface{}{
			"a": map[string]interface{}{"state": string(RunDependencyStateSatisfied), "result": map[string]interface{}{"checkpoint": map[string]interface{}{"state": map[string]interface{}{"a": true}}}},
			"b": map[string]interface{}{"state": string(RunDependencyStateSatisfied), "result": map[string]interface{}{"checkpoint": map[string]interface{}{"state": map[string]interface{}{"b": true}}}},
		},
	}}}
	joined := produceRunbook(t, parallel, runbookInput(run, 2, nil, nil))
	if joined.Final == nil || !reflect.DeepEqual(joined.Final.Output["state"], map[string]interface{}{"a": true, "b": true}) {
		t.Fatalf("joined=%#v", joined)
	}
	loop := newTestRunbookProducer(t, definition, "manual", nil)
	looped := produceRunbook(t, loop, runbookInput(&AgentRun{ID: "loop", Context: map[string]interface{}{"parallel": false, "items": []interface{}{"one", "two"}}}, 1, nil, nil))
	if looped.Final == nil || looped.Final.Output["state"].(map[string]interface{})["last"] != "two" {
		t.Fatalf("loop=%#v", looped)
	}
}

func TestRunbookProducerFailsKernelCallThatEndedWithoutResult(t *testing.T) {
	definition := concurrentRunbookDefinition()
	producer := newTestRunbookProducer(t, definition, "manual", nil)
	run := &AgentRun{ID: "parallel", Context: map[string]interface{}{"parallel": true}}
	out := produceRunbook(t, producer, runbookInput(run, 1, nil, nil))
	run.Checkpoint = out.KernelState
	path := &AgentSessionPath{}
	// Never recorded: the same call is asked for again.
	again := produceRunbook(t, producer, runbookInput(run, 2, nil, path))
	if len(again.ToolCalls) != 1 || again.ToolCalls[0].ID != out.ToolCalls[0].ID {
		t.Fatalf("again=%#v", again)
	}
	// Refused at settle (no group in Run.Output): the node fails with the
	// entry's result.
	path.Entries = []*SessionEntry{{
		ID: "tool:step-1:" + out.ToolCalls[0].ID, RunID: run.ID, StepID: "step-1", Type: SessionEntryTool, State: ToolEntryError,
		ToolCallID: out.ToolCalls[0].ID,
		Tool:       &ToolEntry{Name: string(ControlToolRunbookFork), Result: controlErrorResult(ToolResultRefused, forkRejectedCode, "budget exhausted", nil)},
	}}
	failed := produceRunbook(t, producer, runbookInput(run, 3, nil, path))
	if failed.Failure == nil || !strings.Contains(failed.Failure.Message, "ended without a durable result") || !strings.Contains(failed.Failure.Message, "budget exhausted") {
		t.Fatalf("failed=%#v", failed)
	}
}

func TestRunbookProducerWaitsOnceAndResumesFromCheckpoint(t *testing.T) {
	definition := &runbook.Definition{APIVersion: runbook.APIVersion, ID: "wait", Version: "1", Name: "Wait", Entrypoints: map[string]string{"event": "wait"}, Steps: map[string]runbook.Step{
		"wait": {Kind: runbook.StepWait, Wait: &runbook.WaitStep{Duration: time.Minute, Next: "done"}}, "done": {Kind: runbook.StepEnd, End: &runbook.EndStep{}},
	}}
	producer := newTestRunbookProducer(t, definition, "event", nil)
	now := time.Date(2026, 7, 12, 5, 0, 0, 0, time.UTC)
	producer.now = func() time.Time { return now }
	first := produceRunbook(t, producer, runbookInput(&AgentRun{ID: "run"}, 1, nil, nil))
	if first.Park == nil || first.Park.Status != AgentRunStatusSleeping || first.Park.Wake == nil || first.Park.Wake.Type != "timer" || !first.Park.Wake.WakeAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("first=%#v", first)
	}
	firstTrace, err := RunbookTraceFromCheckpoint(first.KernelState)
	if err != nil || len(firstTrace.Entries) != 1 || firstTrace.Entries[0].StepID != "wait" || firstTrace.Entries[0].Status != RunbookStepTraceWaiting {
		t.Fatalf("waiting trace=%#v err=%v", firstTrace, err)
	}
	second := produceRunbook(t, producer, runbookInput(&AgentRun{ID: "run", Checkpoint: first.KernelState}, 2, nil, nil))
	if second.Final == nil || second.Final.Status != AgentRunStatusCompleted {
		t.Fatalf("second=%#v", second)
	}
	trace, err := RunbookTraceFromCheckpoint(second.KernelState)
	if err != nil || len(trace.Entries) != 2 || trace.Entries[0].Status != RunbookStepTraceSucceeded || trace.Entries[1].StepID != "done" || trace.Entries[1].Status != RunbookStepTraceSucceeded {
		t.Fatalf("resumed trace=%#v err=%v", trace, err)
	}
}

func TestRunbookValueRendersTypedTemplateSegments(t *testing.T) {
	value := runbook.Value{Template: []runbook.TemplateSegment{
		{Text: "Release "}, {Ref: "/input/version"}, {Text: " has metrics "}, {Ref: "/input/metrics"},
	}}
	value.Template = append(value.Template, runbook.TemplateSegment{Text: " optional="}, runbook.TemplateSegment{Ref: "/input/missing"})
	resolved, err := resolveRunbookValue(map[string]interface{}{"input": map[string]interface{}{"version": "2026.07", "metrics": map[string]interface{}{"leads": float64(12)}}}, value)
	if err != nil || resolved != `Release 2026.07 has metrics {"leads":12} optional=` {
		t.Fatalf("resolved=%#v error=%v", resolved, err)
	}
}

func TestRunbookKernelToolsAreNeverOffered(t *testing.T) {
	everything := ControlAuthority{
		ForkAllowed: true, CanWait: true, CanAskUser: true, CanReact: true, CanReportProgress: true,
		DelegateTargets: []HostedAgentTarget{{ID: "a"}}, Operations: []HostedRunbookOperation{{Entrypoint: "x"}},
	}
	for _, spec := range ControlToolSpecs(everything) {
		if spec.Name == ControlToolRunbookFork || spec.Name == ControlToolRunbookDelegate {
			t.Fatalf("kernel-only tool %s offered to a model", spec.Name)
		}
	}
	for _, name := range []ControlToolName{ControlToolRunbookFork, ControlToolRunbookDelegate} {
		if phase, ok := ControlToolPhase(string(name)); !ok || phase != ControlPhaseSettle {
			t.Fatalf("%s phase=%s ok=%t", name, phase, ok)
		}
	}
}

// ---------------------------------------------------------------------------
// Through the RunLoop: the kernel calls settle after the engine state is
// applied, through the real claim tool executor and ControlExecutor.

const runbookLoopWorker = "runbook-loop-worker"

type runbookLoopFixture struct {
	t        *testing.T
	ctx      context.Context
	store    *sqlite.Store
	scope    Scope
	control  *ControlExecutor
	tools    *StepToolDispatcher
	loop     *RunLoop
	producer *RunbookStepProducer
}

func newRunbookLoopFixture(t *testing.T, definition *runbook.Definition) *runbookLoopFixture {
	t.Helper()
	store := newTestKernelStore(t)
	f := &runbookLoopFixture{t: t, ctx: context.Background(), store: store, scope: Scope{Kind: "tenant", ID: "runbook-loop"}}
	collaboration := NewCollaborationService(store)
	f.control = &ControlExecutor{Portfolio: store, Sessions: store, Forks: NewRunForkCoordinator(store), Collaboration: collaboration}
	tools, err := NewStepToolDispatcher(StepToolDispatcherConfig{Sessions: store, Turns: store, Actions: store, Control: f.control})
	if err != nil {
		t.Fatal(err)
	}
	f.tools = tools
	f.producer = newTestRunbookProducer(t, definition, "manual", store)
	loop, err := NewRunLoop(store, nil, nil, RunLoopConfig{
		PlanToolEntries: func(binding *TurnRunnerBinding, session *AgentSession, step *AgentTurn, _ *SessionEntry, calls []LoopToolCall) ([]*SessionEntry, error) {
			return PlanToolCallEntries(binding, &AgentRun{ID: step.RunID, Scope: step.Scope}, step, session.ID, calls)
		},
		PlainTextFinal: f.control.PlainTextFinal,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.loop = loop
	return f
}

func (f *runbookLoopFixture) create(req CreateAgentRunRequest) *AgentRun {
	f.t.Helper()
	req.Scope = f.scope
	if req.Owner.ID == "" {
		req.Owner = ObjectiveOwner{Type: OwnerTypeAgent, ID: "manager"}
	}
	if req.AssignedAgentID == "" {
		req.AssignedAgentID = "manager"
	}
	if req.Source == "" {
		req.Source = RunSourceManual
	}
	run, err := NewPortfolioService(f.store).CreateAgentRun(f.ctx, req)
	if err != nil {
		f.t.Fatal(err)
	}
	return run
}

// drive claims the next queued Run of the scope (it must be one of wantIDs)
// and drives one claim on its run session.
func (f *runbookLoopFixture) drive(wantIDs ...string) (string, RunLoopExit) {
	f.t.Helper()
	now := time.Now().UTC()
	claimed, err := f.store.ClaimNextAgentRun(f.ctx, AgentRunClaim{Scope: f.scope, WorkerID: runbookLoopWorker, Now: now, LeaseDuration: time.Hour, AgingInterval: time.Minute})
	if err != nil || claimed == nil {
		f.t.Fatalf("claim = %#v, %v (want one of %v)", claimed, err, wantIDs)
	}
	wanted := false
	for _, id := range wantIDs {
		wanted = wanted || claimed.ID == id
	}
	if !wanted {
		f.t.Fatalf("claimed %s, want one of %v", claimed.ID, wantIDs)
	}
	session, err := f.store.GetAgentSession(f.ctx, f.scope, RunAgentSessionID(f.scope, claimed.ID))
	if err != nil {
		opened, createErr := f.store.CreateRunAgentSession(f.ctx, CreateRunAgentSessionRequest{
			Scope: f.scope, OwnerRunID: claimed.ID, AgentID: claimed.AssignedAgentID,
			Lease: AgentRunLeaseGuard{WorkerID: runbookLoopWorker, Now: now}, Now: now,
		})
		if createErr != nil {
			f.t.Fatal(createErr)
		}
		session = opened.Session
	}
	claimed = f.run(claimed.ID)
	binding := &TurnRunnerBinding{Producer: f.producer, Session: session, Tools: f.tools.ForClaim(LoopToolClaim{})}
	return claimed.ID, f.loop.Drive(f.ctx, RunLoopClaim{Run: claimed, WorkerID: runbookLoopWorker, Binding: binding})
}

func (f *runbookLoopFixture) run(id string) *AgentRun {
	f.t.Helper()
	run, err := f.store.GetAgentRun(f.ctx, f.scope, id)
	if err != nil {
		f.t.Fatal(err)
	}
	return run
}

func (f *runbookLoopFixture) toolEntries(runID string) []*SessionEntry {
	f.t.Helper()
	path, err := f.store.LoadAgentSessionPath(f.ctx, f.scope, RunAgentSessionID(f.scope, runID))
	if err != nil {
		f.t.Fatal(err)
	}
	var entries []*SessionEntry
	for _, entry := range path.Entries {
		if entry.Type == SessionEntryTool {
			entries = append(entries, entry)
		}
	}
	return entries
}

func TestRunbookForkSettlesAsKernelCallAndJoinsThroughTheLoop(t *testing.T) {
	f := newRunbookLoopFixture(t, concurrentRunbookDefinition())
	source := f.create(CreateAgentRunRequest{Goal: "parallel", Context: map[string]interface{}{"parallel": true}})
	if _, exit := f.drive(source.ID); exit.Err != nil {
		t.Fatalf("first claim: %#v", exit)
	}
	waiting := f.run(source.ID)
	if waiting.Status != AgentRunStatusWaitingForDependency || waiting.WakeCondition == nil || waiting.WakeCondition.Type != "run_dependencies" {
		t.Fatalf("source=%#v", waiting)
	}
	state, err := decodeRunbookState(waiting.Checkpoint)
	if err != nil || state.PendingFork != "fork" || state.PendingCall == "" {
		t.Fatalf("engine state was not applied before the fork: %#v err=%v", state, err)
	}
	trace, err := RunbookTraceFromCheckpoint(waiting.Checkpoint)
	if err != nil || len(trace.Entries) != 2 || trace.Entries[1].StepID != "fork" || trace.Entries[1].Status != RunbookStepTraceWaiting {
		t.Fatalf("source trace=%#v err=%v", trace, err)
	}
	entries := f.toolEntries(source.ID)
	if len(entries) != 1 || entries[0].Tool.Name != string(ControlToolRunbookFork) || entries[0].State != ToolEntryAwaitingChild ||
		entries[0].ToolCallID != state.PendingCall || len(entries[0].Tool.Directive) == 0 {
		t.Fatalf("fork entry=%#v", entries)
	}
	groupID := stableForkIdentifier(source.ID, "fork", "group")
	if waiting.WakeCondition.Reference != groupID {
		t.Fatalf("wake=%#v want group %s", waiting.WakeCondition, groupID)
	}
	children := map[string]string{}
	for _, branch := range []string{"a", "b"} {
		childID := stableForkIdentifier(source.ID, "fork", "child:"+branch)
		children[childID] = branch
		if child := f.run(childID); child.Checkpoint["forkChild"] == nil || child.Budget == nil {
			t.Fatalf("child %s=%#v", branch, child)
		}
	}
	pending := []string{}
	for id := range children {
		pending = append(pending, id)
	}
	for len(pending) > 0 {
		childID, exit := f.drive(pending...)
		if exit.Err != nil {
			t.Fatalf("child %s claim: %#v", childID, exit)
		}
		done := f.run(childID)
		if done.Status != AgentRunStatusCompleted || done.Output["branchId"] != children[childID] {
			t.Fatalf("child %s=%#v", childID, done)
		}
		// The worker resolves a terminal fork child (resolveForkChild).
		if _, err := f.control.Forks.CompleteChild(f.ctx, done, ActivityActor{Type: "worker", ID: runbookLoopWorker}); err != nil {
			t.Fatal(err)
		}
		for i, id := range pending {
			if id == childID {
				pending = append(pending[:i], pending[i+1:]...)
				break
			}
		}
	}
	if queued := f.run(source.ID); queued.Status != AgentRunStatusQueued {
		t.Fatalf("source after join=%#v", queued)
	}
	if _, exit := f.drive(source.ID); exit.Err != nil {
		t.Fatalf("resume claim: %#v", exit)
	}
	completed := f.run(source.ID)
	if completed.Status != AgentRunStatusCompleted || !reflect.DeepEqual(completed.Output["state"], map[string]interface{}{"a": true, "b": true}) {
		t.Fatalf("source completed=%#v", completed)
	}
	entries = f.toolEntries(source.ID)
	if len(entries) != 1 || entries[0].State != ToolEntryCompleted {
		t.Fatalf("fork entry after resume=%#v", entries)
	}
	trace, err = RunbookTraceFromCheckpoint(completed.Checkpoint)
	if err != nil || trace.Entries[1].Status != RunbookStepTraceSucceeded || trace.Entries[len(trace.Entries)-1].StepID != "done" {
		t.Fatalf("completed trace=%#v err=%v", trace, err)
	}
}

func TestRunbookDelegationSettlesAsKernelCallAndJoinsThroughTheLoop(t *testing.T) {
	definition := &runbook.Definition{APIVersion: runbook.APIVersion, ID: "parent", Version: "1", Name: "Parent", Entrypoints: map[string]string{"manual": "delegate"}, Steps: map[string]runbook.Step{
		"delegate": {Kind: runbook.StepDelegate, Delegate: &runbook.DelegateStep{
			AgentID: runbookLiteral("specialist"), Goal: runbookLiteral("Analyze the release"),
			Context: map[string]runbook.Value{"release": runbookLiteral("2026.07")}, Mode: runbook.DelegateReason, ResultPath: "/steps/delegate", Timeout: time.Minute,
			Budget: &runbook.BudgetAllocation{MaxTurns: 2, MaxDurationMS: 60000}, Next: "done",
		}},
		"done": {Kind: runbook.StepEnd, End: &runbook.EndStep{Outputs: map[string]runbook.Value{"answer": {Ref: "/steps/delegate/answer"}}}},
	}}
	f := newRunbookLoopFixture(t, definition)
	source := f.create(CreateAgentRunRequest{Goal: "delegate", Budget: &BudgetPolicy{MaxTurns: 6, MaxDurationMS: 180000}})
	if _, exit := f.drive(source.ID); exit.Err != nil {
		t.Fatalf("first claim: %#v", exit)
	}
	waiting := f.run(source.ID)
	if waiting.Status != AgentRunStatusWaitingForAgent {
		t.Fatalf("source=%#v", waiting)
	}
	state, err := decodeRunbookState(waiting.Checkpoint)
	if err != nil || state.PendingDelegation != "delegate" || state.PendingCall == "" {
		t.Fatalf("engine state was not applied before the delegation: %#v err=%v", state, err)
	}
	requestID := stableCollaborationID(f.scope, delegationIdempotencyKey(source.ID, "delegate"), "request")
	request, err := f.control.Collaboration.GetAgentRequest(f.ctx, f.scope, requestID)
	if err != nil || request.Recipient.ID != "specialist" || request.Goal != "Analyze the release" ||
		request.SharedContext["release"] != "2026.07" || request.BudgetAllocation == nil || request.BudgetAllocation.MaxTurns != 2 {
		t.Fatalf("request=%#v err=%v", request, err)
	}
	entries := f.toolEntries(source.ID)
	if len(entries) != 1 || entries[0].Tool.Name != string(ControlToolRunbookDelegate) || entries[0].State != ToolEntryAwaitingChild ||
		entries[0].Tool.Wait == nil || entries[0].Tool.Wait.AgentRequestID != requestID {
		t.Fatalf("delegate entry=%#v", entries)
	}
	// The specialist accepts and its child Run completes; the collaboration
	// service writes the result to the source and re-queues it.
	accepted, err := f.control.Collaboration.RespondAgentRequest(f.ctx, RespondAgentRequestRequest{
		Scope: f.scope, RequestID: requestID, ExpectedRevision: request.Revision, Decision: AgentRequestDecisionAccept,
		Principal: request.Recipient, Message: "On it.",
	})
	if err != nil || accepted.Child == nil {
		t.Fatalf("accept=%#v err=%v", accepted, err)
	}
	activity := NewRunActivityService(f.store, f.store)
	actor := ActivityActor{Type: "worker", ID: "child-worker"}
	running, _, err := activity.TransitionRun(f.ctx, f.scope, accepted.Child.ID, RunTransitionRequest{
		ExpectedRevision: accepted.Child.Revision, Status: AgentRunStatusRunning, Summary: "started", Actor: actor,
	})
	if err != nil {
		t.Fatal(err)
	}
	ended, _, err := activity.TransitionRun(f.ctx, f.scope, accepted.Child.ID, RunTransitionRequest{
		ExpectedRevision: running.Revision, Status: AgentRunStatusCompleted, Output: map[string]interface{}{"answer": "ship"}, Summary: "done", Actor: actor,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.control.Collaboration.ResolveTerminalAgentRequestChild(f.ctx, ended); err != nil {
		t.Fatal(err)
	}
	queued := f.run(source.ID)
	if queued.Status != AgentRunStatusQueued {
		t.Fatalf("source after child=%#v", queued)
	}
	if results, _ := queued.Output["collaborationResults"].(map[string]interface{}); results[requestID] == nil {
		t.Fatalf("collaboration results=%#v", queued.Output)
	}
	if _, exit := f.drive(source.ID); exit.Err != nil {
		t.Fatalf("resume claim: %#v", exit)
	}
	completed := f.run(source.ID)
	if completed.Status != AgentRunStatusCompleted || completed.Output["answer"] != "ship" {
		t.Fatalf("source completed=%#v", completed)
	}
	if entries := f.toolEntries(source.ID); len(entries) != 1 || entries[0].State != ToolEntryCompleted {
		t.Fatalf("delegate entry after resume=%#v", entries)
	}
}

func TestRunbookKernelCallRefusesStateItWasNotPlannedFor(t *testing.T) {
	f := newRunbookLoopFixture(t, concurrentRunbookDefinition())
	cc := ControlContext{
		Run:     &AgentRun{ID: "run", Checkpoint: map[string]interface{}{"runbook": map[string]interface{}{"current": "fork", "pendingFork": "fork", "pendingCall": "runbook.2.fork"}}},
		Binding: &TurnRunnerBinding{Producer: f.producer}, Origin: ControlOriginKernel,
		Entry:     &SessionEntry{ToolCallID: "runbook.9.fork"},
		Arguments: &runbookKernelArgs{Step: "fork", Visit: 9},
	}
	pending := func(s runbookExecutionState) string { return s.PendingFork }
	if _, _, _, err := runbookKernelCall(cc, pending); err == nil {
		t.Fatal("a stale kernel call settled against newer engine state")
	}
	cc.Entry.ToolCallID = "runbook.2.fork"
	if _, _, _, err := runbookKernelCall(cc, pending); err != nil {
		t.Fatalf("planned call refused: %v", err)
	}
	cc.Origin = ControlOriginModel
	if _, _, _, err := runbookKernelCall(cc, pending); err == nil {
		t.Fatal("a model call reached the runbook engine")
	}
	cc.Origin, cc.Entry = ControlOriginKernel, nil
	if _, _, _, err := runbookKernelCall(cc, pending); err == nil {
		t.Fatal("a kernel call without its tool entry settled")
	}
	result, err := (runbookForkHandler{}).Run(f.ctx, f.control, cc)
	if err == nil || result.Entry != nil {
		t.Fatalf("an entry-less fork call was not refused: %#v", result)
	}
}

func TestRunbookProducerKeepsModelMediaAndOversizeOutputOutOfCheckpoint(t *testing.T) {
	definition := &runbook.Definition{
		APIVersion: runbook.APIVersion, ID: "shots", Version: "1", Name: "Shots", Entrypoints: map[string]string{"daily": "shot"},
		Steps: map[string]runbook.Step{
			"shot": {Kind: runbook.StepAction, Action: &runbook.ActionStep{
				SkillID: "browser", SkillVersion: "1", Action: "screenshot", ResultPath: "/results/shot", Next: "dump",
			}},
			"dump": {Kind: runbook.StepAction, Action: &runbook.ActionStep{
				SkillID: "browser", SkillVersion: "1", Action: "dump", ResultPath: "/results/dump", Next: "done",
			}},
			"done": {Kind: runbook.StepEnd, End: &runbook.EndStep{}},
		},
	}
	calls := fixedActionCalls{}
	producer := newTestRunbookProducer(t, definition, "daily", calls)
	binding := &TurnRunnerBinding{ModelActions: []capability.ModelAction{
		{Name: "browser_screenshot", SkillID: "browser", Version: "1", Action: "screenshot"},
		{Name: "browser_dump", SkillID: "browser", Version: "1", Action: "dump"},
	}}
	run := &AgentRun{ID: "run-shots", Context: map[string]interface{}{}}
	path := &AgentSessionPath{}
	step := func(sequence int64) StepOutput {
		out := produceRunbook(t, producer, runbookInput(run, sequence, binding, path))
		run.Checkpoint = out.KernelState
		return out
	}
	image := strings.Repeat("iVBORw0KGgo", 20000)
	screenshot := map[string]interface{}{
		"url": "https://example.test",
		"modelMedia": map[string]interface{}{
			"images": []interface{}{map[string]interface{}{"mediaType": "image/png", "contentBase64": image}},
		},
	}
	shot := step(1)
	calls["shot-call"] = &ActionCall{ID: "shot-call", RunID: run.ID, Status: ActionCallStatusSucceeded, Output: screenshot}
	appendRunbookToolResult(path, run.ID, shot.ToolCalls[0].ID, ToolEntryCompleted, "shot-call")
	dump := step(2)
	if len(dump.ToolCalls) != 1 || dump.ToolCalls[0].Name != "browser_dump" {
		t.Fatalf("dump=%#v", dump)
	}
	if _, ok := screenshot["modelMedia"]; !ok {
		t.Fatal("the stored ActionCall output was mutated")
	}
	rows := make([]interface{}, 5000)
	for index := range rows {
		rows[index] = map[string]interface{}{"index": index, "text": strings.Repeat("x", 100)}
	}
	calls["dump-call"] = &ActionCall{ID: "dump-call", RunID: run.ID, Status: ActionCallStatusSucceeded, Output: map[string]interface{}{"rows": rows}}
	appendRunbookToolResult(path, run.ID, dump.ToolCalls[0].ID, ToolEntryCompleted, "dump-call")
	if done := step(3); done.Final == nil || done.Final.Status != AgentRunStatusCompleted {
		t.Fatalf("done=%#v", done)
	}
	encoded, err := json.Marshal(run.Checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "contentBase64") || strings.Contains(string(encoded), "modelMedia") {
		t.Fatal("model media reached the runbook checkpoint")
	}
	if len(encoded) > 2*runbookActionResultLimit {
		t.Fatalf("checkpoint is %d bytes", len(encoded))
	}
	results, _ := run.Checkpoint["results"].(map[string]interface{})
	shotResult, _ := results["shot"].(map[string]interface{})
	dumpResult, _ := results["dump"].(map[string]interface{})
	if shotResult["url"] != "https://example.test" || dumpResult["truncated"] != true || dumpResult["available"] != true {
		t.Fatalf("results=%#v", results)
	}
}

func TestRunbookJoinAnyForkJoinsFirstChildThroughTheLoopAndCancelsTheOther(t *testing.T) {
	definition := &runbook.Definition{APIVersion: runbook.APIVersion, ID: "race", Version: "1", Name: "Race", Entrypoints: map[string]string{"manual": "fork"}, Steps: map[string]runbook.Step{
		"fork": {Kind: runbook.StepFork, Fork: &runbook.ForkStep{Branches: map[string]string{"fast": "fast", "slow": "slow"}, Join: "join"}},
		"fast": {Kind: runbook.StepTransform, Transform: &runbook.TransformStep{Assignments: map[string]runbook.Value{"/state/fast": runbookLiteral(true)}, Next: "join"}},
		"slow": {Kind: runbook.StepWait, Wait: &runbook.WaitStep{Duration: time.Hour, Next: "join"}},
		"join": {Kind: runbook.StepJoin, Join: &runbook.JoinStep{Fork: "fork", Mode: runbook.JoinAny, Next: "done"}},
		"done": {Kind: runbook.StepEnd, End: &runbook.EndStep{Outputs: map[string]runbook.Value{"state": {Ref: "/state"}}}},
	}}
	f := newRunbookLoopFixture(t, definition)
	source := f.create(CreateAgentRunRequest{Goal: "race"})
	if _, exit := f.drive(source.ID); exit.Err != nil {
		t.Fatalf("first claim: %#v", exit)
	}
	if waiting := f.run(source.ID); waiting.Status != AgentRunStatusWaitingForDependency {
		t.Fatalf("source=%#v", waiting)
	}
	fastID := stableForkIdentifier(source.ID, "fork", "child:fast")
	slowID := stableForkIdentifier(source.ID, "fork", "child:slow")
	// Both children are queued; whichever is claimed first, the slow one
	// parks on its timer and the fast one completes.
	for range 2 {
		childID, exit := f.drive(fastID, slowID)
		if exit.Err != nil {
			t.Fatalf("child %s claim: %#v", childID, exit)
		}
	}
	if slow := f.run(slowID); slow.Status != AgentRunStatusSleeping {
		t.Fatalf("slow=%#v", slow)
	}
	fast := f.run(fastID)
	if fast.Status != AgentRunStatusCompleted {
		t.Fatalf("fast=%#v", fast)
	}
	if _, err := f.control.Forks.CompleteChild(f.ctx, fast, ActivityActor{Type: "worker", ID: runbookLoopWorker}); err != nil {
		t.Fatal(err)
	}
	if queued := f.run(source.ID); queued.Status != AgentRunStatusQueued {
		t.Fatalf("source after first child=%#v", queued)
	}
	if slow := f.run(slowID); slow.Status != AgentRunStatusCanceled {
		t.Fatalf("losing child was not canceled: %#v", slow)
	}
	if _, exit := f.drive(source.ID); exit.Err != nil {
		t.Fatalf("resume claim: %#v", exit)
	}
	completed := f.run(source.ID)
	if completed.Status != AgentRunStatusCompleted || !reflect.DeepEqual(completed.Output["state"], map[string]interface{}{"fast": true}) {
		t.Fatalf("source completed=%#v", completed)
	}
	if entries := f.toolEntries(source.ID); len(entries) != 1 || entries[0].State != ToolEntryCompleted {
		t.Fatalf("fork entry after resume=%#v", entries)
	}
}
